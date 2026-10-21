package llm

import (
	"strings"
	"testing"
)

func TestNewRequiresKeyAndModel(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"anthropic key", Config{Provider: "anthropic"}, "ANTHROPIC_API_KEY"},
		{"openai key", Config{Provider: "openai", Model: "m"}, "OPENAI_API_KEY"},
		{"openai model", Config{Provider: "openai", Getenv: env(map[string]string{"OPENAI_API_KEY": "k"})}, "--model"},
		{"gemini key", Config{Provider: "gemini", Model: "m"}, "GEMINI_API_KEY"},
		{"gemini model", Config{Provider: "gemini", Getenv: env(map[string]string{"GEMINI_API_KEY": "k"})}, "--model"},
		{"compat key", Config{Provider: "openai-compatible", Model: "m", BaseURL: "http://localhost:1"}, "TFA_API_KEY"},
		{"compat model", Config{Provider: "openai-compatible", BaseURL: "http://localhost:1", Getenv: env(map[string]string{"TFA_API_KEY": "k"})}, "--model"},
		{"compat base", Config{Provider: "openai-compatible", Model: "m", Getenv: env(map[string]string{"TFA_API_KEY": "k"})}, "--base-url"},
		{"unknown", Config{Provider: "nope"}, "nope"},
		{"insecure base", Config{Provider: "openai", Model: "m", BaseURL: "http://example.com", Getenv: env(map[string]string{"OPENAI_API_KEY": "k"})}, "example.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := New(c.cfg); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestNewClaudeNeedsNoKey(t *testing.T) {
	p, err := New(Config{Provider: "claude", Model: "m", ClaudeBin: "/x/claude", Getenv: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	c, ok := p.(*ClaudeCLI)
	if !ok || c.Bin != "/x/claude" || c.Model != "m" {
		t.Fatalf("got %#v", p)
	}
}

func TestNewAnthropicDefaultModel(t *testing.T) {
	p, err := New(Config{Provider: "anthropic", Getenv: env(map[string]string{"ANTHROPIC_API_KEY": "k"})})
	if err != nil || p.Name() != "anthropic" {
		t.Fatalf("p=%v err=%v", p, err)
	}
	if p.(*anthropic).model != DefaultAnthropicModel {
		t.Fatal("default model not applied")
	}
}

func TestIsLocalURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://localhost:8080": true, "http://127.0.0.1:1/v1": true, "http://[::1]:9": true,
		"https://x.com": false, "https://localhost": true,
	} {
		got, err := IsLocalURL(raw)
		if err != nil || got != want {
			t.Errorf("%s: got %v, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"http://x.com", "ftp://localhost", "not a url", "", "https://"} {
		if _, err := IsLocalURL(raw); err == nil {
			t.Errorf("%q: want error", raw)
		}
	}
}
