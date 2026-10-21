package llm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"tf-architect/internal/security"
)

// spyProvider records what Complete was called with.
type spyProvider struct {
	calls          int
	system, prompt string
}

func (s *spyProvider) Name() string { return "spy" }

func (s *spyProvider) Complete(_ context.Context, system, prompt string) (string, error) {
	s.calls++
	s.system, s.prompt = system, prompt
	return "ok", nil
}

func TestClientGateBlocks(t *testing.T) {
	for _, blocked := range []string{"system", "prompt"} {
		t.Run(blocked, func(t *testing.T) {
			spy := &spyProvider{}
			c := NewClient(spy, "system")
			stop := security.New("blocked", "why", "fix")
			c.SetGate(func(s string) (string, error) {
				if s == blocked {
					return "", stop
				}
				return s, nil
			})
			_, err := c.Query(context.Background(), "prompt")
			var se *security.Error
			if !errors.As(err, &se) || se != stop {
				t.Fatalf("want wrapped security error, got %v", err)
			}
			if spy.calls != 0 {
				t.Error("provider called despite gate error")
			}
		})
	}
}

func TestClientGateApplied(t *testing.T) {
	spy := &spyProvider{}
	c := NewClient(spy, "system SECRET")
	c.SetGate(func(s string) (string, error) { return strings.ReplaceAll(s, "SECRET", "MASKED"), nil })

	got, err := c.Query(context.Background(), "prompt SECRET")
	if err != nil {
		t.Fatal(err)
	}
	if got != "ok" || spy.calls != 1 {
		t.Fatalf("got %q after %d calls", got, spy.calls)
	}
	if spy.system != "system MASKED" || spy.prompt != "prompt MASKED" {
		t.Errorf("provider saw system=%q prompt=%q", spy.system, spy.prompt)
	}
}

func TestClientWithoutGate(t *testing.T) {
	spy := &spyProvider{}
	if _, err := NewClient(spy, "s").Query(context.Background(), "p"); err == nil {
		t.Fatal("expected error without a gate")
	}
	if spy.calls != 0 {
		t.Error("provider called without a gate")
	}
}
