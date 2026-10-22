package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tf-architect/internal/fileproto"
)

const testKey = "sk-test-SECRET-123456"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func newTest(t *testing.T, provider, model, url string) Provider {
	t.Helper()
	p, err := New(Config{
		Provider: provider, Model: model, BaseURL: url,
		Getenv: env(map[string]string{
			"ANTHROPIC_API_KEY": testKey, "OPENAI_API_KEY": testKey,
			"GEMINI_API_KEY": testKey, "TFA_API_KEY": testKey,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func fastRetries(t *testing.T) {
	t.Helper()
	old := retryDelays
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { retryDelays = old })
}

func captureServer(t *testing.T, reply string) (*httptest.Server, *http.Request, *map[string]any) {
	t.Helper()
	var got http.Request
	body := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r.Clone(context.Background())
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &body
}

func TestAnthropicRequestShape(t *testing.T) {
	srv, req, body := captureServer(t, `{"content":[{"type":"text","text":"he"},{"type":"tool_use"},{"type":"text","text":"llo"}]}`)
	p := newTest(t, "anthropic", "m1", srv.URL)
	out, err := p.Complete(context.Background(), "SYS", "hi")
	if err != nil || out != "hello" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if req.URL.Path != "/messages" || req.Header.Get("x-api-key") != testKey || req.Header.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("bad request: %s %v", req.URL.Path, req.Header)
	}
	b := *body
	msgs := b["messages"].([]any)
	if b["system"] != "SYS" || b["model"] != "m1" || b["max_tokens"].(float64) != 32000 ||
		msgs[0].(map[string]any)["role"] != "user" || msgs[0].(map[string]any)["content"] != "hi" {
		t.Fatalf("bad body: %v", b)
	}
	if _, ok := b["tools"]; ok {
		t.Fatal("tools must not be sent")
	}
}

func TestOpenAIRequestShape(t *testing.T) {
	for _, name := range []string{"openai", "openai-compatible"} {
		t.Run(name, func(t *testing.T) {
			srv, req, body := captureServer(t, `{"choices":[{"message":{"content":"yo"}}]}`)
			p := newTest(t, name, "m1", srv.URL)
			if p.Name() != name {
				t.Fatalf("name %q", p.Name())
			}
			out, err := p.Complete(context.Background(), "SYS", "hi")
			if err != nil || out != "yo" {
				t.Fatalf("out=%q err=%v", out, err)
			}
			if req.URL.Path != "/chat/completions" || req.Header.Get("Authorization") != "Bearer "+testKey {
				t.Fatalf("bad request: %s %v", req.URL.Path, req.Header)
			}
			msgs := (*body)["messages"].([]any)
			m0, m1 := msgs[0].(map[string]any), msgs[1].(map[string]any)
			if m0["role"] != "system" || m0["content"] != "SYS" || m1["role"] != "user" || m1["content"] != "hi" || (*body)["model"] != "m1" {
				t.Fatalf("bad body: %v", *body)
			}
		})
	}
}

func TestGeminiRequestShape(t *testing.T) {
	srv, req, body := captureServer(t, `{"candidates":[{"content":{"parts":[{"text":"a"},{"text":"b"}]}}]}`)
	p := newTest(t, "gemini", "gem-1", srv.URL)
	out, err := p.Complete(context.Background(), "SYS", "hi")
	if err != nil || out != "ab" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if req.URL.Path != "/models/gem-1:generateContent" || req.Header.Get("x-goog-api-key") != testKey {
		t.Fatalf("bad request: %s %v", req.URL.Path, req.Header)
	}
	si := (*body)["systemInstruction"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"]
	c := (*body)["contents"].([]any)[0].(map[string]any)
	if si != "SYS" || c["role"] != "user" || c["parts"].([]any)[0].(map[string]any)["text"] != "hi" {
		t.Fatalf("bad body: %v", *body)
	}
}

func TestEmptyTextIsError(t *testing.T) {
	srv, _, _ := captureServer(t, `{"content":[]}`)
	if _, err := newTest(t, "anthropic", "", srv.URL).Complete(context.Background(), "s", "p"); err == nil {
		t.Fatal("want error for empty text")
	}
}

func TestRetryOn429(t *testing.T) {
	fastRetries(t)
	var hits atomic.Int32
	codes := []int{429, 500}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(hits.Add(1))
		if n <= len(codes) {
			w.WriteHeader(codes[n-1])
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()
	out, err := newTest(t, "openai", "m", srv.URL).Complete(context.Background(), "s", "p")
	if err != nil || out != "ok" || hits.Load() != 3 {
		t.Fatalf("out=%q err=%v hits=%d", out, err, hits.Load())
	}
}

func TestRetryGivesUp(t *testing.T) {
	fastRetries(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(503)
	}))
	defer srv.Close()
	_, err := newTest(t, "openai", "m", srv.URL).Complete(context.Background(), "s", "p")
	if err == nil || hits.Load() != 3 || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err=%v hits=%d", err, hits.Load())
	}
}

func TestNoRetryOn4xx(t *testing.T) {
	fastRetries(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(400)
	}))
	defer srv.Close()
	if _, err := newTest(t, "openai", "m", srv.URL).Complete(context.Background(), "s", "p"); err == nil || hits.Load() != 1 {
		t.Fatalf("err=%v hits=%d", err, hits.Load())
	}
}

func TestCancelAbortsRequest(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := newTest(t, "openai", "m", srv.URL).Complete(ctx, "s", "p")
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func TestCancelAbortsRetryWait(t *testing.T) {
	old := retryDelays
	retryDelays = []time.Duration{time.Hour, time.Hour}
	defer func() { retryDelays = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := newTest(t, "openai", "m", srv.URL).Complete(ctx, "s", "p")
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func TestKeyNeverInError(t *testing.T) {
	for _, name := range []string{"anthropic", "openai", "gemini"} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(401)
				_, _ = io.WriteString(w, `{"error":"bad key `+testKey+`"}`)
			}))
			defer srv.Close()
			_, err := newTest(t, name, "m", srv.URL).Complete(context.Background(), "s", "p")
			if err == nil || strings.Contains(err.Error(), testKey) || !strings.Contains(err.Error(), "401") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestErrorBodyTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, strings.Repeat("x", 5000))
	}))
	defer srv.Close()
	_, err := newTest(t, "openai", "m", srv.URL).Complete(context.Background(), "s", "p")
	if err == nil || len(err.Error()) > 700 {
		t.Fatalf("err len=%d", len(err.Error()))
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	for _, name := range []string{"anthropic", "gemini", "openai"} {
		t.Run(name, func(t *testing.T) {
			var hitsB atomic.Int32
			b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hitsB.Add(1) }))
			defer b.Close()
			a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, b.URL+"/x", http.StatusTemporaryRedirect)
			}))
			defer a.Close()
			_, err := newTest(t, name, "m", a.URL).Complete(context.Background(), "s", "p")
			if err == nil || hitsB.Load() != 0 || !strings.Contains(err.Error(), "307") {
				t.Fatalf("err=%v hitsB=%d", err, hitsB.Load())
			}
		})
	}
}

func TestTruncatedResponseIsErrTruncated(t *testing.T) {
	cases := map[string]string{
		"anthropic":         `{"content":[{"type":"text","text":"FILE: a.tf"}],"stop_reason":"max_tokens"}`,
		"openai":            `{"choices":[{"message":{"content":"FILE: a.tf"},"finish_reason":"length"}]}`,
		"openai-compatible": `{"choices":[{"message":{"content":"FILE: a.tf"},"finish_reason":"length"}]}`,
		"gemini":            `{"candidates":[{"content":{"parts":[{"text":"FILE: a.tf"}]},"finishReason":"MAX_TOKENS"}]}`,
	}
	for provider, reply := range cases {
		t.Run(provider, func(t *testing.T) {
			srv, _, _ := captureServer(t, reply)
			p := newTest(t, provider, "m", srv.URL)
			// Through the Client, as the orchestrator calls it.
			c := NewClient(p, "SYS")
			c.SetGate(func(s string) (string, error) { return s, nil })
			_, err := c.Query(context.Background(), "hi")
			if !errors.Is(err, fileproto.ErrTruncated) {
				t.Fatalf("want ErrTruncated, got %v", err)
			}
		})
	}
}

func TestCompleteResponseNotTruncated(t *testing.T) {
	cases := map[string]string{
		"anthropic": `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`,
		"openai":    `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`,
		"gemini":    `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`,
	}
	for provider, reply := range cases {
		srv, _, _ := captureServer(t, reply)
		if out, err := newTest(t, provider, "m", srv.URL).Complete(context.Background(), "S", "p"); err != nil || out != "ok" {
			t.Errorf("%s: out=%q err=%v", provider, out, err)
		}
	}
}

func TestResponseBodyIsSizeLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"`)
		chunk := strings.Repeat("a", 1<<20)
		for i := 0; i < maxResponseBody>>20+2; i++ {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, `"}]}`)
	}))
	t.Cleanup(srv.Close)
	_, err := newTest(t, "anthropic", "m", srv.URL).Complete(context.Background(), "S", "p")
	if err == nil {
		t.Fatal("oversized response body must be rejected")
	}
}
