package llm

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

type gemini struct {
	key, model, baseURL string
}

func (g *gemini) Name() string { return "gemini" }

func (g *gemini) Complete(ctx context.Context, system, prompt string) (string, error) {
	type part struct {
		Text string `json:"text"`
	}
	body := map[string]any{
		"systemInstruction": map[string]any{"parts": []part{{system}}},
		"contents":          []map[string]any{{"role": "user", "parts": []part{{prompt}}}},
	}
	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []part `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	endpoint := g.baseURL + "/models/" + url.PathEscape(g.model) + ":generateContent"
	err := postJSON(ctx, endpoint, map[string]string{"x-goog-api-key": g.key}, body, &resp)
	if err != nil {
		return "", errors.New("gemini: " + err.Error())
	}
	var sb strings.Builder
	if len(resp.Candidates) > 0 {
		for _, p := range resp.Candidates[0].Content.Parts {
			sb.WriteString(p.Text)
		}
	}
	if sb.Len() == 0 {
		return "", errors.New("gemini: empty response")
	}
	return sb.String(), nil
}
