package llm

import (
	"context"
	"errors"
)

// openAI serves both OpenAI and any OpenAI-compatible endpoint.
type openAI struct {
	name, key, model, baseURL string
}

func (o *openAI) Name() string { return o.name }

func (o *openAI) Complete(ctx context.Context, system, prompt string) (string, error) {
	body := map[string]any{
		"model": o.model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": prompt},
		},
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	err := postJSON(ctx, o.baseURL+"/chat/completions", map[string]string{
		"Authorization": "Bearer " + o.key,
	}, body, &resp)
	if err != nil {
		return "", errors.New(o.name + ": " + err.Error())
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == "" {
		return "", errors.New(o.name + ": empty response")
	}
	return resp.Choices[0].Message.Content, nil
}
