package main

// OpenAI / OpenAI-compatible adapter: the request is forwarded almost
// untouched, only `model` is rewritten. This covers OpenAI, OpenRouter, Groq,
// DeepSeek, Together, vLLM, Ollama, LM Studio, and the local codeint-api
// gateway itself.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

func (a *App) chatOpenAI(w http.ResponseWriter, r *http.Request, rawBody []byte, req *ChatRequest, entry *ModelEntry, displayModel string) error {
	prov := a.cfg.Providers[entry.Provider]

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return badRequest("Invalid JSON body: %v", err)
	}
	modelJSON, _ := json.Marshal(entry.Model)
	payload["model"] = modelJSON
	body, err := json.Marshal(payload)
	if err != nil {
		return badRequest("could not re-encode request: %v", err)
	}

	upstream, err := a.post(r, prov, prov.BaseURL+"/chat/completions", body)
	if err != nil {
		return err
	}
	defer upstream.Body.Close()

	if upstream.StatusCode != http.StatusOK {
		return upstreamError(upstream)
	}

	if !req.Stream {
		var out map[string]json.RawMessage
		if err := json.NewDecoder(upstream.Body).Decode(&out); err != nil {
			return upstreamFailed(http.StatusBadGateway, "upstream sent an unreadable response: %v", err)
		}
		out["model"] = mustJSON(displayModel)
		writeJSON(w, http.StatusOK, out)
		return nil
	}

	sse := newSSEWriter(w)
	doneSent := false
	readErr := readSSE(upstream.Body, func(event, data string) error {
		if strings.TrimSpace(data) == "" {
			return nil
		}
		if strings.TrimSpace(data) == "[DONE]" {
			sse.done()
			doneSent = true
			return nil
		}
		var frame map[string]json.RawMessage
		if json.Unmarshal([]byte(data), &frame) != nil {
			sse.data(data) // not JSON, forward verbatim
			return nil
		}
		frame["model"] = mustJSON(displayModel)
		sse.jsonChunk(frame)
		return nil
	})
	if readErr != nil && !doneSent {
		sse.jsonChunk(map[string]any{
			"error": errorBody{Message: readErr.Error(), Type: "upstream_error", Code: "stream_interrupted"},
		})
	}
	if !doneSent {
		sse.done()
	}
	return nil
}

// upstreamError turns an upstream non-2xx response into an apiError.
func upstreamError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	_ = json.Unmarshal(body, &parsed)

	message := parsed.Error.Message
	if message == "" {
		message = parsed.Message
	}
	if message == "" {
		message = parsed.Detail
	}
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = resp.Status
	}
	if len(message) > 900 {
		message = message[:900] + "…"
	}

	status := resp.StatusCode
	// Never hand a client our upstream's 401/403: that would look like *our*
	// key is wrong. It is a server-side configuration problem.
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		status = http.StatusBadGateway
	}
	return upstreamFailed(status, "%s", message)
}

func (a *App) post(r *http.Request, prov *Provider, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	for k, v := range prov.Headers {
		req.Header.Set(k, v)
	}
	applyAuth(req, prov)
	resp, err := a.client.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			return nil, upstreamFailed(http.StatusRequestTimeout, "client cancelled the request")
		}
		return nil, upstreamFailed(http.StatusBadGateway, "could not reach %s: %v", prov.BaseURL, err)
	}
	return resp, nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
