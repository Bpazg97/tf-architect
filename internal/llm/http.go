package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// retryDelays are the waits between attempts (so len+1 attempts in total).
// Tests override it.
var retryDelays = []time.Duration{time.Second, 2 * time.Second}

const maxErrBody = 500

// maxResponseBody caps a 2xx body: a broken or hostile endpoint must not
// make tf-architect read without bound.
const maxResponseBody = 32 << 20

// Redirects are never followed: Go would re-send custom key headers and the
// POST body (the prompt) to the target, bypassing the HTTPS/local-only rule.
// A 3xx surfaces as a non-2xx error.
var httpClient = &http.Client{
	Timeout:       10 * time.Minute,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// postJSON POSTs body as JSON and decodes the 2xx response into out. Only 429
// and 5xx are retried. Header values are treated as secrets and scrubbed from
// every error, so an API key can never leak through a message.
func postJSON(ctx context.Context, url string, headers map[string]string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding request: %w", err)
	}
	scrub := scrubber(headers)
	var lastErr error
	for attempt := 0; ; attempt++ {
		retry, err := postOnce(ctx, url, headers, payload, out, scrub)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry || attempt >= len(retryDelays) {
			return lastErr
		}
		t := time.NewTimer(retryDelays[attempt])
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), lastErr)
		case <-t.C:
		}
	}
}

func postOnce(ctx context.Context, url string, headers map[string]string, payload []byte, out any, scrub func(string) string) (retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return false, errors.New(scrub(err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("request failed: %s", scrub(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		msg := scrub(string(b))
		if len(msg) > maxErrBody {
			msg = msg[:maxErrBody] + "...(truncated)"
		}
		retry = resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return retry, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(msg))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(out); err != nil {
		return false, fmt.Errorf("decoding response: %s", scrub(err.Error()))
	}
	return false, nil
}

// scrubber returns a function that masks every header value (and a Bearer
// token's bare value) in a string.
func scrubber(headers map[string]string) func(string) string {
	var secrets []string
	for _, v := range headers {
		v = strings.TrimPrefix(v, "Bearer ")
		if len(v) >= 8 {
			secrets = append(secrets, v)
		}
	}
	return func(s string) string {
		for _, sec := range secrets {
			s = strings.ReplaceAll(s, sec, "[REDACTED]")
		}
		return s
	}
}
