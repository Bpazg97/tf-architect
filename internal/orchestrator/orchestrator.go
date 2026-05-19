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
	noMask    bool

	// Callbacks wired by the CLI/TUI layer
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

			if o.OnQuestion == nil {
				o.status("Warning: no question handler set — skipping clarification (answers required)")
				break
			}
			answers := o.OnQuestion(questions)

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
			sess.AnalysisJSON = reanalysisRaw
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
		case decision.Mode == ModeResume:
			generatePrompt = fmt.Sprintf(
				"%s\n\nGenerate complete, production-grade Terraform IaC based on the analyzed architecture.\n"+
					"Write all files to: %s\n\n"+
					"After writing each module, output exactly: STEP_COMPLETE:<module_name>\n"+
					"This marker is used for progress checkpointing.",
				decision.ContextBlob, o.outputDir,
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
		if len(sess.GeneratedFiles) == 0 {
			o.status("Warning: no .tf files found in %s — generation may have failed silently", o.outputDir)
		}
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
		results2, valErr2 := suite.Run()
		run2 := state.ValidationRun{RunAt: time.Now(), Results: make(map[string]bool)}
		for _, r := range results2 {
			run2.Results[r.Stage] = r.Passed
			run2.Errors = append(run2.Errors, r.Errors...)
			icon := "✓"
			if !r.Passed {
				icon = "✗"
			}
			o.status("  (re-check) %s %s", icon, r.Stage)
		}
		run2.Passed = valErr2 == nil
		sess.ValidationRuns = append(sess.ValidationRuns, run2)
		if valErr2 != nil {
			_ = o.stMgr.Save(sess)
			return fmt.Errorf("validation failed after auto-fix: %w", valErr2)
		}
	}

	// ── DOCUMENT ─────────────────────────────────────────────────────────────
	if sess.CurrentPhase != state.PhaseDone {
		if err := o.stMgr.Advance(sess, state.PhaseDocument, state.Checkpoint{}); err != nil {
			return err
		}

		docs.Generate(o.client, sess, o.outputDir, func(msg string) { o.status("%s", msg) })

		_ = o.stMgr.Advance(sess, state.PhaseDone, state.Checkpoint{})
	}
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
