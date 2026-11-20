package main

// Config loading + model resolution.
//
// One config file describes every upstream you want to use. Model names come
// in two flavours:
//
//	"fast"                → an alias defined under "models"
//	"provider/model-id"   → straight passthrough, no config entry needed
//
// Strings support ${ENV_VAR} and ${ENV_VAR:-fallback}, so API keys never have
// to be written into the file itself.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// expandEnv replaces ${VAR} / ${VAR:-default} inside the raw config text.
func expandEnv(text string) string {
	return envPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := envPattern.FindStringSubmatch(match)
		if value, ok := os.LookupEnv(parts[1]); ok && value != "" {
			return value
		}
		return parts[2]
	})
}

type Provider struct {
	// Type selects the wire protocol: openai | anthropic | gemini.
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`
	// APIKey can be inline or "${ENV_NAME}". APIKeyEnv is a shortcut.
	APIKey    string            `json:"api_key,omitempty"`
	APIKeyEnv string            `json:"api_key_env,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	// Models is a static list shown by GET /v1/models. Optional: for the
	// "openai" type the gateway also asks the upstream for its own list.
	Models []string `json:"models,omitempty"`
}

func (p *Provider) key() string {
	if p.APIKey != "" {
		return p.APIKey
	}
	if p.APIKeyEnv != "" {
		return os.Getenv(p.APIKeyEnv)
	}
	return ""
}

func (p *Provider) kind() string {
	switch strings.ToLower(strings.TrimSpace(p.Type)) {
	case "", "openai", "openai-compatible", "oai":
		return "openai"
	case "anthropic", "claude":
		return "anthropic"
	case "gemini", "google":
		return "gemini"
	default:
		return strings.ToLower(strings.TrimSpace(p.Type))
	}
}

// ModelEntry is one client-facing model name and where it really goes.
type ModelEntry struct {
	ID       string   `json:"id"`
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	Aliases  []string `json:"aliases,omitempty"`
}

type Config struct {
	Host           string               `json:"host"`
	Port           int                  `json:"port"`
	APIKey         string               `json:"api_key,omitempty"`
	APIKeys        []string             `json:"api_keys,omitempty"`
	DefaultModel   string               `json:"default_model,omitempty"`
	TimeoutSeconds int                  `json:"timeout_seconds,omitempty"`
	Providers      map[string]*Provider `json:"providers"`
	Models         []*ModelEntry        `json:"models,omitempty"`

	index map[string]*ModelEntry
	mu    sync.Mutex
	cache map[string]cachedModels
}

type cachedModels struct {
	at     time.Time
	models []string
}

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{}
	if err := json.Unmarshal([]byte(expandEnv(string(raw))), cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.Port == 0 {
		cfg.Port = 10001
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 300
	}
	if len(cfg.Providers) == 0 {
		return nil, fmt.Errorf("config %s has no providers", path)
	}
	for name, prov := range cfg.Providers {
		if prov == nil {
			return nil, fmt.Errorf("provider %q is empty", name)
		}
		if trimBase(prov.BaseURL) == "" {
			return nil, fmt.Errorf("provider %q needs base_url", name)
		}
		switch prov.kind() {
		case "openai", "anthropic", "gemini":
		default:
			return nil, fmt.Errorf("provider %q has unknown type %q (use openai, anthropic or gemini)", name, prov.Type)
		}
		prov.BaseURL = trimBase(prov.BaseURL)
	}
	for _, model := range cfg.Models {
		if model.ID == "" || model.Provider == "" {
			return nil, fmt.Errorf("every models[] entry needs id and provider")
		}
		if _, ok := cfg.Providers[model.Provider]; !ok {
			return nil, fmt.Errorf("model %q points to unknown provider %q", model.ID, model.Provider)
		}
		if model.Model == "" {
			model.Model = model.ID
		}
	}

	cfg.index = map[string]*ModelEntry{}
	for _, model := range cfg.Models {
		cfg.index[model.ID] = model
		for _, alias := range model.Aliases {
			if alias != "" {
				cfg.index[alias] = model
			}
		}
	}
	cfg.cache = map[string]cachedModels{}
	return cfg, nil
}

// resolveModel maps a client-facing model name to a provider + upstream id.
func (c *Config) resolveModel(name string) (*ModelEntry, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		if c.DefaultModel != "" {
			name = c.DefaultModel
		} else if len(c.Models) > 0 {
			return c.Models[0], nil
		} else {
			return nil, badRequest("No model given and no default_model configured")
		}
	}
	if model, ok := c.index[name]; ok {
		return model, nil
	}
	if i := strings.Index(name, "/"); i > 0 {
		providerName, upstreamID := name[:i], name[i+1:]
		if _, ok := c.Providers[providerName]; ok && upstreamID != "" {
			return &ModelEntry{ID: name, Provider: providerName, Model: upstreamID}, nil
		}
	}
	return nil, badRequest("Unknown model %q. Call GET /v1/models for the available IDs, or use \"<provider>/<model>\".",
		name)
}

// modelIDs lists everything we advertise (used in error messages).
func (c *Config) modelIDs() []string {
	ids := make([]string, 0, len(c.index))
	for id := range c.index {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// listModels merges configured models with live discovery from providers.
func (c *Config) listModels() []modelObject {
	now := time.Now()
	seen := map[string]bool{}
	out := make([]modelObject, 0, 32)

	add := func(id, owner string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, modelObject{
			ID:      id,
			Object:  "model",
			Created: now.Unix(),
			OwnedBy: owner,
		})
	}

	for _, model := range c.Models {
		add(model.ID, model.Provider)
	}

	names := make([]string, 0, len(c.Providers))
	for name := range c.Providers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		prov := c.Providers[name]
		for _, id := range prov.Models {
			add(name+"/"+id, name)
		}
		for _, id := range c.discovered(name, prov) {
			add(name+"/"+id, name)
		}
	}
	return out
}

// discovered returns the upstream model list, cached for five minutes.
// Failures are ignored: /v1/models must work even when a provider is down.
func (c *Config) discovered(name string, prov *Provider) []string {
	c.mu.Lock()
	entry, ok := c.cache[name]
	c.mu.Unlock()
	if ok && time.Since(entry.at) < 5*time.Minute {
		return entry.models
	}

	models := fetchProviderModels(prov)

	c.mu.Lock()
	c.cache[name] = cachedModels{at: time.Now(), models: models}
	c.mu.Unlock()
	return models
}

func fetchProviderModels(prov *Provider) []string {
	client := &http.Client{Timeout: 5 * time.Second}
	var endpoint string
	switch prov.kind() {
	case "openai":
		endpoint = prov.BaseURL + "/models"
	case "gemini":
		endpoint = prov.BaseURL + "/models"
	default:
		return nil // Anthropic has no public list endpoint.
	}

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("accept", "application/json")
	applyAuth(req, prov)

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	if prov.kind() == "gemini" {
		var body struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if json.NewDecoder(resp.Body).Decode(&body) != nil {
			return nil
		}
		ids := make([]string, 0, len(body.Models))
		for _, m := range body.Models {
			ids = append(ids, strings.TrimPrefix(m.Name, "models/"))
		}
		return ids
	}

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return nil
	}
	ids := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

// applyAuth sets whichever credential header the provider protocol expects.
func applyAuth(req *http.Request, prov *Provider) {
	key := prov.key()
	if key == "" {
		return
	}
	switch prov.kind() {
	case "anthropic":
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	case "gemini":
		req.Header.Set("x-goog-api-key", key)
	default:
		req.Header.Set("authorization", "Bearer "+key)
	}
}
