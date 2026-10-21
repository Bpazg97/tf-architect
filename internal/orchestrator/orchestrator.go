package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"tf-architect/internal/claude"
	"tf-architect/internal/docs"
	"tf-architect/internal/fsutil"
	"tf-architect/internal/ingestion"
	"tf-architect/internal/masking"
	"tf-architect/internal/state"
)

const maxClarifyRounds = 3

const stepMarker = "STEP_COMPLETE:"

// Orchestrator runs the tf-architect FSM.
//
// Security invariant: when masking is enabled, nothing sensitive reaches
// Claude.
//   - Every prompt and the system prompt pass through maskText inside the
//     client (single choke point, fail closed).
//   - Claude runs confined to outputDir with file tools only, so it cannot
//     read the source document, session state or mask.json.
//   - outputDir is masked before the first Claude call and de-masked once,
//     when Run returns. Validators run on a de-masked temporary copy.
type Orchestrator struct {
	client    *claude.Client
	stMgr     *state.Manager
	workDir   string
	outputDir string
	noMask    bool

	maskMap     masking.MaskMap
	maskMapPath string

	// CustomSensitive are values the user declared sensitive at startup,
	// masked in addition to the built-in patterns.
	CustomSensitive []string

	interrupted atomic.Bool

	// Callbacks wired by the CLI/TUI layer
	OnQuestion func(questions []string) []string
	OnStatus   func(msg string)
	OnChunk    func(chunk string)
}

// maskingNote tells Claude how to treat placeholders, whatever system prompt
// the user supplied.
const maskingNote = `

## MASKED VALUES

Sensitive values were replaced by placeholder tokens: UPPERCASE_CATEGORY_NNN
(for example CIDR_PRIVATE_001, HOST_002, CUSTOM_001) or arn:aws:RESOURCE_NNN.
Treat each placeholder as the real value it stands for. Copy it verbatim
wherever that value is needed. Never alter, split or reformat a placeholder,
never replace it with an invented value, and never ask for the real value.`

// withMaskingNote appends the placeholder instructions unless masking is off
// or the system prompt already carries a MASKED VALUES section.
func withMaskingNote(systemPrompt string, noMask bool) string {
	if noMask || strings.Contains(systemPrompt, "## MASKED VALUES") {
		return systemPrompt
	}
	return systemPrompt + maskingNote
}

func New(workDir, outputDir, systemPrompt string, noMask bool) *Orchestrator {
	systemPrompt = withMaskingNote(systemPrompt, noMask)
	return &Orchestrator{
		client:    claude.New(outputDir, systemPrompt),
		stMgr:     state.NewManager(workDir),
		workDir:   workDir,
		outputDir: outputDir,
		noMask:    noMask,
	}
}

// status reports progress to the user. Placeholders are resolved for display
// only; this text never goes to Claude.
func (o *Orchestrator) status(format string, args ...interface{}) {
	if o.OnStatus != nil {
		o.OnStatus(o.display(fmt.Sprintf(format, args...)))
	}
}

func (o *Orchestrator) display(text string) string {
	if o.noMask {
		return text
	}
	return masking.UnmaskText(text, o.maskMap)
}

// Interrupt stops the run at the next Claude call: nothing more is sent and
// Run returns, de-masking the output on the way out. Safe from any goroutine.
func (o *Orchestrator) Interrupt() {
	o.interrupted.Store(true)
}

// outgoing is the client's gate for every prompt: refuse after an interrupt,
// otherwise mask.
func (o *Orchestrator) outgoing(text string) (string, error) {
	if o.interrupted.Load() {
		return "", fmt.Errorf("interrupted by user")
	}
	return o.maskText(text)
}

// maskText masks sensitive values in text using the session mask map,
// persisting any newly discovered values before the masked text is used.
func (o *Orchestrator) maskText(text string) (string, error) {
	if o.noMask || text == "" {
		return text, nil
	}
	if o.maskMapPath == "" {
		return "", fmt.Errorf("mask map not initialised (aborting to prevent data leak)")
	}
	masked, mm, err := masking.Mask(text, o.maskMap)
	if err != nil {
		return "", fmt.Errorf("masking text: %w", err)
	}
	if err := o.saveMaskMap(mm); err != nil {
		return "", err
	}
	return masked, nil
}

func (o *Orchestrator) saveMaskMap(mm masking.MaskMap) error {
	if len(mm) != len(o.maskMap) {
		if err := masking.SaveMaskMap(o.maskMapPath, mm); err != nil {
			return fmt.Errorf("saving mask map (aborting to prevent data leak): %w", err)
		}
	}
	o.maskMap = mm
	return nil
}

// initMasking loads (or starts) the session mask map and adds the custom
// values. A session that recorded a mask map must be able to load it: fail
// closed otherwise.
func (o *Orchestrator) initMasking(sess *state.Session, prevMM masking.MaskMap) error {
	mm := prevMM
	if sess.MaskMapPath != "" {
		loaded, err := masking.LoadMaskMap(sess.MaskMapPath)
		if err != nil {
			return fmt.Errorf("loading mask map (aborting to prevent data leak): %w", err)
		}
		mm = loaded
	} else {
		sess.MaskMapPath = o.stMgr.MaskMapPath(sess.DocHash)
	}
	mm = masking.AddCustom(mm, o.CustomSensitive)
	// Written even when empty: a recorded MaskMapPath must always load.
	if err := masking.SaveMaskMap(sess.MaskMapPath, mm); err != nil {
		return fmt.Errorf("saving mask map (aborting to prevent data leak): %w", err)
	}
	o.maskMap, o.maskMapPath = mm, sess.MaskMapPath
	return nil
}

// maskOutput masks every text file in outputDir. Idempotent.
func (o *Orchestrator) maskOutput() error {
	mm, err := masking.MaskDir(o.outputDir, o.maskMap)
	if err != nil {
		return fmt.Errorf("masking %s (aborting to prevent data leak): %w", o.outputDir, err)
	}
	return o.saveMaskMap(mm)
}

func (o *Orchestrator) unmaskOutput() {
	if len(o.maskMap) == 0 {
		return
	}
	o.status("De-masking generated files...")
	if err := masking.Unmask(o.outputDir, o.maskMap); err != nil {
		o.status("Warning: %v", err)
	}
}

// checkOutputDir refuses an output directory Claude could use to reach the
// source document or the session state (which holds mask.json).
func (o *Orchestrator) checkOutputDir(docPath string) error {
	stateDir := filepath.Dir(o.stMgr.BaseDir)
	for _, p := range []string{docPath, stateDir} {
		if rel, err := filepath.Rel(o.outputDir, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("output directory %s must not contain %s: Claude can read every file in it", o.outputDir, p)
		}
	}
	return nil
}

func (o *Orchestrator) Run(docPath string) error {
	if err := o.checkOutputDir(docPath); err != nil {
		return err
	}
	// Claude's working directory: it must exist before the first call.
	if err := os.MkdirAll(o.outputDir, 0755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	// ── RESUME DECISION ─────────────────────────────────────────────────────
	var custom []string
	if !o.noMask {
		custom = o.CustomSensitive
	}
	decision, err := DecideResume(docPath, o.stMgr, custom...)
	if err != nil {
		return fmt.Errorf("resume decision: %w", err)
	}
	sess := decision.Session
	o.status("Mode: %s | Phase: %s", decision.Mode, sess.CurrentPhase)

	if o.noMask {
		o.status("Warning: --no-mask enabled — sensitive values will be sent to Claude")
	} else {
		if err := o.initMasking(sess, decision.PrevMaskMap); err != nil {
			return err
		}
		// Single de-mask point: runs on every exit path, after the last
		// Claude call. The next run re-masks before calling Claude again.
		defer o.unmaskOutput()
	}
	o.client.SetSanitizer(o.outgoing)
	o.client.SetContextBlob(decision.ContextBlob)

	// ── INGEST ───────────────────────────────────────────────────────────────
	if sess.CurrentPhase.AtOrBefore(state.PhaseIngest) || sess.DocMarkdown == "" {
		o.status("Converting document: %s", filepath.Base(docPath))
		conv, err := ingestion.Convert(docPath)
		if err != nil {
			return fmt.Errorf("ingestion: %w", err)
		}
		o.status("[%s] ~%d tokens", conv.SourceFormat, conv.EstimatedTokens)

		// Masked with the previous map before anything else touches it, so
		// placeholders match the ones computeMarkdownDiff produced.
		masked, err := o.maskText(conv.Markdown)
		if err != nil {
			return err
		}
		sess.DocMarkdown = masked
		if !o.noMask {
			o.status("Masked %d sensitive values", len(o.maskMap))
		}
		if err := o.stMgr.Advance(sess, state.PhaseAnalyze, state.Checkpoint{}); err != nil {
			return err
		}
	}

	// Claude can read outputDir from here on: mask whatever is on disk,
	// including files de-masked by a previous run or edited by the user.
	if !o.noMask {
		if err := o.maskOutput(); err != nil {
			return err
		}
	}

	// ── VALIDATE ONLY ────────────────────────────────────────────────────────
	if decision.Mode == ModeValidateOnly {
		o.status("Document unchanged. Re-running validation suite...")
		return o.runValidation(sess)
	}

	isDocChange := sess.PreviousDocHash != ""

	// ── ANALYZE ──────────────────────────────────────────────────────────────
	if sess.CurrentPhase.AtOrBefore(state.PhaseAnalyze) {
		o.status("Analyzing architecture...")
		analysisPrompt := BuildAnalysisPrompt(sess.DocMarkdown)
		if isDocChange {
			analysisPrompt = "The document changed since the last run (see DOCUMENT CHANGED). " +
				"Focus on what changed and which Terraform resources need modification.\n\n" + analysisPrompt
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
		if err := o.clarify(sess); err != nil {
			return err
		}
		if err := o.stMgr.Advance(sess, state.PhaseGenerate, state.Checkpoint{}); err != nil {
			return err
		}
	}

	// ── GENERATE ─────────────────────────────────────────────────────────────
	if sess.CurrentPhase.AtOrBefore(state.PhaseGenerate) {
		if err := o.generate(sess, isDocChange); err != nil {
			return err
		}
		if err := o.stMgr.Advance(sess, state.PhaseValidate, state.Checkpoint{}); err != nil {
			return err
		}
	}

	// ── VALIDATE ─────────────────────────────────────────────────────────────
	return o.runValidation(sess)
}

func (o *Orchestrator) clarify(sess *state.Session) error {
	analysis := ParseAnalysisJSON(sess.AnalysisJSON)

	for round := len(sess.Clarifications); round < maxClarifyRounds && !analysis.Ready; round++ {
		if analysis.ParseFailed {
			o.status("Warning: analysis response was not valid JSON — skipping clarification")
			return nil
		}
		questions := BuildQuestions(analysis.Missing, analysis.Inconsistencies)
		if len(questions) == 0 {
			return nil
		}

		o.status("Clarification round %d/%d (%d questions)", round+1, maxClarifyRounds, len(questions))

		if o.OnQuestion == nil {
			o.status("Warning: no question handler set — skipping clarification (answers required)")
			return nil
		}
		shown := make([]string, len(questions))
		for i, q := range questions {
			shown[i] = o.display(q)
		}
		answers := o.OnQuestion(shown)

		// Mask answers BEFORE persisting — users type real CIDRs, account
		// IDs and hostnames here.
		cr := state.ClarificationRound{
			Round:     round,
			Questions: questions,
			Answers:   make(map[int]string),
		}
		for i, a := range answers {
			masked, err := o.maskText(a)
			if err != nil {
				return err
			}
			answers[i] = masked
			cr.Answers[i] = masked
		}

		// Persist the round before sending to Claude (don't lose user answers on error)
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
	return nil
}

func (o *Orchestrator) generate(sess *state.Session, isDocChange bool) error {
	o.status("Generating Terraform IaC in %s...", o.outputDir)

	// Claude's working directory is outputDir: paths in prompts are relative.
	const checkpointNote = "After writing each module, output exactly: " + stepMarker + "<module_name>\n" +
		"This marker is used for progress checkpointing."
	lastStep := sess.LastCheckpoint.GenerateStep

	var prompt string
	switch {
	case isDocChange:
		prompt = "Adjust the existing Terraform files in the current directory to reflect the document changes.\n" +
			"List and read the existing files first. Only modify files that need to change. " +
			"Do not touch unaffected resources.\n" + checkpointNote
	case lastStep != "":
		prompt = fmt.Sprintf(
			"Generation was interrupted at step: %s\n"+
				"Continue generating the remaining modules in the current directory. "+
				"List the existing files first and do NOT regenerate them.\n%s",
			lastStep, checkpointNote,
		)
	default:
		prompt = fmt.Sprintf(
			"Generate complete, production-grade Terraform IaC based on the analyzed architecture.\n"+
				"Write all files to the current directory.\n\n"+
				"Architecture context:\n%s\n\n%s",
			sess.AnalysisJSON, checkpointNote,
		)
	}

	err := o.client.StreamQuery(prompt, func(chunk string) {
		if o.OnChunk != nil {
			o.OnChunk(o.display(chunk))
		}
		// Granular checkpoint per module
		for _, step := range extractSteps(chunk) {
			sess.LastCheckpoint = state.Checkpoint{
				Phase:        state.PhaseGenerate,
				GenerateStep: step,
				CompletedAt:  time.Now(),
			}
			_ = o.stMgr.Save(sess)
			o.status("  ✓ Module complete: %s", step)
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
	return nil
}

func (o *Orchestrator) runValidation(sess *state.Session) error {
	o.status("Running validation suite...")
	run, err := o.validate("")
	if err != nil {
		return err
	}
	sess.ValidationRuns = append(sess.ValidationRuns, run)

	if !run.Passed {
		o.status("Errors detected. Attempting auto-fix...")
		fixPrompt := fmt.Sprintf(
			"Validation errors:\n%s\n\nFix all errors in the Terraform files in the current directory. "+
				"Do not touch files without errors.",
			strings.Join(run.Errors, "\n"),
		)
		if _, err := o.client.Query(fixPrompt); err != nil {
			_ = o.stMgr.Save(sess)
			return fmt.Errorf("auto-fix (state saved): %w", err)
		}

		run, err = o.validate("(re-check) ")
		if err != nil {
			return err
		}
		sess.ValidationRuns = append(sess.ValidationRuns, run)
	}

	// ── DOCUMENT ─────────────────────────────────────────────────────────────
	if sess.CurrentPhase != state.PhaseDone {
		if err := o.stMgr.Advance(sess, state.PhaseDocument, state.Checkpoint{}); err != nil {
			return err
		}
		docs.Generate(o.client, sess, o.outputDir, func(msg string) { o.status("%s", msg) })
		if err := o.stMgr.Advance(sess, state.PhaseDone, state.Checkpoint{}); err != nil {
			return err
		}
	} else if err := o.stMgr.Save(sess); err != nil {
		return err
	}

	// The checks are best-effort: remaining issues are reported, not fixed
	// endlessly. The end user owns validating and testing the code.
	if !run.Passed {
		o.status("⚠ Validation issues remain after auto-fix:")
		for _, e := range run.Errors {
			o.status("    %s", e)
		}
	}
	o.status("✓ Session complete — output in %s", o.outputDir)
	o.status("⚠ Review, validate and test the generated code before use — you are responsible for what you deploy.")
	if !run.Passed {
		return fmt.Errorf("validation issues remain (%d) — review before use", len(run.Errors))
	}
	return nil
}

// extractSteps returns every module name announced in chunk.
func extractSteps(chunk string) []string {
	var steps []string
	for {
		idx := strings.Index(chunk, stepMarker)
		if idx < 0 {
			return steps
		}
		chunk = chunk[idx+len(stepMarker):]
		end := strings.IndexAny(chunk, "\n \t")
		if end < 0 {
			end = len(chunk)
		}
		if step := chunk[:end]; step != "" {
			steps = append(steps, step)
		}
		chunk = chunk[end:]
	}
}

// collectTFFiles returns the .tf files under dir, relative to dir.
func collectTFFiles(dir string) []string {
	paths, _ := fsutil.TFFiles(dir)
	files := make([]string, 0, len(paths))
	for _, p := range paths {
		if rel, err := filepath.Rel(dir, p); err == nil {
			files = append(files, rel)
		}
	}
	return files
}
