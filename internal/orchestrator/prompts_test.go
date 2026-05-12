package orchestrator_test

import (
	"strings"
	"testing"

	"tf-architect/internal/orchestrator"
)

func TestBuildAnalysisPrompt(t *testing.T) {
	prompt := orchestrator.BuildAnalysisPrompt("# Architecture\n\nVPC in eu-west-1")
	if !strings.Contains(prompt, "JSON") {
		t.Error("analysis prompt should request JSON output")
	}
	if !strings.Contains(prompt, "ready") {
		t.Error("analysis prompt should mention 'ready' field")
	}
	if !strings.Contains(prompt, "eu-west-1") {
		t.Error("analysis prompt should contain the document content")
	}
}

func TestParseAnalysisJSONValid(t *testing.T) {
	raw := "Some text before\n```json\n{\"ready\": false, \"missing\": [\"AWS region\", \"instance type\"], \"inconsistencies\": [], \"summary\": \"EKS cluster\"}\n```\nSome text after"

	result := orchestrator.ParseAnalysisJSON(raw)
	if result.Ready {
		t.Error("expected Ready=false")
	}
	if len(result.Missing) != 2 {
		t.Errorf("expected 2 missing items, got %d", len(result.Missing))
	}
	if result.Summary != "EKS cluster" {
		t.Errorf("unexpected summary: %s", result.Summary)
	}
}

func TestParseAnalysisJSONFallback(t *testing.T) {
	result := orchestrator.ParseAnalysisJSON("I cannot parse this as JSON at all")
	if result.Ready {
		t.Error("unparseable response should yield Ready=false")
	}
	if len(result.Missing) == 0 {
		t.Error("expected at least one missing item for parse failure")
	}
}

func TestBuildQuestions(t *testing.T) {
	qs := orchestrator.BuildQuestions(
		[]string{"What AWS region?", "What instance type?"},
		[]string{"Conflicting AZ count"},
	)
	if len(qs) != 3 {
		t.Errorf("expected 3 questions, got %d", len(qs))
	}
}

func TestBuildClarifyPrompt(t *testing.T) {
	questions := []string{"What AWS region?", "What instance type?"}
	answers := []string{"eu-west-1", "t3.medium"}
	prompt := orchestrator.BuildClarifyPrompt(questions, answers)

	if !strings.Contains(prompt, "eu-west-1") {
		t.Error("clarify prompt should contain answers")
	}
	if !strings.Contains(prompt, "JSON") {
		t.Error("clarify prompt should request JSON")
	}
}
