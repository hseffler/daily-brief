package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMistralChatSendsToolsAndParsesToolCalls(t *testing.T) {
	var gotAuth, gotPath, gotContentType string
	var gotBody mistralAPIRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}
		w.Write([]byte(`{
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": "",
					"tool_calls": [{
						"id": "call_1",
						"type": "function",
						"function": {"name": "fetch_webpage", "arguments": "{\"url\":\"https://www.zeit.de/\"}"}
					}]
				},
				"finish_reason": "tool_calls"
			}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`))
	}))
	defer srv.Close()

	client := NewMistralClient("test-key", srv.URL, srv.Client(), testLogger())
	resp, err := client.Chat(context.Background(), ChatRequest{
		Model:       "mistral-large-latest",
		Messages:    []ChatMessage{{Role: "user", Content: Content("hi")}},
		Tools:       toolSpecs([]Tool{newWebFetchTool(100, 5, testLogger())}),
		Temperature: 0.3,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization header: got %q, want %q", gotAuth, "Bearer test-key")
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path: got %q, want /chat/completions", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type: got %q", gotContentType)
	}
	if gotBody.ToolChoice != "auto" {
		t.Errorf("tool_choice: got %q, want auto", gotBody.ToolChoice)
	}
	if !gotBody.ParallelToolCalls {
		t.Errorf("parallel_tool_calls should be true")
	}
	if len(gotBody.Tools) != 1 || gotBody.Tools[0].Function.Name != "fetch_webpage" {
		t.Errorf("tools not sent correctly: %+v", gotBody.Tools)
	}
	if gotBody.Temperature != 0.3 {
		t.Errorf("temperature: got %v", gotBody.Temperature)
	}

	if len(resp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
	}
	msg := resp.Choices[0].Message
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "fetch_webpage" {
		t.Errorf("unexpected tool call: %+v", tc)
	}
	if tc.Function.Arguments != `{"url":"https://www.zeit.de/"}` {
		t.Errorf("unexpected arguments: %q", tc.Function.Arguments)
	}
	if resp.Usage.TotalTokens != 15 {
		t.Errorf("total tokens: got %d, want 15", resp.Usage.TotalTokens)
	}
}

func TestMistralChatErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Unauthorized"}`))
	}))
	defer srv.Close()

	client := NewMistralClient("bad-key", srv.URL, srv.Client(), testLogger())
	_, err := client.Chat(context.Background(), ChatRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: Content("hi")}}})
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention status 401: %v", err)
	}
}

func TestMistralClientDefaultBaseURL(t *testing.T) {
	c := NewMistralClient("k", "", nil, testLogger())
	if c.baseURL != defaultMistralBaseURL {
		t.Errorf("baseURL: got %q, want %q", c.baseURL, defaultMistralBaseURL)
	}
}

func TestMistralChatRetriesTransientErrors(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		switch n {
		case 1:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"message":"rate limited"}`))
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"message":"unavailable"}`))
		default:
			w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		}
	}))
	defer srv.Close()

	client := NewMistralClient("k", srv.URL, srv.Client(), testLogger())
	client.backoffBase = time.Millisecond

	resp, err := client.Chat(context.Background(), ChatRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: Content("hi")}}})
	if err != nil {
		t.Fatalf("Chat should succeed after transient failures: %v", err)
	}
	if got := string(resp.Choices[0].Message.Content); got != "ok" {
		t.Errorf("content: got %q, want %q", got, "ok")
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 3 {
		t.Errorf("expected 3 requests (429 + 503 + ok), got %d", requests)
	}
}

func TestMistralChatDoesNotRetryClientErrors(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"message":"bad request"}`))
	}))
	defer srv.Close()

	client := NewMistralClient("k", srv.URL, srv.Client(), testLogger())
	client.backoffBase = time.Millisecond

	_, err := client.Chat(context.Background(), ChatRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: Content("hi")}}})
	if err == nil {
		t.Fatal("expected error for 400")
	}
	var apiErr *mistralAPIError
	if !errors.As(err, &apiErr) || apiErr.statusCode != http.StatusBadRequest {
		t.Errorf("expected mistralAPIError 400, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Errorf("400 must not be retried: expected 1 request, got %d", requests)
	}
}

func TestMistralChatGivesUpAfterMaxAttempts(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"boom"}`))
	}))
	defer srv.Close()

	client := NewMistralClient("k", srv.URL, srv.Client(), testLogger())
	client.backoffBase = time.Millisecond

	_, err := client.Chat(context.Background(), ChatRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: Content("hi")}}})
	if err == nil {
		t.Fatal("expected error when all attempts fail")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status 500: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != mistralMaxAttempts {
		t.Errorf("expected %d attempts, got %d", mistralMaxAttempts, requests)
	}
}

func TestContentUnmarshalString(t *testing.T) {
	var c Content
	if err := json.Unmarshal([]byte(`"hello world"`), &c); err != nil {
		t.Fatalf("Unmarshal string: %v", err)
	}
	if string(c) != "hello world" {
		t.Errorf("got %q, want %q", c, "hello world")
	}
}

func TestContentUnmarshalArrayOfParts(t *testing.T) {
	// Newer Mistral models return content as an array of typed parts.
	var c Content
	raw := `[{"type":"text","text":"Hello "},{"type":"text","text":"world"}]`
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("Unmarshal array: %v", err)
	}
	if string(c) != "Hello world" {
		t.Errorf("got %q, want %q", c, "Hello world")
	}
}

func TestContentUnmarshalArrayIgnoresNonTextParts(t *testing.T) {
	var c Content
	raw := `[{"type":"image_url","image_url":{"url":"https://example.com/x.png"}},{"type":"text","text":"caption"}]`
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("Unmarshal array: %v", err)
	}
	if string(c) != "caption" {
		t.Errorf("got %q, want %q", c, "caption")
	}
}

func TestContentMarshalEmitsString(t *testing.T) {
	data, err := json.Marshal(Content("hi"))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(data) != `"hi"` {
		t.Errorf("got %s, want %s", data, `"hi"`)
	}
}

func TestMistralChatParsesArrayContent(t *testing.T) {
	// Regression test: newer Mistral models return content as an array of
	// content parts, not a plain string.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": [{"type":"text","text":"Your brief follows."}]
				},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`))
	}))
	defer srv.Close()

	client := NewMistralClient("test-key", srv.URL, srv.Client(), testLogger())
	resp, err := client.Chat(context.Background(), ChatRequest{
		Model:    "mistral-large-4",
		Messages: []ChatMessage{{Role: "user", Content: Content("hi")}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := string(resp.Choices[0].Message.Content); got != "Your brief follows." {
		t.Errorf("content: got %q, want %q", got, "Your brief follows.")
	}
}
