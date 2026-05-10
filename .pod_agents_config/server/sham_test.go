package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestShamModelsListsLocalShamEndpoint(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	shamModels(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Object != "list" || len(body.Data) != 1 || body.Data[0].ID != shamModelID {
		t.Fatalf("unexpected models response: %#v", body)
	}
}

func TestShamChatCompletionsReturnsCannedReply(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"local-sham-endpoint","messages":[]}`))
	shamChatCompletions(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	var body struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != shamModelID {
		t.Fatalf("model: got %q, want %q", body.Model, shamModelID)
	}
	if len(body.Choices) != 1 || body.Choices[0].Message.Content != shamReply {
		t.Fatalf("unexpected choices: %#v", body.Choices)
	}
	if body.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason: got %q, want stop", body.Choices[0].FinishReason)
	}
}

func TestShamChatCompletionsStreamsSSE(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"local-sham-endpoint","stream":true,"messages":[]}`))
	shamChatCompletions(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type: got %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, shamReply) {
		t.Fatalf("stream body missing canned reply: %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream body missing [DONE] terminator: %q", body)
	}
}

func TestShamAnthropicMessagesReturnsCannedReply(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"local-sham-endpoint","messages":[]}`))
	shamAnthropicMessages(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	var body struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Type != "message" || body.Role != "assistant" || body.Model != shamModelID {
		t.Fatalf("unexpected envelope: %#v", body)
	}
	if len(body.Content) != 1 || body.Content[0].Text != shamReply {
		t.Fatalf("unexpected content: %#v", body.Content)
	}
	if body.StopReason != "end_turn" {
		t.Fatalf("stop_reason: got %q, want end_turn", body.StopReason)
	}
}

func TestShamRejectsWrongMethod(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler func(w *httptest.ResponseRecorder, r *httptest.ResponseRecorder)
	}{} {
		_ = tc
	}
	// Simple direct check: GET /v1/chat/completions should be 405.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/chat/completions", nil)
	shamChatCompletions(rec, req)
	if rec.Code != 405 {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}
