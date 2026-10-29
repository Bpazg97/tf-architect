package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"tf-architect/internal/security"
)

// MinClaudeVersion is the oldest Claude Code release whose isolation flags
// have been verified against the init event.
const MinClaudeVersion = "2.1.306"

const isolationFix = "upgrade Claude Code or use --provider anthropic with an API key"

// ClaudeCLI runs the Claude Code CLI with the user's login, fully isolated:
// no tools, no CLAUDE.md, no memory, no plugins or hooks, no transcript, and
// a fresh empty working directory. The init event is checked on every call
// and any sign of leaked user configuration aborts the call.
type ClaudeCLI struct {
	Bin       string // defaults to "claude"
	Model     string // passed as --model when set
	ConfigDir string // defaults to $CLAUDE_CONFIG_DIR, else ~/.claude

	warn func(string)
}

// SetWarn sets where non-fatal problems are reported.
func (c *ClaudeCLI) SetWarn(fn func(string)) { c.warn = fn }

// Name identifies the provider.
func (c *ClaudeCLI) Name() string { return "claude" }

// Preflight sends a trivial prompt so a misconfigured or too-old CLI is
// caught before any project data is sent.
func (c *ClaudeCLI) Preflight(ctx context.Context) error {
	_, err := c.Complete(ctx, "", "Reply with OK.")
	return err
}

// Complete runs one isolated claude -p call and returns the assistant text.
func (c *ClaudeCLI) Complete(ctx context.Context, system, prompt string) (string, error) {
	cwd, err := os.MkdirTemp("", "tfa-claude-")
	if err != nil {
		return "", fmt.Errorf("creating claude working dir: %w", err)
	}
	defer os.RemoveAll(cwd)
	defer func() {
		if c.removeTranscript(cwd) && c.warn != nil {
			c.warn("removed a Claude CLI transcript that should not exist (--no-session-persistence was ignored)")
		}
	}()

	// The system prompt goes through a file and the prompt through stdin:
	// argv is size-limited (128 KiB per argument on Linux), visible in ps,
	// and a prompt starting with "-" would be parsed as an option. The file
	// lives outside the CLI's cwd so the model's directory stays empty.
	sysDir, err := os.MkdirTemp("", "tfa-claude-sys-")
	if err != nil {
		return "", fmt.Errorf("creating system prompt dir: %w", err)
	}
	defer os.RemoveAll(sysDir)
	sysFile := filepath.Join(sysDir, "system.txt")
	if err := os.WriteFile(sysFile, []byte(system), 0o600); err != nil {
		return "", fmt.Errorf("writing system prompt: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.bin(), c.buildArgs(sysFile)...)
	cmd.Dir = cwd
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = append(os.Environ(),
		"CLAUDE_CODE_DISABLE_CLAUDE_MDS=1",
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("starting claude: %w (is claude installed?)", err)
	}

	st := readEvents(stdout, cwd, cancel)
	waitErr := cmd.Wait()
	switch {
	case st.violation != nil:
		return "", st.violation
	case st.resultErr != nil:
		return "", fmt.Errorf("%w%s", st.resultErr, formatStderr(stderr.String()))
	case st.scanErr != nil:
		return "", fmt.Errorf("reading claude output: %w", st.scanErr)
	case waitErr != nil:
		return "", fmt.Errorf("claude exited: %w%s", waitErr, formatStderr(stderr.String()))
	case !st.sawInit:
		return "", missingInit()
	case !st.sawResult:
		return "", fmt.Errorf("claude sent no result event%s", formatStderr(stderr.String()))
	case st.truncated:
		return "", truncated("claude")
	}
	return st.text.String(), nil
}

func (c *ClaudeCLI) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return "claude"
}

func (c *ClaudeCLI) configDir() string {
	if c.ConfigDir != "" {
		return c.ConfigDir
	}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// removeTranscript deletes the CLI's project dir for our temp cwd, in case
// --no-session-persistence is ever ignored. The cwd is a fresh temp dir, so
// that project dir can only be ours; nothing else under ConfigDir is touched.
// It reports whether a transcript existed.
func (c *ClaudeCLI) removeTranscript(cwd string) bool {
	root := c.configDir()
	if root == "" {
		return false
	}
	found := false
	paths := []string{cwd}
	if real, err := filepath.EvalSymlinks(cwd); err == nil && real != cwd {
		paths = append(paths, real)
	}
	for _, p := range paths {
		dir := filepath.Join(root, "projects", slug(p))
		if _, err := os.Lstat(dir); err == nil {
			found = true
		}
		_ = os.RemoveAll(dir)
	}
	return found
}

func (c *ClaudeCLI) buildArgs(systemFile string) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--safe-mode",
		"--no-session-persistence",
		"--restricted",
		"--strict-mcp-config",
		"--setting-sources", "",
		"--tools", "",
		"--permission-prompts", "none",
		"--system-prompt-file", systemFile,
	}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	return args
}

// streamState is what readEvents learned from one CLI run.
type streamState struct {
	text      strings.Builder
	sawInit   bool
	sawResult bool
	truncated bool // the model stopped at its output limit
	violation error
	resultErr error
	scanErr   error
}

// readEvents consumes the stream-json output until EOF, so the process can
// always be reaped. Assistant text is kept only after a clean init event;
// on a violation the process is cancelled and the rest is discarded.
func readEvents(stdout io.Reader, cwd string, cancel func()) *streamState {
	st := &streamState{}
	fail := func(err error) {
		if st.violation == nil {
			st.violation = err
			st.text.Reset()
			cancel()
		}
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	for scanner.Scan() {
		if st.violation != nil {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		switch ev["type"] {
		case "system":
			if ev["subtype"] != "init" {
				continue
			}
			if err := checkInit(ev, cwd); err != nil {
				fail(err)
				continue
			}
			st.sawInit = true

		case "assistant":
			text := assistantText(ev)
			if text == "" {
				continue
			}
			if !st.sawInit {
				fail(missingInit())
				continue
			}
			st.text.WriteString(text)
			if msg, _ := ev["message"].(map[string]any); msg["stop_reason"] == "max_tokens" {
				st.truncated = true
			}

		case "result":
			st.sawResult = true
			isErr, _ := ev["is_error"].(bool)
			subtype, _ := ev["subtype"].(string)
			if isErr {
				result, _ := ev["result"].(string)
				st.resultErr = fmt.Errorf("claude error: %s", result)
			} else if subtype != "success" {
				// The result text of a failed run is not trustworthy, and
				// may be partial: never return it.
				st.resultErr = fmt.Errorf("claude run did not succeed (result subtype %q)", subtype)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		// Drain so the child does not block on a full pipe before Wait.
		_, _ = io.Copy(io.Discard, stdout)
		st.scanErr = err
	}
	return st
}

func assistantText(ev map[string]any) string {
	msg, _ := ev["message"].(map[string]any)
	content, _ := msg["content"].([]any)
	var sb strings.Builder
	for _, block := range content {
		b, _ := block.(map[string]any)
		if b["type"] == "text" {
			if text, ok := b["text"].(string); ok {
				sb.WriteString(text)
			}
		}
	}
	return sb.String()
}

// checkInit verifies from the CLI's init event that the isolation flags took
// effect, and returns a *security.Error naming the first violation.
func checkInit(ev map[string]any, wantCwd string) error {
	tools, ok := ev["tools"].([]any)
	if !ok {
		return isolationErr("Claude CLI did not report its tools", "tf-architect cannot verify the model has no tools")
	}
	if len(tools) > 0 {
		return isolationErr(fmt.Sprintf("Claude CLI enabled tool %v", tools[0]),
			"a tool lets the model read or change data outside the prompt")
	}

	servers, ok := ev["mcp_servers"].([]any)
	if !ok {
		return isolationErr("Claude CLI did not report its MCP servers", "tf-architect cannot verify no MCP server is connected")
	}
	if len(servers) > 0 {
		return isolationErr(fmt.Sprintf("Claude CLI connected MCP server %q", entryName(servers[0])),
			"MCP servers can inject instructions or read data outside tf-architect's control")
	}

	plugins, ok := ev["plugins"].([]any)
	if !ok {
		return isolationErr("Claude CLI did not report its plugins", "tf-architect cannot verify no user plugin is loaded")
	}
	for _, p := range plugins {
		m, _ := p.(map[string]any)
		if m["path"] != "builtin" {
			return isolationErr(fmt.Sprintf("Claude CLI loaded user plugin %q", entryName(p)),
				"plugins can inject instructions or read data outside tf-architect's control")
		}
	}

	cwd, _ := ev["cwd"].(string)
	if !sameDir(cwd, wantCwd) {
		return isolationErr(fmt.Sprintf("Claude CLI ran in %q instead of its empty temp dir", cwd),
			"the model could see files outside tf-architect's control")
	}

	version, _ := ev["claude_code_version"].(string)
	if !versionAtLeast(version, MinClaudeVersion) {
		return isolationErr(fmt.Sprintf("Claude CLI version %q is older than %s", version, MinClaudeVersion),
			"its isolation flags have not been verified")
	}
	return nil
}

func missingInit() error {
	return isolationErr("Claude CLI sent no init event before answering",
		"tf-architect cannot verify the call was isolated")
}

func isolationErr(what, why string) *security.Error {
	return security.New(what, why, isolationFix)
}

func entryName(v any) string {
	if m, ok := v.(map[string]any); ok {
		if name, ok := m["name"].(string); ok {
			return name
		}
	}
	return fmt.Sprint(v)
}

func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return false
	}
	return ra == rb
}

// versionAtLeast compares dotted versions numerically per segment. Anything
// unparsable counts as too old.
func versionAtLeast(v, min string) bool {
	got, ok := parseVersion(v)
	if !ok {
		return false
	}
	want, _ := parseVersion(min)
	for i := range want {
		g := 0
		if i < len(got) {
			g = got[i]
		}
		if g != want[i] {
			return g > want[i]
		}
	}
	return true
}

func parseVersion(v string) ([]int, bool) {
	if v == "" {
		return nil, false
	}
	var out []int
	for _, seg := range strings.Split(v, ".") {
		n, err := strconv.Atoi(seg)
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// slug maps a path to the directory name the CLI uses under projects/.
func slug(p string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, p)
}

// formatStderr returns the tail of stderr for error context (empty if none).
func formatStderr(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	const max = 500
	if len(s) > max {
		s = "..." + s[len(s)-max:]
	}
	return " — stderr: " + s
}
