package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	// toolResultMaxChars caps a single tool result so one huge web page cannot
	// blow up the model context.
	toolResultMaxChars = 12000
	// toolTimeout bounds a single tool execution so a hung fetch cannot stall
	// the whole agent loop.
	toolTimeout = 90 * time.Second
)

// Agent runs the agentic loop: it sends the conversation to the LLM, executes
// any requested tool calls (in parallel, since they are independent), feeds
// the results back, and repeats until the model answers without tool calls.
type Agent struct {
	llm          ChatClient
	model        string
	temperature  float64
	tools        []Tool
	systemPrompt string
	userPrompt   string
	maxIter      int
	logger       *logrus.Logger
}

// NewAgent creates an Agent. maxIter <= 0 falls back to a sane default.
func NewAgent(llm ChatClient, model string, temperature float64, tools []Tool, systemPrompt, userPrompt string, maxIter int, logger *logrus.Logger) *Agent {
	if maxIter <= 0 {
		maxIter = 12
	}
	if logger == nil {
		logger = logrus.New()
	}
	return &Agent{
		llm:          llm,
		model:        model,
		temperature:  temperature,
		tools:        tools,
		systemPrompt: systemPrompt,
		userPrompt:   userPrompt,
		maxIter:      maxIter,
		logger:       logger,
	}
}

// Run executes the agent loop and returns the final brief text.
func (a *Agent) Run(ctx context.Context) (string, error) {
	messages := []ChatMessage{
		{Role: "system", Content: Content(a.systemPrompt)},
		{Role: "user", Content: Content(a.userPrompt)},
	}

	for iter := 1; iter <= a.maxIter; iter++ {
		req := ChatRequest{
			Model:       a.model,
			Messages:    messages,
			Tools:       toolSpecs(a.tools),
			Temperature: a.temperature,
		}

		resp, err := a.llm.Chat(ctx, req)
		if err != nil {
			return "", fmt.Errorf("LLM request failed: %w", err)
		}
		if len(resp.Choices) == 0 {
			return "", errors.New("LLM returned no choices")
		}
		choice := resp.Choices[0]
		messages = append(messages, choice.Message)

		a.logger.WithFields(logrus.Fields{
			"iteration":     iter,
			"finish_reason": choice.FinishReason,
			"tool_calls":    len(choice.Message.ToolCalls),
			"prompt_tokens": resp.Usage.PromptTokens,
			"output_tokens": resp.Usage.CompletionTokens,
			"total_tokens":  resp.Usage.TotalTokens,
		}).Debug("LLM response received")

		// No tool calls: the model produced the final brief.
		if len(choice.Message.ToolCalls) == 0 {
			if strings.TrimSpace(string(choice.Message.Content)) == "" {
				return "", errors.New("LLM returned an empty final message")
			}
			if choice.FinishReason == "length" {
				a.logger.Warn("LLM stopped at the token limit — the brief may be truncated")
			}
			return string(choice.Message.Content), nil
		}

		// Execute all requested tool calls. Calls with different names are
		// independent and run concurrently; calls with the same name run
		// sequentially, because a tool may keep per-call state that is not
		// safe for concurrent use.
		results := make([]string, len(choice.Message.ToolCalls))
		var nameMu sync.Map // map[string]*sync.Mutex
		var wg sync.WaitGroup
		for i, tc := range choice.Message.ToolCalls {
			wg.Add(1)
			go func(i int, tc ToolCall) {
				defer wg.Done()
				mu, _ := nameMu.LoadOrStore(tc.Function.Name, new(sync.Mutex))
				mu.(*sync.Mutex).Lock()
				results[i] = a.executeTool(ctx, tc)
				mu.(*sync.Mutex).Unlock()
			}(i, tc)
		}
		wg.Wait()

		for i, tc := range choice.Message.ToolCalls {
			messages = append(messages, ChatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    Content(results[i]),
			})
		}
	}

	return "", fmt.Errorf("agent did not produce a final brief within %d iterations", a.maxIter)
}

// executeTool runs a single tool call and returns its output as a string. All
// failures are converted to error text so the model can react and retry
// instead of the whole run dying.
func (a *Agent) executeTool(ctx context.Context, tc ToolCall) string {
	tool := findTool(a.tools, tc.Function.Name)
	if tool == nil {
		return fmt.Sprintf("Error: unknown tool %q", tc.Function.Name)
	}

	args := strings.TrimSpace(tc.Function.Arguments)
	if args == "" {
		args = "{}"
	}

	toolCtx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	start := time.Now()
	out, err := tool.Run(toolCtx, json.RawMessage(args))
	duration := time.Since(start)

	if err != nil {
		a.logger.WithFields(logrus.Fields{
			"tool": tc.Function.Name,
			"err":  err,
		}).Warn("tool execution failed")
		return fmt.Sprintf("Error: tool %q failed: %v", tc.Function.Name, err)
	}

	if cut, truncated := truncateRunes(out, toolResultMaxChars); truncated {
		out = cut + "\n... (truncated)"
	}

	a.logger.WithFields(logrus.Fields{
		"tool":     tc.Function.Name,
		"duration": duration.Round(time.Millisecond),
		"out_len":  len(out),
	}).Info("tool executed")

	return out
}

// toolSpecs converts the tool list to the API representation sent with every
// chat request.
func toolSpecs(tools []Tool) []ToolSpec {
	specs := make([]ToolSpec, 0, len(tools))
	for _, t := range tools {
		specs = append(specs, ToolSpec{
			Type: "function",
			Function: ToolSpecDetail{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	return specs
}
