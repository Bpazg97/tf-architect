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
	_ = orchestrator.New(workDir, outputDir, "", p, true).Run(docPath)
	if len(p.calls) == 0 {
		t.Error("noMask run must still query the provider through the gate")
	}
}
