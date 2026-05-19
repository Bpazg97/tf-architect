# tf-architect Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a Go CLI tool (`tf-architect`) that reads HLD/LLD documents and generates validated, production-grade AWS Terraform IaC using Claude Code as the AI engine, with full session persistence for resumable runs and layered documentation generation at the end.

**Architecture:** The tool follows a 7-phase FSM (ingest→analyze→clarify→generate→validate→document→done) orchestrated in Go. Claude Code CLI is called as a subprocess in `--bare --permission-mode bypassPermissions` mode with `--allowedTools Bash,Read,Write`. Session state persists to `.tf-architect/sessions/<doc-hash>/session.json` so runs are fully resumable after interruption or token exhaustion. Context is re-injected explicitly into each Claude invocation (workaround for the known `--resume` context-loss bug). Documentation is a two-pass process: Claude writes architectural docs first, then `terraform-docs` injects technical docs into `BEGIN_TF_DOCS` markers.

**Tech Stack:** Go 1.26, Cobra v1.8 (CLI), `os/exec` for Claude Code subprocess, `bufio` + NDJSON for stream-json parsing, `terraform`, `tflint`, `checkov`, `terraform-docs`

---

## File Map

```
terraform-aws/                         ← repo root (working dir)
├── go.mod
├── go.sum
├── Makefile
├── prompts/
│   └── system.md                      ← Golden rules system prompt (embedded)
├── cmd/
│   └── main.go                        ← Cobra CLI: run, status, reset subcommands
├── internal/
│   ├── ingestion/
│   │   └── converter.go               ← PDF/DOCX/MD/TXT → Markdown
│   ├── state/
│   │   └── session.go                 ← Session struct, Manager, HashDoc, PhaseOrder
│   ├── claude/
│   │   └── client.go                  ← Claude Code subprocess wrapper
│   ├── orchestrator/
│   │   ├── orchestrator.go            ← FSM runner
│   │   ├── resume.go                  ← ResumeDecision logic
│   │   └── prompts.go                 ← buildAnalysisPrompt, parseAnalysisJSON, etc.
│   ├── validation/
│   │   └── suite.go                   ← tf init, tf validate, tflint, checkov, golden-rules audit
│   └── docs/
│       ├── generator.go               ← Orchestrates both doc layers
│       ├── tfdocs.go                  ← Runs terraform-docs, writes .terraform-docs.yml
│       └── architectural.go           ← Claude-generated docs (ARCHITECTURE, RUNBOOK, VARIABLES)
└── docs/superpowers/plans/            ← This file lives here
```

---

## Task 1: Initialize Go Module and Scaffold

**Files:**
- Create: `go.mod`
- Create: `Makefile`
- Create: `prompts/system.md`

- [ ] **Step 1.1: Initialize Go module**

```bash
cd /home/bpazg/terraform-aws
go mod init tf-architect
```

Expected output: `go: creating new go.mod: module tf-architect`

- [ ] **Step 1.2: Add Cobra dependency**

```bash
go get github.com/spf13/cobra@v1.8.1
```

- [ ] **Step 1.3: Create directory scaffold**

```bash
mkdir -p cmd internal/ingestion internal/state internal/claude \
         internal/orchestrator internal/validation internal/docs prompts
```

- [ ] **Step 1.4: Write Makefile**

Create `Makefile`:
```makefile
.PHONY: build test lint install

BINARY := tf-architect

build:
	go build -o $(BINARY) ./cmd/

test:
	go test ./... -v -count=1

lint:
	go vet ./...

install: build
	cp $(BINARY) /usr/local/bin/$(BINARY)

deps-check:
	@which terraform     || echo "MISSING: terraform"
	@which tflint        || echo "MISSING: tflint (optional)"
	@which checkov       || echo "MISSING: checkov (optional)"
	@which terraform-docs || echo "MISSING: terraform-docs (optional)"
	@which pdftotext     || echo "MISSING: pdftotext (apt install poppler-utils)"
	@which pandoc        || echo "MISSING: pandoc (optional, improves .docx quality)"
```

- [ ] **Step 1.5: Write system prompt (Golden Rules)**

Create `prompts/system.md`:
```markdown
You are an expert AWS infrastructure engineer generating production-grade Terraform IaC.
You write files directly to disk using the Write and Bash tools.

## MANDATORY GOLDEN RULES — never violate these

1. **Remote State**: Always configure `backend "s3"` with DynamoDB state locking.
2. **Version Constraints**: Always set `required_version` in terraform block and pin every provider in `required_providers`.
3. **Tagging**: Always use `default_tags` in the AWS provider OR `merge(var.tags, {...})` on every resource. Never create untagged resources.
4. **No Plaintext Secrets**: Use `aws_secretsmanager_secret_version` data source or `sensitive` variables. Never hardcode passwords, tokens, or API keys.
5. **Security Groups**: Never use `cidr_blocks = ["0.0.0.0/0"]` for ingress. Use specific CIDRs or security group references.
6. **IAM Least Privilege**: Never use `"Action": "*"` or `"Resource": "*"` without a documented justification comment.
7. **No Hardcoded Account IDs**: Use `data "aws_caller_identity" "current" {}` and reference `data.aws_caller_identity.current.account_id`.
8. **Encryption at Rest**: Enable encryption for all storage: S3 (SSE), RDS (storage_encrypted=true), EBS (encrypted=true), EFS (encrypted=true).
9. **Multi-AZ**: Deploy stateful services (RDS, ElastiCache, ALB) across at least 2 availability zones.
10. **Sensitive Outputs**: Mark any output containing a secret or ARN as `sensitive = true`.

## FILE STRUCTURE CONVENTION

Use this module layout:
```
<output-dir>/
├── main.tf           (root module: calls child modules, declares backend)
├── variables.tf      (all input variables with descriptions and types)
├── outputs.tf        (all outputs)
├── providers.tf      (terraform block + AWS provider with default_tags)
├── versions.tf       (required_version + required_providers)
├── data.tf           (data sources)
└── modules/
    └── <component>/
        ├── main.tf
        ├── variables.tf
        └── outputs.tf
```

## GENERATION PROTOCOL

When asked to generate Terraform IaC:
1. Write each file using the Write tool (never echo to files via Bash).
2. After writing each module, output the line: `STEP_COMPLETE:<module_name>`
3. Never hardcode region — always use `var.aws_region`.
4. Use `var.environment` for environment-specific config (dev/staging/prod).
5. Add a `tags` variable to every module with type `map(string)` and `default = {}`.
```

- [ ] **Step 1.6: Commit scaffold**

```bash
git add go.mod Makefile prompts/ cmd/ internal/
git commit -m "chore: initialize tf-architect Go module and scaffold"
```

---

## Task 2: Document Ingestion Package

**Files:**
- Create: `internal/ingestion/converter.go`
- Create: `internal/ingestion/converter_test.go`

- [ ] **Step 2.1: Write failing test**

Create `internal/ingestion/converter_test.go`:
```go
package ingestion_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tf-architect/internal/ingestion"
)

func TestConvertMarkdown(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.md")
	content := "# Test\n\nHello world"
	_ = os.WriteFile(path, []byte(content), 0644)

	result, err := ingestion.Convert(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Markdown != content {
		t.Errorf("expected %q, got %q", content, result.Markdown)
	}
	if result.WasConverted {
		t.Error("markdown should not be marked as converted")
	}
	if result.SourceFormat != ".md" {
		t.Errorf("expected .md, got %s", result.SourceFormat)
	}
}

func TestConvertTxt(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.txt")
	content := "Hello plain text"
	_ = os.WriteFile(path, []byte(content), 0644)

	result, err := ingestion.Convert(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Markdown != content {
		t.Errorf("expected %q, got %q", content, result.Markdown)
	}
}

func TestConvertUnsupportedFormat(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.xlsx")
	_ = os.WriteFile(path, []byte("data"), 0644)

	_, err := ingestion.Convert(path)
	if err == nil {
		t.Error("expected error for unsupported format")
	}
	if !strings.Contains(err.Error(), "unsupported format") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestEstimateTokens(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.md")
	content := strings.Repeat("a", 4000)
	_ = os.WriteFile(path, []byte(content), 0644)

	result, err := ingestion.Convert(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// ~4 chars per token → ~1000 tokens
	if result.EstimatedTokens < 900 || result.EstimatedTokens > 1100 {
		t.Errorf("expected ~1000 tokens, got %d", result.EstimatedTokens)
	}
}
```

- [ ] **Step 2.2: Run test to verify it fails**

```bash
go test ./internal/ingestion/ -v 2>&1 | head -20
```

Expected: `cannot find package` or `no Go files`

- [ ] **Step 2.3: Implement converter.go**

Create `internal/ingestion/converter.go`:
```go
package ingestion

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type ConversionResult struct {
	Markdown        string
	EstimatedTokens int
	SourceFormat    string
	WasConverted    bool
}

// Convert accepts any supported path and returns clean Markdown.
// Text extraction is always preferred over vision/OCR.
func Convert(path string) (*ConversionResult, error) {
	ext := strings.ToLower(filepath.Ext(path))

	switch ext {
	case ".md", ".txt", ".rst":
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		md := string(data)
		return &ConversionResult{
			Markdown:        md,
			EstimatedTokens: estimateTokens(md),
			SourceFormat:    ext,
			WasConverted:    false,
		}, nil

	case ".pdf":
		return convertPDF(path)

	case ".docx", ".doc":
		return convertDOCX(path)

	default:
		return nil, fmt.Errorf("unsupported format: %s", ext)
	}
}

// convertPDF uses pdftotext (poppler) for native text extraction.
// Falls back to tesseract OCR if the PDF is image-only (scanned).
func convertPDF(path string) (*ConversionResult, error) {
	var out bytes.Buffer
	cmd := exec.Command("pdftotext", "-layout", "-enc", "UTF-8", path, "-")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pdftotext failed (install poppler-utils): %w", err)
	}
	extracted := out.String()

	if isScannedPDF(extracted, path) {
		return convertPDFwithOCR(path)
	}

	md := cleanAndMarkdownify(extracted)
	return &ConversionResult{
		Markdown:        md,
		EstimatedTokens: estimateTokens(md),
		SourceFormat:    ".pdf",
		WasConverted:    true,
	}, nil
}

func isScannedPDF(text, path string) bool {
	pages := countPDFPages(path)
	if pages == 0 {
		return false
	}
	charsPerPage := len(strings.TrimSpace(text)) / pages
	return charsPerPage < 100
}

func countPDFPages(path string) int {
	var out bytes.Buffer
	cmd := exec.Command("pdfinfo", path)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return 0
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "Pages:") {
			var n int
			fmt.Sscanf(strings.TrimPrefix(line, "Pages:"), "%d", &n)
			return n
		}
	}
	return 0
}

func convertPDFwithOCR(path string) (*ConversionResult, error) {
	tmpDir, err := os.MkdirTemp("", "tf-ocr-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	cmd := exec.Command("pdftoppm", "-r", "300", "-png", path, filepath.Join(tmpDir, "page"))
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pdftoppm failed (install poppler-utils): %w", err)
	}

	var allText strings.Builder
	pages, _ := filepath.Glob(filepath.Join(tmpDir, "page-*.png"))
	for _, page := range pages {
		var ocrOut bytes.Buffer
		ocrCmd := exec.Command("tesseract", page, "stdout", "-l", "spa+eng", "--psm", "1")
		ocrCmd.Stdout = &ocrOut
		if err := ocrCmd.Run(); err != nil {
			continue
		}
		allText.WriteString(ocrOut.String())
		allText.WriteString("\n\n")
	}

	md := cleanAndMarkdownify(allText.String())
	return &ConversionResult{
		Markdown:        md,
		EstimatedTokens: estimateTokens(md),
		SourceFormat:    ".pdf (scanned→OCR)",
		WasConverted:    true,
	}, nil
}

func convertDOCX(path string) (*ConversionResult, error) {
	if _, err := exec.LookPath("pandoc"); err == nil {
		return convertDOCXwithPandoc(path)
	}
	return convertDOCXwithPython(path)
}

func convertDOCXwithPandoc(path string) (*ConversionResult, error) {
	var out bytes.Buffer
	cmd := exec.Command("pandoc", path, "-t", "markdown_strict", "--wrap=none")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pandoc failed: %w", err)
	}
	md := out.String()
	return &ConversionResult{
		Markdown:        md,
		EstimatedTokens: estimateTokens(md),
		SourceFormat:    ".docx (pandoc)",
		WasConverted:    true,
	}, nil
}

func convertDOCXwithPython(path string) (*ConversionResult, error) {
	pyScript := `import sys, docx, re
def docx_to_md(path):
    doc = docx.Document(path)
    lines = []
    for p in doc.paragraphs:
        style = p.style.name.lower()
        text = p.text.strip()
        if not text:
            lines.append("")
            continue
        if "heading 1" in style:
            lines.append(f"# {text}")
        elif "heading 2" in style:
            lines.append(f"## {text}")
        elif "heading 3" in style:
            lines.append(f"### {text}")
        elif "list" in style:
            lines.append(f"- {text}")
        else:
            lines.append(text)
    for table in doc.tables:
        rows = []
        for i, row in enumerate(table.rows):
            cells = [c.text.strip().replace("\n", " ") for c in row.cells]
            rows.append("| " + " | ".join(cells) + " |")
            if i == 0:
                rows.append("| " + " | ".join(["---"] * len(cells)) + " |")
        lines.extend(rows)
        lines.append("")
    return "\n".join(lines)
print(docx_to_md(sys.argv[1]))`

	var out, stderr bytes.Buffer
	cmd := exec.Command("python3", "-c", pyScript, path)
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("python-docx failed (pip install python-docx): %w\n%s", err, stderr.String())
	}
	md := out.String()
	return &ConversionResult{
		Markdown:        md,
		EstimatedTokens: estimateTokens(md),
		SourceFormat:    ".docx (python-docx)",
		WasConverted:    true,
	}, nil
}

// cleanAndMarkdownify removes repeating headers/footers and normalises PDF text.
func cleanAndMarkdownify(text string) string {
	lines := strings.Split(text, "\n")

	freq := make(map[string]int)
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if len(trimmed) > 5 && len(trimmed) < 100 {
			freq[trimmed]++
		}
	}

	pageCount := strings.Count(text, "\f") + 1
	threshold := pageCount / 5
	if threshold < 3 {
		threshold = 3
	}

	var cleaned []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "\f" {
			continue
		}
		if trimmed == "" && len(cleaned) > 0 && cleaned[len(cleaned)-1] == "" {
			continue
		}
		if freq[trimmed] >= threshold {
			continue
		}
		cleaned = append(cleaned, trimmed)
	}

	return strings.Join(cleaned, "\n")
}

// estimateTokens approximates token count (~4 chars/token for technical EN/ES text).
func estimateTokens(text string) int {
	return len(text) / 4
}
```

- [ ] **Step 2.4: Run tests to verify they pass**

```bash
go test ./internal/ingestion/ -v -count=1
```

Expected: all 4 tests PASS

- [ ] **Step 2.5: Commit**

```bash
git add internal/ingestion/
git commit -m "feat(ingestion): PDF/DOCX/MD/TXT converter with OCR fallback"
```

---

## Task 3: Session State Package

**Files:**
- Create: `internal/state/session.go`
- Create: `internal/state/session_test.go`

- [ ] **Step 3.1: Write failing test**

Create `internal/state/session_test.go`:
```go
package state_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"tf-architect/internal/state"
)

func TestHashDoc(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "doc.md")
	_ = os.WriteFile(path, []byte("hello"), 0644)

	h1, err := state.HashDoc(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(h1) != 64 {
		t.Errorf("expected 64-char SHA256 hex, got %d chars", len(h1))
	}

	// Same content → same hash
	h2, _ := state.HashDoc(path)
	if h1 != h2 {
		t.Error("same content should produce same hash")
	}

	// Different content → different hash
	_ = os.WriteFile(path, []byte("different"), 0644)
	h3, _ := state.HashDoc(path)
	if h1 == h3 {
		t.Error("different content should produce different hash")
	}
}

func TestManagerSaveLoad(t *testing.T) {
	tmp := t.TempDir()
	mgr := state.NewManager(tmp)

	sess := &state.Session{
		DocPath:      "/tmp/test.md",
		DocHash:      "abc123",
		CreatedAt:    time.Now(),
		CurrentPhase: state.PhaseIngest,
	}

	if err := mgr.Save(sess); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := mgr.Load("abc123")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected session, got nil")
	}
	if loaded.DocPath != sess.DocPath {
		t.Errorf("expected %s, got %s", sess.DocPath, loaded.DocPath)
	}
}

func TestManagerLoadNonExistent(t *testing.T) {
	tmp := t.TempDir()
	mgr := state.NewManager(tmp)

	loaded, err := mgr.Load("nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loaded != nil {
		t.Error("expected nil for non-existent session")
	}
}

func TestPhaseOrder(t *testing.T) {
	if !state.PhaseIngest.Before(state.PhaseAnalyze) {
		t.Error("Ingest should be before Analyze")
	}
	if !state.PhaseGenerate.Before(state.PhaseValidate) {
		t.Error("Generate should be before Validate")
	}
	if state.PhaseDone.Before(state.PhaseIngest) {
		t.Error("Done should not be before Ingest")
	}
}

func TestFindByDocPath(t *testing.T) {
	tmp := t.TempDir()
	mgr := state.NewManager(tmp)

	sess := &state.Session{
		DocPath:      "/tmp/my-doc.pdf",
		DocHash:      "deadbeef",
		CreatedAt:    time.Now(),
		CurrentPhase: state.PhaseDone,
	}
	_ = mgr.Save(sess)

	found, err := mgr.FindByDocPath("/tmp/my-doc.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if found == nil {
		t.Fatal("expected to find session by path")
	}
	if found.DocHash != "deadbeef" {
		t.Errorf("expected deadbeef, got %s", found.DocHash)
	}

	// Non-existent path returns nil
	notFound, _ := mgr.FindByDocPath("/tmp/other.pdf")
	if notFound != nil {
		t.Error("expected nil for unknown path")
	}
}
```

- [ ] **Step 3.2: Run test to verify it fails**

```bash
go test ./internal/state/ -v 2>&1 | head -10
```

Expected: `no Go files` or package errors

- [ ] **Step 3.3: Implement session.go**

Create `internal/state/session.go`:
```go
package state

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Phase string

const (
	PhaseIngest   Phase = "ingest"
	PhaseAnalyze  Phase = "analyze"
	PhaseClarify  Phase = "clarify"
	PhaseGenerate Phase = "generate"
	PhaseValidate Phase = "validate"
	PhaseDocument Phase = "document"
	PhaseDone     Phase = "done"
	PhaseFailed   Phase = "failed"
)

var phaseOrder = map[Phase]int{
	PhaseIngest:   0,
	PhaseAnalyze:  1,
	PhaseClarify:  2,
	PhaseGenerate: 3,
	PhaseValidate: 4,
	PhaseDocument: 5,
	PhaseDone:     6,
	PhaseFailed:   -1,
}

func (p Phase) Before(other Phase) bool {
	return phaseOrder[p] < phaseOrder[other]
}

func (p Phase) AtOrBefore(other Phase) bool {
	return phaseOrder[p] <= phaseOrder[other]
}

type Checkpoint struct {
	Phase        Phase     `json:"phase"`
	ClarifyRound int       `json:"clarify_round,omitempty"`
	GenerateStep string    `json:"generate_step,omitempty"`
	CompletedAt  time.Time `json:"completed_at"`
}

type ClarificationRound struct {
	Round     int            `json:"round"`
	Questions []string       `json:"questions"`
	Answers   map[int]string `json:"answers"`
}

type ValidationRun struct {
	RunAt   time.Time       `json:"run_at"`
	Passed  bool            `json:"passed"`
	Results map[string]bool `json:"results"`
	Errors  []string        `json:"errors,omitempty"`
}

type Session struct {
	DocPath   string    `json:"doc_path"`
	DocHash   string    `json:"doc_hash"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	CurrentPhase   Phase      `json:"current_phase"`
	LastCheckpoint Checkpoint `json:"last_checkpoint"`

	ClaudeSessionID string `json:"claude_session_id,omitempty"`

	DocMarkdown    string               `json:"doc_markdown"`
	AnalysisJSON   string               `json:"analysis_json"`
	Clarifications []ClarificationRound `json:"clarifications"`
	GeneratedFiles []string             `json:"generated_files"`
	ValidationRuns []ValidationRun      `json:"validation_runs"`

	PreviousDocHash string `json:"previous_doc_hash,omitempty"`
	DiffSummary     string `json:"diff_summary,omitempty"`
}

type Manager struct {
	BaseDir string
}

func NewManager(projectDir string) *Manager {
	return &Manager{
		BaseDir: filepath.Join(projectDir, ".tf-architect", "sessions"),
	}
}

func (m *Manager) sessionDir(docHash string) string {
	return filepath.Join(m.BaseDir, docHash)
}

func (m *Manager) sessionPath(docHash string) string {
	return filepath.Join(m.sessionDir(docHash), "session.json")
}

func (m *Manager) Load(docHash string) (*Session, error) {
	data, err := os.ReadFile(m.sessionPath(docHash))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Session
	return &s, json.Unmarshal(data, &s)
}

func (m *Manager) Save(s *Session) error {
	dir := m.sessionDir(s.DocHash)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	s.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.sessionPath(s.DocHash), data, 0644)
}

func (m *Manager) Advance(s *Session, phase Phase, checkpoint Checkpoint) error {
	s.CurrentPhase = phase
	checkpoint.Phase = phase
	checkpoint.CompletedAt = time.Now()
	s.LastCheckpoint = checkpoint
	return m.Save(s)
}

// FindByDocPath returns the most recently updated session for a given doc path.
func (m *Manager) FindByDocPath(docPath string) (*Session, error) {
	entries, err := os.ReadDir(m.BaseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var latest *Session
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		s, err := m.Load(entry.Name())
		if err != nil || s == nil {
			continue
		}
		if s.DocPath == docPath {
			if latest == nil || s.UpdatedAt.After(latest.UpdatedAt) {
				latest = s
			}
		}
	}
	return latest, nil
}

// HashDoc returns the SHA256 hex digest of the file at path.
func HashDoc(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum), nil
}
```

- [ ] **Step 3.4: Run tests**

```bash
go test ./internal/state/ -v -count=1
```

Expected: all 5 tests PASS

- [ ] **Step 3.5: Commit**

```bash
git add internal/state/
git commit -m "feat(state): session persistence with phase ordering and doc hashing"
```

---

## Task 4: Claude Code Client

**Files:**
- Create: `internal/claude/client.go`
- Create: `internal/claude/client_test.go`

- [ ] **Step 4.1: Write failing test**

Create `internal/claude/client_test.go`:
```go
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
```

- [ ] **Step 4.2: Run test to verify it fails**

```bash
go test ./internal/claude/ -v 2>&1 | head -10
```

Expected: package not found

- [ ] **Step 4.3: Implement client.go**

Create `internal/claude/client.go`:
```go
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
			// Capture session_id from init event
			if sid, ok := event["session_id"].(string); ok && sid != "" {
				c.sessionID = sid
			}

		case "assistant":
			// Stream text content blocks as they arrive
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
			// Capture final session_id (may differ from init if --fork-session)
			if sid, ok := event["session_id"].(string); ok && sid != "" {
				c.sessionID = sid
			}
			// Check for errors
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

	// Include session ID for best-effort continuity (context still injected via prompt)
	if c.sessionID != "" {
		args = append(args, "--resume", c.sessionID)
	}

	args = append(args, prompt)
	return args
}
```

- [ ] **Step 4.4: Run tests**

```bash
go test ./internal/claude/ -v -count=1
```

Expected: all 3 tests PASS (TestClaudeAvailable will skip if claude not in PATH)

- [ ] **Step 4.5: Commit**

```bash
git add internal/claude/
git commit -m "feat(claude): Claude Code CLI subprocess wrapper with stream-json parsing"
```

---

## Task 5: Validation Suite

**Files:**
- Create: `internal/validation/suite.go`
- Create: `internal/validation/suite_test.go`

- [ ] **Step 5.1: Write failing test**

Create `internal/validation/suite_test.go`:
```go
package validation_test

import (
	"os"
	"path/filepath"
	"testing"

	"tf-architect/internal/validation"
)

func TestGoldenRulesAuditPasses(t *testing.T) {
	dir := t.TempDir()
	// Write minimal valid Terraform that passes all golden rules
	content := `
terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.0" }
  }
  backend "s3" {
    bucket = "my-tfstate"
    key    = "terraform.tfstate"
    region = "eu-west-1"
  }
}

provider "aws" {
  default_tags { tags = var.tags }
}

variable "tags" { type = map(string); default = {} }
`
	_ = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(content), 0644)

	suite := validation.New(dir)
	result := suite.RunGoldenRulesAudit()

	if !result.Passed {
		t.Errorf("expected golden rules to pass, got errors: %v", result.Errors)
	}
}

func TestGoldenRulesAuditDetectsMissingBackend(t *testing.T) {
	dir := t.TempDir()
	content := `
terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.0" }
  }
}
provider "aws" {
  default_tags { tags = {} }
}
`
	_ = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(content), 0644)

	suite := validation.New(dir)
	result := suite.RunGoldenRulesAudit()

	if result.Passed {
		t.Error("expected golden rules to fail due to missing backend")
	}

	found := false
	for _, e := range result.Errors {
		if len(e) > 0 {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected at least one error for missing backend")
	}
}

func TestGoldenRulesAuditDetectsOpenIngress(t *testing.T) {
	dir := t.TempDir()
	content := `
resource "aws_security_group" "bad" {
  ingress {
    cidr_blocks = ["0.0.0.0/0"]
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
  }
}
`
	_ = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(content), 0644)

	suite := validation.New(dir)
	result := suite.RunGoldenRulesAudit()

	if result.Passed {
		t.Error("expected golden rules to fail due to open ingress")
	}
}
```

- [ ] **Step 5.2: Run test to verify it fails**

```bash
go test ./internal/validation/ -v 2>&1 | head -10
```

- [ ] **Step 5.3: Implement suite.go**

Create `internal/validation/suite.go`:
```go
package validation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type ValidationResult struct {
	Stage  string
	Passed bool
	Output string
	Errors []string
}

type Suite struct {
	Dir string
}

func New(dir string) *Suite {
	return &Suite{Dir: dir}
}

// Run executes all available validators in order. Stops at the first critical failure.
func (s *Suite) Run() ([]ValidationResult, error) {
	var results []ValidationResult

	r := s.runTerraformInit()
	results = append(results, r)
	if !r.Passed {
		return results, fmt.Errorf("terraform init failed — cannot proceed")
	}

	r = s.runTerraformValidate()
	results = append(results, r)
	if !r.Passed {
		return results, fmt.Errorf("terraform validate failed")
	}

	if _, err := exec.LookPath("tflint"); err == nil {
		results = append(results, s.runTFLint())
	}

	if _, err := exec.LookPath("checkov"); err == nil {
		results = append(results, s.runCheckov())
	}

	results = append(results, s.RunGoldenRulesAudit())

	// Return error if any stage failed
	for _, r := range results {
		if !r.Passed {
			return results, fmt.Errorf("validation failed at stage: %s", r.Stage)
		}
	}
	return results, nil
}

func (s *Suite) runTerraformInit() ValidationResult {
	var out bytes.Buffer
	cmd := exec.Command("terraform", "init", "-backend=false", "-no-color", "-input=false")
	cmd.Dir = s.Dir
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return ValidationResult{
		Stage:  "terraform init",
		Passed: err == nil,
		Output: out.String(),
	}
}

func (s *Suite) runTerraformValidate() ValidationResult {
	var out bytes.Buffer
	cmd := exec.Command("terraform", "validate", "-no-color", "-json")
	cmd.Dir = s.Dir
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()

	var tfResult struct {
		Valid        bool `json:"valid"`
		Diagnostics []struct {
			Severity string `json:"severity"`
			Summary  string `json:"summary"`
			Detail   string `json:"detail"`
		} `json:"diagnostics"`
	}

	var errs []string
	if jsonErr := json.Unmarshal(out.Bytes(), &tfResult); jsonErr == nil {
		for _, d := range tfResult.Diagnostics {
			if d.Severity == "error" {
				errs = append(errs, fmt.Sprintf("%s: %s", d.Summary, d.Detail))
			}
		}
	}

	return ValidationResult{
		Stage:  "terraform validate",
		Passed: err == nil && len(errs) == 0,
		Output: out.String(),
		Errors: errs,
	}
}

func (s *Suite) runTFLint() ValidationResult {
	var out bytes.Buffer
	cmd := exec.Command("tflint", "--format", "json", "--recursive")
	cmd.Dir = s.Dir
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return ValidationResult{
		Stage:  "tflint",
		Passed: err == nil,
		Output: out.String(),
	}
}

func (s *Suite) runCheckov() ValidationResult {
	var out bytes.Buffer
	cmd := exec.Command("checkov",
		"-d", s.Dir,
		"--framework", "terraform",
		"--output", "json",
		"--quiet",
		"--skip-check", "CKV_AWS_144,CKV_AWS_18",
	)
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return ValidationResult{
		Stage:  "checkov",
		Passed: err == nil,
		Output: out.String(),
	}
}

type ruleCheck struct {
	Name     string
	Pattern  string
	IsRegex  bool
	Inverted bool // true = flag if NOT found (must-have)
	Severity string
	Message  string
}

// RunGoldenRulesAudit is exported so tests can call it directly.
func (s *Suite) RunGoldenRulesAudit() ValidationResult {
	files, _ := collectTFFiles(s.Dir)
	content := concatenateTFFiles(files)

	rules := []ruleCheck{
		{
			Name:     "no-0.0.0.0-ingress",
			Pattern:  `cidr_blocks\s*=\s*\[?"0\.0\.0\.0/0"`,
			IsRegex:  true,
			Severity: "error",
			Message:  "Security group with ingress 0.0.0.0/0 — golden rule violation",
		},
		{
			Name:     "no-hardcoded-account-id",
			Pattern:  `"\d{12}"`,
			IsRegex:  true,
			Severity: "warning",
			Message:  "Possible hardcoded AWS account ID — use data.aws_caller_identity",
		},
		{
			Name:     "no-wildcard-iam-action",
			Pattern:  `"Action"\s*:\s*"\*"`,
			IsRegex:  true,
			Severity: "error",
			Message:  "IAM policy with Action:* — least privilege violation",
		},
		{
			Name:     "no-plaintext-secret",
			Pattern:  `(?i)(password|secret|token|api_key)\s*=\s*"[^$][^{]`,
			IsRegex:  true,
			Severity: "error",
			Message:  "Possible plaintext secret — use aws_secretsmanager or sensitive variable",
		},
		{
			Name:     "has-required-tags",
			Pattern:  "default_tags",
			IsRegex:  false,
			Inverted: true,
			Severity: "warning",
			Message:  "No default_tags detected — golden rule: all resources must be tagged",
		},
		{
			Name:     "has-backend-config",
			Pattern:  `backend\s+"s3"`,
			IsRegex:  true,
			Inverted: true,
			Severity: "error",
			Message:  "No S3 backend configured — local state violates golden rules",
		},
		{
			Name:     "has-required-version",
			Pattern:  "required_version",
			IsRegex:  false,
			Inverted: true,
			Severity: "warning",
			Message:  "terraform.required_version not found",
		},
		{
			Name:     "has-provider-version",
			Pattern:  "required_providers",
			IsRegex:  false,
			Inverted: true,
			Severity: "warning",
			Message:  "required_providers not found — provider versions unpinned",
		},
	}

	var errs, warnings []string
	for _, rule := range rules {
		var found bool
		if rule.IsRegex {
			re, err := regexp.Compile(rule.Pattern)
			if err == nil {
				found = re.MatchString(content)
			}
		} else {
			found = strings.Contains(content, rule.Pattern)
		}

		violated := found
		if rule.Inverted {
			violated = !found
		}

		if violated {
			msg := fmt.Sprintf("[%s] %s: %s", strings.ToUpper(rule.Severity), rule.Name, rule.Message)
			if rule.Severity == "error" {
				errs = append(errs, msg)
			} else {
				warnings = append(warnings, msg)
			}
		}
	}

	all := append(errs, warnings...)
	return ValidationResult{
		Stage:  "golden-rules-audit",
		Passed: len(errs) == 0,
		Output: strings.Join(all, "\n"),
		Errors: errs,
	}
}

func collectTFFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".tf") {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

func concatenateTFFiles(files []string) string {
	var sb strings.Builder
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err == nil {
			sb.Write(data)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}
```

- [ ] **Step 5.4: Run tests**

```bash
go test ./internal/validation/ -v -count=1
```

Expected: all 3 tests PASS

- [ ] **Step 5.5: Commit**

```bash
git add internal/validation/
git commit -m "feat(validation): tf-validate, tflint, checkov and golden-rules audit suite"
```

---

## Task 6: Orchestrator Helpers (Prompts)

**Files:**
- Create: `internal/orchestrator/prompts.go`
- Create: `internal/orchestrator/prompts_test.go`

- [ ] **Step 6.1: Write failing test**

Create `internal/orchestrator/prompts_test.go`:
```go
package orchestrator_test

import (
	"strings"
	"testing"

	"tf-architect/internal/orchestrator"
)

func TestBuildAnalysisPrompt(t *testing.T) {
	prompt := orchestrator.BuildAnalysisPrompt("# Architecture\n\nVPC in eu-west-1")
	if !strings.Contains(prompt, "JSON") {
		t.Error("analysis prompt should request JSON output")
	}
	if !strings.Contains(prompt, "ready") {
		t.Error("analysis prompt should mention 'ready' field")
	}
	if !strings.Contains(prompt, "eu-west-1") {
		t.Error("analysis prompt should contain the document content")
	}
}

func TestParseAnalysisJSONValid(t *testing.T) {
	raw := `Some text before
` + "```json" + `
{"ready": false, "missing": ["AWS region", "instance type"], "inconsistencies": [], "summary": "EKS cluster"}
` + "```" + `
Some text after`

	result := orchestrator.ParseAnalysisJSON(raw)
	if result.Ready {
		t.Error("expected Ready=false")
	}
	if len(result.Missing) != 2 {
		t.Errorf("expected 2 missing items, got %d", len(result.Missing))
	}
	if result.Summary != "EKS cluster" {
		t.Errorf("unexpected summary: %s", result.Summary)
	}
}

func TestParseAnalysisJSONFallback(t *testing.T) {
	// Unparseable response → not ready, one missing item
	result := orchestrator.ParseAnalysisJSON("I cannot parse this as JSON at all")
	if result.Ready {
		t.Error("unparseable response should yield Ready=false")
	}
	if len(result.Missing) == 0 {
		t.Error("expected at least one missing item for parse failure")
	}
}

func TestBuildQuestions(t *testing.T) {
	qs := orchestrator.BuildQuestions(
		[]string{"What AWS region?", "What instance type?"},
		[]string{"Conflicting AZ count"},
	)
	if len(qs) != 3 {
		t.Errorf("expected 3 questions, got %d", len(qs))
	}
}

func TestBuildClarifyPrompt(t *testing.T) {
	questions := []string{"What AWS region?", "What instance type?"}
	answers := []string{"eu-west-1", "t3.medium"}
	prompt := orchestrator.BuildClarifyPrompt(questions, answers)

	if !strings.Contains(prompt, "eu-west-1") {
		t.Error("clarify prompt should contain answers")
	}
	if !strings.Contains(prompt, "JSON") {
		t.Error("clarify prompt should request JSON")
	}
}
```

- [ ] **Step 6.2: Run test to verify it fails**

```bash
go test ./internal/orchestrator/ -v 2>&1 | head -10
```

- [ ] **Step 6.3: Implement prompts.go**

Create `internal/orchestrator/prompts.go`:
```go
package orchestrator

import (
	"encoding/json"
	"fmt"
	"strings"
)

// AnalysisResult is Claude's response to the architecture analysis prompt.
type AnalysisResult struct {
	Ready           bool     `json:"ready"`
	Missing         []string `json:"missing"`
	Inconsistencies []string `json:"inconsistencies"`
	Summary         string   `json:"summary"`
}

// BuildAnalysisPrompt returns the prompt sent to Claude for the initial analysis phase.
func BuildAnalysisPrompt(markdown string) string {
	return fmt.Sprintf(`Analyze the following architecture document and respond with ONLY valid JSON — no other text.

Output this exact JSON structure:
{
  "ready": false,
  "missing": ["list every piece of information required to generate Terraform that is absent from the document"],
  "inconsistencies": ["list any design conflicts, contradictions, or ambiguities that need resolution"],
  "summary": "one paragraph describing the architecture"
}

Set "ready": true ONLY when ALL of the following are known:
- AWS region(s)
- VPC CIDR and subnet strategy
- Every compute resource type and size
- Every storage resource type and size
- IAM roles / service accounts required
- Environment (dev / staging / prod)
- Whether multi-account or single-account

Architecture document:
---
%s`, markdown)
}

// ParseAnalysisJSON extracts and parses the JSON analysis from Claude's response.
// Handles responses where JSON is wrapped in markdown code fences.
func ParseAnalysisJSON(raw string) AnalysisResult {
	// Try to extract JSON from ```json ... ``` fences first
	if idx := strings.Index(raw, "```json"); idx >= 0 {
		start := idx + len("```json")
		end := strings.Index(raw[start:], "```")
		if end >= 0 {
			raw = strings.TrimSpace(raw[start : start+end])
		}
	} else if idx := strings.Index(raw, "{"); idx >= 0 {
		// Try bare JSON object
		end := strings.LastIndex(raw, "}")
		if end > idx {
			raw = raw[idx : end+1]
		}
	}

	var result AnalysisResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return AnalysisResult{
			Ready:   false,
			Missing: []string{"Unable to parse architecture analysis — Claude response was not valid JSON"},
		}
	}
	return result
}

// BuildQuestions converts missing items and inconsistencies into user-facing questions.
func BuildQuestions(missing, inconsistencies []string) []string {
	var questions []string
	questions = append(questions, missing...)
	for _, inc := range inconsistencies {
		questions = append(questions, "Please clarify: "+inc)
	}
	return questions
}

// BuildClarifyPrompt constructs the follow-up prompt after the user answers questions.
func BuildClarifyPrompt(questions, answers []string) string {
	var sb strings.Builder
	sb.WriteString("The following clarifications were provided by the user:\n\n")
	for i, q := range questions {
		a := "(no answer provided)"
		if i < len(answers) && answers[i] != "" {
			a = answers[i]
		}
		sb.WriteString(fmt.Sprintf("Q: %s\nA: %s\n\n", q, a))
	}
	sb.WriteString(`Based on these answers, re-analyze the architecture and respond with ONLY valid JSON:

{
  "ready": true,
  "missing": ["any remaining gaps — empty array if none"],
  "inconsistencies": ["any remaining conflicts — empty array if none"],
  "summary": "updated architecture description incorporating the answers"
}`)
	return sb.String()
}

// BuildContext reconstructs the session context for injection into new Claude calls.
// This is the workaround for the --resume context-loss bug in -p mode.
func BuildContext(sess interface{ GetContext() contextData }) string {
	return buildContextFromData(sess.GetContext())
}

type contextData struct {
	DocPath        string
	CurrentPhase   string
	CheckpointStep string
	AnalysisJSON   string
	Clarifications []clarificationEntry
	GeneratedFiles []string
	ValidationErrs []string
}

type clarificationEntry struct {
	Question string
	Answer   string
}

func buildContextFromData(d contextData) string {
	var sb strings.Builder
	sb.WriteString("## PREVIOUS SESSION CONTEXT\n\n")
	sb.WriteString(fmt.Sprintf("Document: %s\n", d.DocPath))
	sb.WriteString(fmt.Sprintf("Last completed phase: %s\n", d.CurrentPhase))
	if d.CheckpointStep != "" {
		sb.WriteString(fmt.Sprintf("Last checkpoint step: %s\n", d.CheckpointStep))
	}
	sb.WriteString("\n")

	if d.AnalysisJSON != "" {
		sb.WriteString("### Architecture analysis:\n")
		sb.WriteString(d.AnalysisJSON)
		sb.WriteString("\n\n")
	}

	if len(d.Clarifications) > 0 {
		sb.WriteString("### Clarifications gathered:\n")
		for _, c := range d.Clarifications {
			sb.WriteString(fmt.Sprintf("Q: %s\nA: %s\n", c.Question, c.Answer))
		}
		sb.WriteString("\n")
	}

	if len(d.GeneratedFiles) > 0 {
		sb.WriteString("### Already generated Terraform files:\n")
		for _, f := range d.GeneratedFiles {
			sb.WriteString(fmt.Sprintf("- %s\n", f))
		}
		sb.WriteString("\n")
	}

	if len(d.ValidationErrs) > 0 {
		sb.WriteString("### Pending validation errors to fix:\n")
		for _, e := range d.ValidationErrs {
			sb.WriteString(fmt.Sprintf("  - %s\n", e))
		}
	}

	return sb.String()
}
```

- [ ] **Step 6.4: Run tests**

```bash
go test ./internal/orchestrator/ -run TestBuild -run TestParse -v -count=1
```

Expected: all 5 tests PASS

- [ ] **Step 6.5: Commit**

```bash
git add internal/orchestrator/prompts.go internal/orchestrator/prompts_test.go
git commit -m "feat(orchestrator): prompt builders, JSON parser, context injector"
```

---

## Task 7: Resume Logic

**Files:**
- Create: `internal/orchestrator/resume.go`
- Create: `internal/orchestrator/resume_test.go`

- [ ] **Step 7.1: Write failing test**

Create `internal/orchestrator/resume_test.go`:
```go
package orchestrator_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"tf-architect/internal/orchestrator"
	"tf-architect/internal/state"
)

func TestDecideNew(t *testing.T) {
	tmp := t.TempDir()
	docPath := filepath.Join(tmp, "doc.md")
	_ = os.WriteFile(docPath, []byte("# LLD\n\nVPC in eu-west-1"), 0644)

	mgr := state.NewManager(tmp)
	decision, err := orchestrator.DecideResume(docPath, mgr)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Mode != orchestrator.ModeNew {
		t.Errorf("expected ModeNew, got %s", decision.Mode)
	}
	if decision.Session == nil {
		t.Fatal("expected non-nil session")
	}
}

func TestDecideResumeIncomplete(t *testing.T) {
	tmp := t.TempDir()
	docPath := filepath.Join(tmp, "doc.md")
	content := []byte("# LLD\n\nVPC in eu-west-1")
	_ = os.WriteFile(docPath, content, 0644)

	hash, _ := state.HashDoc(docPath)
	mgr := state.NewManager(tmp)

	// Save an interrupted session
	sess := &state.Session{
		DocPath:      docPath,
		DocHash:      hash,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
		CurrentPhase: state.PhaseGenerate, // interrupted mid-generate
	}
	_ = mgr.Save(sess)

	decision, err := orchestrator.DecideResume(docPath, mgr)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Mode != orchestrator.ModeResume {
		t.Errorf("expected ModeResume, got %s", decision.Mode)
	}
}

func TestDecideValidateOnly(t *testing.T) {
	tmp := t.TempDir()
	docPath := filepath.Join(tmp, "doc.md")
	_ = os.WriteFile(docPath, []byte("# LLD"), 0644)

	hash, _ := state.HashDoc(docPath)
	mgr := state.NewManager(tmp)

	sess := &state.Session{
		DocPath:      docPath,
		DocHash:      hash,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
		CurrentPhase: state.PhaseDone,
	}
	_ = mgr.Save(sess)

	decision, err := orchestrator.DecideResume(docPath, mgr)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Mode != orchestrator.ModeValidateOnly {
		t.Errorf("expected ModeValidateOnly, got %s", decision.Mode)
	}
}

func TestDecideDocChanged(t *testing.T) {
	tmp := t.TempDir()
	docPath := filepath.Join(tmp, "doc.md")
	originalContent := []byte("# LLD\n\nOriginal content")
	_ = os.WriteFile(docPath, originalContent, 0644)

	hash, _ := state.HashDoc(docPath)
	mgr := state.NewManager(tmp)

	sess := &state.Session{
		DocPath:      docPath,
		DocHash:      hash,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
		CurrentPhase: state.PhaseDone,
		DocMarkdown:  string(originalContent),
	}
	_ = mgr.Save(sess)

	// Now change the document
	_ = os.WriteFile(docPath, []byte("# LLD\n\nUpdated content with new section"), 0644)

	decision, err := orchestrator.DecideResume(docPath, mgr)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Mode != orchestrator.ModeDocChanged {
		t.Errorf("expected ModeDocChanged, got %s", decision.Mode)
	}
	if decision.Session.PreviousDocHash != hash {
		t.Error("expected PreviousDocHash to be set to old hash")
	}
}
```

- [ ] **Step 7.2: Run test to verify it fails**

```bash
go test ./internal/orchestrator/ -run TestDecide -v 2>&1 | head -15
```

- [ ] **Step 7.3: Implement resume.go**

Create `internal/orchestrator/resume.go`:
```go
package orchestrator

import (
	"fmt"
	"strings"
	"time"

	"tf-architect/internal/ingestion"
	"tf-architect/internal/state"
)

type ResumeMode string

const (
	ModeNew          ResumeMode = "new"
	ModeResume       ResumeMode = "resume"
	ModeDocChanged   ResumeMode = "doc_changed"
	ModeValidateOnly ResumeMode = "validate_only"
)

type ResumeDecision struct {
	Mode        ResumeMode
	Session     *state.Session
	ContextBlob string
}

// DecideResume analyses persisted state and determines how to proceed.
func DecideResume(docPath string, mgr *state.Manager) (*ResumeDecision, error) {
	currentHash, err := state.HashDoc(docPath)
	if err != nil {
		return nil, fmt.Errorf("hashing document: %w", err)
	}

	prev, err := mgr.FindByDocPath(docPath)
	if err != nil {
		return nil, err
	}

	// CASE 1: No previous session → full run
	if prev == nil {
		return &ResumeDecision{
			Mode: ModeNew,
			Session: &state.Session{
				DocPath:      docPath,
				DocHash:      currentHash,
				CreatedAt:    time.Now(),
				CurrentPhase: state.PhaseIngest,
			},
		}, nil
	}

	// CASE 2: Same hash, completed → validate only
	if prev.DocHash == currentHash && prev.CurrentPhase == state.PhaseDone {
		return &ResumeDecision{
			Mode:        ModeValidateOnly,
			Session:     prev,
			ContextBlob: buildContextBlob(prev),
		}, nil
	}

	// CASE 3: Same hash, interrupted → resume from checkpoint
	if prev.DocHash == currentHash {
		return &ResumeDecision{
			Mode:        ModeResume,
			Session:     prev,
			ContextBlob: buildContextBlob(prev),
		}, nil
	}

	// CASE 4: Hash changed → document was modified
	diffSummary := computeMarkdownDiff(prev.DocMarkdown, docPath)
	newSession := &state.Session{
		DocPath:         docPath,
		DocHash:         currentHash,
		CreatedAt:       time.Now(),
		CurrentPhase:    state.PhaseAnalyze,
		PreviousDocHash: prev.DocHash,
		DiffSummary:     diffSummary,
		GeneratedFiles:  prev.GeneratedFiles, // preserve existing files for incremental update
	}
	return &ResumeDecision{
		Mode:        ModeDocChanged,
		Session:     newSession,
		ContextBlob: buildContextBlobForDocChange(prev, diffSummary),
	}, nil
}

func buildContextBlob(s *state.Session) string {
	var sb strings.Builder
	sb.WriteString("## PREVIOUS SESSION CONTEXT\n\n")
	sb.WriteString(fmt.Sprintf("Document: %s\n", s.DocPath))
	sb.WriteString(fmt.Sprintf("Last completed phase: %s\n", string(s.CurrentPhase)))
	if s.LastCheckpoint.GenerateStep != "" {
		sb.WriteString(fmt.Sprintf("Last checkpoint step: %s\n", s.LastCheckpoint.GenerateStep))
	}
	sb.WriteString("\n")

	if s.AnalysisJSON != "" {
		sb.WriteString("### Architecture analysis already performed:\n")
		sb.WriteString(s.AnalysisJSON)
		sb.WriteString("\n\n")
	}

	if len(s.Clarifications) > 0 {
		sb.WriteString("### Clarifications already gathered:\n")
		for _, c := range s.Clarifications {
			for i, q := range c.Questions {
				if a, ok := c.Answers[i]; ok {
					sb.WriteString(fmt.Sprintf("Q: %s\nA: %s\n", q, a))
				}
			}
		}
		sb.WriteString("\n")
	}

	if len(s.GeneratedFiles) > 0 {
		sb.WriteString("### Already generated Terraform files:\n")
		for _, f := range s.GeneratedFiles {
			sb.WriteString(fmt.Sprintf("- %s\n", f))
		}
		sb.WriteString("\n")
	}

	if len(s.ValidationRuns) > 0 {
		last := s.ValidationRuns[len(s.ValidationRuns)-1]
		sb.WriteString("### Last validation run:\n")
		for stage, passed := range last.Results {
			icon := "✓"
			if !passed {
				icon = "✗"
			}
			sb.WriteString(fmt.Sprintf("%s %s\n", icon, stage))
		}
		if len(last.Errors) > 0 {
			sb.WriteString("Pending errors:\n")
			for _, e := range last.Errors {
				sb.WriteString(fmt.Sprintf("  - %s\n", e))
			}
		}
	}

	return sb.String()
}

func buildContextBlobForDocChange(prev *state.Session, diffSummary string) string {
	base := buildContextBlob(prev)
	return base + fmt.Sprintf(
		"\n## DOCUMENT CHANGED\n\nDiff summary:\n%s\n\n"+
			"INSTRUCTION: Do NOT regenerate from scratch. Only adjust existing "+
			"Terraform files to reflect the changes above. Preserve unchanged resources.\n",
		diffSummary,
	)
}

func computeMarkdownDiff(prevMarkdown, newDocPath string) string {
	result, err := ingestion.Convert(newDocPath)
	if err != nil {
		return "Unable to compute diff — full re-analysis required"
	}
	newMarkdown := result.Markdown

	prevLines := strings.Split(prevMarkdown, "\n")
	newLines := strings.Split(newMarkdown, "\n")

	prevSet := make(map[string]bool)
	for _, l := range prevLines {
		if strings.TrimSpace(l) != "" {
			prevSet[l] = true
		}
	}
	newSet := make(map[string]bool)
	for _, l := range newLines {
		if strings.TrimSpace(l) != "" {
			newSet[l] = true
		}
	}

	var added, removed []string
	for _, l := range newLines {
		if !prevSet[l] && strings.TrimSpace(l) != "" {
			added = append(added, l)
		}
	}
	for _, l := range prevLines {
		if !newSet[l] && strings.TrimSpace(l) != "" {
			removed = append(removed, l)
		}
	}

	truncate := func(lines []string, max int) []string {
		if len(lines) > max {
			return append(lines[:max], fmt.Sprintf("... (%d more lines)", len(lines)-max))
		}
		return lines
	}

	var sb strings.Builder
	if len(added) > 0 {
		sb.WriteString("ADDED:\n")
		for _, l := range truncate(added, 20) {
			sb.WriteString(fmt.Sprintf("  + %s\n", l))
		}
	}
	if len(removed) > 0 {
		sb.WriteString("REMOVED:\n")
		for _, l := range truncate(removed, 20) {
			sb.WriteString(fmt.Sprintf("  - %s\n", l))
		}
	}
	if sb.Len() == 0 {
		return "No significant textual changes detected"
	}
	return sb.String()
}
```

- [ ] **Step 7.4: Run tests**

```bash
go test ./internal/orchestrator/ -run TestDecide -v -count=1
```

Expected: all 4 TestDecide tests PASS

- [ ] **Step 7.5: Commit**

```bash
git add internal/orchestrator/resume.go internal/orchestrator/resume_test.go
git commit -m "feat(orchestrator): resume decision logic with doc-hash change detection"
```

---

## Task 8: Orchestrator FSM

**Files:**
- Create: `internal/orchestrator/orchestrator.go`

- [ ] **Step 8.1: Implement orchestrator.go**

Create `internal/orchestrator/orchestrator.go`:
```go
package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tf-architect/internal/claude"
	"tf-architect/internal/docs"
	"tf-architect/internal/ingestion"
	"tf-architect/internal/state"
	"tf-architect/internal/validation"
)

const maxClarifyRounds = 3

// Orchestrator runs the tf-architect FSM.
type Orchestrator struct {
	client    *claude.Client
	stMgr     *state.Manager
	workDir   string
	outputDir string

	// Callbacks wired by the CLI/TUI layer
	OnQuestion func(questions []string) []string
	OnStatus   func(msg string)
	OnChunk    func(chunk string)
}

func New(workDir, outputDir, systemPrompt string) *Orchestrator {
	return &Orchestrator{
		client:    claude.New(workDir, systemPrompt),
		stMgr:     state.NewManager(workDir),
		workDir:   workDir,
		outputDir: outputDir,
	}
}

func (o *Orchestrator) status(format string, args ...interface{}) {
	if o.OnStatus != nil {
		o.OnStatus(fmt.Sprintf(format, args...))
	}
}

func (o *Orchestrator) Run(docPath string) error {
	// ── RESUME DECISION ─────────────────────────────────────────────────────
	decision, err := DecideResume(docPath, o.stMgr)
	if err != nil {
		return fmt.Errorf("resume decision: %w", err)
	}
	sess := decision.Session
	o.status("Mode: %s | Phase: %s", decision.Mode, sess.CurrentPhase)

	if decision.ContextBlob != "" {
		o.client.SetContextBlob(decision.ContextBlob)
	}

	// ── VALIDATE ONLY ────────────────────────────────────────────────────────
	if decision.Mode == ModeValidateOnly {
		o.status("Document unchanged. Re-running validation suite...")
		return o.runValidation(sess)
	}

	// ── INGEST ───────────────────────────────────────────────────────────────
	if sess.CurrentPhase.AtOrBefore(state.PhaseIngest) || sess.DocMarkdown == "" {
		o.status("Converting document: %s", filepath.Base(docPath))
		conv, err := ingestion.Convert(docPath)
		if err != nil {
			return fmt.Errorf("ingestion: %w", err)
		}
		sess.DocMarkdown = conv.Markdown
		o.status("[%s] ~%d tokens", conv.SourceFormat, conv.EstimatedTokens)
		if err := o.stMgr.Advance(sess, state.PhaseAnalyze, state.Checkpoint{}); err != nil {
			return err
		}
	}

	// ── ANALYZE ──────────────────────────────────────────────────────────────
	if sess.CurrentPhase.AtOrBefore(state.PhaseAnalyze) {
		o.status("Analyzing architecture...")
		var analysisPrompt string
		if decision.Mode == ModeDocChanged {
			analysisPrompt = fmt.Sprintf(
				"%s\n\n## CURRENT DOCUMENT (UPDATED):\n%s\n\n"+
					"Analyze only what changed. Which Terraform resources need modification?",
				decision.ContextBlob, sess.DocMarkdown,
			)
		} else {
			analysisPrompt = BuildAnalysisPrompt(sess.DocMarkdown)
		}

		analysisRaw, err := o.client.Query(analysisPrompt)
		if err != nil {
			_ = o.stMgr.Save(sess)
			return fmt.Errorf("analyze (state saved): %w", err)
		}
		sess.AnalysisJSON = analysisRaw
		if err := o.stMgr.Advance(sess, state.PhaseClarify, state.Checkpoint{}); err != nil {
			return err
		}
	}

	// ── CLARIFY ──────────────────────────────────────────────────────────────
	if sess.CurrentPhase.AtOrBefore(state.PhaseClarify) {
		analysis := ParseAnalysisJSON(sess.AnalysisJSON)
		startRound := len(sess.Clarifications)

		for round := startRound; round < maxClarifyRounds && !analysis.Ready; round++ {
			questions := BuildQuestions(analysis.Missing, analysis.Inconsistencies)
			if len(questions) == 0 {
				break
			}

			o.status("Clarification round %d/%d (%d questions)", round+1, maxClarifyRounds, len(questions))

			var answers []string
			if o.OnQuestion != nil {
				answers = o.OnQuestion(questions)
			}

			// Persist the round before sending to Claude (don't lose user answers on error)
			cr := state.ClarificationRound{
				Round:     round,
				Questions: questions,
				Answers:   make(map[int]string),
			}
			for i, a := range answers {
				cr.Answers[i] = a
			}
			sess.Clarifications = append(sess.Clarifications, cr)
			if err := o.stMgr.Save(sess); err != nil {
				return err
			}

			reanalysisRaw, err := o.client.Query(BuildClarifyPrompt(questions, answers))
			if err != nil {
				return fmt.Errorf("clarify round %d (state saved): %w", round, err)
			}
			analysis = ParseAnalysisJSON(reanalysisRaw)
		}

		if err := o.stMgr.Advance(sess, state.PhaseGenerate, state.Checkpoint{}); err != nil {
			return err
		}
	}

	// ── GENERATE ─────────────────────────────────────────────────────────────
	if sess.CurrentPhase.AtOrBefore(state.PhaseGenerate) {
		o.status("Generating Terraform IaC in %s...", o.outputDir)
		if err := os.MkdirAll(o.outputDir, 0755); err != nil {
			return fmt.Errorf("creating output dir: %w", err)
		}

		var generatePrompt string
		lastStep := sess.LastCheckpoint.GenerateStep

		switch {
		case decision.Mode == ModeDocChanged:
			generatePrompt = fmt.Sprintf(
				"%s\n\nAdjust existing Terraform files in %s to reflect document changes.\n"+
					"Only modify files that need to change. Do not touch unaffected resources.\n"+
					"After each module, output: STEP_COMPLETE:<module_name>",
				decision.ContextBlob, o.outputDir,
			)
		case lastStep != "":
			generatePrompt = fmt.Sprintf(
				"%s\n\nGeneration was interrupted at step: %s\n"+
					"Continue generating remaining modules. Do NOT regenerate files already in %s.\n"+
					"Check existing files first: ls -la %s\n"+
					"After each module, output: STEP_COMPLETE:<module_name>",
				decision.ContextBlob, lastStep, o.outputDir, o.outputDir,
			)
		default:
			generatePrompt = fmt.Sprintf(
				"Generate complete, production-grade Terraform IaC based on the analyzed architecture.\n"+
					"Write all files to: %s\n\n"+
					"Architecture context:\n%s\n\n"+
					"After writing each module, output exactly: STEP_COMPLETE:<module_name>\n"+
					"This marker is used for progress checkpointing.",
				o.outputDir, sess.AnalysisJSON,
			)
		}

		err := o.client.StreamQuery(generatePrompt, func(chunk string) {
			if o.OnChunk != nil {
				o.OnChunk(chunk)
			}
			// Granular checkpoint per module
			if strings.Contains(chunk, "STEP_COMPLETE:") {
				step := extractStep(chunk)
				if step != "" {
					sess.LastCheckpoint = state.Checkpoint{
						Phase:        state.PhaseGenerate,
						GenerateStep: step,
						CompletedAt:  time.Now(),
					}
					_ = o.stMgr.Save(sess)
					o.status("  ✓ Module complete: %s", step)
				}
			}
		})
		if err != nil {
			_ = o.stMgr.Save(sess)
			return fmt.Errorf("generate (state saved at step '%s'): %w",
				sess.LastCheckpoint.GenerateStep, err)
		}

		sess.GeneratedFiles = collectTFFiles(o.outputDir)
		if err := o.stMgr.Advance(sess, state.PhaseValidate, state.Checkpoint{}); err != nil {
			return err
		}
	}

	// ── VALIDATE ─────────────────────────────────────────────────────────────
	return o.runValidation(sess)
}

func (o *Orchestrator) runValidation(sess *state.Session) error {
	o.status("Running validation suite...")
	suite := validation.New(o.outputDir)
	results, valErr := suite.Run()

	run := state.ValidationRun{
		RunAt:   time.Now(),
		Results: make(map[string]bool),
	}
	for _, r := range results {
		run.Results[r.Stage] = r.Passed
		run.Errors = append(run.Errors, r.Errors...)
		icon := "✓"
		if !r.Passed {
			icon = "✗"
		}
		o.status("  %s %s", icon, r.Stage)
	}
	run.Passed = valErr == nil
	sess.ValidationRuns = append(sess.ValidationRuns, run)

	if valErr != nil {
		o.status("Errors detected. Attempting auto-fix...")
		fixPrompt := fmt.Sprintf(
			"%s\n\nValidation errors:\n%s\n\nFix all errors in %s. Do not touch files without errors.",
			buildContextBlob(sess),
			strings.Join(run.Errors, "\n"),
			o.outputDir,
		)
		if _, err := o.client.Query(fixPrompt); err != nil {
			_ = o.stMgr.Save(sess)
			return fmt.Errorf("auto-fix (state saved): %w", err)
		}

		// Re-validate after fix
		results2, _ := suite.Run()
		run2 := state.ValidationRun{RunAt: time.Now(), Results: make(map[string]bool)}
		for _, r := range results2 {
			run2.Results[r.Stage] = r.Passed
			icon := "✓"
			if !r.Passed {
				icon = "✗"
			}
			o.status("  (re-check) %s %s", icon, r.Stage)
		}
		sess.ValidationRuns = append(sess.ValidationRuns, run2)
	}

	// ── DOCUMENT ─────────────────────────────────────────────────────────────
	if err := o.stMgr.Advance(sess, state.PhaseDocument, state.Checkpoint{}); err != nil {
		return err
	}

	if err := docs.Generate(o.client, sess, o.outputDir, o.status); err != nil {
		o.status("Warning: documentation phase failed: %v", err)
	}

	_ = o.stMgr.Advance(sess, state.PhaseDone, state.Checkpoint{})
	o.status("✓ Session complete — output in %s", o.outputDir)
	return nil
}

func extractStep(chunk string) string {
	idx := strings.Index(chunk, "STEP_COMPLETE:")
	if idx < 0 {
		return ""
	}
	rest := chunk[idx+len("STEP_COMPLETE:"):]
	end := strings.IndexAny(rest, "\n \t")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

func collectTFFiles(dir string) []string {
	var files []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(path, ".tf") {
			rel, _ := filepath.Rel(dir, path)
			files = append(files, rel)
		}
		return nil
	})
	return files
}
```

- [ ] **Step 8.2: Verify it compiles**

```bash
go build ./internal/orchestrator/ 2>&1
```

Expected: no output (clean compile). If there are import cycle errors due to `docs` package, that will be resolved in Task 9.

- [ ] **Step 8.3: Commit**

```bash
git add internal/orchestrator/orchestrator.go
git commit -m "feat(orchestrator): 7-phase FSM with resumable checkpointing and auto-fix"
```

---

## Task 9: Documentation Generation Package

**Files:**
- Create: `internal/docs/generator.go`
- Create: `internal/docs/tfdocs.go`
- Create: `internal/docs/architectural.go`

- [ ] **Step 9.1: Implement tfdocs.go**

Create `internal/docs/tfdocs.go`:
```go
package docs

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const tfDocsConfig = `formatter: "markdown table"

recursive:
  enabled: true
  path: modules
  include-main: true

sections:
  show:
    - requirements
    - providers
    - modules
    - resources
    - inputs
    - outputs

output:
  file: README.md
  mode: inject
  template: |-
    <!-- BEGIN_TF_DOCS -->
    {{ .Content }}
    <!-- END_TF_DOCS -->

sort:
  enabled: true
  by: name

settings:
  anchor: true
  default: true
  description: true
  hide-empty: true
  indent: 2
  required: true
  sensitive: true
  type: true
`

// RunTFDocs writes .terraform-docs.yml and runs terraform-docs against outputDir.
func RunTFDocs(outputDir string) (string, error) {
	if _, err := exec.LookPath("terraform-docs"); err != nil {
		return "", fmt.Errorf("terraform-docs not found — install from https://terraform-docs.io")
	}

	configPath := filepath.Join(outputDir, ".terraform-docs.yml")
	if err := os.WriteFile(configPath, []byte(tfDocsConfig), 0644); err != nil {
		return "", fmt.Errorf("writing .terraform-docs.yml: %w", err)
	}

	if err := ensureREADMEMarkers(outputDir); err != nil {
		return "", fmt.Errorf("ensuring README markers: %w", err)
	}

	var out bytes.Buffer
	cmd := exec.Command("terraform-docs", ".")
	cmd.Dir = outputDir
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("terraform-docs: %w\n%s", err, out.String())
	}
	return out.String(), nil
}

// ensureREADMEMarkers walks all dirs with .tf files and creates stub READMEs
// with BEGIN_TF_DOCS/END_TF_DOCS markers if they don't already exist.
func ensureREADMEMarkers(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if base == ".terraform" || base == ".tf-architect" || base == ".git" {
			return filepath.SkipDir
		}

		hasTF, err := dirHasTFFiles(path)
		if err != nil || !hasTF {
			return nil
		}

		readmePath := filepath.Join(path, "README.md")
		if _, err := os.Stat(readmePath); os.IsNotExist(err) {
			moduleName := filepath.Base(path)
			stub := fmt.Sprintf("# %s\n\n<!-- BEGIN_TF_DOCS -->\n<!-- END_TF_DOCS -->\n",
				strings.ToTitle(moduleName))
			return os.WriteFile(readmePath, []byte(stub), 0644)
		}
		return nil
	})
}

func dirHasTFFiles(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".tf") {
			return true, nil
		}
	}
	return false, nil
}
```

- [ ] **Step 9.2: Implement architectural.go**

Create `internal/docs/architectural.go`:
```go
package docs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SessionData is the subset of state.Session needed for doc generation.
// Using a local struct avoids an import cycle with the orchestrator package.
type SessionData struct {
	DocPath        string
	DocMarkdown    string
	AnalysisJSON   string
	Clarifications []ClarificationEntry
	GeneratedFiles []string
	CreatedAt      time.Time
}

type ClarificationEntry struct {
	Question string
	Answer   string
}

// Client is the interface the docs package expects from the Claude client.
type Client interface {
	Query(prompt string) (string, error)
}

type architecturalDocs struct {
	RootREADME   string
	Architecture string
	Runbook      string
	Variables    string
}

// GenerateArchitecturalDocs generates the four documentation files via Claude.
func GenerateArchitecturalDocs(client Client, sess SessionData, outputDir string, onStatus func(string)) error {
	onStatus("Generating architectural documentation...")

	prompt := buildDocsPrompt(sess, outputDir)
	raw, err := client.Query(prompt)
	if err != nil {
		return fmt.Errorf("architectural docs generation: %w", err)
	}

	adocs := parseDocSections(raw)

	docsDir := filepath.Join(outputDir, "docs")
	if err := os.MkdirAll(docsDir, 0755); err != nil {
		return err
	}

	files := map[string]string{
		filepath.Join(docsDir, "ARCHITECTURE.md"): adocs.Architecture,
		filepath.Join(docsDir, "RUNBOOK.md"):       adocs.Runbook,
		filepath.Join(docsDir, "VARIABLES.md"):     adocs.Variables,
		filepath.Join(docsDir, "CHANGELOG.md"):     changelogStub(sess),
	}

	for path, content := range files {
		if content == "" {
			continue
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return fmt.Errorf("writing %s: %w", filepath.Base(path), err)
		}
		onStatus(fmt.Sprintf("  ✓ docs/%s", filepath.Base(path)))
	}

	if adocs.RootREADME != "" {
		if err := injectREADMEPrefix(outputDir, adocs.RootREADME); err != nil {
			return fmt.Errorf("injecting README prefix: %w", err)
		}
		onStatus("  ✓ README.md (architectural prefix)")
	}

	return nil
}

func buildDocsPrompt(sess SessionData, outputDir string) string {
	var tfFileList strings.Builder
	for _, f := range sess.GeneratedFiles {
		tfFileList.WriteString(fmt.Sprintf("- %s\n", f))
	}

	var decisions strings.Builder
	for _, c := range sess.Clarifications {
		decisions.WriteString(fmt.Sprintf("- **%s**: %s\n", c.Question, c.Answer))
	}

	return fmt.Sprintf(`You are a senior infrastructure architect writing documentation for a Terraform project.

## Source Document
%s

## Architecture Decisions Made During Generation
%s

## Generated Terraform Files
%s

---

Generate the following four documentation sections.
Use EXACTLY these delimiters — they are parsed programmatically:

===BEGIN_ROOT_README===
Write a concise README.md introduction (NOT tables of variables/outputs — terraform-docs handles those).
Include: one-line project description, architecture overview (2-3 paragraphs), prerequisites, quick start.
End with a link table to the docs/ files.
===END_ROOT_README===

===BEGIN_ARCHITECTURE===
Write docs/ARCHITECTURE.md:
- System context and business purpose
- AWS account and region strategy
- Network topology with actual CIDRs from the code
- Security model (IAM, SGs, KMS) with actual resource names
- ASCII or Mermaid data-flow diagram
- Key design decisions and rationale (especially from clarifications)
- Known limitations
===END_ARCHITECTURE===

===BEGIN_RUNBOOK===
Write docs/RUNBOOK.md:
- Day-2 operations: scaling, certificate rotation, secret rotation
- Incident response: common failure modes and remediation
- Cost optimization: main cost drivers and right-sizing
- Backup and DR: RTO/RPO targets, restore procedures
- Destroy procedure: safe teardown order
Reference actual resource names, module paths, variable names.
===END_RUNBOOK===

===BEGIN_VARIABLES===
Write docs/VARIABLES.md:
- Variables grouped by concern (network, compute, database, security)
- For each required variable: purpose, accepted values, examples per env (dev/staging/prod)
- Sensitive variables: how to provide them (env var, tfvars, Secrets Manager)
- Recommended tfvars structure per environment
===END_VARIABLES===`,
		sess.DocMarkdown,
		decisions.String(),
		tfFileList.String(),
	)
}

func parseDocSections(raw string) architecturalDocs {
	extract := func(begin, end string) string {
		startIdx := strings.Index(raw, begin)
		endIdx := strings.Index(raw, end)
		if startIdx < 0 || endIdx < 0 || endIdx <= startIdx {
			return ""
		}
		return strings.TrimSpace(raw[startIdx+len(begin) : endIdx])
	}

	return architecturalDocs{
		RootREADME:   extract("===BEGIN_ROOT_README===", "===END_ROOT_README==="),
		Architecture: extract("===BEGIN_ARCHITECTURE===", "===END_ARCHITECTURE==="),
		Runbook:      extract("===BEGIN_RUNBOOK===", "===END_RUNBOOK==="),
		Variables:    extract("===BEGIN_VARIABLES===", "===END_VARIABLES==="),
	}
}

func injectREADMEPrefix(outputDir, prefix string) error {
	readmePath := filepath.Join(outputDir, "README.md")
	const beginMarker = "<!-- BEGIN_TF_DOCS -->"

	existing, err := os.ReadFile(readmePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	var finalContent string
	if len(existing) > 0 && strings.Contains(string(existing), beginMarker) {
		idx := strings.Index(string(existing), beginMarker)
		finalContent = prefix + "\n\n" + string(existing)[idx:]
	} else {
		finalContent = prefix + "\n\n" + beginMarker + "\n<!-- END_TF_DOCS -->\n"
	}

	return os.WriteFile(readmePath, []byte(finalContent), 0644)
}

func changelogStub(sess SessionData) string {
	return fmt.Sprintf(`# Changelog

All notable changes to this infrastructure are documented here.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [Unreleased]

### Added
- Initial infrastructure generated from %s on %s

---
*Maintained manually. terraform-docs manages README.md files in each module.*
`,
		filepath.Base(sess.DocPath),
		sess.CreatedAt.Format("2006-01-02"),
	)
}
```

- [ ] **Step 9.3: Implement generator.go**

Create `internal/docs/generator.go`:
```go
package docs

import (
	"time"

	"tf-architect/internal/state"
)

// Generate runs both documentation layers in the correct order:
// 1. Architectural docs (Claude) — writes prefix + docs/ directory
// 2. terraform-docs — injects technical docs into BEGIN_TF_DOCS markers
//
// Order matters: Claude writes the prefix first, then terraform-docs injects
// into the markers without overwriting the prefix.
func Generate(client Client, sess *state.Session, outputDir string, onStatus func(string)) error {
	onStatus("── Documentation ───────────────────────────")

	// Build SessionData from state.Session (avoids import cycle)
	sd := SessionData{
		DocPath:        sess.DocPath,
		DocMarkdown:    sess.DocMarkdown,
		AnalysisJSON:   sess.AnalysisJSON,
		GeneratedFiles: sess.GeneratedFiles,
		CreatedAt:      sess.CreatedAt,
	}
	for _, c := range sess.Clarifications {
		for i, q := range c.Questions {
			if a, ok := c.Answers[i]; ok {
				sd.Clarifications = append(sd.Clarifications, ClarificationEntry{
					Question: q,
					Answer:   a,
				})
			}
		}
	}
	if sd.CreatedAt.IsZero() {
		sd.CreatedAt = time.Now()
	}

	// Pass 1: architectural docs
	if err := GenerateArchitecturalDocs(client, sd, outputDir, onStatus); err != nil {
		onStatus("  ⚠ Architectural docs failed: " + err.Error())
		// Continue — IaC is usable even if docs fail
	}

	// Pass 2: terraform-docs (technical)
	onStatus("Running terraform-docs...")
	_, err := RunTFDocs(outputDir)
	if err != nil {
		onStatus("  ⚠ terraform-docs: " + err.Error())
		onStatus("  Install: https://terraform-docs.io/user-guide/installation/")
		return nil // non-fatal
	}
	onStatus("  ✓ terraform-docs (variables/outputs/resources injected into README.md)")
	onStatus("── Documentation complete ──────────────────")
	return nil
}
```

- [ ] **Step 9.4: Verify everything compiles**

```bash
go build ./... 2>&1
```

Expected: clean compile. Fix any type mismatches if they appear.

- [ ] **Step 9.5: Commit**

```bash
git add internal/docs/
git commit -m "feat(docs): two-pass documentation — architectural (Claude) + technical (terraform-docs)"
```

---

## Task 10: CLI Entry Point

**Files:**
- Create: `cmd/main.go`

- [ ] **Step 10.1: Implement cmd/main.go**

Create `cmd/main.go`:
```go
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"tf-architect/internal/orchestrator"
	"tf-architect/internal/state"
)

var (
	outputDir  string
	forceFlag  bool
	systemFile string
)

func main() {
	root := &cobra.Command{
		Use:   "tf-architect",
		Short: "Generate production Terraform IaC from HLD/LLD documents",
		Long: `tf-architect reads architecture documents (PDF, DOCX, MD, TXT) and 
generates validated, production-grade AWS Terraform using Claude Code.

It asks clarifying questions when information is missing, runs terraform validate,
golden-rules audit, tflint, and checkov, then generates full documentation.

Sessions are persisted: interrupted runs resume automatically from the last checkpoint.`,
	}

	runCmd := &cobra.Command{
		Use:   "run <document>",
		Short: "Generate Terraform IaC from a document",
		Args:  cobra.ExactArgs(1),
		RunE:  runGenerate,
	}
	runCmd.Flags().StringVarP(&outputDir, "output", "o", "", "Output directory for Terraform files (default: <docname>-tf/)")
	runCmd.Flags().BoolVarP(&forceFlag, "force", "f", false, "Force full re-run, ignoring saved session state")
	runCmd.Flags().StringVar(&systemFile, "system-file", "", "Path to custom system prompt file (default: embedded golden rules)")

	statusCmd := &cobra.Command{
		Use:   "status <document>",
		Short: "Show session status for a document",
		Args:  cobra.ExactArgs(1),
		RunE:  runStatus,
	}

	resetCmd := &cobra.Command{
		Use:   "reset <document>",
		Short: "Delete saved session state for a document",
		Args:  cobra.ExactArgs(1),
		RunE:  runReset,
	}

	root.AddCommand(runCmd, statusCmd, resetCmd)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func runGenerate(cmd *cobra.Command, args []string) error {
	docPath, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("resolving document path: %w", err)
	}

	if _, err := os.Stat(docPath); os.IsNotExist(err) {
		return fmt.Errorf("document not found: %s", docPath)
	}

	workDir, _ := os.Getwd()

	if outputDir == "" {
		base := strings.TrimSuffix(filepath.Base(docPath), filepath.Ext(docPath))
		outputDir = filepath.Join(workDir, base+"-tf")
	}

	// Force re-run: delete existing session
	if forceFlag {
		hash, err := state.HashDoc(docPath)
		if err == nil {
			mgr := state.NewManager(workDir)
			sessionDir := filepath.Join(mgr.BaseDir, hash)
			if err := os.RemoveAll(sessionDir); err == nil {
				fmt.Printf("Cleared session for %s\n", filepath.Base(docPath))
			}
		}
	}

	systemPrompt := embeddedSystemPrompt()
	if systemFile != "" {
		data, err := os.ReadFile(systemFile)
		if err != nil {
			return fmt.Errorf("reading system file: %w", err)
		}
		systemPrompt = string(data)
	}

	orch := orchestrator.New(workDir, outputDir, systemPrompt)

	orch.OnStatus = func(msg string) {
		fmt.Println(msg)
	}

	orch.OnChunk = func(chunk string) {
		fmt.Print(chunk)
	}

	orch.OnQuestion = func(questions []string) []string {
		fmt.Println("\n── Clarification needed ─────────────────────")
		answers := make([]string, len(questions))
		reader := bufio.NewReader(os.Stdin)
		for i, q := range questions {
			fmt.Printf("\n[%d/%d] %s\n> ", i+1, len(questions), q)
			answer, _ := reader.ReadString('\n')
			answers[i] = strings.TrimSpace(answer)
		}
		fmt.Println("─────────────────────────────────────────────")
		return answers
	}

	return orch.Run(docPath)
}

func runStatus(cmd *cobra.Command, args []string) error {
	docPath, _ := filepath.Abs(args[0])
	workDir, _ := os.Getwd()

	hash, err := state.HashDoc(docPath)
	if err != nil {
		return fmt.Errorf("hashing document: %w", err)
	}

	mgr := state.NewManager(workDir)
	sess, err := mgr.Load(hash)
	if err != nil {
		return err
	}
	if sess == nil {
		fmt.Printf("No session found for %s\n", filepath.Base(docPath))
		return nil
	}

	fmt.Printf("Document: %s\n", sess.DocPath)
	fmt.Printf("Hash:     %s\n", sess.DocHash[:12]+"...")
	fmt.Printf("Phase:    %s\n", sess.CurrentPhase)
	fmt.Printf("Created:  %s\n", sess.CreatedAt.Format("2006-01-02 15:04"))
	fmt.Printf("Updated:  %s\n", sess.UpdatedAt.Format("2006-01-02 15:04"))
	if sess.LastCheckpoint.GenerateStep != "" {
		fmt.Printf("Last step: %s\n", sess.LastCheckpoint.GenerateStep)
	}
	if len(sess.GeneratedFiles) > 0 {
		fmt.Printf("Generated: %d Terraform files\n", len(sess.GeneratedFiles))
	}
	if len(sess.ValidationRuns) > 0 {
		last := sess.ValidationRuns[len(sess.ValidationRuns)-1]
		result := "PASSED"
		if !last.Passed {
			result = "FAILED"
		}
		fmt.Printf("Last validation: %s (%s)\n", result, last.RunAt.Format("15:04"))
	}
	return nil
}

func runReset(cmd *cobra.Command, args []string) error {
	docPath, _ := filepath.Abs(args[0])
	workDir, _ := os.Getwd()

	hash, err := state.HashDoc(docPath)
	if err != nil {
		return fmt.Errorf("hashing document: %w", err)
	}

	mgr := state.NewManager(workDir)
	sessionDir := filepath.Join(mgr.BaseDir, hash)
	if err := os.RemoveAll(sessionDir); err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	fmt.Printf("Session for %s cleared.\n", filepath.Base(docPath))
	return nil
}

// embeddedSystemPrompt reads the system prompt from the prompts/ directory
// relative to the binary location, falling back to a minimal inline version.
func embeddedSystemPrompt() string {
	// Try relative to working directory first (dev mode)
	candidates := []string{
		"prompts/system.md",
		filepath.Join(filepath.Dir(os.Args[0]), "prompts/system.md"),
	}
	for _, path := range candidates {
		if data, err := os.ReadFile(path); err == nil {
			return string(data)
		}
	}
	// Minimal fallback — golden rules inline
	return `You are an expert AWS infrastructure engineer generating production-grade Terraform IaC.
Always configure S3 backend, pin provider versions, use default_tags, encrypt all storage,
never use 0.0.0.0/0 ingress, use least-privilege IAM, never hardcode secrets.
After each module write: STEP_COMPLETE:<module_name>`
}
```

- [ ] **Step 10.2: Build and test the CLI**

```bash
go build -o tf-architect ./cmd/ && echo "Build OK"
./tf-architect --help
./tf-architect run --help
```

Expected: help text printed without errors.

- [ ] **Step 10.3: Run full test suite**

```bash
go test ./... -v -count=1 2>&1 | tail -30
```

Expected: all tests PASS. No compile errors.

- [ ] **Step 10.4: Test end-to-end with a simple doc (smoke test)**

```bash
cat > /tmp/test-lld.md << 'EOF'
# Test Architecture

## Overview
A simple AWS VPC with public and private subnets in eu-west-1.

## Components
- VPC: 10.0.0.0/16
- Public subnets: 10.0.1.0/24, 10.0.2.0/24 (AZ a and b)
- Private subnets: 10.0.10.0/24, 10.0.11.0/24
- Internet Gateway attached to public subnets
- NAT Gateway in first public subnet

## Environment
- Region: eu-west-1
- Environment: dev
- AWS Account: use data source

## Tags
All resources tagged with: Project=test, Environment=dev, ManagedBy=terraform
EOF

./tf-architect run /tmp/test-lld.md --output /tmp/test-tf-output
```

Expected: Claude Code generates Terraform files, validation runs, docs generated.

- [ ] **Step 10.5: Commit**

```bash
git add cmd/
git commit -m "feat(cmd): Cobra CLI with run/status/reset subcommands and interactive Q&A"
```

---

## Task 11: Install and Dependency Check

**Files:**
- Modify: `Makefile` (add `deps-install` target)

- [ ] **Step 11.1: Add install targets to Makefile**

Edit `Makefile` to add after the existing targets:
```makefile
deps-install:
	@echo "Installing optional IaC validation tools..."
	@which tflint || curl -s https://raw.githubusercontent.com/terraform-linters/tflint/master/install_linux.sh | bash
	@which terraform-docs || (curl -Lo /tmp/terraform-docs.tar.gz \
		https://github.com/terraform-docs/terraform-docs/releases/download/v0.19.0/terraform-docs-v0.19.0-linux-amd64.tar.gz && \
		tar -xzf /tmp/terraform-docs.tar.gz -C /tmp && \
		chmod +x /tmp/terraform-docs && \
		mv /tmp/terraform-docs /usr/local/bin/)
	@which checkov || pip install checkov
	@echo "Done. Run 'make deps-check' to verify."
```

- [ ] **Step 11.2: Run dependency check**

```bash
make deps-check
```

Expected output lists which tools are present and which are missing (missing = optional, not blocking).

- [ ] **Step 11.3: Install binary**

```bash
make install
tf-architect --version 2>/dev/null || tf-architect --help | head -5
```

- [ ] **Step 11.4: Final commit**

```bash
git add Makefile
git commit -m "chore: add deps-install and deps-check Makefile targets"
```

---

## Self-Review: Spec Coverage Check

| Requirement | Task |
|---|---|
| CLI tool in Go | Task 10 |
| PDF/DOCX/MD/TXT ingestion | Task 2 |
| Claude Code as AI engine | Task 4 |
| FSM: analyze → clarify → generate → validate → document | Task 8 |
| Clarifying questions when data is missing | Task 6 + 8 |
| Detects inconsistencies | Task 6 (BuildAnalysisPrompt) |
| `terraform validate` at the end | Task 5 |
| Golden rules audit | Task 5 |
| tflint + checkov | Task 5 |
| Session persistence (resumable) | Task 3 |
| Resume after token exhaustion | Task 7 + 8 (checkpoint per module) |
| Re-execution on doc change (incremental) | Task 7 (ModeDocChanged) |
| Force re-run flag | Task 10 (`--force`) |
| Architectural documentation (Claude) | Task 9 |
| terraform-docs technical docs | Task 9 |
| docs/ARCHITECTURE.md, RUNBOOK.md, VARIABLES.md, CHANGELOG.md | Task 9 |
| status subcommand | Task 10 |
| reset subcommand | Task 10 |
| auto-fix after validation errors | Task 8 (runValidation) |

**No gaps found.**

---

## System Prerequisites Summary

```bash
# Required
apt install poppler-utils      # pdftotext, pdfinfo, pdftoppm (PDF ingestion)
go >= 1.21                     # Go toolchain
terraform >= 1.5               # Already installed

# Recommended (improves quality)
apt install pandoc             # Better DOCX→MD conversion
apt install tesseract-ocr tesseract-ocr-spa  # Scanned PDF OCR

# Optional (validation)
# tflint — installed by make deps-install
# checkov — pip install checkov
# terraform-docs — installed by make deps-install
```
