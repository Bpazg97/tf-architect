// Package llm is the provider-neutral way tf-architect talks to an AI model.
// Every prompt leaves the process through Client, which runs the security
// gate first; providers only ever see gated text.
package llm

import (
	"context"
	"fmt"
)

// Provider sends one self-contained request to a model and returns its text.
type Provider interface {
	Name() string
	Complete(ctx context.Context, system, prompt string) (string, error)
}

// Preflighter is implemented by providers that can check, before any real
// data is sent, that they are reachable and configured safely.
type Preflighter interface {
	Preflight(ctx context.Context) error
}

// Client is the single choke point between tf-architect and a Provider.
type Client struct {
	provider Provider
	system   string
	gate     func(string) (string, error)
}

// NewClient returns a Client that sends system as the system prompt of every query.
func NewClient(p Provider, system string) *Client {
	return &Client{provider: p, system: system}
}

// SetGate installs the function applied to the system prompt and to every
// prompt before they leave the process. If it fails, nothing is sent.
func (c *Client) SetGate(fn func(string) (string, error)) {
	c.gate = fn
}

// Query gates the system prompt and prompt, then sends them to the provider.
func (c *Client) Query(ctx context.Context, prompt string) (string, error) {
	system := c.system
	if c.gate != nil {
		var err error
		if system, err = c.gate(system); err != nil {
			return "", fmt.Errorf("gating system prompt (nothing sent): %w", err)
		}
		if prompt, err = c.gate(prompt); err != nil {
			return "", fmt.Errorf("gating prompt (nothing sent): %w", err)
		}
	}
	return c.provider.Complete(ctx, system, prompt)
}
