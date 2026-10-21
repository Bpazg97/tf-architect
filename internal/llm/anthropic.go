package llm

import (
	"context"
	"errors"
	"strings"
)

// DefaultAnthropicModel is used when no --model is given for anthropic.
const DefaultAnthropicModel = "claude-sonnet-5-5"

type anthropic struct {
	key, model, baseURL string
}

func (a *anthropic) Name() string { return "anthropic" }

func (a *anthropic) Complete(ctx context.Context, system, prompt string) (string, error) {
	body := map[string]any{
		"model":      a.model,
		"max_tokens": 32000,
		"system":     system,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	}
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	err := postJSON(ctx, a.baseURL+"/messages", map[string]string{
		"x-api-key":         a.key,
		"anthropic-version": "2023-06-01",
	}, body, &resp)
	if err != nil {
		return "", errors.New("anthropic: " + err.Error())
	}
	var sb strings.Builder
	for _, c := range resp.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	if sb.Len() == 0 {
		return "", errors.New("anthropic: empty response")
	}
	return sb.String(), nil
}
