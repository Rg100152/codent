package main

// Anthropic adapter: converts OpenAI chat requests to /v1/messages and back
// (including tools and streaming).

import (
	"encoding/json"
	"net/http"
	"strings"
)

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        string             `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Stream        bool               `json:"stream,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    any                `json:"tool_choice,omitempty"`
}

func buildAnthropicRequest(req *ChatRequest, upstreamModel string) (*anthropicRequest, error) {
	out := &anthropicRequest{
		Model:         upstreamModel,
		MaxTokens:     4096,
		Stream:        req.Stream,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: stopSequences(req.Stop),
	}
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		out.MaxTokens = *req.MaxTokens
	} else if req.MaxCompTok != nil && *req.MaxCompTok > 0 {
		out.MaxTokens = *req.MaxCompTok
	}

	var systemParts []string

	appendBlocks := func(role string, blocks []anthropicBlock) {
		if len(blocks) == 0 {
			return
		}
		n := len(out.Messages)
		if n > 0 && out.Messages[n-1].Role == role {
			out.Messages[n-1].Content = append(out.Messages[n-1].Content, blocks...)
			return
		}
		out.Messages = append(out.Messages, anthropicMessage{Role: role, Content: blocks})
	}

	for _, msg := range req.Messages {
		switch strings.ToLower(msg.Role) {
		case "system", "developer":
			if text := contentText(msg.Content); text != "" {
				systemParts = append(systemParts, text)
			}

		case "assistant":
			var blocks []anthropicBlock
			if text := contentText(msg.Content); text != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: text})
			}
			for _, call := range msg.ToolCalls {
				args := strings.TrimSpace(call.Function.Arguments)
				if args == "" || !json.Valid([]byte(args)) {
					args = "{}"
				}
				id := call.ID
				if id == "" {
					id = newID("toolu_")
				}
				blocks = append(blocks, anthropicBlock{
					Type:  "tool_use",
					ID:    id,
					Name:  call.Function.Name,
					Input: json.RawMessage(args),
				})
			}
			appendBlocks("assistant", blocks)

		case "tool", "function":
			appendBlocks("user", []anthropicBlock{{
				Type:      "tool_result",
				ToolUseID: msg.ToolCallID,
				Content:   contentText(msg.Content),
			}})

		default:
			if text := contentText(msg.Content); text != "" {
				appendBlocks("user", []anthropicBlock{{Type: "text", Text: text}})
			}
		}
	}

	if len(out.Messages) == 0 {
		return nil, badRequest("messages must contain at least one user or assistant message")
	}
	out.System = strings.Join(systemParts, "\n\n")

	for _, tool := range req.Tools {
		if tool.Type != "" && tool.Type != "function" {
			continue
		}
		schema := tool.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, anthropicTool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			InputSchema: schema,
		})
	}

	switch toolChoiceMode(req.ToolChoice) {
	case "auto":
		out.ToolChoice = map[string]string{"type": "auto"}
	case "required", "any":
		out.ToolChoice = map[string]string{"type": "any"}
	case "none":
		out.Tools = nil
	}
	if len(out.Tools) == 0 {
		out.ToolChoice = nil
	}
	return out, nil
}

type anthropicResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func anthropicFinishReason(reason string) string {
	switch reason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

func (a *App) chatAnthropic(w http.ResponseWriter, r *http.Request, req *ChatRequest, entry *ModelEntry, displayModel string) error {
	prov := a.cfg.Providers[entry.Provider]

	payload, err := buildAnthropicRequest(req, entry.Model)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return badRequest("could not encode request: %v", err)
	}

	upstream, err := a.post(r, prov, prov.BaseURL+"/messages", body)
	if err != nil {
		return err
	}
	defer upstream.Body.Close()
	if upstream.StatusCode != http.StatusOK {
		return upstreamError(upstream)
	}

	if !req.Stream {
		var parsed anthropicResponse
		if err := json.NewDecoder(upstream.Body).Decode(&parsed); err != nil {
			return upstreamFailed(http.StatusBadGateway, "upstream sent an unreadable response: %v", err)
		}
		if parsed.Error != nil {
			return upstreamFailed(http.StatusBadGateway, "%s", parsed.Error.Message)
		}

		var texts []string
		var toolCalls []any
		for _, block := range parsed.Content {
			switch block.Type {
			case "text":
				texts = append(texts, block.Text)
			case "tool_use":
				args := "{}"
				if len(block.Input) > 0 {
					args = string(block.Input)
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   block.ID,
					"type": "function",
					"function": map[string]any{
						"name":      block.Name,
						"arguments": args,
					},
				})
			}
		}

		message := map[string]any{"role": "assistant", "content": strings.Join(texts, "")}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
			if message["content"] == "" {
				message["content"] = nil
			}
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"id":      parsed.ID,
			"object":  "chat.completion",
			"created": nowUnix(),
			"model":   displayModel,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": anthropicFinishReason(parsed.StopReason),
				"logprobs":      nil,
			}},
			"usage": map[string]any{
				"prompt_tokens":     parsed.Usage.InputTokens,
				"completion_tokens": parsed.Usage.OutputTokens,
				"total_tokens":      parsed.Usage.InputTokens + parsed.Usage.OutputTokens,
			},
		})
		return nil
	}

	// ---- streaming ----
	sse := newSSEWriter(w)
	id := newID("chatcmpl-")
	started := false
	stopReason := ""
	toolIndex := 0
	blocks := map[int]int{} // anthropic content block index → openai tool index

	send := func(delta map[string]any) {
		if !started {
			started = true
			if _, ok := delta["role"]; !ok {
				delta["role"] = "assistant"
			}
		}
		sse.jsonChunk(completionChunk(id, displayModel, delta, nil))
	}

	readErr := readSSE(upstream.Body, func(event, data string) error {
		if strings.TrimSpace(data) == "" {
			return nil
		}
		var frame struct {
			Type  string          `json:"type"`
			Index int             `json:"index"`
			Delta json.RawMessage `json:"delta"`
			Block json.RawMessage `json:"block"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			return nil
		}
		if frame.Type == "" {
			frame.Type = event
		}

		switch frame.Type {
		case "message_start":
			send(map[string]any{"content": ""})

		case "content_block_start":
			var block struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			_ = json.Unmarshal(frame.Block, &block)
			if block.Type == "tool_use" {
				blocks[frame.Index] = toolIndex
				send(map[string]any{"tool_calls": []any{map[string]any{
					"index":    toolIndex,
					"id":       block.ID,
					"type":     "function",
					"function": map[string]any{"name": block.Name, "arguments": ""},
				}}})
				toolIndex++
			}

		case "content_block_delta":
			var delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			}
			_ = json.Unmarshal(frame.Delta, &delta)
			switch delta.Type {
			case "text_delta":
				if delta.Text != "" {
					send(map[string]any{"content": delta.Text})
				}
			case "input_json_delta":
				if delta.PartialJSON != "" {
					send(map[string]any{"tool_calls": []any{map[string]any{
						"index":    blocks[frame.Index],
						"function": map[string]any{"arguments": delta.PartialJSON},
					}}})
				}
			}

		case "message_delta":
			var delta struct {
				StopReason string `json:"stop_reason"`
			}
			_ = json.Unmarshal(frame.Delta, &delta)
			if delta.StopReason != "" {
				stopReason = delta.StopReason
			}

		case "message_stop":
			sse.jsonChunk(finalChunk(id, displayModel, anthropicFinishReason(stopReason)))
			sse.done()
			return nil

		case "error":
			sse.jsonChunk(map[string]any{
				"error": errorBody{Message: frame.Error.Message, Type: "upstream_error", Code: "upstream_error"},
			})
			sse.done()
			return nil
		}
		return nil
	})

	if readErr != nil {
		sse.jsonChunk(map[string]any{
			"error": errorBody{Message: readErr.Error(), Type: "upstream_error", Code: "stream_interrupted"},
		})
		sse.done()
	}
	return nil
}
