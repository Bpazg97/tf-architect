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
		GeneratedFiles:  prev.GeneratedFiles,
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
