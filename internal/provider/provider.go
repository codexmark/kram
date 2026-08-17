// Package provider defines the common interface every upstream LLM backend
// implements, and the normalized streaming event shape the router/server
// work with regardless of which provider served the request.
package provider

import (
	"context"

	"github.com/codexmark/kram-gateway/internal/openai"
)

// StreamEvent is one normalized increment of a chat completion stream.
// Providers translate their native wire format into a sequence of these.
type StreamEvent struct {
	Delta string
	Done  bool
	Usage *openai.Usage
	Err   error
}

// Provider is anything that can serve a chat completion request.
type Provider interface {
	// ID is the stable identifier used in config, logs and telemetry.
	ID() string
	// Kind is the adapter family (anthropic, gemini, openai-compat) — useful
	// for diagnostics, not for routing decisions.
	Kind() string
	// ChatCompletion issues the request upstream and streams back normalized
	// events. The channel is always closed by the provider, exactly once,
	// after a final event with Done=true or Err set.
	ChatCompletion(ctx context.Context, req openai.ChatCompletionRequest) (<-chan StreamEvent, error)
}
