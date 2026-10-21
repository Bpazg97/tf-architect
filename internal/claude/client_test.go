package claude_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// fakeClaude puts a stub `claude` on PATH that records its arguments and
// emits the given stream-json result line. Returns the args file path.
func fakeClaude(t *testing.T, resultLine string) string {
	t.Helper()
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\necho '" + resultLine + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

func TestSanitizerAppliedToPromptSystemPromptAndBlob(t *testing.T) {
	argsFile := fakeClaude(t, `{"type":"result","is_error":false}`)
	c := claude.New(t.TempDir(), "system SECRET")
	c.SetContextBlob("blob SECRET")
	c.SetSanitizer(func(s string) (string, error) { return strings.ReplaceAll(s, "SECRET", "MASKED"), nil })

	if _, err := c.Query("prompt SECRET"); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile)
	if strings.Contains(string(args), "SECRET") {
		t.Errorf("unsanitized text sent to claude:\n%s", args)
	}
	for _, want := range []string{"--restricted", "--tools", "Read,Write,Edit,Glob,Grep"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("missing %q in args:\n%s", want, args)
		}
	}
	if strings.Contains(string(args), "Bash") || strings.Contains(string(args), "bypassPermissions") {
		t.Errorf("claude must not get Bash or bypassPermissions:\n%s", args)
	}
}

func TestSanitizerErrorSendsNothing(t *testing.T) {
	argsFile := fakeClaude(t, `{"type":"result","is_error":false}`)
	c := claude.New(t.TempDir(), "system")
	c.SetSanitizer(func(string) (string, error) { return "", errors.New("boom") })

	if _, err := c.Query("prompt"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Error("claude was executed despite sanitizer failure")
	}
}

func TestResultErrorReturnedAfterProcessExit(t *testing.T) {
	fakeClaude(t, `{"type":"result","is_error":true,"result":"overloaded"}`)
	c := claude.New(t.TempDir(), "")
	_, err := c.Query("x")
	if err == nil || !strings.Contains(err.Error(), "overloaded") {
		t.Errorf("expected claude error, got %v", err)
	}
}
