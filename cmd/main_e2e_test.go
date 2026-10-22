package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"tf-architect/internal/security"
)

var e2eSecrets = []string{"123456789012", "10.20.0.0/16", "10.20.30.40", "db.prod.acme.com", "acme-corp", "10.99.99.99"}

const e2eTF = `terraform {
  required_version = ">= 1.5"
  required_providers {}
  backend "s3" {}
}
provider "aws" {
  default_tags {}
}
resource "aws_vpc" "main" {
  cidr_block = "CIDR_PRIVATE_001"
}
`

const e2eDocs = `===BEGIN_ROOT_README===
# Project
VPC CIDR_PRIVATE_001
===END_ROOT_README===
===BEGIN_ARCHITECTURE===
Network: CIDR_PRIVATE_001
===END_ARCHITECTURE===
===BEGIN_RUNBOOK===
Runbook
===END_RUNBOOK===
===BEGIN_VARIABLES===
Variables
===END_VARIABLES===
===BEGIN_PROJECT_AGENTS===
### Layout
VPC CIDR_PRIVATE_001
===END_PROJECT_AGENTS===`

func e2eFile(path, content string) string {
	return "===FILE: " + path + "===\n" + content + "===END FILE===\n"
}

// e2eModel answers like a model following the file protocol, keyed on the
// task header of the prompt.
func e2eModel(prompt string) string {
	switch {
	case strings.Contains(prompt, "===BEGIN_ROOT_README==="):
		return e2eDocs
	case strings.Contains(prompt, "## TASK: ANALYZE"), strings.Contains(prompt, "## TASK: CLARIFY"):
		return `{"ready": true, "missing": [], "inconsistencies": [], "summary": "ok"}`
	case strings.Contains(prompt, "## TASK: PLAN"):
		return `{"modules": [{"name": "vpc", "files": ["modules/vpc/main.tf"]}]}`
	case strings.Contains(prompt, "## TASK: MODULE vpc"):
		return e2eFile("modules/vpc/main.tf", "# vpc\n")
	case strings.Contains(prompt, "## TASK: REVIEW"):
		return "NO_CHANGES"
	case strings.Contains(prompt, "## TASK: MODULE root"):
		return e2eFile("main.tf", e2eTF)
	case strings.Contains(prompt, "## TASK: FIX"):
		return e2eFile("main.tf", e2eTF+"# fixed\n")
	case strings.Contains(prompt, "## TASK: ADJUST"):
		return e2eFile("main.tf", e2eTF+"# adjusted\n")
	}
	return "OK"
}

// TestHelperClaude is the stub claude binary, re-executed by the script that
// installClaudeStub writes. It logs stdin+args and answers via e2eModel.
func TestHelperClaude(t *testing.T) {
	if os.Getenv("GO_HELPER") != "1" {
		return
	}
	args := os.Args
	if i := slices.Index(args, "--"); i >= 0 {
		args = args[i+1:]
	}
	cwd, _ := os.Getwd()
	stdin, _ := io.ReadAll(os.Stdin)
	var system []byte
	if i := slices.Index(args, "--system-prompt-file"); i >= 0 && i+1 < len(args) {
		system, _ = os.ReadFile(args[i+1])
	}
	f, _ := os.OpenFile(os.Getenv("HELPER_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	fmt.Fprintf(f, "%s\n%s\n%s\n", strings.Join(args, "\x00"), system, stdin)
	f.Close()

	cwdJSON, _ := json.Marshal(cwd)
	text, _ := json.Marshal(e2eModel(string(stdin)))
	fmt.Println(`{"type":"system","subtype":"init","cwd":` + string(cwdJSON) +
		`,"tools":[],"mcp_servers":[],"plugins":[{"name":"cc-plugin-agents-md","path":"builtin","source":"cc-plugin-agents-md@builtin"}],"claude_code_version":"2.1.306"}`)
	fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":` + string(text) + `}]}}`)
	fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":` + string(text) + `}`)
	os.Exit(0)
}

// installClaudeStub points claudeBin at a stub and returns its request log.
func installClaudeStub(t *testing.T) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "requests.log")
	script := filepath.Join(dir, "claude")
	body := "#!/bin/sh\nGO_HELPER=1 exec '" + os.Args[0] + "' -test.run='^TestHelperClaude$' -- \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HELPER_LOG", logPath)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "config"))
	old := claudeBin
	claudeBin = script
	t.Cleanup(func() { claudeBin = old })
	return logPath
}

// fakeAPI serves a fake model for one API provider and records bodies.
type fakeAPI struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newFakeAPI(t *testing.T, provider string) *fakeAPI {
	f := &fakeAPI{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(raw))
		f.mu.Unlock()
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
			Contents []struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"contents"`
		}
		_ = json.Unmarshal(raw, &req)
		var prompt string
		switch {
		case len(req.Messages) > 0:
			prompt = req.Messages[len(req.Messages)-1].Content
		case len(req.Contents) > 0 && len(req.Contents[0].Parts) > 0:
			prompt = req.Contents[0].Parts[0].Text
		}
		out := e2eModel(prompt)
		var resp any
		switch provider {
		case "anthropic":
			resp = map[string]any{"content": []map[string]string{{"type": "text", "text": out}}}
		case "gemini":
			resp = map[string]any{"candidates": []map[string]any{{"content": map[string]any{"parts": []map[string]string{{"text": out}}}}}}
		default:
			resp = map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": out}}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAPI) sent() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.bodies, "\n")
}

// installTerraform puts a stub terraform first on a restricted PATH.
func installTerraform(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n  validate) echo '{\"valid\": true, \"diagnostics\": []}' ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin:/bin")
}

// setFlags sets the package flag variables for one runGenerate call.
func setFlags(t *testing.T, provider, baseURL, agents, out string) {
	t.Helper()
	oldP, oldM, oldB, oldY, oldA, oldO, oldN, oldF := providerFlag, modelFlag, baseURLFlag, yesCustomFlag, agentsFile, outputDir, noMaskFlag, forceFlag
	t.Cleanup(func() {
		providerFlag, modelFlag, baseURLFlag, yesCustomFlag, agentsFile, outputDir, noMaskFlag, forceFlag = oldP, oldM, oldB, oldY, oldA, oldO, oldN, oldF
	})
	providerFlag, modelFlag, baseURLFlag, yesCustomFlag, agentsFile, outputDir, noMaskFlag, forceFlag = provider, "test-model", baseURL, false, agents, out, false, false
}

func writeDoc(t *testing.T, dir string) string {
	t.Helper()
	doc := filepath.Join(dir, "doc.md")
	text := "# LLD\n\nAccount 123456789012, VPC 10.20.0.0/16, DB host db.prod.acme.com at 10.20.30.40.\nOwner = \"acme-corp\"\n"
	if err := os.WriteFile(doc, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestProviderMatrixNoLeak(t *testing.T) {
	keys := map[string]string{
		"anthropic":         "ANTHROPIC_API_KEY",
		"openai":            "OPENAI_API_KEY",
		"gemini":            "GEMINI_API_KEY",
		"openai-compatible": "TFA_API_KEY",
	}
	for _, provider := range []string{"claude", "anthropic", "openai", "gemini", "openai-compatible"} {
		t.Run(provider, func(t *testing.T) {
			installTerraform(t)
			work := t.TempDir()
			t.Chdir(work)
			doc := writeDoc(t, work)
			agents := filepath.Join(work, "agents.md")
			if err := os.WriteFile(agents, []byte("Rules. Our account is 123456789012.\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(work, "out")

			var sent func() string
			baseURL := ""
			if provider == "claude" {
				logPath := installClaudeStub(t)
				sent = func() string { b, _ := os.ReadFile(logPath); return string(b) }
			} else {
				api := newFakeAPI(t, provider)
				t.Setenv(keys[provider], "test-key")
				baseURL = api.URL
				sent = api.sent
			}
			setFlags(t, provider, baseURL, agents, out)

			if err := runGenerate(&cobra.Command{}, []string{doc}); err != nil {
				t.Fatalf("runGenerate: %v", err)
			}
			got := sent()
			if !strings.Contains(got, "## TASK: PLAN") {
				t.Fatalf("model never saw the PLAN task; sent:\n%.500s", got)
			}
			for _, s := range e2eSecrets {
				if strings.Contains(got, s) {
					t.Errorf("%s: leaked %q to the provider", provider, s)
				}
			}
			if provider != "claude" && strings.Contains(got, "test-key") {
				t.Error("API key must travel in headers, not in the body")
			}
			tf, err := os.ReadFile(filepath.Join(out, "main.tf"))
			if err != nil || !strings.Contains(string(tf), "10.20.0.0/16") {
				t.Errorf("main.tf not de-masked: %v\n%s", err, tf)
			}
			if _, err := os.Stat(filepath.Join(out, "AGENTS.md")); err != nil {
				t.Errorf("AGENTS.md missing: %v", err)
			}
		})
	}
}

func TestCustomEndpointNeedsConfirmation(t *testing.T) {
	const url = "https://llm.example.com/v1"
	var out bytes.Buffer

	// Non-interactive without the flag: stop.
	err := confirmCustomEndpoint(url, false, false, bufio.NewReader(strings.NewReader("y\n")), &out)
	var se *security.Error
	if !errors.As(err, &se) {
		t.Fatalf("want *security.Error, got %v", err)
	}
	// The flag accepts it; local URLs never ask.
	if err := confirmCustomEndpoint(url, true, false, nil, &out); err != nil {
		t.Errorf("--yes-custom-endpoint: %v", err)
	}
	if err := confirmCustomEndpoint("http://localhost:11434/v1", false, false, nil, &out); err != nil {
		t.Errorf("local URL: %v", err)
	}
	if err := confirmCustomEndpoint("", false, false, nil, &out); err != nil {
		t.Errorf("no URL: %v", err)
	}
	// Interactive: default is No.
	for in, ok := range map[string]bool{"y\n": true, "YES\n": true, "\n": false, "n\n": false, "maybe\n": false, "": false} {
		err := confirmCustomEndpoint(url, false, true, bufio.NewReader(strings.NewReader(in)), &out)
		if (err == nil) != ok {
			t.Errorf("answer %q: err=%v, want ok=%v", in, err, ok)
		}
		if err != nil && !errors.As(err, &se) {
			t.Errorf("answer %q: want *security.Error, got %v", in, err)
		}
	}
	if !strings.Contains(out.String(), "Continue? [y/N]") {
		t.Error("prompt missing")
	}

	// End to end: runGenerate stops before any provider call.
	work := t.TempDir()
	t.Chdir(work)
	doc := writeDoc(t, work)
	t.Setenv("TFA_API_KEY", "k")
	setFlags(t, "openai-compatible", url, "", filepath.Join(work, "out"))
	err = runGenerate(&cobra.Command{}, []string{doc})
	if !errors.As(err, &se) {
		t.Fatalf("runGenerate: want *security.Error, got %v", err)
	}
}

func TestSecurityErrorRendersPopup(t *testing.T) {
	var b bytes.Buffer
	se := security.New("what happened", "why it matters", "how to fix")
	if code := reportError(&b, fmt.Errorf("run: %w", se)); code != 2 {
		t.Errorf("exit code %d, want 2", code)
	}
	if !strings.Contains(b.String(), "SECURITY STOP") || !strings.Contains(b.String(), "Nothing sensitive was sent to the AI provider.") {
		t.Errorf("no pop-up:\n%s", b.String())
	}

	b.Reset()
	if code := reportError(&b, context.DeadlineExceeded); code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}
	if got := b.String(); got != "Error: context deadline exceeded\n" || strings.Contains(got, "SECURITY") {
		t.Errorf("plain error output: %q", got)
	}
	if reportError(&b, nil) != 0 {
		t.Error("nil error must exit 0")
	}
}
