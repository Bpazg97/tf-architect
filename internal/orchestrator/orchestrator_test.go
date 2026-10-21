package orchestrator_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"tf-architect/internal/orchestrator"
	"tf-architect/internal/security"
	"tf-architect/internal/state"
)

// secrets appear in the document, the system prompt and a file the user left
// in the output directory. None may reach the provider.
var secrets = []string{"123456789012", "10.20.0.0/16", "10.20.30.40", "db.prod.acme.com", "acme-corp", "10.99.99.99", "Phoenix", "phoenix"}

const generatedTF = `terraform {
  required_version = ">= 1.5"
  required_providers {}
  backend "s3" {}
}
provider "aws" {
  default_tags {}
}
resource "aws_vpc" "main" {
  cidr_block = "CIDR_PRIVATE_001"
}
`

const docSections = `===BEGIN_ROOT_README===
# Project
VPC CIDR_PRIVATE_001
===END_ROOT_README===
===BEGIN_ARCHITECTURE===
Network: CIDR_PRIVATE_001
===END_ARCHITECTURE===
===BEGIN_RUNBOOK===
Runbook
===END_RUNBOOK===
===BEGIN_VARIABLES===
Variables
===END_VARIABLES===`

func fileBlock(path, content string) string {
	return "===FILE: " + path + "===\n" + content + "===END FILE===\n"
}

// fakeModel answers like a model following the file protocol, keyed on the
// task header of the prompt.
func fakeModel(system, prompt string) string {
	switch {
	case strings.Contains(prompt, "===BEGIN_ROOT_README==="):
		return docSections
	case strings.Contains(prompt, "## TASK: ANALYZE"), strings.Contains(prompt, "## TASK: CLARIFY"):
		return `{"ready": true, "missing": [], "inconsistencies": [], "summary": "ok"}`
	case strings.Contains(prompt, "## TASK: PLAN"):
		return `{"modules": [{"name": "vpc", "files": ["modules/vpc/main.tf"]}]}`
	case strings.Contains(prompt, "## TASK: MODULE vpc"):
		return fileBlock("modules/vpc/main.tf", "# vpc\n")
	case strings.Contains(prompt, "## TASK: REVIEW"):
		return "NO_CHANGES"
	case strings.Contains(prompt, "## TASK: MODULE root"):
		return "Here are the files.\n" + fileBlock("main.tf", generatedTF)
	case strings.Contains(prompt, "## TASK: FIX"):
		return fileBlock("main.tf", generatedTF+"# fixed\n")
	case strings.Contains(prompt, "## TASK: ADJUST"):
		return fileBlock("main.tf", generatedTF+"# adjusted\n")
	}
	return ""
}

type call struct{ system, prompt string }

// fakeProvider records every request and answers with respond (fakeModel by
// default).
type fakeProvider struct {
	mu      sync.Mutex
	calls   []call
	respond func(system, prompt string) string
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Complete(_ context.Context, system, prompt string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{system, prompt})
	if f.respond != nil {
		return f.respond(system, prompt), nil
	}
	return fakeModel(system, prompt), nil
}

func (f *fakeProvider) sent() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sb strings.Builder
	for _, c := range f.calls {
		sb.WriteString(c.system + "\n" + c.prompt + "\n")
	}
	return sb.String()
}

func (f *fakeProvider) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// failingPreflight fails its preflight check.
type failingPreflight struct{ fakeProvider }

func (f *failingPreflight) Preflight(context.Context) error {
	return security.New("preflight failed", "isolation unverified", "fix it")
}

// installTerraform puts a stub terraform first on PATH. It logs the .tf
// content it validates.
func installTerraform(t *testing.T, valid bool) (tfLog string) {
	t.Helper()
	bin := t.TempDir()
	tfLog = filepath.Join(bin, "terraform.log")
	validateOut := `echo '{"valid": true, "diagnostics": []}'`
	if !valid {
		validateOut = `echo '{"valid": false, "diagnostics": [{"severity": "error", "summary": "bad", "detail": "x"}]}'; exit 1`
	}
	terraform := `#!/bin/sh
cat *.tf >> ` + tfLog + ` 2>/dev/null
case "$1" in
  validate) ` + validateOut + ` ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(terraform), 0755); err != nil {
		t.Fatal(err)
	}
	// No tflint/checkov/terraform-docs: only the stub and coreutils.
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin:/bin")
	return tfLog
}

func assertNoSecrets(t *testing.T, what, content string, secrets []string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(content, s) {
			t.Errorf("%s leaked %q", what, s)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRunNeverSendsSensitiveData(t *testing.T) {
	installTerraform(t, true)
	workDir := t.TempDir()
	outputDir := filepath.Join(workDir, "doc-tf")
	docPath := filepath.Join(workDir, "doc.md")
	doc := "# LLD\n\nProject Phoenix (phoenix-core). Account 123456789012, VPC 10.20.0.0/16, DB host db.prod.acme.com at 10.20.30.40.\nOwner = \"acme-corp\"\n"
	if err := os.WriteFile(docPath, []byte(doc), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		t.Fatal(err)
	}
	userFile := `bastion_ip = "10.99.99.99"`
	if err := os.WriteFile(filepath.Join(outputDir, "user.tfvars"), []byte(userFile), 0644); err != nil {
		t.Fatal(err)
	}

	p := &fakeProvider{}
	run := func() {
		t.Helper()
		orch := orchestrator.New(workDir, outputDir, "Rules. Our account is 123456789012.", p, false)
		orch.CustomSensitive = []string{"Phoenix"}
		if err := orch.Run(docPath); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	run()
	sent := p.sent()
	if len(p.calls) == 0 {
		t.Fatal("provider never called")
	}
	assertNoSecrets(t, "first run", sent, secrets)
	if !strings.Contains(sent, "## MASKED VALUES") {
		t.Error("system prompt lacks the placeholder instructions")
	}

	// Files on disk hold real values; the user's file is untouched.
	if tf := readFile(t, filepath.Join(outputDir, "main.tf")); !strings.Contains(tf, "10.20.0.0/16") || strings.Contains(tf, "CIDR_PRIVATE_001") {
		t.Errorf("main.tf not de-masked:\n%s", tf)
	}
	if uf := readFile(t, filepath.Join(outputDir, "user.tfvars")); uf != userFile {
		t.Errorf("user file changed: %s", uf)
	}
	if md := readFile(t, filepath.Join(outputDir, "docs", "ARCHITECTURE.md")); !strings.Contains(md, "10.20.0.0/16") {
		t.Errorf("docs not de-masked:\n%s", md)
	}

	// Doc-changed run sends the real files on disk: still nothing leaks.
	p.reset()
	if err := os.WriteFile(docPath, []byte(doc+"\nNew cache at 10.20.30.41.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run()
	sent = p.sent()
	if !strings.Contains(sent, "## TASK: ADJUST") {
		t.Fatal("doc-changed run did not adjust the project")
	}
	assertNoSecrets(t, "doc-changed run", sent, append(secrets, "10.20.30.41"))
	tf := readFile(t, filepath.Join(outputDir, "main.tf"))
	if !strings.Contains(tf, "10.20.0.0/16") || !strings.Contains(tf, "# adjusted") {
		t.Errorf("main.tf not adjusted with real values:\n%s", tf)
	}
}

func TestTaskPromptsStartWithHeader(t *testing.T) {
	installTerraform(t, false)
	workDir, docPath, outputDir := setupDoc(t)
	p := &fakeProvider{}
	_ = orchestrator.New(workDir, outputDir, "", p, false).Run(docPath)
	for _, c := range p.calls {
		if !strings.HasPrefix(c.prompt, "## TASK: ") {
			t.Errorf("prompt does not start with a task header:\n%.200s", c.prompt)
		}
	}
	if len(p.calls) < 4 {
		t.Errorf("expected analyze, generate, fix and docs calls, got %d", len(p.calls))
	}
}

func TestUnknownPlaceholderWarns(t *testing.T) {
	installTerraform(t, true)
	workDir, docPath, outputDir := setupDoc(t)
	p := &fakeProvider{respond: func(system, prompt string) string {
		if strings.Contains(prompt, "## TASK: MODULE root") {
			return fileBlock("network.tf", `cidr = "CIDR_PRIVATE_999"`+"\n")
		}
		return fakeModel(system, prompt)
	}}
	orch := orchestrator.New(workDir, outputDir, "", p, false)
	var lines []string
	orch.OnStatus = func(msg string) { lines = append(lines, msg) }
	_ = orch.Run(docPath)
	for _, l := range lines {
		if strings.Contains(l, "network.tf") && strings.Contains(l, "CIDR_PRIVATE_999") {
			return
		}
	}
	t.Errorf("no warning naming network.tf and CIDR_PRIVATE_999 in:\n%s", strings.Join(lines, "\n"))
}

func TestPreflightRunsBeforeFirstQuery(t *testing.T) {
	installTerraform(t, true)
	workDir, docPath, outputDir := setupDoc(t)
	p := &failingPreflight{}
	err := orchestrator.New(workDir, outputDir, "", p, false).Run(docPath)
	var se *security.Error
	if !errors.As(err, &se) {
		t.Fatalf("expected *security.Error, got %v", err)
	}
	if len(p.calls) != 0 {
		t.Errorf("Complete called %d times after a failed preflight", len(p.calls))
	}
}

func TestValidatorsSeeRealValues(t *testing.T) {
	tfLog := installTerraform(t, true)
	workDir, docPath, outputDir := setupDoc(t)
	if err := orchestrator.New(workDir, outputDir, "", &fakeProvider{}, false).Run(docPath); err != nil {
		t.Fatalf("Run: %v", err)
	}
	validated := readFile(t, tfLog)
	if !strings.Contains(validated, "10.20.0.0/16") || strings.Contains(validated, "CIDR_PRIVATE_001") {
		t.Errorf("validators must see real values, saw:\n%s", validated)
	}
}

func TestRunRefusesOutputDirContainingState(t *testing.T) {
	workDir := t.TempDir()
	docPath := filepath.Join(workDir, "doc.md")
	_ = os.WriteFile(docPath, []byte("# doc"), 0644)
	p := &fakeProvider{}
	err := orchestrator.New(workDir, workDir, "", p, false).Run(docPath)
	var se *security.Error
	if !errors.As(err, &se) || !strings.Contains(err.Error(), "must not contain") {
		t.Errorf("expected security refusal, got %v", err)
	}
	if len(p.calls) != 0 {
		t.Errorf("provider called %d times", len(p.calls))
	}
}

// setupDoc creates a workDir with a document; returns docPath and outputDir.
func setupDoc(t *testing.T) (workDir, docPath, outputDir string) {
	t.Helper()
	workDir = t.TempDir()
	docPath = filepath.Join(workDir, "doc.md")
	if err := os.WriteFile(docPath, []byte("# LLD\n\nVPC 10.20.0.0/16\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return workDir, docPath, filepath.Join(workDir, "doc-tf")
}

func TestRunValidationIssuesDoNotBlockDocs(t *testing.T) {
	installTerraform(t, false)
	workDir, docPath, outputDir := setupDoc(t)
	p := &fakeProvider{}

	err := orchestrator.New(workDir, outputDir, "", p, false).Run(docPath)
	if err == nil || !strings.Contains(err.Error(), "validation issues remain") {
		t.Fatalf("expected validation issues error, got %v", err)
	}
	sent := p.sent()
	if !strings.Contains(sent, "## TASK: FIX") || !strings.Contains(sent, "Validation errors:") {
		t.Error("auto-fix was not attempted")
	}
	if !strings.Contains(sent, "===BEGIN_ROOT_README===") {
		t.Error("documentation was not generated despite remaining issues")
	}
	sess, _ := state.NewManager(workDir).FindByDocPath(docPath)
	if sess == nil || sess.CurrentPhase != state.PhaseDone {
		t.Errorf("session should be done, got %+v", sess)
	}
	tf := readFile(t, filepath.Join(outputDir, "main.tf"))
	if !strings.Contains(tf, "# fixed") || strings.Contains(tf, "CIDR_PRIVATE_001") {
		t.Errorf("fix not applied with real values:\n%s", tf)
	}
}

func TestRunInterruptedSendsNothing(t *testing.T) {
	installTerraform(t, true)
	workDir, docPath, outputDir := setupDoc(t)
	p := &fakeProvider{}

	orch := orchestrator.New(workDir, outputDir, "", p, false)
	orch.Interrupt()
	if err := orch.Run(docPath); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("expected interrupt error, got %v", err)
	}
	if len(p.calls) > 0 {
		t.Errorf("provider called after interrupt:\n%s", p.sent())
	}
}

func TestNoMaskStillSends(t *testing.T) {
	installTerraform(t, true)
	workDir, docPath, outputDir := setupDoc(t)
	p := &fakeProvider{}
	if err := orchestrator.New(workDir, outputDir, "", p, true).Run(docPath); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.calls) == 0 {
		t.Error("noMask run must still query the provider through the gate")
	}
}

func isDocsPrompt(prompt string) bool {
	return strings.Contains(prompt, "===BEGIN_ROOT_README===")
}

func TestInterruptDuringDocsKeepsSessionResumable(t *testing.T) {
	installTerraform(t, true)
	workDir, docPath, outputDir := setupDoc(t)
	var orch *orchestrator.Orchestrator
	p := &fakeProvider{respond: func(system, prompt string) string {
		if isDocsPrompt(prompt) {
			// The in-flight request dies with a provider error, as a killed
			// subprocess would.
			orch.Interrupt()
			return ""
		}
		return fakeModel(system, prompt)
	}}
	orch = orchestrator.New(workDir, outputDir, "", p, false)
	if err := orch.Run(docPath); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("expected interrupt error, got %v", err)
	}
	sess, _ := state.NewManager(workDir).FindByDocPath(docPath)
	if sess == nil || sess.CurrentPhase == state.PhaseDone {
		t.Errorf("session must not be done after an interrupted docs phase, got %+v", sess)
	}
}

func TestMaskedPathIsWrittenWithRealValue(t *testing.T) {
	installTerraform(t, false) // forces a FIX call with the rendered project
	workDir, docPath, outputDir := setupDoc(t)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "phoenix-net.tf"), []byte("# net\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var fixPrompt string
	p := &fakeProvider{respond: func(system, prompt string) string {
		if strings.Contains(prompt, "## TASK: FIX") {
			fixPrompt = prompt
			// Echo the path exactly as the model saw it.
			for _, line := range strings.Split(prompt, "\n") {
				if strings.HasPrefix(line, "===FILE: ") && strings.HasSuffix(line, "-net.tf===") {
					path := strings.TrimSuffix(strings.TrimPrefix(line, "===FILE: "), "===")
					return fileBlock(path, "# fixed\n")
				}
			}
		}
		return fakeModel(system, prompt)
	}}
	orch := orchestrator.New(workDir, outputDir, "", p, false)
	orch.CustomSensitive = []string{"Phoenix"}
	_ = orch.Run(docPath)
	if fixPrompt == "" || strings.Contains(strings.ToLower(fixPrompt), "phoenix") {
		t.Fatalf("FIX prompt missing or path not masked:\n%.300s", fixPrompt)
	}
	if got := readFile(t, filepath.Join(outputDir, "phoenix-net.tf")); got != "# fixed\n" {
		t.Errorf("fix not written to the real path, got %q", got)
	}
	entries, _ := os.ReadDir(outputDir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "CUSTOM_") {
			t.Errorf("file written under a placeholder path: %s", e.Name())
		}
	}
}

func TestGateFailureDuringDocsStopsRun(t *testing.T) {
	installTerraform(t, true)
	workDir, docPath, outputDir := setupDoc(t)
	hash, err := state.HashDoc(docPath)
	if err != nil {
		t.Fatal(err)
	}
	maskPath := state.NewManager(workDir).MaskMapPath(hash)
	p := &fakeProvider{respond: func(system, prompt string) string {
		if strings.Contains(prompt, "## TASK: MODULE root") {
			// A new case variant of the custom value reaches the docs prompt
			// through the file list, so the gate must save the mask map;
			// a directory in its place makes that fail.
			if err := os.Remove(maskPath); err != nil {
				t.Error(err)
			}
			if err := os.Mkdir(maskPath, 0700); err != nil {
				t.Error(err)
			}
			return fileBlock("main.tf", generatedTF) + fileBlock("phoenix-db.tf", "# db\n")
		}
		if isDocsPrompt(prompt) {
			t.Error("docs prompt sent despite a failed gate")
		}
		return fakeModel(system, prompt)
	}}
	orch := orchestrator.New(workDir, outputDir, "", p, false)
	orch.CustomSensitive = []string{"Phoenix"}
	err = orch.Run(docPath)
	var se *security.Error
	if !errors.As(err, &se) {
		t.Fatalf("expected *security.Error, got %v", err)
	}
}

// headers returns the task header of every call, e.g. "MODULE vpc".
func (f *fakeProvider) headers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		line, _, _ := strings.Cut(c.prompt, "\n")
		out = append(out, strings.TrimPrefix(line, "## TASK: "))
	}
	return out
}

const twoModulePlan = `{"modules": [{"name": "vpc", "files": ["modules/vpc/main.tf"]}, {"name": "rds", "files": ["modules/rds/main.tf"]}]}`

func planModel(system, prompt string) string {
	switch {
	case strings.Contains(prompt, "## TASK: PLAN"):
		return twoModulePlan
	case strings.Contains(prompt, "## TASK: MODULE vpc"):
		return fileBlock("modules/vpc/main.tf", "# vpc 10.20.0.0/16\n")
	case strings.Contains(prompt, "## TASK: MODULE rds"):
		return fileBlock("modules/rds/main.tf", "# rds\n")
	}
	return fakeModel(system, prompt)
}

func generateHeaders(p *fakeProvider) []string {
	var out []string
	for _, h := range p.headers() {
		if h == "PLAN" || h == "REVIEW" || strings.HasPrefix(h, "MODULE ") {
			out = append(out, h)
		}
	}
	return out
}

func equal(a, b []string) bool { return strings.Join(a, "|") == strings.Join(b, "|") }

func TestGenerateCallsPerModuleThenReview(t *testing.T) {
	installTerraform(t, true)
	workDir, docPath, outputDir := setupDoc(t)
	var rdsPrompt string
	p := &fakeProvider{respond: func(system, prompt string) string {
		if strings.Contains(prompt, "## TASK: MODULE rds") {
			rdsPrompt = prompt
		}
		return planModel(system, prompt)
	}}
	if err := orchestrator.New(workDir, outputDir, "", p, false).Run(docPath); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"PLAN", "MODULE vpc", "MODULE rds", "MODULE root", "REVIEW"}
	if got := generateHeaders(p); !equal(got, want) {
		t.Errorf("headers = %v, want %v", got, want)
	}
	if !strings.Contains(rdsPrompt, "modules/vpc/main.tf") || strings.Contains(rdsPrompt, "10.20.0.0/16") || !strings.Contains(rdsPrompt, "CIDR_PRIVATE_001") {
		t.Errorf("MODULE rds prompt must carry the masked files already written:\n%s", rdsPrompt)
	}
	sess, _ := state.NewManager(workDir).FindByDocPath(docPath)
	if sess == nil || !strings.Contains(sess.GenerationPlan, `"rds"`) {
		t.Errorf("plan not persisted: %+v", sess)
	}
}

// resumeAt runs a session whose generate phase stopped after step.
func resumeAt(t *testing.T, step string) *fakeProvider {
	t.Helper()
	installTerraform(t, true)
	workDir, docPath, outputDir := setupDoc(t)
	hash, err := state.HashDoc(docPath)
	if err != nil {
		t.Fatal(err)
	}
	mgr := state.NewManager(workDir)
	sess := &state.Session{
		DocPath: docPath, DocHash: hash, CurrentPhase: state.PhaseGenerate,
		DocMarkdown: "# LLD", AnalysisJSON: `{"ready": true}`,
		GenerationPlan: twoModulePlan,
		LastCheckpoint: state.Checkpoint{Phase: state.PhaseGenerate, GenerateStep: step},
	}
	if err := mgr.Save(sess); err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{respond: planModel}
	if err := orchestrator.New(workDir, outputDir, "", p, false).Run(docPath); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return p
}

func TestResumeSkipsCompletedModules(t *testing.T) {
	p := resumeAt(t, "rds")
	if got, want := generateHeaders(p), []string{"MODULE root", "REVIEW"}; !equal(got, want) {
		t.Errorf("headers = %v, want %v", got, want)
	}
}

func TestResumeSkipsDoneReview(t *testing.T) {
	p := resumeAt(t, "review")
	if got := generateHeaders(p); len(got) != 0 {
		t.Errorf("generation headers after a done review: %v", got)
	}
}

func TestTruncatedTwiceStops(t *testing.T) {
	installTerraform(t, true)
	workDir, docPath, outputDir := setupDoc(t)
	p := &fakeProvider{respond: func(system, prompt string) string {
		if strings.Contains(prompt, "## TASK: MODULE vpc") {
			return "===FILE: modules/vpc/main.tf===\nresource \"x\" \"y\" {\n"
		}
		return fakeModel(system, prompt)
	}}
	err := orchestrator.New(workDir, outputDir, "", p, false).Run(docPath)
	var se *security.Error
	if !errors.As(err, &se) {
		t.Fatalf("expected *security.Error, got %v", err)
	}
	n := 0
	for _, h := range p.headers() {
		if h == "MODULE vpc" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("MODULE vpc called %d times, want 2", n)
	}
}

func TestReviewNoChanges(t *testing.T) {
	installTerraform(t, true)
	workDir, docPath, outputDir := setupDoc(t)
	p := &fakeProvider{}
	if err := orchestrator.New(workDir, outputDir, "", p, false).Run(docPath); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := readFile(t, filepath.Join(outputDir, "main.tf")); !strings.Contains(got, "10.20.0.0/16") || strings.Contains(got, "# fixed") {
		t.Errorf("main.tf changed by a NO_CHANGES review:\n%s", got)
	}
}
