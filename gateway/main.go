// codeint-gateway — one OpenAI-compatible API in front of every model.
//
//	OpenAI SDK / Cursor / Continue / Cline / curl
//	              │  /v1/chat/completions
//	              ▼
//	      codeint-gateway  (Go, zero dependencies)
//	              │
//	   ┌──────────┼──────────┬─────────────┐
//	   ▼          ▼          ▼             ▼
//	 OpenAI   Anthropic   Gemini   any OpenAI-compatible
//	                                 (Ollama, OpenRouter, …)
//
// Run:  go run . -config config.json
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const version = "1.0.0"

type App struct {
	cfg    *Config
	client *http.Client
	keys   []string
}

func main() {
	configPath := flag.String("config", "config.json", "path to the gateway config file")
	flag.Parse()
	if envPath := os.Getenv("GATEWAY_CONFIG"); envPath != "" && *configPath == "config.json" {
		*configPath = envPath
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("codeint-gateway: %v", err)
	}
	if host := os.Getenv("GATEWAY_HOST"); host != "" {
		cfg.Host = host
	}
	if port := os.Getenv("GATEWAY_PORT"); port != "" {
		cfg.Port = atoi(port, cfg.Port)
	}

	app := &App{
		cfg: cfg,
		client: &http.Client{
			Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
		},
	}
	for _, key := range append([]string{cfg.APIKey}, cfg.APIKeys...) {
		if key = strings.TrimSpace(key); key != "" {
			app.keys = append(app.keys, key)
		}
	}

	server := &http.Server{
		Addr:              cfg.Host + ":" + itoa(cfg.Port),
		Handler:           app.logRequests(app.routes()),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	addr := "http://" + server.Addr
	log.Printf("codeint-gateway v%s — %d providers, %d named models", version, len(cfg.Providers), len(cfg.Models))
	for name, prov := range cfg.Providers {
		keyState := "no key"
		if prov.key() != "" {
			keyState = "key set"
		}
		log.Printf("  provider %-12s %-10s %s (%s)", name, prov.kind(), prov.BaseURL, keyState)
	}
	if len(app.keys) == 0 {
		log.Printf("warning: no api_key configured — anyone who can reach this port can use it")
	}
	log.Printf("listening on %s/v1", addr)

	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("codeint-gateway: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
	log.Printf("codeint-gateway stopped")
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeError(w, http.StatusNotFound, "not_found", "Unknown endpoint: "+r.URL.Path, "invalid_request_error")
			return
		}
		a.handleHome(w, r)
	})
	mux.HandleFunc("/health", a.handleHealth)
	mux.HandleFunc("/v1", func(w http.ResponseWriter, r *http.Request) {
		if !a.authorized(r) {
			a.unauthorized(w)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"service": "codeint-gateway",
			"version": version,
			"endpoints": []string{
				"GET  /v1/models",
				"POST /v1/chat/completions",
			},
		})
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !a.authorized(r) {
			a.unauthorized(w)
			return
		}
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use GET", "invalid_request_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data":   a.cfg.listModels(),
		})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			a.preflight(w)
			return
		}
		if !a.authorized(r) {
			a.unauthorized(w)
			return
		}
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use POST", "invalid_request_error")
			return
		}
		a.handleChat(w, r)
	})
	return mux
}

func (a *App) handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "Could not read request body", "invalid_request_error")
		return
	}

	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON body: "+err.Error(), "invalid_request_error")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "messages must be a non-empty array", "invalid_request_error")
		return
	}

	entry, err := a.cfg.resolveModel(req.Model)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	prov := a.cfg.Providers[entry.Provider]

	displayModel := req.Model
	if strings.TrimSpace(displayModel) == "" {
		displayModel = entry.ID
	}

	start := time.Now()
	var handleErr error
	switch prov.kind() {
	case "openai":
		handleErr = a.chatOpenAI(w, r, body, &req, entry, displayModel)
	case "anthropic":
		handleErr = a.chatAnthropic(w, r, &req, entry, displayModel)
	case "gemini":
		handleErr = a.chatGemini(w, r, &req, entry, displayModel)
	default:
		handleErr = badRequest("provider %q has unsupported type %q", entry.Provider, prov.Type)
	}

	if handleErr != nil {
		log.Printf("[error] %s → %s/%s: %v", displayModel, entry.Provider, entry.Model, handleErr)
		writeAPIError(w, handleErr)
		return
	}
	log.Printf("[chat] %-24s → %-10s %-28s stream=%-5v %s",
		displayModel, entry.Provider, entry.Model, req.Stream, time.Since(start).Round(time.Millisecond))
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	providers := make([]string, 0, len(a.cfg.Providers))
	for name, prov := range a.cfg.Providers {
		providers = append(providers, name+" ("+prov.kind()+")")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"service":   "codeint-gateway",
		"version":   version,
		"providers": providers,
		"models":    len(a.cfg.listModels()),
		"time":      time.Now().UTC().Format(time.RFC3339),
	})
}

func (a *App) handleHome(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if host == "" {
		host = "127.0.0.1:" + itoa(a.cfg.Port)
	}
	base := "http://" + host + "/v1"
	key := "<YOUR_API_KEY>"
	if len(a.keys) == 0 {
		key = "(no key configured)"
	}
	models := a.cfg.listModels()
	sample := "provider/model"
	if len(models) > 0 {
		sample = models[0].ID
	}

	w.Header().Set("content-type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>codeint-gateway</title>
<style>body{font:15px/1.65 ui-monospace,monospace;max-width:840px;margin:40px auto;padding:0 20px;color:#ddd;background:#111}h1{color:#7ee787}pre{background:#1c1c1c;padding:14px;border-radius:8px;overflow:auto}a{color:#79c0ff}code{color:#ffd479}</style>
</head><body>
<h1>codeint-gateway v`+version+`</h1>
<p>One OpenAI-compatible API for every model — OpenAI, Anthropic, Gemini, Ollama, OpenRouter and anything else that speaks the OpenAI protocol.</p>
<p>Base URL: <code>`+base+`</code> &nbsp;·&nbsp; <a href="/health">/health</a> &nbsp;·&nbsp; <a href="/v1/models">/v1/models</a></p>
<h3>Try it</h3><pre>curl `+base+`/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer `+key+`" \
  -d '{"model":"`+sample+`","messages":[{"role":"user","content":"Say PONG"}]}'</pre>
<h3>Python</h3><pre>python3 py/codeint.py chat "Say PONG"
python3 py/codeint.py models</pre>
<p>Free and open source. Built for students and developers.</p>
</body></html>`)
}

func (a *App) preflight(w http.ResponseWriter) {
	h := w.Header()
	h.Set("access-control-allow-origin", "*")
	h.Set("access-control-allow-methods", "GET, POST, OPTIONS")
	h.Set("access-control-allow-headers", "authorization, content-type, x-api-key, anthropic-version")
	h.Set("access-control-max-age", "86400")
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) authorized(r *http.Request) bool {
	if len(a.keys) == 0 {
		return true
	}
	supplied := ""
	if header := r.Header.Get("authorization"); strings.HasPrefix(strings.ToLower(header), "bearer ") {
		supplied = strings.TrimSpace(header[7:])
	}
	if supplied == "" {
		supplied = r.Header.Get("x-api-key")
	}
	for _, key := range a.keys {
		if subtle.ConstantTimeCompare([]byte(supplied), []byte(key)) == 1 {
			return true
		}
	}
	return false
}

func (a *App) unauthorized(w http.ResponseWriter) {
	writeError(w, http.StatusUnauthorized,
		"invalid_api_key",
		"Invalid or missing API key. Send Authorization: Bearer <API_KEY>.",
		"invalid_request_error")
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (a *App) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		if r.URL.Path != "/health" && r.URL.Path != "/" {
			log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
		}
	})
}

func atoi(s string, fallback int) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return fallback
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return fallback
	}
	return n
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
