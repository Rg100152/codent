package main

// Shared helpers: OpenAI-style request/response types, JSON + SSE writers,
// and a tiny SSE reader used by the Anthropic and Gemini adapters.

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

/* ------------------------------------------------------------ OpenAI types */

// ChatRequest is the subset of the OpenAI chat body we understand. Unknown
// fields are preserved separately when a request is forwarded verbatim.
type ChatRequest struct {
	Model       string          `json:"model"`
	Messages    []Message       `json:"messages"`
	Stream      bool            `json:"stream"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	MaxCompTok  *int            `json:"max_completion_tokens,omitempty"`
	Stop        json.RawMessage `json:"stop,omitempty"`
	Tools       []Tool          `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
}

type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

/* --------------------------------------------------------------- models API */

type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

/* ------------------------------------------------------------------ errors */

type apiError struct {
	Status  int
	Message string
	Code    string
	Type    string
}

func (e *apiError) Error() string { return e.Message }

func badRequest(format string, a ...any) *apiError {
	return &apiError{
		Status:  http.StatusBadRequest,
		Message: fmt.Sprintf(format, a...),
		Code:    "invalid_request_error",
		Type:    "invalid_request_error",
	}
}

func upstreamFailed(status int, format string, a ...any) *apiError {
	if status < 400 {
		status = http.StatusBadGateway
	}
	return &apiError{
		Status:  status,
		Message: fmt.Sprintf(format, a...),
		Code:    "upstream_error",
		Type:    "upstream_error",
	}
}

/* ------------------------------------------------------------- HTTP output */

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":{"message":"internal encoding error"}}`, http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("content-type", "application/json; charset=utf-8")
	h.Set("access-control-allow-origin", "*")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   any    `json:"param"`
	Code    string `json:"code"`
}

func writeError(w http.ResponseWriter, status int, code, message, kind string) {
	if kind == "" {
		kind = "invalid_request_error"
	}
	writeJSON(w, status, map[string]any{
		"error": errorBody{Message: message, Type: kind, Param: nil, Code: code},
	})
}

func writeAPIError(w http.ResponseWriter, err error) {
	if e, ok := err.(*apiError); ok {
		writeError(w, e.Status, e.Code, e.Message, e.Type)
		return
	}
	writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "server_error")
}

/* -------------------------------------------------------------- SSE output */

type sseWriter struct {
	w  http.ResponseWriter
	fl http.Flusher
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	h := w.Header()
	h.Set("content-type", "text/event-stream; charset=utf-8")
	h.Set("cache-control", "no-cache, no-transform")
	h.Set("connection", "keep-alive")
	h.Set("x-accel-buffering", "no")
	h.Set("access-control-allow-origin", "*")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	return &sseWriter{w: w, fl: fl}
}

func (s *sseWriter) flush() {
	if s.fl != nil {
		s.fl.Flush()
	}
}

func (s *sseWriter) data(payload string) {
	_, _ = io.WriteString(s.w, "data: "+payload+"\n\n")
	s.flush()
}

func (s *sseWriter) jsonChunk(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.data(string(b))
}

func (s *sseWriter) event(name string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, b)
	s.flush()
}

func (s *sseWriter) done() {
	s.data("[DONE]")
}

// completionChunk builds one OpenAI `chat.completion.chunk` frame.
func completionChunk(id, model string, delta map[string]any, finishReason any) map[string]any {
	if delta == nil {
		delta = map[string]any{}
	}
	return map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
}

func textChunk(id, model, text string) map[string]any {
	return completionChunk(id, model, map[string]any{"content": text}, nil)
}

// finalChunk closes a stream with a finish_reason.
func finalChunk(id, model, reason string) map[string]any {
	return completionChunk(id, model, map[string]any{}, reason)
}

/* --------------------------------------------------------------- utilities */

func nowUnix() int64 { return time.Now().Unix() }

func newID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// contentText flattens OpenAI message content (string or part array) to text.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			switch {
			case p.Text != "":
				out = append(out, p.Text)
			case p.Type == "image_url" || p.Type == "image" || p.Type == "input_image":
				out = append(out, "[image omitted]")
			}
		}
		return strings.Join(out, "\n")
	}
	return ""
}

func stopSequences(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil && one != "" {
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many
	}
	return nil
}

func toolChoiceMode(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

func trimBase(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), "/")
}

/* ---------------------------------------------------------------- SSE input */

// readSSE parses an upstream event stream, calling fn for each frame.
func readSSE(r io.Reader, fn func(event string, data string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	event, data := "", ""
	flush := func() error {
		if event == "" && data == "" {
			return nil
		}
		e, d := event, data
		event, data = "", ""
		return fn(e, d)
	}

	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			chunk := strings.TrimPrefix(line, "data:")
			chunk = strings.TrimPrefix(chunk, " ")
			if data != "" {
				data += "\n"
			}
			data += chunk
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return flush()
}
