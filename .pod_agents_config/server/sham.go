package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const shamModelID = "local-sham-endpoint"
const shamReply = "This request succeeded"

type shamRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func decodeShamRequest(r *http.Request) shamRequest {
	var req shamRequest
	if r.Body == nil {
		return req
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return req
	}
	_ = json.Unmarshal(body, &req)
	return req
}

func shamID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func shamModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data": []map[string]any{{
			"id":       shamModelID,
			"object":   "model",
			"created":  time.Now().Unix(),
			"owned_by": "pod-agents-manager",
		}},
	})
}

func shamChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req := decodeShamRequest(r)
	model := req.Model
	if model == "" {
		model = shamModelID
	}

	if req.Stream {
		writeShamChatStream(w, model)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":      shamID("chatcmpl"),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": shamReply,
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 4,
			"total_tokens":      4,
		},
	})
}

func writeShamChatStream(w http.ResponseWriter, model string) {
	flusher, ok := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	id := shamID("chatcmpl")
	created := time.Now().Unix()

	deltaChunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{
				"role":    "assistant",
				"content": shamReply,
			},
			"finish_reason": nil,
		}},
	}
	stopChunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
	}
	for _, chunk := range []map[string]any{deltaChunk, stopChunk} {
		buf, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", buf)
		if ok {
			flusher.Flush()
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if ok {
		flusher.Flush()
	}
}

func shamTextCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req := decodeShamRequest(r)
	model := req.Model
	if model == "" {
		model = shamModelID
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":      shamID("cmpl"),
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"text":          shamReply,
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 4,
			"total_tokens":      4,
		},
	})
}

func shamAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req := decodeShamRequest(r)
	model := req.Model
	if model == "" {
		model = shamModelID
	}

	if req.Stream {
		writeShamAnthropicStream(w, model)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":            shamID("msg"),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"content": []map[string]any{{
			"type": "text",
			"text": shamReply,
		}},
		"usage": map[string]any{
			"input_tokens":  0,
			"output_tokens": 4,
		},
	})
}

func writeShamAnthropicStream(w http.ResponseWriter, model string) {
	flusher, ok := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	id := shamID("msg")

	emit := func(event string, data map[string]any) {
		buf, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, buf)
		if ok {
			flusher.Flush()
		}
	}

	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	emit("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	})
	emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{"type": "text_delta", "text": shamReply},
	})
	emit("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": 0,
	})
	emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 4},
	})
	emit("message_stop", map[string]any{"type": "message_stop"})
}
