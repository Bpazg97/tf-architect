package docs

import (
	"time"

	"tf-architect/internal/state"
)

// Generate runs both documentation layers in the correct order:
// 1. Architectural docs (Claude) — writes prefix + docs/ directory
// 2. terraform-docs — injects technical docs into BEGIN_TF_DOCS markers
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
