package main

// Google Gemini adapter: converts OpenAI chat requests to generateContent /
// streamGenerateContent and back (including function calling).

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}

type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiDeclaration `json:"functionDeclarations"`
}

type geminiGenerationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

type geminiToolConfig struct {
	FunctionCallingConfig struct {
		Mode                 string   `json:"mode"`
		AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
	} `json:"functionCallingConfig"`
}

type geminiRequest struct {
	Contents          []geminiContent         `json:"contents"`
	SystemInstruction *geminiContent          `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig,omitempty"`
	Tools             []geminiTool            `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig       `json:"toolConfig,omitempty"`
}

func buildGeminiRequest(req *ChatRequest, upstreamModel string) (*geminiRequest, error) {
	out := &geminiRequest{}

	var systemParts []string
	callNames := map[string]string{} // tool_call id → function name (Gemini needs names)

	appendParts := func(role string, parts []geminiPart) {
		if len(parts) == 0 {
			return
		}
		n := len(out.Contents)
		if n > 0 && out.Contents[n-1].Role == role {
			out.Contents[n-1].Parts = append(out.Contents[n-1].Parts, parts...)
			return
		}
		out.Contents = append(out.Contents, geminiContent{Role: role, Parts: parts})
	}

	for _, msg := range req.Messages {
		switch strings.ToLower(msg.Role) {
		case "system", "developer":
			if text := contentText(msg.Content); text != "" {
				systemParts = append(systemParts, text)
			}

		case "assistant":
			var parts []geminiPart
			if text := contentText(msg.Content); text != "" {
				parts = append(parts, geminiPart{Text: text})
			}
			for _, call := range msg.ToolCalls {
				args := strings.TrimSpace(call.Function.Arguments)
				if args == "" || !json.Valid([]byte(args)) {
					args = "{}"
				}
				callNames[call.ID] = call.Function.Name
				parts = append(parts, geminiPart{FunctionCall: &geminiFunctionCall{
					Name: call.Function.Name,
					Args: json.RawMessage(args),
				}})
			}
			appendParts("model", parts)

		case "tool", "function":
			name := callNames[msg.ToolCallID]
			if name == "" {
				name = msg.Name
			}
			if name == "" {
				name = "tool"
			}
			appendParts("user", []geminiPart{{FunctionResponse: &geminiFunctionResponse{
				Name:     name,
				Response: map[string]any{"result": contentText(msg.Content)},
			}}})

		default:
			if text := contentText(msg.Content); text != "" {
				appendParts("user", []geminiPart{{Text: text}})
			}
		}
	}

	if len(out.Contents) == 0 {
		return nil, badRequest("messages must contain at least one user or assistant message")
	}
	if len(systemParts) > 0 {
		out.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: strings.Join(systemParts, "\n\n")}}}
	}

	gen := &geminiGenerationConfig{Temperature: req.Temperature, TopP: req.TopP, StopSequences: stopSequences(req.Stop)}
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		gen.MaxOutputTokens = req.MaxTokens
	} else if req.MaxCompTok != nil && *req.MaxCompTok > 0 {
		gen.MaxOutputTokens = req.MaxCompTok
	}
	out.GenerationConfig = gen

	var declarations []geminiDeclaration
	for _, tool := range req.Tools {
		if tool.Type != "" && tool.Type != "function" {
			continue
		}
		decl := geminiDeclaration{Name: tool.Function.Name, Description: tool.Function.Description}
		if len(tool.Function.Parameters) > 0 {
			decl.Parameters = tool.Function.Parameters
		}
		declarations = append(declarations, decl)
	}
	if len(declarations) > 0 {
		out.Tools = []geminiTool{{FunctionDeclarations: declarations}}
	}

	mode := toolChoiceMode(req.ToolChoice)
	if len(declarations) > 0 {
		switch mode {
		case "none":
			cfg := &geminiToolConfig{}
			cfg.FunctionCallingConfig.Mode = "NONE"
			out.ToolConfig = cfg
		case "required", "any":
			cfg := &geminiToolConfig{}
			cfg.FunctionCallingConfig.Mode = "ANY"
			out.ToolConfig = cfg
		}
	}
	return out, nil
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text         string `json:"text"`
				FunctionCall *struct {
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				} `json:"functionCall"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func geminiFinishReason(reason string) string {
	switch reason {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT":
		return "content_filter"
	default:
		return "stop"
	}
}

func geminiURL(prov *Provider, model, method string, stream bool) string {
	path := prov.BaseURL + "/models/" + url.PathEscape(model) + ":" + method
	if stream {
		path += "?alt=sse"
	}
	return path
}

func (a *App) chatGemini(w http.ResponseWriter, r *http.Request, req *ChatRequest, entry *ModelEntry, displayModel string) error {
	prov := a.cfg.Providers[entry.Provider]

	payload, err := buildGeminiRequest(req, entry.Model)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return badRequest("could not encode request: %v", err)
	}

	method := "generateContent"
	if req.Stream {
		method = "streamGenerateContent"
	}
	upstream, err := a.post(r, prov, geminiURL(prov, entry.Model, method, req.Stream), body)
	if err != nil {
		return err
	}
	defer upstream.Body.Close()
	if upstream.StatusCode != http.StatusOK {
		return upstreamError(upstream)
	}

	if !req.Stream {
		var parsed geminiResponse
		if err := json.NewDecoder(upstream.Body).Decode(&parsed); err != nil {
			return upstreamFailed(http.StatusBadGateway, "upstream sent an unreadable response: %v", err)
		}
		if parsed.Error != nil {
			return upstreamFailed(http.StatusBadGateway, "%s", parsed.Error.Message)
		}

		var texts []string
		var toolCalls []any
		finish := "stop"
		if len(parsed.Candidates) > 0 {
			finish = geminiFinishReason(parsed.Candidates[0].FinishReason)
			for i, part := range parsed.Candidates[0].Content.Parts {
				if part.Text != "" {
					texts = append(texts, part.Text)
				}
				if part.FunctionCall != nil {
					args := "{}"
					if len(part.FunctionCall.Args) > 0 {
						args = string(part.FunctionCall.Args)
					}
					toolCalls = append(toolCalls, map[string]any{
						"index": i,
						"id":    newID("call_"),
						"type":  "function",
						"function": map[string]any{
							"name":      part.FunctionCall.Name,
							"arguments": args,
						},
					})
				}
			}
		}
		if len(toolCalls) > 0 && finish == "stop" {
			finish = "tool_calls"
		}

		message := map[string]any{"role": "assistant", "content": strings.Join(texts, "")}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
			if message["content"] == "" {
				message["content"] = nil
			}
		}

		usage := map[string]any{
			"prompt_tokens":     parsed.UsageMetadata.PromptTokenCount,
			"completion_tokens": parsed.UsageMetadata.CandidatesTokenCount,
			"total_tokens":      parsed.UsageMetadata.TotalTokenCount,
		}
		if parsed.UsageMetadata.TotalTokenCount == 0 {
			usage["total_tokens"] = parsed.UsageMetadata.PromptTokenCount + parsed.UsageMetadata.CandidatesTokenCount
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"id":      newID("chatcmpl-"),
			"object":  "chat.completion",
			"created": nowUnix(),
			"model":   displayModel,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finish,
				"logprobs":      nil,
			}},
			"usage": usage,
		})
		return nil
	}

	// ---- streaming ----
	sse := newSSEWriter(w)
	id := newID("chatcmpl-")
	started := false
	finish := ""
	toolIndex := 0

	send := func(delta map[string]any) {
		if !started {
			started = true
			if _, ok := delta["role"]; !ok {
				delta["role"] = "assistant"
			}
		}
		sse.jsonChunk(completionChunk(id, displayModel, delta, nil))
	}

	readErr := readSSE(upstream.Body, func(_ string, data string) error {
		if strings.TrimSpace(data) == "" {
			return nil
		}
		var frame geminiResponse
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			return nil
		}
		if frame.Error != nil {
			sse.jsonChunk(map[string]any{
				"error": errorBody{Message: frame.Error.Message, Type: "upstream_error", Code: "upstream_error"},
			})
			sse.done()
			return nil
		}
		if len(frame.Candidates) == 0 {
			return nil
		}
		candidate := frame.Candidates[0]
		for _, part := range candidate.Content.Parts {
			if part.Text != "" {
				send(map[string]any{"content": part.Text})
			}
			if part.FunctionCall != nil {
				args := "{}"
				if len(part.FunctionCall.Args) > 0 {
					args = string(part.FunctionCall.Args)
				}
				send(map[string]any{"tool_calls": []any{map[string]any{
					"index": toolIndex,
					"id":    newID("call_"),
					"type":  "function",
					"function": map[string]any{
						"name":      part.FunctionCall.Name,
						"arguments": args,
					},
				}}})
				toolIndex++
			}
		}
		if candidate.FinishReason != "" {
			finish = geminiFinishReason(candidate.FinishReason)
		}
		return nil
	})

	if readErr != nil {
		sse.jsonChunk(map[string]any{
			"error": errorBody{Message: readErr.Error(), Type: "upstream_error", Code: "stream_interrupted"},
		})
		sse.done()
		return nil
	}
	if finish == "" {
		finish = "stop"
	}
	if toolIndex > 0 && finish == "stop" {
		finish = "tool_calls"
	}
	sse.jsonChunk(finalChunk(id, displayModel, finish))
	sse.done()
	return nil
}
