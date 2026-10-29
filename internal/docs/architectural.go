package docs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tf-architect/internal/security"
)

// SessionData is the subset of state.Session needed for doc generation.
type SessionData struct {
	DocPath        string
	DocMarkdown    string
	Clarifications []ClarificationEntry
	GeneratedFiles []string
	CreatedAt      time.Time
}

type ClarificationEntry struct {
	Question string
	Answer   string
}

// Client is the interface the docs package expects from the model client.
type Client interface {
	Query(ctx context.Context, prompt string) (string, error)
}

// TaskHeader opens the docs prompt, like every other task prompt.
const TaskHeader = "## TASK: DOCS"

// agentsHeader opens the generated AGENTS.md. The file is written with real
// values, so it must not be shared with an AI provider as is.
const agentsHeader = `> This file contains real infrastructure values. Do not paste it into an AI tool you do not trust, and do not commit it to a public repository.

`

type architecturalDocs struct {
	RootREADME    string
	Architecture  string
	Runbook       string
	Variables     string
	ProjectAgents string
}

// GenerateArchitecturalDocs generates the documentation files and AGENTS.md
// via the model. project is the masked rendered project; every file written
// goes through unmask, so the output directory holds real values.
func GenerateArchitecturalDocs(ctx context.Context, client Client, sess SessionData, outputDir, agentsBase, project string, unmask func(string) string, onStatus func(string)) error {
	onStatus("Generating architectural documentation...")

	prompt := buildDocsPrompt(sess, project)
	raw, err := client.Query(ctx, prompt)
	if err != nil {
		// AGENTS.md is always written; only a security stop or an interrupt
		// ends the run before anything more is done.
		var se *security.Error
		if !errors.As(err, &se) && ctx.Err() == nil {
			if werr := writeAgentsMD(outputDir, agentsBase, ""); werr == nil {
				onStatus("  ✓ AGENTS.md (base rules only)")
			}
		}
		return fmt.Errorf("architectural docs generation: %w", err)
	}

	adocs := parseDocSections(raw)
	adocs.RootREADME = unmask(adocs.RootREADME)
	adocs.Architecture = unmask(adocs.Architecture)
	adocs.Runbook = unmask(adocs.Runbook)
	adocs.Variables = unmask(adocs.Variables)
	adocs.ProjectAgents = unmask(adocs.ProjectAgents)

	docsDir := filepath.Join(outputDir, "docs")
	if err := os.MkdirAll(docsDir, 0755); err != nil {
		return err
	}

	files := map[string]string{
		filepath.Join(docsDir, "ARCHITECTURE.md"): adocs.Architecture,
		filepath.Join(docsDir, "RUNBOOK.md"):      adocs.Runbook,
		filepath.Join(docsDir, "VARIABLES.md"):    adocs.Variables,
	}
	// CHANGELOG.md is maintained manually after the first run.
	changelog := filepath.Join(docsDir, "CHANGELOG.md")
	if _, err := os.Stat(changelog); os.IsNotExist(err) {
		files[changelog] = unmask(changelogStub(sess))
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

	if adocs.ProjectAgents == "" {
		onStatus("  ⚠ the model returned no project section; AGENTS.md has only the base rules")
	}
	if err := writeAgentsMD(outputDir, agentsBase, adocs.ProjectAgents); err != nil {
		return err
	}
	onStatus("  ✓ AGENTS.md")

	return nil
}

func writeAgentsMD(outputDir, base, project string) error {
	if err := os.WriteFile(filepath.Join(outputDir, "AGENTS.md"), []byte(buildAgentsMD(base, project)), 0644); err != nil {
		return fmt.Errorf("writing AGENTS.md: %w", err)
	}
	return nil
}

// buildAgentsMD joins the header, the provider-neutral base and the
// model-written project section.
func buildAgentsMD(base, project string) string {
	out := agentsHeader + base
	if project != "" {
		out += "\n\n## Project\n\n" + project + "\n"
	}
	return out
}

func buildDocsPrompt(sess SessionData, project string) string {
	var tfFileList strings.Builder
	for _, f := range sess.GeneratedFiles {
		tfFileList.WriteString("- " + f + "\n")
	}

	var decisions strings.Builder
	for _, c := range sess.Clarifications {
		decisions.WriteString("- **" + c.Question + "**: " + c.Answer + "\n")
	}

	return TaskHeader + "\n\n" + `You are a senior infrastructure architect writing documentation for a Terraform project.

## Source Document
` + sess.DocMarkdown + `

## Architecture Decisions Made During Generation
` + decisions.String() + `

## Generated Terraform Files
` + tfFileList.String() + `
## Current Project Files
` + project + `

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
===END_VARIABLES===

===BEGIN_PROJECT_AGENTS===
Write the project-specific part of AGENTS.md, for any AI tool that edits this
repository later (it is appended under a "## Project" heading, so start with
subsections, not a title):
- What the project is and its module layout
- Naming, tagging and variable conventions actually used in the code
- Where state, environments and sensitive inputs live
- What to preserve and what to avoid when changing it
Reference real module paths, resource names and values from the files above.
===END_PROJECT_AGENTS===`
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
		RootREADME:    extract("===BEGIN_ROOT_README===", "===END_ROOT_README==="),
		Architecture:  extract("===BEGIN_ARCHITECTURE===", "===END_ARCHITECTURE==="),
		Runbook:       extract("===BEGIN_RUNBOOK===", "===END_RUNBOOK==="),
		Variables:     extract("===BEGIN_VARIABLES===", "===END_VARIABLES==="),
		ProjectAgents: extract("===BEGIN_PROJECT_AGENTS===", "===END_PROJECT_AGENTS==="),
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
