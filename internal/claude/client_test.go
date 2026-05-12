package claude_test

import (
	"os/exec"
	"testing"

	"tf-architect/internal/claude"
)

func TestNewClient(t *testing.T) {
	c := claude.New("/tmp", "You are a test assistant")
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestClaudeAvailable(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not in PATH — skipping")
	}
}

func TestSetContextBlob(t *testing.T) {
	c := claude.New("/tmp", "system")
	c.SetContextBlob("prior context")
	// SetContextBlob should not panic; consumed on first Query call
}
