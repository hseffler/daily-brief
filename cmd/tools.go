package main

import (
	"context"
	"encoding/json"
)

// Tool is a function the agent (LLM) can call. Parameters is a JSON Schema
// object describing the arguments; Run receives the raw JSON arguments.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Run         func(ctx context.Context, args json.RawMessage) (string, error)
}

// findTool returns the tool with the given name, or nil.
func findTool(tools []Tool, name string) *Tool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

// truncateRunes cuts s to at most max runes and reports whether it cut
// anything. It is rune-based so multi-byte UTF-8 characters are never split
// (byte slicing could produce invalid UTF-8 at the cut).
func truncateRunes(s string, max int) (string, bool) {
	runes := []rune(s)
	if len(runes) <= max {
		return s, false
	}
	return string(runes[:max]), true
}
