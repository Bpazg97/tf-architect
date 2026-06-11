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
	"tf-architect/internal/masking"
	"tf-architect/internal/state"
	"tf-architect/internal/validation"
)

const maxClarifyRounds = 3

// Orchestrator runs the tf-architect FSM.
//
// Security invariant: when masking is enabled, every byte sent to Claude
// (document, context blob, clarification answers, validation errors) and
// every file Claude can read in outputDir is masked. Files are de-masked
// exactly once, at the very end of the pipeline, after the last Claude call.
type Orchestrator struct {
	client    *claude.Client
	stMgr     *state.Manager
	workDir   string
	outputDir string
	noMask    bool

	maskMap     masking.MaskMap
	maskMapPath string

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

// maskText masks sensitive values in text using the session mask map,
// persisting any newly discovered values. Returns text unchanged when
// masking is disabled or no mask map path is established yet.
func (o *Orchestrator) maskText(text string) (string, error) {
	if o.noMask || text == "" || o.maskMapPath == "" {
		return text, nil
	}
	masked, mm, err := masking.Mask(text, o.maskMap)
	if err != nil {
		return "", fmt.Errorf("masking text: %w", err)
	}
	o.maskMap = mm
	if err := masking.SaveMaskMap(o.maskMapPath, mm); err != nil {
		return "", fmt.Errorf("saving mask map (aborting to prevent data leak): %w", err)
	}
	return masked, nil
}

func (o *Orchestrator) Run(docPath string) error {
	// ── RESUME DECISION ─────────────────────────────────────────────────────
	decision, err := DecideResume(docPath, o.stMgr)
	if err != nil {
		return fmt.Errorf("resume decision: %w", err)
	}
	sess := decision.Session
	o.status("Mode: %s | Phase: %s", decision.Mode, sess.CurrentPhase)

	// Restore mask map for resumed sessions so all later text is sanitized
	// with stable placeholders.
	if !o.noMask && sess.MaskMapPath != "" {
		if mm, loadErr := masking.LoadMaskMap(sess.MaskMapPath); loadErr == nil {
			o.maskMap = mm
			o.maskMapPath = sess.MaskMapPath
		}
	}
	if decision.ContextBlob != "" {
		blob, maskErr := o.maskText(decision.ContextBlob)
		if maskErr != nil {
			return maskErr
		}
		decision.ContextBlob = blob
	}

	// ── VALIDATE ONLY ────────────────────────────────────────────────────────
	if decision.Mode == ModeValidateOnly {
		o.status("Document unchanged. Re-running validation suite...")
		return o.runValidation(sess, false)
	}

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
			existingMM := o.maskMap
			if existingMM == nil && sess.PreviousDocHash != "" {
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
			o.maskMap = mm
			o.maskMapPath = maskPath
			o.status("Masked %d sensitive values", len(mm))

			// Sanitize context blob (may contain diff lines from the new raw document).
			if decision.ContextBlob != "" {
				maskedBlob, blobErr := o.maskText(decision.ContextBlob)
				if blobErr != nil {
					return blobErr
				}
				decision.ContextBlob = maskedBlob
			}
		}

		if err := o.stMgr.Advance(sess, state.PhaseAnalyze, state.Checkpoint{}); err != nil {
			return err
		}
	}

	// ── ANALYZE ──────────────────────────────────────────────────────────────
	if sess.CurrentPhase.AtOrBefore(state.PhaseAnalyze) {
		o.status("Analyzing architecture...")
		var analysisPrompt string
		switch {
		case decision.Mode == ModeDocChanged:
			analysisPrompt = fmt.Sprintf(
				"%s\n\n## CURRENT DOCUMENT (UPDATED):\n%s\n\n"+
					"Analyze only what changed. Which Terraform resources need modification?",
				decision.ContextBlob, sess.DocMarkdown,
			)
		case decision.Mode == ModeResume && decision.ContextBlob != "":
			analysisPrompt = decision.ContextBlob + "\n\n---\n\n" + BuildAnalysisPrompt(sess.DocMarkdown)
		default:
			analysisPrompt = BuildAnalysisPrompt(sess.DocMarkdown)
		}

		analysisRaw, err := o.client.Query(analysisPrompt)
		if err != nil {
			_ = o.stMgr.Save(sess)
			return fmt.Errorf("analyze (state saved): %w", err)
		}
		sess.AnalysisJSON = analysisRaw
		sess.ClaudeSessionID = o.client.GetSessionID()
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

			// Mask answers BEFORE persisting or sending — users type real
			// CIDRs, account IDs and hostnames here.
			for i := range answers {
				masked, maskErr := o.maskText(answers[i])
				if maskErr != nil {
					return maskErr
				}
				answers[i] = masked
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

		// Doc-changed runs edit files left de-masked by the previous completed
		// run — re-mask them so Claude never reads real values.
		if decision.Mode == ModeDocChanged && !o.noMask && len(o.maskMap) > 0 {
			o.status("Re-masking existing Terraform files before AI edits...")
			if err := masking.MaskDir(o.outputDir, o.maskMap); err != nil {
				return fmt.Errorf("masking existing files (aborting to prevent data leak): %w", err)
			}
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
		sess.ClaudeSessionID = o.client.GetSessionID()
		if len(sess.GeneratedFiles) == 0 {
			o.status("Warning: no .tf files found in %s — generation may have failed silently", o.outputDir)
		}
		if err := o.stMgr.Advance(sess, state.PhaseValidate, state.Checkpoint{}); err != nil {
			return err
		}
	}

	// ── VALIDATE ─────────────────────────────────────────────────────────────
	// Files stay masked through validation, auto-fix and docs; de-masking
	// happens once at the end of runValidation, after the last Claude call.
	filesMasked := !o.noMask && len(o.maskMap) > 0
	return o.runValidation(sess, filesMasked)
}

func (o *Orchestrator) runValidation(sess *state.Session, filesMasked bool) error {
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
	// Mask error strings before persisting — validate-only runs operate on
	// de-masked files, so raw values can appear in tool output.
	if len(run.Errors) > 0 {
		maskedErrs, maskErr := o.maskText(strings.Join(run.Errors, "\n"))
		if maskErr != nil {
			return maskErr
		}
		run.Errors = strings.Split(maskedErrs, "\n")
	}
	sess.ValidationRuns = append(sess.ValidationRuns, run)

	if valErr != nil {
		o.status("Errors detected. Attempting auto-fix...")

		// Auto-fix lets Claude read files in outputDir. If they are currently
		// de-masked (validate-only path), re-mask them first.
		if !o.noMask && !filesMasked && len(o.maskMap) > 0 {
			o.status("Re-masking files before AI auto-fix...")
			if err := masking.MaskDir(o.outputDir, o.maskMap); err != nil {
				return fmt.Errorf("masking files before auto-fix (aborting to prevent data leak): %w", err)
			}
			filesMasked = true
		}

		blob, blobErr := o.maskText(buildContextBlob(sess))
		if blobErr != nil {
			return blobErr
		}
		fixPrompt := fmt.Sprintf(
			"%s\n\nValidation errors:\n%s\n\nFix all errors in %s. Do not touch files without errors.",
			blob,
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
			o.unmaskOutput(filesMasked)
			return fmt.Errorf("validation failed after auto-fix: %w", valErr2)
		}
	}

	// ── DOCUMENT ─────────────────────────────────────────────────────────────
	// Runs while files are still masked: the docs prompt instructs Claude to
	// read resource names/CIDRs from the code.
	if sess.CurrentPhase != state.PhaseDone {
		if err := o.stMgr.Advance(sess, state.PhaseDocument, state.Checkpoint{}); err != nil {
			return err
		}
		docs.Generate(o.client, sess, o.outputDir, func(msg string) { o.status("%s", msg) })
	}

	// ── UNMASK (single point, after the last Claude call) ───────────────────
	o.unmaskOutput(filesMasked)

	if sess.CurrentPhase != state.PhaseDone {
		_ = o.stMgr.Advance(sess, state.PhaseDone, state.Checkpoint{})
	}
	o.status("✓ Session complete — output in %s", o.outputDir)
	return nil
}

func (o *Orchestrator) unmaskOutput(filesMasked bool) {
	if o.noMask || !filesMasked || len(o.maskMap) == 0 {
		return
	}
	o.status("De-masking generated files...")
	if err := masking.Unmask(o.outputDir, o.maskMap); err != nil {
		o.status("Warning: %v", err)
	}
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
