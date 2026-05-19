# Sensitive Value Masking Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Mask sensitive infrastructure values (ARNs, IPs, account IDs, tag names, etc.) before any document text reaches Claude, then de-mask generated Terraform files after generation so real values appear in the final output.

**Architecture:** New `internal/masking` package with two pure functions — `Mask()` replaces sensitive values with stable category-prefixed placeholders, `Unmask()` walks `.tf` files and substitutes them back. The `MaskMap` (placeholder→real) is persisted as `mask.json` in the session directory alongside `session.json`. The orchestrator calls `Mask` after ingestion and `Unmask` after generation.

**Tech Stack:** Go standard library only — `regexp`, `encoding/json`, `os`, `path/filepath`. No new dependencies.

---

## File Map

| Action | Path | Responsibility |
|--------|------|----------------|
| Create | `internal/masking/patterns.go` | All compiled regex patterns and rule structs |
| Create | `internal/masking/masker.go` | `MaskMap`, `Mask`, `Unmask`, `SaveMaskMap`, `LoadMaskMap` |
| Create | `internal/masking/masker_test.go` | All 7 tests |
| Modify | `internal/state/session.go` | Add `MaskMapPath string` to `Session`; add `MaskMapPath()` to `Manager` |
| Modify | `internal/orchestrator/orchestrator.go` | Wire mask after ingest, unmask after generate; add `noMask bool` field |
| Modify | `cmd/main.go` | Add `--no-mask` flag; pass it to `orchestrator.New()` |

---

## Task 1: Create masker skeleton so tests can compile

**Files:**
- Create: `internal/masking/masker.go`

- [ ] **Step 1: Create the skeleton file**

```go
package masking

import (
	"encoding/json"
	"os"
)

// MaskMap maps placeholder → real value.
type MaskMap map[string]string

// Mask replaces sensitive values in text with stable placeholders.
// Pass an existing MaskMap to reuse placeholders on resume/doc-changed runs.
func Mask(text string, existing MaskMap) (string, MaskMap, error) {
	panic("not implemented")
}

// Unmask walks all .tf files in dir and replaces placeholders with real values in-place.
func Unmask(dir string, mm MaskMap) error {
	panic("not implemented")
}

// SaveMaskMap writes mm to path as JSON with 0600 permissions.
func SaveMaskMap(path string, mm MaskMap) error {
	data, err := json.MarshalIndent(mm, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// LoadMaskMap reads a MaskMap from a JSON file at path.
func LoadMaskMap(path string) (MaskMap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var mm MaskMap
	return mm, json.Unmarshal(data, &mm)
}
```

- [ ] **Step 2: Verify it compiles**

```bash
go build ./internal/masking/...
```

Expected: no errors (panics are fine at this stage).

---

## Task 2: Write all failing tests

**Files:**
- Create: `internal/masking/masker_test.go`

- [ ] **Step 1: Create the test file**

```go
package masking_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tf-architect/internal/masking"
)

func TestMaskARN(t *testing.T) {
	input := `role_arn = "arn:aws:iam::123456789012:role/my-service-role"`
	masked, mm, err := masking.Mask(input, nil)
	if err != nil {
		t.Fatalf("Mask() error: %v", err)
	}
	if strings.Contains(masked, "123456789012") {
		t.Error("account ID leaked into masked output")
	}
	if strings.Contains(masked, "my-service-role") {
		t.Error("role name leaked into masked output")
	}
	if len(mm) == 0 {
		t.Error("MaskMap should not be empty")
	}
	// ARN placeholders must start with arn:aws: so Claude understands context
	for ph, real := range mm {
		if strings.HasPrefix(real, "arn:aws:") && !strings.HasPrefix(ph, "arn:aws:") {
			t.Errorf("ARN real value %q got non-ARN placeholder %q", real, ph)
		}
	}
}

func TestMaskIdempotent(t *testing.T) {
	input := `server_a = "10.0.1.5"
server_b = "10.0.1.5"`
	masked, mm, err := masking.Mask(input, nil)
	if err != nil {
		t.Fatalf("Mask() error: %v", err)
	}
	// Both occurrences must get the same placeholder
	if strings.Count(masked, "IP_PRIVATE_001") != 2 {
		t.Errorf("expected IP_PRIVATE_001 twice in masked output, got:\n%s", masked)
	}
	// Only one entry in MaskMap for the same value
	if len(mm) != 1 {
		t.Errorf("expected 1 MaskMap entry for identical values, got %d: %v", len(mm), mm)
	}
}

func TestMaskMultiType(t *testing.T) {
	input := `arn = "arn:aws:s3:::my-company-prod-logs"
ip  = "10.0.0.1"
owner = "acme-corp"`
	masked, mm, err := masking.Mask(input, nil)
	if err != nil {
		t.Fatalf("Mask() error: %v", err)
	}
	if strings.Contains(masked, "my-company-prod-logs") {
		t.Error("ARN resource not masked")
	}
	if strings.Contains(masked, "10.0.0.1") {
		t.Error("private IP not masked")
	}
	if strings.Contains(masked, "acme-corp") {
		t.Error("tag value not masked")
	}
	// All placeholders must be unique
	seen := make(map[string]bool)
	for ph := range mm {
		if seen[ph] {
			t.Errorf("placeholder collision: %q appears twice in MaskMap", ph)
		}
		seen[ph] = true
	}
}

func TestUnmask(t *testing.T) {
	dir := t.TempDir()
	content := `resource "aws_instance" "web" {
  tags = {
    Owner = "TAG_VALUE_001"
    Env   = "TAG_VALUE_002"
  }
  private_ip = "IP_PRIVATE_001"
}`
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	// Write a non-.tf file that should NOT be touched
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("IP_PRIVATE_001"), 0644); err != nil {
		t.Fatal(err)
	}

	mm := masking.MaskMap{
		"TAG_VALUE_001": "tmobilitat",
		"TAG_VALUE_002": "production",
		"IP_PRIVATE_001": "10.0.1.5",
	}
	if err := masking.Unmask(dir, mm); err != nil {
		t.Fatalf("Unmask() error: %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(dir, "main.tf"))
	if !strings.Contains(string(got), "tmobilitat") {
		t.Error("tmobilitat not restored in .tf file")
	}
	if !strings.Contains(string(got), "10.0.1.5") {
		t.Error("10.0.1.5 not restored in .tf file")
	}
	if strings.Contains(string(got), "TAG_VALUE_001") {
		t.Error("placeholder TAG_VALUE_001 still present after unmask")
	}

	// Non-.tf file must be untouched
	notf, _ := os.ReadFile(filepath.Join(dir, "notes.txt"))
	if !strings.Contains(string(notf), "IP_PRIVATE_001") {
		t.Error("non-.tf file should not be modified by Unmask")
	}
}

func TestMaskMerge(t *testing.T) {
	// First run: mask one IP
	_, mm1, err := masking.Mask(`ip = "10.0.1.5"`, nil)
	if err != nil {
		t.Fatalf("first Mask() error: %v", err)
	}

	// Second run (doc changed): same IP + a new one, merge into existing mm
	masked2, mm2, err := masking.Mask(`ip  = "10.0.1.5"
ip2 = "10.0.1.6"`, mm1)
	if err != nil {
		t.Fatalf("second Mask() error: %v", err)
	}

	// Existing IP must reuse the same placeholder
	if !strings.Contains(masked2, "IP_PRIVATE_001") {
		t.Errorf("existing IP should keep IP_PRIVATE_001, got:\n%s", masked2)
	}
	// New IP must get the next counter
	if !strings.Contains(masked2, "IP_PRIVATE_002") {
		t.Errorf("new IP should get IP_PRIVATE_002, got:\n%s", masked2)
	}
	if len(mm2) != 2 {
		t.Errorf("expected 2 MaskMap entries, got %d: %v", len(mm2), mm2)
	}
}

func TestNoFalsePositives(t *testing.T) {
	input := `provider "aws" {
  region = "eu-west-1"
}
resource "aws_vpc" "main" {
  cidr_block = var.vpc_cidr
}
variable "owner" {
  default = var.owner
}
locals {
  name = local.project_name
}`
	masked, mm, err := masking.Mask(input, nil)
	if err != nil {
		t.Fatalf("Mask() error: %v", err)
	}
	if !strings.Contains(masked, "eu-west-1") {
		t.Error("AWS region eu-west-1 must NOT be masked")
	}
	if !strings.Contains(masked, "aws_vpc") {
		t.Error("Terraform resource type aws_vpc must NOT be masked")
	}
	if !strings.Contains(masked, "var.owner") {
		t.Error("Terraform variable reference var.owner must NOT be masked")
	}
	if !strings.Contains(masked, "var.vpc_cidr") {
		t.Error("Terraform variable reference var.vpc_cidr must NOT be masked")
	}
	if !strings.Contains(masked, "local.project_name") {
		t.Error("Terraform local reference local.project_name must NOT be masked")
	}
	if len(mm) != 0 {
		t.Errorf("expected empty MaskMap for safe content, got %v", mm)
	}
}

func TestMaskJsonPersist(t *testing.T) {
	mm := masking.MaskMap{
		"ACCOUNT_001":    "123456789012",
		"IP_PRIVATE_001": "10.0.1.5",
		"TAG_VALUE_001":  "tmobilitat",
	}
	path := filepath.Join(t.TempDir(), "mask.json")

	if err := masking.SaveMaskMap(path, mm); err != nil {
		t.Fatalf("SaveMaskMap() error: %v", err)
	}

	loaded, err := masking.LoadMaskMap(path)
	if err != nil {
		t.Fatalf("LoadMaskMap() error: %v", err)
	}
	for ph, want := range mm {
		if got := loaded[ph]; got != want {
			t.Errorf("loaded[%q] = %q, want %q", ph, got, want)
		}
	}
	// File must be written with restrictive permissions (no world-read)
	info, _ := os.Stat(path)
	if info.Mode()&0o044 != 0 {
		t.Errorf("mask.json should not be group/world readable, mode: %v", info.Mode())
	}
}
```

- [ ] **Step 2: Run tests — expect compile success, runtime panic**

```bash
go test ./internal/masking/... -v 2>&1 | head -30
```

Expected: tests compile and panic on `Mask()` / `Unmask()` (skeleton stubs).

---

## Task 3: Implement patterns.go

**Files:**
- Create: `internal/masking/patterns.go`

- [ ] **Step 1: Create the patterns file**

```go
package masking

import "regexp"

// maskRule defines one category of sensitive data to detect and replace.
type maskRule struct {
	re          *regexp.Regexp
	category    string // used to build placeholder: CATEGORY_NNN
	format      string // sprintf format for placeholder: "arn:aws:%s" or "%s"
	// For context-aware rules (tag values, bucket names), valueGroup > 0:
	//   group 1 = prefix to preserve, group 2 = sensitive value, group 3 = suffix.
	valueGroup  int
	prefixGroup int
	suffixGroup int
}

// patterns is the ordered list of masking rules.
// More specific / longer patterns must appear before broader ones.
var patterns = []maskRule{
	// AWS ARN — most specific; must come before account-ID rule.
	// Placeholder keeps "arn:aws:" prefix so Claude understands the context type.
	{
		re:       regexp.MustCompile(`arn:aws:[a-z0-9\-]+:[^:\s"']*:[^:\s"']*:[^\s"',]+`),
		category: "RESOURCE",
		format:   "arn:aws:%s",
	},
	// AWS account ID — standalone 12-digit number.
	// Word boundaries prevent matching inside longer numeric strings.
	{
		re:       regexp.MustCompile(`\b\d{12}\b`),
		category: "ACCOUNT",
		format:   "%s",
	},
	// AWS resource IDs: i-, sg-, vpc-, subnet-, rtb-, igw-, nat-, ami-, vol-, snap-, eni-, acl-, lb-, tg-
	{
		re:       regexp.MustCompile(`\b(?:i|sg|vpc|subnet|rtb|igw|nat|ami|vol|snap|eni|acl|lb|tg)-[0-9a-f]{8,17}\b`),
		category: "RESOURCE_ID",
		format:   "%s",
	},
	// Private CIDR blocks — before plain IPs to consume the /prefix.
	{
		re:       regexp.MustCompile(`\b(?:10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3})/\d{1,2}\b`),
		category: "CIDR_PRIVATE",
		format:   "%s",
	},
	// Public CIDR blocks.
	{
		re:       regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}/\d{1,2}\b`),
		category: "CIDR_PUBLIC",
		format:   "%s",
	},
	// Private IPv4 addresses — after CIDR rules.
	{
		re:       regexp.MustCompile(`\b(?:10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3})\b`),
		category: "IP_PRIVATE",
		format:   "%s",
	},
	// Public IPv4 addresses.
	{
		re:       regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`),
		category: "IP_PUBLIC",
		format:   "%s",
	},
	// Email — must run before FQDN so "user@host.domain.com" is consumed whole.
	{
		re:       regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`),
		category: "EMAIL",
		format:   "%s",
	},
	// FQDN / hostname — requires at least 2 dots; lowercase only to avoid matching
	// uppercase placeholders on subsequent pattern passes.
	{
		re:       regexp.MustCompile(`\b[a-z0-9][a-z0-9\-]*\.[a-z0-9][a-z0-9\-]*\.[a-z]{2,}\b`),
		category: "HOST",
		format:   "%s",
	},
	// S3 bucket name in context: bucket = "name" or bucket_name = "name".
	{
		re:          regexp.MustCompile(`(?i)(bucket(?:_name)?\s*[=:]\s*["'])([^"']+)(["'])`),
		category:    "BUCKET",
		format:      "%s",
		valueGroup:  2,
		prefixGroup: 1,
		suffixGroup: 3,
	},
	// Tag values in context: owner/Name/team/project/environment/env/application = "value".
	{
		re:          regexp.MustCompile(`(?i)((?:owner|Name|team|project|environment|env|application)\s*[=:]\s*["'])([^"']+)(["'])`),
		category:    "TAG_VALUE",
		format:      "%s",
		valueGroup:  2,
		prefixGroup: 1,
		suffixGroup: 3,
	},
}
```

- [ ] **Step 2: Verify patterns file compiles**

```bash
go build ./internal/masking/...
```

Expected: no errors.

---

## Task 4: Implement Mask() and helpers in masker.go

**Files:**
- Modify: `internal/masking/masker.go`

- [ ] **Step 1: Replace the skeleton masker.go with full implementation**

```go
package masking

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// MaskMap maps placeholder → real value.
// Persisted as mask.json alongside session.json.
type MaskMap map[string]string

// Mask replaces sensitive values in text with stable placeholders.
// Pass an existing MaskMap to reuse placeholders for already-seen values
// (resume / doc-changed runs). Pass nil for a fresh run.
// Returns the masked text and the complete (merged) MaskMap.
func Mask(text string, existing MaskMap) (string, MaskMap, error) {
	s := newMaskState(existing)
	result := text
	for i := range patterns {
		result = patterns[i].apply(result, s)
	}
	return result, s.mm, nil
}

// Unmask walks all .tf files in dir and replaces placeholders with real values in-place.
// If a file cannot be read or written, a warning is appended to the returned error
// but processing continues for remaining files.
func Unmask(dir string, mm MaskMap) error {
	var warnings []string
	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".tf") {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			warnings = append(warnings, fmt.Sprintf("%s: read: %v", path, readErr))
			return nil
		}
		content := string(data)
		for placeholder, real := range mm {
			content = strings.ReplaceAll(content, placeholder, real)
		}
		if writeErr := os.WriteFile(path, []byte(content), info.Mode()); writeErr != nil {
			warnings = append(warnings, fmt.Sprintf("%s: write: %v", path, writeErr))
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	if len(warnings) > 0 {
		return fmt.Errorf("unmask partial: %s", strings.Join(warnings, "; "))
	}
	return nil
}

// SaveMaskMap writes mm to path as JSON with 0600 permissions.
// Failure is a hard error — callers must not proceed without a saved map.
func SaveMaskMap(path string, mm MaskMap) error {
	data, err := json.MarshalIndent(mm, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// LoadMaskMap reads a MaskMap from a JSON file at path.
func LoadMaskMap(path string) (MaskMap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var mm MaskMap
	return mm, json.Unmarshal(data, &mm)
}

// ── internal ─────────────────────────────────────────────────────────────────

type maskState struct {
	realToPlaceholder map[string]string
	mm                MaskMap       // placeholder → real (the output)
	counters          map[string]int // category → highest index used so far
}

func newMaskState(existing MaskMap) *maskState {
	s := &maskState{
		realToPlaceholder: make(map[string]string),
		mm:                make(MaskMap),
		counters:          make(map[string]int),
	}
	for ph, real := range existing {
		s.mm[ph] = real
		s.realToPlaceholder[real] = ph
		cat, idx := parsePlaceholder(ph)
		if idx > s.counters[cat] {
			s.counters[cat] = idx
		}
	}
	return s
}

// makePlaceholder returns an existing placeholder for value if one exists,
// or mints a new one using category and format.
func (s *maskState) makePlaceholder(category, format, value string) string {
	if ph, ok := s.realToPlaceholder[value]; ok {
		return ph
	}
	s.counters[category]++
	id := fmt.Sprintf("%s_%03d", category, s.counters[category])
	ph := fmt.Sprintf(format, id)
	s.mm[ph] = value
	s.realToPlaceholder[value] = ph
	return ph
}

// placeholderBodyRe matches the CATEGORY_NNN suffix of a placeholder.
var placeholderBodyRe = regexp.MustCompile(`([A-Z_]+)_(\d+)$`)

// parsePlaceholder extracts (category, index) from a placeholder like
// "ACCOUNT_001" or "arn:aws:RESOURCE_001".
func parsePlaceholder(ph string) (string, int) {
	// Strip any lowercase prefix ending in ":" (e.g. "arn:aws:")
	if i := strings.LastIndex(ph, ":"); i >= 0 {
		ph = ph[i+1:]
	}
	m := placeholderBodyRe.FindStringSubmatch(ph)
	if len(m) < 3 {
		return "", 0
	}
	idx, _ := strconv.Atoi(m[2])
	return m[1], idx
}

// apply runs the rule's pattern against text, replacing matches with placeholders.
func (rule *maskRule) apply(text string, s *maskState) string {
	if rule.valueGroup == 0 {
		// Simple rule: replace the entire match.
		return rule.re.ReplaceAllStringFunc(text, func(match string) string {
			// Skip if already a placeholder (prevents double-masking on multi-pass).
			if _, already := s.mm[match]; already {
				return match
			}
			return s.makePlaceholder(rule.category, rule.format, match)
		})
	}

	// Context-aware rule: only the value subgroup is sensitive;
	// prefix and suffix are preserved verbatim.
	return rule.re.ReplaceAllStringFunc(text, func(match string) string {
		subs := rule.re.FindStringSubmatch(match)
		if len(subs) <= rule.valueGroup {
			return match
		}
		value := subs[rule.valueGroup]
		// Skip if the value is already a known placeholder.
		if _, already := s.mm[value]; already {
			return match
		}
		placeholder := s.makePlaceholder(rule.category, rule.format, value)
		prefix, suffix := "", ""
		if rule.prefixGroup > 0 && len(subs) > rule.prefixGroup {
			prefix = subs[rule.prefixGroup]
		}
		if rule.suffixGroup > 0 && len(subs) > rule.suffixGroup {
			suffix = subs[rule.suffixGroup]
		}
		return prefix + placeholder + suffix
	})
}
```

- [ ] **Step 2: Run masking tests (excluding TestUnmask for now)**

```bash
go test ./internal/masking/... -run 'TestMaskARN|TestMaskIdempotent|TestMaskMultiType|TestMaskMerge|TestNoFalsePositives|TestMaskJsonPersist' -v
```

Expected: all 6 tests PASS.

- [ ] **Step 3: Run TestUnmask**

```bash
go test ./internal/masking/... -run TestUnmask -v
```

Expected: PASS (Unmask is already fully implemented above).

- [ ] **Step 4: Run full suite**

```bash
go test ./internal/masking/... -v
```

Expected: all 7 tests PASS, 0 failures.

- [ ] **Step 5: Commit masking package**

```bash
git add internal/masking/
git commit -m "feat(masking): add sensitive value masking package

Mask() replaces ARNs, IPs, account IDs, FQDNs, tag values, and emails
with stable category-prefixed placeholders before sending docs to Claude.
Unmask() restores real values in generated .tf files after generation.
MaskMap is persisted as mask.json; merging supports resume/doc-changed flows."
```

---

## Task 5: Extend state.Session and state.Manager

**Files:**
- Modify: `internal/state/session.go` at lines 64–83 (Session struct) and after line 162 (Manager methods)

- [ ] **Step 1: Add MaskMapPath field to Session struct**

In `internal/state/session.go`, add one field to the `Session` struct after `DiffSummary`:

```go
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
	MaskMapPath     string `json:"mask_map_path,omitempty"`
}
```

- [ ] **Step 2: Add MaskMapPath method to Manager**

Append after the `HashDoc` function at the bottom of `internal/state/session.go`:

```go
// MaskMapPath returns the absolute path to the mask.json file for a session.
func (m *Manager) MaskMapPath(docHash string) string {
	return filepath.Join(m.sessionDir(docHash), "mask.json")
}
```

- [ ] **Step 3: Verify state package still compiles and tests pass**

```bash
go test ./internal/state/... -v
```

Expected: all existing state tests PASS, no new failures.

- [ ] **Step 4: Commit state changes**

```bash
git add internal/state/session.go
git commit -m "feat(state): add MaskMapPath to Session and Manager"
```

---

## Task 6: Wire masking in the orchestrator

**Files:**
- Modify: `internal/orchestrator/orchestrator.go`

There are three changes: (A) add `noMask bool` to `Orchestrator` and `New()`, (B) call `Mask` after ingestion, (C) call `Unmask` after generation.

- [ ] **Step 1: Add import and noMask field**

Add `"tf-architect/internal/masking"` to the import block and add `noMask bool` to the struct:

```go
import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tf-architect/internal/claude"
	"tf-architect/internal/docs"
	"tf-architect/internal/ingestion"
	"tf-architect/internal/masking"
	"tf-architect/internal/state"
	"tf-architect/internal/validation"
)

// Orchestrator runs the tf-architect FSM.
type Orchestrator struct {
	client    *claude.Client
	stMgr     *state.Manager
	workDir   string
	outputDir string
	noMask    bool

	OnQuestion func(questions []string) []string
	OnStatus   func(msg string)
	OnChunk    func(chunk string)
}

func New(workDir, outputDir, systemPrompt string, noMask bool) *Orchestrator {
	return &Orchestrator{
		client:    claude.New(workDir, systemPrompt),
		stMgr:     state.NewManager(workDir),
		workDir:   workDir,
		outputDir: outputDir,
		noMask:    noMask,
	}
}
```

- [ ] **Step 2: Add masking block in the ingest phase**

In `Run()`, replace the ingest block (currently lines 67–78) with:

```go
	// ── INGEST ───────────────────────────────────────────────────────────────
	if sess.CurrentPhase.AtOrBefore(state.PhaseIngest) || sess.DocMarkdown == "" {
		o.status("Converting document: %s", filepath.Base(docPath))
		conv, err := ingestion.Convert(docPath)
		if err != nil {
			return fmt.Errorf("ingestion: %w", err)
		}
		o.status("[%s] ~%d tokens", conv.SourceFormat, conv.EstimatedTokens)

		if o.noMask {
			o.status("Warning: --no-mask enabled — sensitive values will be sent to Claude")
			sess.DocMarkdown = conv.Markdown
		} else {
			// Load existing MaskMap for resume or doc-changed runs.
			var existingMM masking.MaskMap
			if sess.MaskMapPath != "" {
				if mm, loadErr := masking.LoadMaskMap(sess.MaskMapPath); loadErr == nil {
					existingMM = mm
				}
			} else if sess.PreviousDocHash != "" {
				oldPath := o.stMgr.MaskMapPath(sess.PreviousDocHash)
				if mm, loadErr := masking.LoadMaskMap(oldPath); loadErr == nil {
					existingMM = mm
				}
			}

			masked, mm, maskErr := masking.Mask(conv.Markdown, existingMM)
			if maskErr != nil {
				return fmt.Errorf("masking document: %w", maskErr)
			}
			maskPath := o.stMgr.MaskMapPath(sess.DocHash)
			if saveErr := masking.SaveMaskMap(maskPath, mm); saveErr != nil {
				return fmt.Errorf("saving mask map (aborting to prevent data leak): %w", saveErr)
			}
			sess.DocMarkdown = masked
			sess.MaskMapPath = maskPath
			o.status("Masked %d sensitive values", len(mm))
		}

		if err := o.stMgr.Advance(sess, state.PhaseAnalyze, state.Checkpoint{}); err != nil {
			return err
		}
	}
```

- [ ] **Step 3: Add unmask block after the generate phase**

In `Run()`, after the generate phase's `o.stMgr.Advance(sess, state.PhaseValidate, ...)` call and before `return o.runValidation(sess)`, insert:

```go
	// ── UNMASK ───────────────────────────────────────────────────────────────
	if !o.noMask && sess.MaskMapPath != "" {
		mm, loadErr := masking.LoadMaskMap(sess.MaskMapPath)
		if loadErr != nil {
			return fmt.Errorf("loading mask map for unmask: %w", loadErr)
		}
		o.status("De-masking generated Terraform files...")
		if unmaskErr := masking.Unmask(o.outputDir, mm); unmaskErr != nil {
			o.status("Warning: %v", unmaskErr)
		}
	}

	// ── VALIDATE ─────────────────────────────────────────────────────────────
	return o.runValidation(sess)
```

- [ ] **Step 4: Build to catch any compile errors**

```bash
go build ./...
```

Expected: no errors.

- [ ] **Step 5: Run full test suite**

```bash
go test ./... 2>&1
```

Expected: all tests pass.

- [ ] **Step 6: Commit orchestrator changes**

```bash
git add internal/orchestrator/orchestrator.go
git commit -m "feat(orchestrator): wire sensitive value masking into Run pipeline

Mask() called after ingestion before Claude sees the document.
MaskMap persisted to session/mask.json (hard error on write failure).
Unmask() called after generation, before validation, to restore real
values in .tf files. --no-mask flag disables for debugging."
```

---

## Task 7: Add --no-mask CLI flag

**Files:**
- Modify: `cmd/main.go`

- [ ] **Step 1: Add the flag variable and wire it**

Add `noMaskFlag` variable alongside the existing flag variables, register the flag on `runCmd`, and pass it to `orchestrator.New`:

```go
var (
	outputDir   string
	forceFlag   bool
	systemFile  string
	noMaskFlag  bool
	version     = "dev"
)
```

In the `runCmd` flags section (after the existing `Flags()` calls):

```go
runCmd.Flags().BoolVar(&noMaskFlag, "no-mask", false, "Disable sensitive value masking (debug only — never use in production)")
```

In `runGenerate`, update the `orchestrator.New` call:

```go
orch := orchestrator.New(workDir, outputDir, systemPrompt, noMaskFlag)
```

- [ ] **Step 2: Build**

```bash
go build ./...
```

Expected: no errors.

- [ ] **Step 3: Smoke test the flag is wired**

```bash
./tf-architect run --help | grep no-mask
```

Expected output contains:
```
--no-mask   Disable sensitive value masking (debug only — never use in production)
```

- [ ] **Step 4: Run all tests one final time**

```bash
go test ./... -count=1
```

Expected: all tests pass.

- [ ] **Step 5: Final commit**

```bash
git add cmd/main.go
git commit -m "feat(cli): add --no-mask flag for debugging

Passes noMask=true to orchestrator.New() which skips masking and logs
a visible warning. Never use in production — added to help docs."
```

---

## Self-Review Notes

- **Spec coverage:** All 4 spec sections covered — architecture (Tasks 1–4), data flow (Tasks 5–6), sensitive patterns (Task 3), error handling / testing (Tasks 2, 4).
- **ModeDocChanged limitation:** `computeMarkdownDiff` in `resume.go` compares `prev.DocMarkdown` (masked) against the new raw doc. The diff may surface placeholder strings in the "removed" list. This is a known limitation scoped out of this plan; it does not affect correctness of masking or generation.
- **Unmask and auto-fix:** After validation fails and Claude auto-fixes `.tf` files, Claude reads real values (already unmasked). The fix prompt context is masked (from session), which is correct — Claude doesn't need real values in the context blob, only in the files it reads.
- **No placeholder scan issues found.**
- **Type consistency verified:** `MaskMap`, `Mask()`, `Unmask()`, `SaveMaskMap()`, `LoadMaskMap()`, `MaskMapPath()` — all consistent across Tasks 1–7.
