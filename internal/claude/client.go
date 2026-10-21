package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Client wraps the Claude Code CLI (claude -p) as a subprocess.
//
// Claude runs confined to workDir: --restricted removes Bash and the other
// code-running tools and keeps the file tools inside the working directory,
// so it cannot read the source document, session state or mask.json.
// Context is injected explicitly into prompts as a workaround for the
// known --resume context-loss bug in -p mode.
type Client struct {
	workDir      string
	systemPrompt string
	sessionID    string
	contextBlob  string
	sanitize     func(string) (string, error)
}

// fileTools are the only tools Claude gets: read and edit files in workDir.
const fileTools = "Read,Write,Edit,Glob,Grep"

func New(workDir, systemPrompt string) *Client {
	return &Client{
		workDir:      workDir,
		systemPrompt: systemPrompt,
	}
}

// SetSanitizer installs a function applied to every prompt and to the system
// prompt before they leave the process. If it fails, nothing is sent.
func (c *Client) SetSanitizer(fn func(string) (string, error)) {
	c.sanitize = fn
}

// SetContextBlob sets context that is prepended once to the next query.
func (c *Client) SetContextBlob(blob string) {
	c.contextBlob = blob
}

// Query sends a prompt and returns the full accumulated text response.
func (c *Client) Query(prompt string) (string, error) {
	var sb strings.Builder
	err := c.StreamQuery(prompt, func(chunk string) {
		sb.WriteString(chunk)
	})
	return sb.String(), err
}

// StreamQuery sends a prompt and calls onChunk for each text chunk as it arrives.
// The context blob (if set) is prepended to the prompt once, then cleared.
func (c *Client) StreamQuery(prompt string, onChunk func(string)) error {
	finalPrompt := prompt
	if c.contextBlob != "" {
		finalPrompt = c.contextBlob + "\n\n---\n\n" + prompt
	}

	systemPrompt := c.systemPrompt
	if c.sanitize != nil {
		var err error
		if finalPrompt, err = c.sanitize(finalPrompt); err != nil {
			return fmt.Errorf("sanitizing prompt (nothing sent): %w", err)
		}
		if systemPrompt, err = c.sanitize(systemPrompt); err != nil {
			return fmt.Errorf("sanitizing system prompt (nothing sent): %w", err)
		}
	}

	c.contextBlob = ""

	args := c.buildArgs(systemPrompt, finalPrompt)
	cmd := exec.Command("claude", args...)
	cmd.Dir = c.workDir

	var stderr strings.Builder
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting claude: %w (is claude installed?)", err)
	}

	resultErr, scanErr := c.readEvents(stdout, onChunk)
	waitErr := cmd.Wait()
	switch {
	case resultErr != nil:
		return fmt.Errorf("%w%s", resultErr, formatStderr(stderr.String()))
	case scanErr != nil:
		return fmt.Errorf("reading claude output: %w", scanErr)
	case waitErr != nil:
		return fmt.Errorf("claude exited: %w%s", waitErr, formatStderr(stderr.String()))
	}
	return nil
}

// readEvents consumes the stream-json output until EOF, so the process can
// always be reaped. Returns the error reported by a result event, if any.
func (c *Client) readEvents(stdout io.Reader, onChunk func(string)) (resultErr, scanErr error) {
	scanner := bufio.NewScanner(stdout)
	// 4 MB buffer for large JSON lines (tool outputs can be large)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var event map[string]interface{}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		switch event["type"] {
		case "system":
			if sid, ok := event["session_id"].(string); ok && sid != "" {
				c.sessionID = sid
			}

		case "assistant":
			msg, ok := event["message"].(map[string]interface{})
			if !ok {
				continue
			}
			content, ok := msg["content"].([]interface{})
			if !ok {
				continue
			}
			for _, block := range content {
				b, ok := block.(map[string]interface{})
				if !ok {
					continue
				}
				if b["type"] == "text" {
					if text, ok := b["text"].(string); ok && text != "" {
						onChunk(text)
					}
				}
			}

		case "result":
			if sid, ok := event["session_id"].(string); ok && sid != "" {
				c.sessionID = sid
			}
			if isErr, _ := event["is_error"].(bool); isErr {
				result, _ := event["result"].(string)
				resultErr = fmt.Errorf("claude error: %s", result)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		// Drain so the child does not block on a full pipe before Wait.
		_, _ = io.Copy(io.Discard, stdout)
		return resultErr, err
	}
	return resultErr, nil
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

func (c *Client) buildArgs(systemPrompt, prompt string) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--bare",
		"--restricted",
		"--strict-mcp-config",
		"--tools", fileTools,
		"--permission-mode", "acceptEdits",
		"--permission-prompts", "none",
	}

	if systemPrompt != "" {
		args = append(args, "--system-prompt", systemPrompt)
	}

	if c.sessionID != "" {
		args = append(args, "--resume", c.sessionID)
	}

	args = append(args, prompt)
	return args
}
