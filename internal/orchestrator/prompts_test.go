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
	prompt := orchestrator.BuildClarifyPrompt(`{"ready": false, "summary": "EKS cluster"}`, questions, answers)

	if !strings.Contains(prompt, "eu-west-1") {
		t.Error("clarify prompt should contain answers")
	}
	if !strings.Contains(prompt, "JSON") {
		t.Error("clarify prompt should request JSON")
	}
	if !strings.Contains(prompt, "EKS cluster") {
		t.Error("clarify prompt should include the previous analysis")
	}
}

func TestParseGenerationPlanAppendsRoot(t *testing.T) {
	plan, err := orchestrator.ParseGenerationPlan("```json\n{\"modules\": [{\"name\": \"vpc\", \"files\": [\"modules/vpc/main.tf\"]}]}\n```")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Modules) != 2 || plan.Modules[1].Name != "root" || len(plan.Modules[1].Files) != 6 {
		t.Errorf("root not appended: %+v", plan)
	}
}

func TestParseGenerationPlanKeepsRootLast(t *testing.T) {
	plan, err := orchestrator.ParseGenerationPlan(`{"modules": [{"name": "root", "files": ["main.tf"]}, {"name": "vpc", "files": []}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Modules) != 2 || plan.Modules[0].Name != "vpc" || plan.Modules[1].Name != "root" || len(plan.Modules[1].Files) != 1 {
		t.Errorf("root not kept last with its files: %+v", plan)
	}
}

func TestParseGenerationPlanRejects(t *testing.T) {
	for _, raw := range []string{`{"modules": []}`, `{"modules": [{"name": " ", "files": []}]}`, `nope`} {
		if _, err := orchestrator.ParseGenerationPlan(raw); err == nil {
			t.Errorf("expected error for %q", raw)
		}
	}
}
