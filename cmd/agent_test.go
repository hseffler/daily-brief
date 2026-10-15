package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeChatClient returns scripted responses in order and records requests.
type fakeChatClient struct {
	mu        sync.Mutex
	responses []ChatResponse
	calls     int
	lastReq   ChatRequest
}

func (f *fakeChatClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastReq = req
	if f.calls >= len(f.responses) {
		return nil, errors.New("no more scripted responses")
	}
	r := f.responses[f.calls]
	f.calls++
	return &r, nil
}

func (f *fakeChatClient) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func toolCallMsg(id, name, args string) ChatMessage {
	return ChatMessage{
		Role: "assistant",
		ToolCalls: []ToolCall{{
			ID:       id,
			Type:     "function",
			Function: ToolCallRequest{Name: name, Arguments: args},
		}},
	}
}

func finalMsg(content string) ChatResponse {
	return ChatResponse{Choices: []ChatChoice{{
		Message:      ChatMessage{Role: "assistant", Content: Content(content)},
		FinishReason: "stop",
	}}}
}

func TestAgentRunsToolsInParallelAndReturnsFinal(t *testing.T) {
	var mu sync.Mutex
	executed := map[string]int{}

	mkTool := func(name string, delay time.Duration) Tool {
		return Tool{
			Name:        name,
			Description: name,
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			Run: func(ctx context.Context, args json.RawMessage) (string, error) {
				time.Sleep(delay)
				mu.Lock()
				executed[name]++
				mu.Unlock()
				return name + "-result", nil
			},
		}
	}

	llm := &fakeChatClient{responses: []ChatResponse{
		{Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "1", Type: "function", Function: ToolCallRequest{Name: "slow", Arguments: "{}"}},
			{ID: "2", Type: "function", Function: ToolCallRequest{Name: "fast", Arguments: "{}"}},
		}}}}},
		finalMsg("FINAL BRIEF"),
	}}

	agent := NewAgent(llm, "model", 0.3, []Tool{mkTool("slow", 120*time.Millisecond), mkTool("fast", 10*time.Millisecond)},
		"sys", "user", 5, testLogger())

	start := time.Now()
	out, err := agent.Run(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "FINAL BRIEF" {
		t.Errorf("got %q, want FINAL BRIEF", out)
	}
	if executed["slow"] != 1 || executed["fast"] != 1 {
		t.Errorf("tools not executed exactly once: %v", executed)
	}
	// Sequential execution would take >= 130ms; parallel must be faster.
	if elapsed >= 130*time.Millisecond {
		t.Errorf("tools appear to run sequentially: elapsed %v", elapsed)
	}

	// The second LLM request must contain the tool results, in request order,
	// with matching tool_call_ids.
	msgs := llm.lastReq.Messages
	var toolMsgs []ChatMessage
	for _, m := range msgs {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != 2 {
		t.Fatalf("expected 2 tool messages, got %d", len(toolMsgs))
	}
	if toolMsgs[0].ToolCallID != "1" || string(toolMsgs[0].Content) != "slow-result" || toolMsgs[0].Name != "slow" {
		t.Errorf("unexpected first tool message: %+v", toolMsgs[0])
	}
	if toolMsgs[1].ToolCallID != "2" || string(toolMsgs[1].Content) != "fast-result" || toolMsgs[1].Name != "fast" {
		t.Errorf("unexpected second tool message: %+v", toolMsgs[1])
	}
	// The system and user prompts must be the first two messages.
	if msgs[0].Role != "system" || string(msgs[0].Content) != "sys" {
		t.Errorf("first message should be the system prompt: %+v", msgs[0])
	}
	if msgs[1].Role != "user" || string(msgs[1].Content) != "user" {
		t.Errorf("second message should be the user prompt: %+v", msgs[1])
	}
	// Tool specs must be sent with the request.
	if len(llm.lastReq.Tools) != 2 {
		t.Errorf("expected 2 tool specs in request, got %d", len(llm.lastReq.Tools))
	}
}

func TestAgentMaxIterations(t *testing.T) {
	tool := Tool{
		Name:        "loop",
		Description: "loop",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		Run:         func(ctx context.Context, args json.RawMessage) (string, error) { return "ok", nil },
	}
	// The model keeps requesting tools forever.
	llm := &fakeChatClient{responses: []ChatResponse{
		{Choices: []ChatChoice{{Message: toolCallMsg("1", "loop", "{}")}}},
		{Choices: []ChatChoice{{Message: toolCallMsg("2", "loop", "{}")}}},
		{Choices: []ChatChoice{{Message: toolCallMsg("3", "loop", "{}")}}},
	}}
	agent := NewAgent(llm, "model", 0.3, []Tool{tool}, "sys", "user", 3, testLogger())

	_, err := agent.Run(context.Background())
	if err == nil {
		t.Fatal("expected max-iterations error")
	}
	if !strings.Contains(err.Error(), "3 iterations") {
		t.Errorf("error should mention iteration limit: %v", err)
	}
	if llm.requestCount() != 3 {
		t.Errorf("expected 3 LLM calls, got %d", llm.requestCount())
	}
}

func TestAgentUnknownToolAndToolErrorAreReportedToModel(t *testing.T) {
	failing := Tool{
		Name:        "failing",
		Description: "failing",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		Run:         func(ctx context.Context, args json.RawMessage) (string, error) { return "", errors.New("boom") },
	}
	llm := &fakeChatClient{responses: []ChatResponse{
		{Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "1", Type: "function", Function: ToolCallRequest{Name: "nonexistent", Arguments: "{}"}},
			{ID: "2", Type: "function", Function: ToolCallRequest{Name: "failing", Arguments: "{}"}},
		}}}}},
		finalMsg("recovered"),
	}}
	agent := NewAgent(llm, "model", 0.3, []Tool{failing}, "sys", "user", 5, testLogger())

	out, err := agent.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "recovered" {
		t.Errorf("got %q, want recovered", out)
	}

	var toolMsgs []ChatMessage
	for _, m := range llm.lastReq.Messages {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != 2 {
		t.Fatalf("expected 2 tool messages, got %d", len(toolMsgs))
	}
	if !strings.Contains(string(toolMsgs[0].Content), "unknown tool") {
		t.Errorf("expected unknown-tool error, got %q", toolMsgs[0].Content)
	}
	if !strings.Contains(string(toolMsgs[1].Content), "boom") {
		t.Errorf("expected tool error text, got %q", toolMsgs[1].Content)
	}
}

func TestAgentEmptyFinalMessage(t *testing.T) {
	llm := &fakeChatClient{responses: []ChatResponse{
		finalMsg("   "),
	}}
	agent := NewAgent(llm, "model", 0.3, nil, "sys", "user", 5, testLogger())
	if _, err := agent.Run(context.Background()); err == nil {
		t.Fatal("expected error for empty final message")
	}
}

func TestAgentLLMErrorPropagates(t *testing.T) {
	llm := &fakeChatClient{responses: nil} // always errors
	agent := NewAgent(llm, "model", 0.3, nil, "sys", "user", 5, testLogger())
	_, err := agent.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "LLM request failed") {
		t.Fatalf("expected LLM error, got %v", err)
	}
}

func TestFindTool(t *testing.T) {
	tools := []Tool{{Name: "a"}, {Name: "b"}}
	if findTool(tools, "b") == nil {
		t.Error("expected to find tool b")
	}
	if findTool(tools, "c") != nil {
		t.Error("expected nil for missing tool")
	}
}

func TestAgentWarnsOnLengthFinishReason(t *testing.T) {
	// finish_reason "length" means the model hit the token limit: the brief
	// may be truncated. The content is still returned (partial is better than
	// nothing) and the warning is logged.
	llm := &fakeChatClient{responses: []ChatResponse{
		{Choices: []ChatChoice{{
			Message:      ChatMessage{Role: "assistant", Content: Content("partial brief")},
			FinishReason: "length",
		}}},
	}}
	agent := NewAgent(llm, "model", 0.3, nil, "sys", "user", 5, testLogger())

	out, err := agent.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "partial brief" {
		t.Errorf("got %q, want the (partial) content", out)
	}
}

func TestAgentSerializesSameNameToolCalls(t *testing.T) {
	var mu sync.Mutex
	inFlight := map[string]int{}
	maxInFlight := map[string]int{}

	mkTool := func(name string, delay time.Duration) Tool {
		return Tool{
			Name:        name,
			Description: name,
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			Run: func(ctx context.Context, args json.RawMessage) (string, error) {
				mu.Lock()
				inFlight[name]++
				if inFlight[name] > maxInFlight[name] {
					maxInFlight[name] = inFlight[name]
				}
				mu.Unlock()
				time.Sleep(delay)
				mu.Lock()
				inFlight[name]--
				mu.Unlock()
				return name, nil
			},
		}
	}

	// Two calls to "a" (must not overlap) plus one to "b" (must overlap with a).
	llm := &fakeChatClient{responses: []ChatResponse{
		{Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "1", Type: "function", Function: ToolCallRequest{Name: "a", Arguments: "{}"}},
			{ID: "2", Type: "function", Function: ToolCallRequest{Name: "a", Arguments: "{}"}},
			{ID: "3", Type: "function", Function: ToolCallRequest{Name: "b", Arguments: "{}"}},
		}}}}},
		finalMsg("done"),
	}}

	agent := NewAgent(llm, "model", 0.3,
		[]Tool{mkTool("a", 80*time.Millisecond), mkTool("b", 80*time.Millisecond)},
		"sys", "user", 5, testLogger())

	start := time.Now()
	out, err := agent.Run(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "done" {
		t.Errorf("got %q, want done", out)
	}
	if maxInFlight["a"] != 1 {
		t.Errorf("same-name calls must be serialized: max in-flight a = %d, want 1", maxInFlight["a"])
	}
	// The two a calls run serially (2x80ms = 160ms) while b overlaps them,
	// so the total must be well under a fully sequential 3x80ms = 240ms.
	if elapsed >= 220*time.Millisecond {
		t.Errorf("different-name calls should run in parallel: elapsed %v", elapsed)
	}
}
