package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"tf-architect/internal/fileproto"
	"tf-architect/internal/security"
)

// helperRecord is what the stub claude writes about how it was started.
type helperRecord struct {
	Args   []string `json:"args"`
	Env    []string `json:"env"`
	Cwd    string   `json:"cwd"`
	Stdin  string   `json:"stdin"`
	System string   `json:"system"` // content of --system-prompt-file
}

// TestHelperClaude is not a real test: it is the stub claude binary. It runs
// only when re-executed by the script that stubCLI writes.
func TestHelperClaude(t *testing.T) {
	if os.Getenv("GO_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	cwd, _ := os.Getwd()
	stdin, _ := io.ReadAll(os.Stdin)
	var system []byte
	if i := slices.Index(args, "--system-prompt-file"); i >= 0 && i+1 < len(args) {
		system, _ = os.ReadFile(args[i+1])
	}
	rec, _ := json.Marshal(helperRecord{Args: args, Env: os.Environ(), Cwd: cwd, Stdin: string(stdin), System: string(system)})
	_ = os.WriteFile(os.Getenv("HELPER_OUT"), rec, 0o600)

	if root := os.Getenv("HELPER_TRANSCRIPT_ROOT"); root != "" {
		dir := filepath.Join(root, "projects", slug(cwd))
		_ = os.MkdirAll(dir, 0o700)
		_ = os.WriteFile(filepath.Join(dir, "x.jsonl"), []byte("{}\n"), 0o600)
	}

	cwdJSON, _ := json.Marshal(cwd)
	clean := `{"type":"system","subtype":"init","cwd":` + string(cwdJSON) +
		`,"tools":[],"mcp_servers":[],"plugins":[{"name":"cc-plugin-agents-md","path":"builtin","source":"cc-plugin-agents-md@builtin"}],"claude_code_version":"2.1.306"}`
	var lines []string
	switch os.Getenv("HELPER_INIT") {
	case "clean":
		lines = append(lines, clean)
	case "bad-plugin":
		lines = append(lines, strings.Replace(clean, `"path":"builtin"`, `"path":"/home/u/.claude/plugins/caveman"`, 1))
	case "none":
	}
	stop := "end_turn"
	if os.Getenv("HELPER_RESULT") == "truncated" {
		stop = "max_tokens"
	}
	lines = append(lines,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"SECRET-ANSWER"}],"stop_reason":"`+stop+`"}}`)
	switch os.Getenv("HELPER_RESULT") {
	case "", "truncated":
		lines = append(lines, `{"type":"result","subtype":"success","is_error":false,"result":"SECRET-ANSWER"}`)
	case "max-turns":
		lines = append(lines, `{"type":"result","subtype":"error_max_turns","is_error":false,"result":"SECRET-ANSWER"}`)
	case "none":
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	os.Exit(0)
}

// stubCLI returns a ClaudeCLI whose Bin re-runs this test binary as the
// helper above, and the path of the file the helper records into.
func stubCLI(t *testing.T, init string) (*ClaudeCLI, string) {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "record.json")
	script := filepath.Join(dir, "claude")
	body := "#!/bin/sh\nGO_HELPER=1 exec '" + os.Args[0] + "' -test.run='^TestHelperClaude$' -- \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HELPER_OUT", out)
	t.Setenv("HELPER_INIT", init)
	t.Setenv("HELPER_RESULT", "")
	return &ClaudeCLI{Bin: script, ConfigDir: filepath.Join(dir, "config")}, out
}

func readRecord(t *testing.T, path string) helperRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("stub claude did not run: %v", err)
	}
	var rec helperRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestClaudeArgsAndEnv(t *testing.T) {
	c, out := stubCLI(t, "clean")
	c.Model = "claude-test-model"

	got, err := c.Complete(context.Background(), "SYS", "PROMPT")
	if err != nil {
		t.Fatal(err)
	}
	if got != "SECRET-ANSWER" {
		t.Errorf("got %q", got)
	}
	rec := readRecord(t, out)

	if len(rec.Args) < 3 {
		t.Fatalf("args too short: %q", rec.Args)
	}
	sysFile := rec.Args[len(rec.Args)-3]
	want := []string{
		"-p", "--output-format", "stream-json", "--verbose", "--safe-mode",
		"--no-session-persistence", "--restricted", "--strict-mcp-config",
		"--setting-sources", "", "--tools", "", "--permission-prompts", "none",
		"--system-prompt-file", sysFile, "--model", "claude-test-model",
	}
	if !slices.Equal(rec.Args, want) {
		t.Errorf("args:\n got %q\nwant %q", rec.Args, want)
	}
	if rec.Stdin != "PROMPT" || rec.System != "SYS" {
		t.Errorf("stdin=%q system file=%q", rec.Stdin, rec.System)
	}
	if !filepath.IsAbs(sysFile) || strings.HasPrefix(sysFile, rec.Cwd+string(filepath.Separator)) {
		t.Errorf("system prompt file %q must be absolute and outside the CLI cwd %q", sysFile, rec.Cwd)
	}
	if _, err := os.Stat(sysFile); !os.IsNotExist(err) {
		t.Errorf("system prompt file %s not removed after Complete", sysFile)
	}
	for _, kv := range []string{"CLAUDE_CODE_DISABLE_CLAUDE_MDS=1", "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1"} {
		if !slices.Contains(rec.Env, kv) {
			t.Errorf("env missing %s", kv)
		}
	}
	here, _ := os.Getwd()
	if rec.Cwd == here {
		t.Error("claude ran in the caller's cwd")
	}
	if _, err := os.Stat(rec.Cwd); !os.IsNotExist(err) {
		t.Errorf("temp cwd %s not removed after Complete", rec.Cwd)
	}
}

func TestClaudeArgsWithoutModel(t *testing.T) {
	c, out := stubCLI(t, "clean")
	if _, err := c.Complete(context.Background(), "SYS", "PROMPT"); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(readRecord(t, out).Args, "--model") {
		t.Error("--model passed without a model")
	}
}

func TestCheckInit(t *testing.T) {
	cwd, otherDir := t.TempDir(), t.TempDir()
	clean := func() map[string]any {
		return map[string]any{
			"type": "system", "subtype": "init", "cwd": cwd,
			"tools": []any{}, "mcp_servers": []any{},
			"plugins": []any{map[string]any{
				"name": "cc-plugin-agents-md", "path": "builtin", "source": "cc-plugin-agents-md@builtin",
			}},
			"claude_code_version": "2.1.306",
		}
	}
	if err := checkInit(clean(), cwd); err != nil {
		t.Fatalf("clean init rejected: %v", err)
	}
	newer := clean()
	newer["claude_code_version"] = "2.10.0"
	if err := checkInit(newer, cwd); err != nil {
		t.Errorf("newer version rejected: %v", err)
	}

	cases := map[string]func(map[string]any){
		"tool":       func(ev map[string]any) { ev["tools"] = []any{"Read"} },
		"mcp server": func(ev map[string]any) { ev["mcp_servers"] = []any{map[string]any{"name": "x"}} },
		"user plugin": func(ev map[string]any) {
			ev["plugins"] = []any{map[string]any{"name": "x", "path": "/home/u/.claude/plugins/x"}}
		},
		"cwd mismatch":    func(ev map[string]any) { ev["cwd"] = "/somewhere/else" },
		"other real dir":  func(ev map[string]any) { ev["cwd"] = otherDir },
		"old version":     func(ev map[string]any) { ev["claude_code_version"] = "2.1.100" },
		"missing version": func(ev map[string]any) { delete(ev, "claude_code_version") },
		"missing tools":   func(ev map[string]any) { delete(ev, "tools") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			ev := clean()
			mutate(ev)
			err := checkInit(ev, cwd)
			var se *security.Error
			if !errors.As(err, &se) {
				t.Fatalf("want *security.Error, got %v", err)
			}
			if se.What == "" || se.Why == "" || se.Fix == "" {
				t.Errorf("incomplete security error: %+v", se)
			}
		})
	}
}

func TestCompleteDiscardsOnBadInit(t *testing.T) {
	for _, init := range []string{"bad-plugin", "none"} {
		t.Run(init, func(t *testing.T) {
			c, _ := stubCLI(t, init)
			got, err := c.Complete(context.Background(), "SYS", "PROMPT")
			var se *security.Error
			if !errors.As(err, &se) {
				t.Fatalf("want *security.Error, got %v", err)
			}
			if strings.Contains(got, "SECRET-ANSWER") || strings.Contains(err.Error(), "SECRET-ANSWER") {
				t.Errorf("text after a bad init leaked: %q / %v", got, err)
			}
		})
	}
}

func TestTranscriptCleanup(t *testing.T) {
	c, out := stubCLI(t, "clean")
	t.Setenv("HELPER_TRANSCRIPT_ROOT", c.ConfigDir)
	keep := filepath.Join(c.ConfigDir, "projects", "other", "y.jsonl")
	if err := os.MkdirAll(filepath.Dir(keep), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var warnings []string
	c.SetWarn(func(w string) { warnings = append(warnings, w) })
	if _, err := c.Complete(context.Background(), "SYS", "PROMPT"); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "transcript") {
		t.Errorf("removed transcript not reported: %q", warnings)
	}
	transcript := filepath.Join(c.ConfigDir, "projects", slug(readRecord(t, out).Cwd))
	if _, err := os.Stat(transcript); !os.IsNotExist(err) {
		t.Errorf("transcript dir %s not removed", transcript)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("unrelated config file removed: %v", err)
	}
}

func TestSlug(t *testing.T) {
	if got := slug("/tmp/tfa-claude-123/x_y.z"); got != "-tmp-tfa-claude-123-x-y-z" {
		t.Errorf("slug = %q", got)
	}
}

func TestVersionAtLeast(t *testing.T) {
	for v, want := range map[string]bool{
		"2.1.306": true, "2.1.307": true, "2.2.0": true, "3.0.0": true, "2.1.1000": true,
		"2.1.305": false, "2.1.100": false, "2.0.999": false, "1.9.400": false, "": false, "abc": false,
	} {
		if got := versionAtLeast(v, MinClaudeVersion); got != want {
			t.Errorf("versionAtLeast(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestPreflight(t *testing.T) {
	c, out := stubCLI(t, "clean")
	if err := c.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readRecord(t, out).Stdin; got != "Reply with OK." {
		t.Errorf("preflight prompt = %q", got)
	}

	bad, _ := stubCLI(t, "bad-plugin")
	var se *security.Error
	if err := bad.Preflight(context.Background()); !errors.As(err, &se) {
		t.Errorf("want *security.Error, got %v", err)
	}
}

func TestLargePromptViaStdin(t *testing.T) {
	c, out := stubCLI(t, "clean")
	prompt := strings.Repeat("x", 300*1024)
	if _, err := c.Complete(context.Background(), "SYS", prompt); err != nil {
		t.Fatal(err)
	}
	if got := readRecord(t, out).Stdin; got != prompt {
		t.Errorf("stdin got %d bytes, want %d", len(got), len(prompt))
	}
}

func TestPromptIsNotAnOption(t *testing.T) {
	c, out := stubCLI(t, "clean")
	if _, err := c.Complete(context.Background(), "SYS", "--tools=Bash"); err != nil {
		t.Fatal(err)
	}
	rec := readRecord(t, out)
	if rec.Stdin != "--tools=Bash" {
		t.Errorf("stdin = %q", rec.Stdin)
	}
	if slices.Contains(rec.Args, "--tools=Bash") {
		t.Errorf("prompt reached argv: %q", rec.Args)
	}
}

func TestClaudeResultMustBeSuccess(t *testing.T) {
	for _, result := range []string{"max-turns", "none"} {
		t.Run(result, func(t *testing.T) {
			c, _ := stubCLI(t, "clean")
			t.Setenv("HELPER_RESULT", result)
			got, err := c.Complete(context.Background(), "SYS", "PROMPT")
			if err == nil {
				t.Fatalf("want error, got %q", got)
			}
		})
	}
}

func TestClaudeMaxTokensIsErrTruncated(t *testing.T) {
	c, _ := stubCLI(t, "clean")
	t.Setenv("HELPER_RESULT", "truncated")
	if _, err := c.Complete(context.Background(), "SYS", "PROMPT"); !errors.Is(err, fileproto.ErrTruncated) {
		t.Fatalf("want ErrTruncated, got %v", err)
	}
}

func TestNoTranscriptNoWarning(t *testing.T) {
	c, _ := stubCLI(t, "clean")
	var warnings []string
	c.SetWarn(func(w string) { warnings = append(warnings, w) })
	if _, err := c.Complete(context.Background(), "SYS", "PROMPT"); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %q", warnings)
	}
}
