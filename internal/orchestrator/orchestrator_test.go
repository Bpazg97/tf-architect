package orchestrator_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tf-architect/internal/orchestrator"
	"tf-architect/internal/state"
)

// secrets appear in the document, the system prompt, a user answer-free run
// and a file the user left in the output directory. None may reach Claude.
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

// installFakes puts stub claude/terraform binaries first on PATH.
// claude logs its arguments and every file it could read in its working
// directory; terraform logs the .tf content it validates.
func installFakes(t *testing.T, valid bool) (claudeLog, tfLog string) {
	t.Helper()
	bin := t.TempDir()
	claudeLog = filepath.Join(bin, "claude.log")
	tfLog = filepath.Join(bin, "terraform.log")
	tfFile := filepath.Join(bin, "generated.tf")
	if err := os.WriteFile(tfFile, []byte(generatedTF), 0644); err != nil {
		t.Fatal(err)
	}

	claude := `#!/bin/sh
printf '%s\n' "$@" >> ` + claudeLog + `
find . -type f -exec cat {} + >> ` + claudeLog + ` 2>/dev/null
case "$*" in
  *"Generate complete"*) cp ` + tfFile + ` main.tf ;;
esac
echo '{"type":"system","session_id":"s1"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"{\"ready\": true, \"missing\": [], \"inconsistencies\": [], \"summary\": \"ok\"}\nSTEP_COMPLETE:vpc"}]}}'
echo '{"type":"result","is_error":false}'
`
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
	for name, body := range map[string]string{"claude": claude, "terraform": terraform} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	// No tflint/checkov/terraform-docs: only the stubs and coreutils.
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin:/bin")
	return claudeLog, tfLog
}

func assertNoSecrets(t *testing.T, what, content string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(content, s) {
			t.Errorf("%s leaked %q", what, s)
		}
	}
}

func TestRunNeverSendsSensitiveDataToClaude(t *testing.T) {
	claudeLog, tfLog := installFakes(t, true)
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

	run := func() {
		t.Helper()
		orch := orchestrator.New(workDir, outputDir, "Rules. Our account is 123456789012.", false)
		orch.CustomSensitive = []string{"Phoenix"}
		if err := orch.Run(docPath); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	run()
	sent, _ := os.ReadFile(claudeLog)
	if len(sent) == 0 {
		t.Fatal("fake claude never called")
	}
	assertNoSecrets(t, "first run", string(sent))
	if !strings.Contains(string(sent), "## MASKED VALUES") {
		t.Error("system prompt lacks the placeholder instructions")
	}

	// Output is delivered with real values; validators saw real values.
	tf, _ := os.ReadFile(filepath.Join(outputDir, "main.tf"))
	if !strings.Contains(string(tf), "10.20.0.0/16") {
		t.Errorf("main.tf not de-masked:\n%s", tf)
	}
	uf, _ := os.ReadFile(filepath.Join(outputDir, "user.tfvars"))
	if string(uf) != userFile {
		t.Errorf("user file not restored: %s", uf)
	}
	validated, _ := os.ReadFile(tfLog)
	if !strings.Contains(string(validated), "10.20.0.0/16") || strings.Contains(string(validated), "CIDR_PRIVATE_001") {
		t.Errorf("validators must see real values, saw:\n%s", validated)
	}

	// Doc-changed run edits the de-masked files: still nothing leaks.
	if err := os.Truncate(claudeLog, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(docPath, []byte(doc+"\nNew cache at 10.20.30.41.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	secrets = append(secrets, "10.20.30.41")
	run()
	sent, _ = os.ReadFile(claudeLog)
	if len(sent) == 0 {
		t.Fatal("fake claude never called on doc-changed run")
	}
	assertNoSecrets(t, "doc-changed run", string(sent))
	tf, _ = os.ReadFile(filepath.Join(outputDir, "main.tf"))
	if !strings.Contains(string(tf), "10.20.0.0/16") {
		t.Errorf("main.tf not de-masked after doc-changed run:\n%s", tf)
	}
}

func TestRunRefusesOutputDirContainingState(t *testing.T) {
	workDir := t.TempDir()
	docPath := filepath.Join(workDir, "doc.md")
	_ = os.WriteFile(docPath, []byte("# doc"), 0644)
	orch := orchestrator.New(workDir, workDir, "", false)
	if err := orch.Run(docPath); err == nil || !strings.Contains(err.Error(), "must not contain") {
		t.Errorf("expected refusal, got %v", err)
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
	claudeLog, _ := installFakes(t, false)
	workDir, docPath, outputDir := setupDoc(t)

	err := orchestrator.New(workDir, outputDir, "", false).Run(docPath)
	if err == nil || !strings.Contains(err.Error(), "validation issues remain") {
		t.Fatalf("expected validation issues error, got %v", err)
	}
	sent, _ := os.ReadFile(claudeLog)
	if !strings.Contains(string(sent), "Validation errors:") {
		t.Error("auto-fix was not attempted")
	}
	if !strings.Contains(string(sent), "===BEGIN_ROOT_README===") {
		t.Error("documentation was not generated despite remaining issues")
	}
	sess, _ := state.NewManager(workDir).FindByDocPath(docPath)
	if sess == nil || sess.CurrentPhase != state.PhaseDone {
		t.Errorf("session should be done, got %+v", sess)
	}
	tf, _ := os.ReadFile(filepath.Join(outputDir, "main.tf"))
	if strings.Contains(string(tf), "CIDR_PRIVATE_001") {
		t.Error("output left masked after validation issues")
	}
}

func TestRunInterruptedSendsNothing(t *testing.T) {
	claudeLog, _ := installFakes(t, true)
	workDir, docPath, outputDir := setupDoc(t)

	orch := orchestrator.New(workDir, outputDir, "", false)
	orch.Interrupt()
	if err := orch.Run(docPath); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("expected interrupt error, got %v", err)
	}
	if sent, _ := os.ReadFile(claudeLog); len(sent) > 0 {
		t.Errorf("claude called after interrupt:\n%s", sent)
	}
}
