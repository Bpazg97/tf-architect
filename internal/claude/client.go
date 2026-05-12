package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Client wraps the Claude Code CLI (claude -p) as a subprocess.
// Uses --bare --permission-mode bypassPermissions for fully automated runs.
// Context is injected explicitly into prompts as a workaround for the
// known --resume context-loss bug in -p mode.
type Client struct {
	workDir      string
	systemPrompt string
	sessionID    string
	contextBlob  string
}

func New(workDir, systemPrompt string) *Client {
	return &Client{
		workDir:      workDir,
		systemPrompt: systemPrompt,
	}
}

func (c *Client) SetContextBlob(blob string) {
	c.contextBlob = blob
}

func (c *Client) GetSessionID() string {
	return c.sessionID
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
		c.contextBlob = ""
	}

	args := c.buildArgs(finalPrompt)
	cmd := exec.Command("claude", args...)
	cmd.Dir = c.workDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting claude: %w (is claude installed?)", err)
	}

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
				return fmt.Errorf("claude error: %s", result)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading claude output: %w", err)
	}

	return cmd.Wait()
}

func (c *Client) buildArgs(prompt string) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--bare",
		"--permission-mode", "bypassPermissions",
		"--allowedTools", "Bash,Read,Write",
	}

	if c.systemPrompt != "" {
		args = append(args, "--system-prompt", c.systemPrompt)
	}

	if c.sessionID != "" {
		args = append(args, "--resume", c.sessionID)
	}

	args = append(args, prompt)
	return args
}
