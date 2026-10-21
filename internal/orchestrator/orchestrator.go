package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"tf-architect/internal/docs"
	"tf-architect/internal/fileproto"
	"tf-architect/internal/fsutil"
	"tf-architect/internal/ingestion"
	"tf-architect/internal/llm"
	"tf-architect/internal/masking"
	"tf-architect/internal/security"
	"tf-architect/internal/state"
	"tf-architect/internal/validation"
)

const maxClarifyRounds = 3

// Task headers: every task prompt starts with one of them.
const (
	taskAnalyze = "## TASK: ANALYZE"
	taskClarify = "## TASK: CLARIFY"
	taskPlan    = "## TASK: PLAN"
	taskModule  = "## TASK: MODULE "
	taskReview  = "## TASK: REVIEW"
	taskAdjust  = "## TASK: ADJUST"
	taskFix     = "## TASK: FIX"
	taskDocs    = "## TASK: DOCS"
)

// reviewStep is the generate checkpoint recorded once the review is applied.
// Module checkpoints are the module names.
const reviewStep = "review"

// Orchestrator runs the tf-architect FSM.
//
// Security invariant: when masking is enabled, nothing sensitive reaches the
// AI provider.
//   - The model has no tools: it only sees the text tf-architect puts in a
//     prompt, and returns files in the fileproto text protocol.
//   - Every prompt and the system prompt pass through outgoing inside the
//     llm.Client (single choke point, fail closed), which masks them.
//   - outputDir always holds real values: project files are masked when they
//     are placed in a prompt, and model output is de-masked when written.
type Orchestrator struct {
	client    *llm.Client
	provider  llm.Provider
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
	ctx         context.Context
	cancel      context.CancelFunc

	// Callbacks wired by the CLI/TUI layer
	OnQuestion func(questions []string) []string
	OnStatus   func(msg string)
}

// maskingNote tells the model how to treat placeholders, whatever system
// prompt the user supplied.
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

// New returns an Orchestrator that writes the project to outputDir and talks
// to p with agents (AGENTS.md) as the system prompt.
func New(workDir, outputDir, agents string, p llm.Provider, noMask bool) *Orchestrator {
	ctx, cancel := context.WithCancel(context.Background())
	o := &Orchestrator{
		client:    llm.NewClient(p, withMaskingNote(agents, noMask)),
		provider:  p,
		stMgr:     state.NewManager(workDir),
		workDir:   workDir,
		outputDir: outputDir,
		noMask:    noMask,
		ctx:       ctx,
		cancel:    cancel,
	}
	// Set even with noMask: the client refuses to send without a gate.
	o.client.SetGate(o.outgoing)
	return o
}

// status reports progress to the user. Placeholders are resolved for display
// only; this text never goes to the provider.
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

// Interrupt stops the run: the in-flight request is cancelled, nothing more
// is sent and Run returns. Safe from any goroutine.
func (o *Orchestrator) Interrupt() {
	o.interrupted.Store(true)
	o.cancel()
}

var errInterrupted = errors.New("interrupted by user")

// outgoing is the client's gate for every prompt: refuse after an interrupt,
// otherwise mask.
func (o *Orchestrator) outgoing(text string) (string, error) {
	if o.interrupted.Load() {
		return "", errInterrupted
	}
	return o.maskText(text)
}

// maskingError is the fail-closed stop for any masking or mask map failure.
func maskingError(action string, err error) error {
	return security.New(
		fmt.Sprintf("Masking failed while %s: %v.", action, err),
		"Without a working mask, real infrastructure values could be sent to the AI provider.",
		"Check that the session directory (.tf-architect) is readable and writable, then re-run the command.",
	)
}

// maskText masks sensitive values in text using the session mask map,
// persisting any newly discovered values before the masked text is used.
func (o *Orchestrator) maskText(text string) (string, error) {
	if o.noMask || text == "" {
		return text, nil
	}
	if o.maskMapPath == "" {
		return "", maskingError("masking text", errors.New("mask map not initialised"))
	}
	masked, mm, err := masking.Mask(text, o.maskMap)
	if err != nil {
		return "", maskingError("masking text", err)
	}
	if err := o.saveMaskMap(mm); err != nil {
		return "", err
	}
	return masked, nil
}

func (o *Orchestrator) saveMaskMap(mm masking.MaskMap) error {
	if len(mm) != len(o.maskMap) {
		if err := masking.SaveMaskMap(o.maskMapPath, mm); err != nil {
			return maskingError("saving the mask map", err)
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
			return maskingError("loading the mask map", err)
		}
		mm = loaded
	} else {
		sess.MaskMapPath = o.stMgr.MaskMapPath(sess.DocHash)
	}
	mm = masking.AddCustom(mm, o.CustomSensitive)
	// Written even when empty: a recorded MaskMapPath must always load.
	if err := masking.SaveMaskMap(sess.MaskMapPath, mm); err != nil {
		return maskingError("saving the mask map", err)
	}
	o.maskMap, o.maskMapPath = mm, sess.MaskMapPath
	return nil
}

// checkOutputDir refuses an output directory holding the source document or
// the session state (which holds mask.json): project files are sent to the
// model, so neither may be among them.
func (o *Orchestrator) checkOutputDir(docPath string) error {
	stateDir := filepath.Dir(o.stMgr.BaseDir)
	for _, p := range []string{docPath, stateDir} {
		if rel, err := filepath.Rel(o.outputDir, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return security.New(
				fmt.Sprintf("Output directory %s must not contain %s.", o.outputDir, p),
				"Project files are sent to the AI provider, so the source document or the mask map could be read from it.",
				"Choose an output directory (--output) outside the document's location and the .tf-architect state directory.",
			)
		}
	}
	return nil
}

// preflight lets the provider verify its isolation before any data is sent.
func (o *Orchestrator) preflight() error {
	if o.interrupted.Load() {
		return errInterrupted
	}
	pf, ok := o.provider.(llm.Preflighter)
	if !ok {
		return nil
	}
	if err := pf.Preflight(o.ctx); err != nil {
		return fmt.Errorf("%s preflight: %w", o.provider.Name(), err)
	}
	return nil
}

// ask sends one stateless task: its header, the current session context
// (unless withContext is false) and body.
func (o *Orchestrator) ask(sess *state.Session, header, body string, withContext bool) (string, error) {
	var sb strings.Builder
	sb.WriteString(header + "\n\n")
	if withContext {
		sb.WriteString(buildContextBlob(sess) + "\n")
	}
	sb.WriteString(body)
	return o.client.Query(o.ctx, sb.String())
}

// completeFiles sends a task whose answer is files in the protocol and
// returns them. A truncated answer is retried once; a second truncation
// stops the run.
func (o *Orchestrator) completeFiles(sess *state.Session, header, body string) ([]fileproto.Change, error) {
	for attempt := 0; ; attempt++ {
		resp, err := o.ask(sess, header, body, true)
		if err != nil {
			return nil, err
		}
		changes, warnings, err := fileproto.Parse(resp)
		if errors.Is(err, fileproto.ErrTruncated) {
			if attempt == 0 {
				o.status("Warning: the AI response was cut off — retrying once")
				continue
			}
			return nil, security.New(
				"the AI response was cut off twice",
				"The model hit its output limit, so its files are incomplete and were not written.",
				"Use a model with a larger output limit or simplify the document.",
			)
		}
		for _, w := range warnings {
			o.status("Warning: %s", w)
		}
		if err != nil {
			return nil, err
		}
		return changes, nil
	}
}

// askFiles runs completeFiles and writes the result.
func (o *Orchestrator) askFiles(sess *state.Session, header, body string) error {
	changes, err := o.completeFiles(sess, header, body)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		o.status("Warning: the model returned no files")
	}
	return o.applyChanges(changes)
}

// applyChanges writes the model's files to outputDir with real values,
// warning about placeholders the mask map cannot resolve.
func (o *Orchestrator) applyChanges(changes []fileproto.Change) error {
	// Paths were masked in the rendered project too; Apply validates the
	// de-masked ones.
	unmasked := make([]fileproto.Change, len(changes))
	for i, c := range changes {
		c.Path = masking.UnmaskText(c.Path, o.maskMap)
		unmasked[i] = c
	}
	changes = unmasked
	if !o.noMask {
		for _, c := range changes {
			if u := masking.UnresolvedPlaceholders(c.Path+"\n"+c.Content, o.maskMap); len(u) > 0 {
				o.status("Warning: %s contains unknown placeholders, written as-is: %s", c.Path, strings.Join(u, ", "))
			}
		}
	}
	if err := fileproto.Apply(o.outputDir, changes, func(s string) string {
		return masking.UnmaskText(s, o.maskMap)
	}); err != nil {
		return err
	}
	for _, c := range changes {
		verb := "wrote"
		if c.Delete {
			verb = "deleted"
		}
		o.status("  ✓ %s %s", verb, c.Path)
	}
	return nil
}

// renderProject returns the project files in the protocol, for edit tasks.
func (o *Orchestrator) renderProject() (string, error) {
	files, err := fileproto.ReadProject(o.outputDir, fileproto.DefaultBudget)
	if err != nil {
		return "", err
	}
	return fileproto.Render(files), nil
}

func (o *Orchestrator) Run(docPath string) error {
	if err := o.checkOutputDir(docPath); err != nil {
		return err
	}
	// Validators run here even if the model returns no files.
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
		o.status("Warning: --no-mask enabled — sensitive values will be sent to the AI provider")
	} else if err := o.initMasking(sess, decision.PrevMaskMap); err != nil {
		return err
	}
	if err := o.preflight(); err != nil {
		return err
	}

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

	// ── VALIDATE ONLY ────────────────────────────────────────────────────────
	if decision.Mode == ModeValidateOnly {
		o.status("Document unchanged. Re-running validation suite...")
		return o.runValidation(sess)
	}

	isDocChange := sess.PreviousDocHash != ""

	// ── ANALYZE ──────────────────────────────────────────────────────────────
	if sess.CurrentPhase.AtOrBefore(state.PhaseAnalyze) {
		o.status("Analyzing architecture...")
		body := BuildAnalysisPrompt(sess.DocMarkdown)
		if isDocChange {
			body = "The document changed since the last run (see DOCUMENT CHANGED). " +
				"Focus on what changed and which Terraform resources need modification.\n\n" + body
		}
		// A brand-new session has no context worth sending yet.
		analysisRaw, err := o.ask(sess, taskAnalyze, body, decision.Mode != ModeNew)
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

		// Persist the round before sending (don't lose user answers on error)
		sess.Clarifications = append(sess.Clarifications, cr)
		if err := o.stMgr.Save(sess); err != nil {
			return err
		}

		body := BuildClarifyPrompt(sess.AnalysisJSON, questions, answers) +
			"\n\nArchitecture document:\n---\n" + sess.DocMarkdown
		reanalysisRaw, err := o.ask(sess, taskClarify, body, true)
		if err != nil {
			return fmt.Errorf("clarify round %d (state saved): %w", round, err)
		}
		sess.AnalysisJSON = reanalysisRaw
		analysis = ParseAnalysisJSON(reanalysisRaw)
	}
	return nil
}

func (o *Orchestrator) generate(sess *state.Session, isDocChange bool) error {
	step := sess.LastCheckpoint.GenerateStep
	switch {
	case step == reviewStep:
		o.status("Project already generated and reviewed — skipping generation")
	case isDocChange:
		o.status("Adjusting Terraform IaC in %s...", o.outputDir)
		project, err := o.renderProject()
		if err != nil {
			return err
		}
		body := "Adjust the existing Terraform project to reflect the document changes (see DOCUMENT CHANGED).\n" +
			"Return only the files that must change, in full. Do not touch unaffected resources.\n\n" +
			"Current project files:\n" + project
		if err := o.askFiles(sess, taskAdjust, body); err != nil {
			_ = o.stMgr.Save(sess)
			return fmt.Errorf("adjust (state saved): %w", err)
		}
		o.checkpoint(sess, reviewStep)
	default:
		if err := o.generateStepwise(sess); err != nil {
			_ = o.stMgr.Save(sess)
			return fmt.Errorf("generate (state saved): %w", err)
		}
	}

	sess.GeneratedFiles = collectTFFiles(o.outputDir)
	if len(sess.GeneratedFiles) == 0 {
		o.status("Warning: no .tf files found in %s — generation may have failed silently", o.outputDir)
	}
	return nil
}

func (o *Orchestrator) checkpoint(sess *state.Session, step string) {
	sess.LastCheckpoint = state.Checkpoint{Phase: state.PhaseGenerate, GenerateStep: step, CompletedAt: time.Now()}
	_ = o.stMgr.Save(sess)
}

// generateStepwise plans the modules, generates one per call (root last) and
// reviews the whole project, checkpointing after each module and the review.
func (o *Orchestrator) generateStepwise(sess *state.Session) error {
	done := sess.LastCheckpoint.GenerateStep
	var plan GenerationPlan
	if sess.GenerationPlan != "" {
		p, err := ParseGenerationPlan(sess.GenerationPlan)
		if err != nil {
			return err
		}
		plan = p
	} else {
		// No plan (new run, or a legacy checkpoint): plan and start over.
		done = ""
		o.status("Planning modules...")
		raw, err := o.ask(sess, taskPlan, BuildPlanPrompt(sess.DocMarkdown), true)
		if err != nil {
			return err
		}
		if plan, err = ParseGenerationPlan(raw); err != nil {
			return err
		}
		valid, err := json.Marshal(plan)
		if err != nil {
			return err
		}
		sess.GenerationPlan = string(valid)
		if err := o.stMgr.Save(sess); err != nil {
			return err
		}
	}

	start := 0
	for i, m := range plan.Modules {
		if m.Name == done {
			start = i + 1
		}
	}
	for _, m := range plan.Modules[start:] {
		o.status("Generating module %s...", m.Name)
		project, err := o.renderProject()
		if err != nil {
			return err
		}
		body := BuildModulePrompt(plan, m, project, sess.DocMarkdown)
		if err := o.askFiles(sess, taskModule+m.Name, body); err != nil {
			return fmt.Errorf("module %s: %w", m.Name, err)
		}
		o.checkpoint(sess, m.Name)
	}

	o.status("Reviewing the whole project...")
	project, err := o.renderProject()
	if err != nil {
		return err
	}
	changes, err := o.completeFiles(sess, taskReview, BuildReviewPrompt(project))
	if err != nil {
		return fmt.Errorf("review: %w", err)
	}
	if len(changes) == 0 {
		o.status("Review found nothing to change")
	} else if err := o.applyChanges(changes); err != nil {
		return fmt.Errorf("review: %w", err)
	}
	o.checkpoint(sess, reviewStep)
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
		project, err := o.renderProject()
		if err != nil {
			return err
		}
		body := fmt.Sprintf(
			"Validation errors:\n%s\n\nFix all errors. Return only the files you change, in full. "+
				"Do not touch files without errors.\n\nCurrent project files:\n%s",
			strings.Join(run.Errors, "\n"), project,
		)
		if err := o.askFiles(sess, taskFix, body); err != nil {
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
		dc := &docsClient{o: o, sess: sess}
		docs.Generate(dc, sess, o.outputDir, func(msg string) { o.status("%s", msg) })
		// Docs are best-effort, but a security stop or an interrupt is not.
		// An interrupt can surface as a provider or context error, so the
		// flag is checked too; the session stays resumable at the docs phase.
		var se *security.Error
		if errors.As(dc.err, &se) {
			return dc.err
		}
		if o.interrupted.Load() || errors.Is(dc.err, context.Canceled) {
			_ = o.stMgr.Save(sess)
			return fmt.Errorf("documentation (state saved): %w", errInterrupted)
		}
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

// docsClient adapts the orchestrator to docs.Client: queries go through the
// gate with the session context, and answers come back with real values
// because the docs package writes them to disk.
type docsClient struct {
	o    *Orchestrator
	sess *state.Session
	err  error // last query error, which docs.Generate only reports
}

func (d *docsClient) Query(prompt string) (string, error) {
	resp, err := d.o.ask(d.sess, taskDocs, prompt, true)
	if err != nil {
		d.err = err
		return "", err
	}
	return masking.UnmaskText(resp, d.o.maskMap), nil
}

// maxStageOutput bounds raw validator output forwarded to auto-fix.
const maxStageOutput = 2000

// validate runs the suite on outputDir (real values) and returns a
// ValidationRun whose errors are masked before they are stored or sent.
func (o *Orchestrator) validate(label string) (state.ValidationRun, error) {
	results, suiteErr := validation.New(o.outputDir).Run()

	run := state.ValidationRun{
		RunAt:   time.Now(),
		Passed:  suiteErr == nil,
		Results: make(map[string]bool),
	}
	for _, r := range results {
		run.Results[r.Stage] = r.Passed
		icon := "✓"
		switch {
		case r.Skipped:
			icon = "–"
		case !r.Passed:
			icon = "✗"
		}
		o.status("  %s%s %s", label, icon, r.Stage)

		errs := r.Errors
		if !r.Passed && len(errs) == 0 && r.Output != "" {
			// Stages without structured errors (tflint, checkov, init).
			errs = []string{r.Stage + ": " + truncate(strings.TrimSpace(r.Output), maxStageOutput)}
		}
		for _, e := range errs {
			// The model knows paths relative to the project root only.
			e = strings.ReplaceAll(e, o.outputDir+string(filepath.Separator), "")
			e = strings.ReplaceAll(e, o.outputDir, ".")
			masked, err := o.maskText(e)
			if err != nil {
				return state.ValidationRun{}, err
			}
			run.Errors = append(run.Errors, masked)
		}
	}
	return run, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "... (truncated)"
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
