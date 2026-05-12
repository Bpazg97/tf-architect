package docs

import (
	"errors"
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
		tfFileList.WriteString("- " + f + "\n")
	}

	var decisions strings.Builder
	for _, c := range sess.Clarifications {
		decisions.WriteString("- **" + c.Question + "**: " + c.Answer + "\n")
	}

	return `You are a senior infrastructure architect writing documentation for a Terraform project.

## Source Document
` + sess.DocMarkdown + `

## Architecture Decisions Made During Generation
` + decisions.String() + `

## Generated Terraform Files
` + tfFileList.String() + `

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
===END_VARIABLES===`
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
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	existingStr := string(existing)
	var finalContent string
	if existingStr != "" && strings.Contains(existingStr, beginMarker) {
		idx := strings.Index(existingStr, beginMarker)
		finalContent = prefix + "\n\n" + existingStr[idx:]
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
