package orchestrator_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"tf-architect/internal/orchestrator"
	"tf-architect/internal/state"
)

func TestDecideNew(t *testing.T) {
	tmp := t.TempDir()
	docPath := filepath.Join(tmp, "doc.md")
	_ = os.WriteFile(docPath, []byte("# LLD\n\nVPC in eu-west-1"), 0644)

	mgr := state.NewManager(tmp)
	decision, err := orchestrator.DecideResume(docPath, mgr)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Mode != orchestrator.ModeNew {
		t.Errorf("expected ModeNew, got %s", decision.Mode)
	}
	if decision.Session == nil {
		t.Fatal("expected non-nil session")
	}
}

func TestDecideResumeIncomplete(t *testing.T) {
	tmp := t.TempDir()
	docPath := filepath.Join(tmp, "doc.md")
	content := []byte("# LLD\n\nVPC in eu-west-1")
	_ = os.WriteFile(docPath, content, 0644)

	hash, _ := state.HashDoc(docPath)
	mgr := state.NewManager(tmp)

	sess := &state.Session{
		DocPath:      docPath,
		DocHash:      hash,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
		CurrentPhase: state.PhaseGenerate,
	}
	_ = mgr.Save(sess)

	decision, err := orchestrator.DecideResume(docPath, mgr)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Mode != orchestrator.ModeResume {
		t.Errorf("expected ModeResume, got %s", decision.Mode)
	}
}

func TestDecideValidateOnly(t *testing.T) {
	tmp := t.TempDir()
	docPath := filepath.Join(tmp, "doc.md")
	_ = os.WriteFile(docPath, []byte("# LLD"), 0644)

	hash, _ := state.HashDoc(docPath)
	mgr := state.NewManager(tmp)

	sess := &state.Session{
		DocPath:      docPath,
		DocHash:      hash,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
		CurrentPhase: state.PhaseDone,
	}
	_ = mgr.Save(sess)

	decision, err := orchestrator.DecideResume(docPath, mgr)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Mode != orchestrator.ModeValidateOnly {
		t.Errorf("expected ModeValidateOnly, got %s", decision.Mode)
	}
}

func TestDecideDocChanged(t *testing.T) {
	tmp := t.TempDir()
	docPath := filepath.Join(tmp, "doc.md")
	originalContent := []byte("# LLD\n\nOriginal content")
	_ = os.WriteFile(docPath, originalContent, 0644)

	hash, _ := state.HashDoc(docPath)
	mgr := state.NewManager(tmp)

	sess := &state.Session{
		DocPath:      docPath,
		DocHash:      hash,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
		CurrentPhase: state.PhaseDone,
		DocMarkdown:  string(originalContent),
	}
	_ = mgr.Save(sess)

	_ = os.WriteFile(docPath, []byte("# LLD\n\nUpdated content with new section"), 0644)

	decision, err := orchestrator.DecideResume(docPath, mgr)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Mode != orchestrator.ModeDocChanged {
		t.Errorf("expected ModeDocChanged, got %s", decision.Mode)
	}
	if decision.Session.PreviousDocHash != hash {
		t.Error("expected PreviousDocHash to be set to old hash")
	}
}
