package state_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"tf-architect/internal/state"
)

func TestHashDoc(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "doc.md")
	_ = os.WriteFile(path, []byte("hello"), 0644)

	h1, err := state.HashDoc(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(h1) != 64 {
		t.Errorf("expected 64-char SHA256 hex, got %d chars", len(h1))
	}

	h2, _ := state.HashDoc(path)
	if h1 != h2 {
		t.Error("same content should produce same hash")
	}

	_ = os.WriteFile(path, []byte("different"), 0644)
	h3, _ := state.HashDoc(path)
	if h1 == h3 {
		t.Error("different content should produce different hash")
	}
}

func TestManagerSaveLoad(t *testing.T) {
	tmp := t.TempDir()
	mgr := state.NewManager(tmp)

	sess := &state.Session{
		DocPath:      "/tmp/test.md",
		DocHash:      "abc123",
		CreatedAt:    time.Now(),
		CurrentPhase: state.PhaseIngest,
	}

	if err := mgr.Save(sess); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := mgr.Load("abc123")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected session, got nil")
	}
	if loaded.DocPath != sess.DocPath {
		t.Errorf("expected %s, got %s", sess.DocPath, loaded.DocPath)
	}
}

func TestManagerLoadNonExistent(t *testing.T) {
	tmp := t.TempDir()
	mgr := state.NewManager(tmp)

	loaded, err := mgr.Load("nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loaded != nil {
		t.Error("expected nil for non-existent session")
	}
}

func TestPhaseOrder(t *testing.T) {
	if !state.PhaseIngest.Before(state.PhaseAnalyze) {
		t.Error("Ingest should be before Analyze")
	}
	if !state.PhaseGenerate.Before(state.PhaseValidate) {
		t.Error("Generate should be before Validate")
	}
	if state.PhaseDone.Before(state.PhaseIngest) {
		t.Error("Done should not be before Ingest")
	}
}

func TestFindByDocPath(t *testing.T) {
	tmp := t.TempDir()
	mgr := state.NewManager(tmp)

	sess := &state.Session{
		DocPath:      "/tmp/my-doc.pdf",
		DocHash:      "deadbeef",
		CreatedAt:    time.Now(),
		CurrentPhase: state.PhaseDone,
	}
	_ = mgr.Save(sess)

	found, err := mgr.FindByDocPath("/tmp/my-doc.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if found == nil {
		t.Fatal("expected to find session by path")
	}
	if found.DocHash != "deadbeef" {
		t.Errorf("expected deadbeef, got %s", found.DocHash)
	}

	notFound, _ := mgr.FindByDocPath("/tmp/other.pdf")
	if notFound != nil {
		t.Error("expected nil for unknown path")
	}
}
