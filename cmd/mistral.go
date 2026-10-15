package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultMistralBaseURL is the Mistral API endpoint. It is a constant in code
// (not a secret); only the API key is sensitive and comes from the config.
const defaultMistralBaseURL = "https://api.mistral.ai/v1"

// ChatClient is the seam between the agent loop and the LLM API. Tests inject
// a fake implementation; production uses MistralClient.
type ChatClient interface {
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}

// ChatRequest is a single chat-completions call.
type ChatRequest struct {
	Model       string
	Messages    []ChatMessage
	Tools       []ToolSpec
	Temperature float64
}

// Content is a chat message content that accepts both wire formats the
// Mistral API uses: a plain string ("content": "hello") and an array of
// content parts ("content": [{"type":"text","text":"hello"}]). Newer
// models return the array form; the agent always works with the flattened
// string. Marshaling always emits the plain string form.
type Content string

// contentPart is one element of the array content form.
type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// UnmarshalJSON accepts either a JSON string or an array of content parts.
// For the array form, the text of all parts with type "text" is concatenated
// (other part types, e.g. images, are ignored).
func (c *Content) UnmarshalJSON(data []byte) error {
	// Try the plain string form first.
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*c = Content(s)
		return nil
	}
	// Fall back to the array-of-parts form.
	var parts []contentPart
	if err := json.Unmarshal(data, &parts); err != nil {
		return fmt.Errorf("content is neither string nor array of parts: %w", err)
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	*c = Content(sb.String())
	return nil
}

// MarshalJSON always emits the plain string form.
func (c Content) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(c))
}

// ChatMessage is one message in the conversation. It mirrors the Mistral
// (OpenAI-compatible) chat message shape, including tool calls and tool
// results.
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    Content    `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a single tool invocation requested by the model.
type ToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function ToolCallRequest `json:"function"`
}

// ToolCallRequest carries the tool name and its arguments as a JSON string.
type ToolCallRequest struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolSpec advertises a tool to the model.
type ToolSpec struct {
	Type     string         `json:"type"` // always "function"
	Function ToolSpecDetail `json:"function"`
}

// ToolSpecDetail is the function declaration inside a ToolSpec.
type ToolSpecDetail struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ChatResponse is the parsed chat-completions response.
type ChatResponse struct {
	Choices []ChatChoice `json:"choices"`
	Usage   ChatUsage    `json:"usage"`
}

// ChatChoice is one completion choice.
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ChatUsage reports token consumption for logging.
type ChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// mistralAPIRequest is the wire format sent to /chat/completions.
type mistralAPIRequest struct {
	Model             string        `json:"model"`
	Messages          []ChatMessage `json:"messages"`
	Tools             []ToolSpec    `json:"tools,omitempty"`
	ToolChoice        string        `json:"tool_choice,omitempty"`
	ParallelToolCalls bool          `json:"parallel_tool_calls"`
	Temperature       float64       `json:"temperature"`
}

// MistralClient talks to the Mistral chat completions API.
type MistralClient struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

// NewMistralClient creates a client. An empty baseURL falls back to the public
// Mistral API; a nil httpClient gets a default with a generous timeout (LLM
// calls with tool loops can take a while).
func NewMistralClient(apiKey, baseURL string, httpClient *http.Client) *MistralClient {
	if baseURL == "" {
		baseURL = defaultMistralBaseURL
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 180 * time.Second}
	}
	return &MistralClient{
		apiKey:     apiKey,
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpClient,
	}
}

// Chat performs one chat-completions call with tool support enabled.
func (c *MistralClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	apiReq := mistralAPIRequest{
		Model:             req.Model,
		Messages:          req.Messages,
		Tools:             req.Tools,
		ToolChoice:        "auto",
		ParallelToolCalls: true,
		Temperature:       req.Temperature,
	}
	body, err := json.Marshal(apiReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Mistral API request failed: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("Mistral API returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var chatResp ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return nil, fmt.Errorf("failed to parse Mistral API response: %w", err)
	}
	return &chatResp, nil
}
