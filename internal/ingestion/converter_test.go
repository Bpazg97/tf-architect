package ingestion_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tf-architect/internal/ingestion"
)

func TestConvertMarkdown(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.md")
	content := "# Test\n\nHello world"
	_ = os.WriteFile(path, []byte(content), 0644)

	result, err := ingestion.Convert(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Markdown != content {
		t.Errorf("expected %q, got %q", content, result.Markdown)
	}
	if result.WasConverted {
		t.Error("markdown should not be marked as converted")
	}
	if result.SourceFormat != ".md" {
		t.Errorf("expected .md, got %s", result.SourceFormat)
	}
}

func TestConvertTxt(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.txt")
	content := "Hello plain text"
	_ = os.WriteFile(path, []byte(content), 0644)

	result, err := ingestion.Convert(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Markdown != content {
		t.Errorf("expected %q, got %q", content, result.Markdown)
	}
}

func TestConvertUnsupportedFormat(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.xlsx")
	_ = os.WriteFile(path, []byte("data"), 0644)

	_, err := ingestion.Convert(path)
	if err == nil {
		t.Error("expected error for unsupported format")
	}
	if !strings.Contains(err.Error(), "unsupported format") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestEstimateTokens(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.md")
	content := strings.Repeat("a", 4000)
	_ = os.WriteFile(path, []byte(content), 0644)

	result, err := ingestion.Convert(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// ~4 chars per token → ~1000 tokens
	if result.EstimatedTokens < 900 || result.EstimatedTokens > 1100 {
		t.Errorf("expected ~1000 tokens, got %d", result.EstimatedTokens)
	}
}
