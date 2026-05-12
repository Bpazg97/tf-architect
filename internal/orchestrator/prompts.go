package orchestrator

import (
	"encoding/json"
	"fmt"
	"strings"
)

// AnalysisResult is Claude's response to the architecture analysis prompt.
type AnalysisResult struct {
	Ready           bool     `json:"ready"`
	Missing         []string `json:"missing"`
	Inconsistencies []string `json:"inconsistencies"`
	Summary         string   `json:"summary"`
}

// BuildAnalysisPrompt returns the prompt sent to Claude for the initial analysis phase.
func BuildAnalysisPrompt(markdown string) string {
	return fmt.Sprintf(`Analyze the following architecture document and respond with ONLY valid JSON — no other text.

Output this exact JSON structure:
{
  "ready": false,
  "missing": ["list every piece of information required to generate Terraform that is absent from the document"],
  "inconsistencies": ["list any design conflicts, contradictions, or ambiguities that need resolution"],
  "summary": "one paragraph describing the architecture"
}

Set "ready": true ONLY when ALL of the following are known:
- AWS region(s)
- VPC CIDR and subnet strategy
- Every compute resource type and size
- Every storage resource type and size
- IAM roles / service accounts required
- Environment (dev / staging / prod)
- Whether multi-account or single-account

Architecture document:
---
%s`, markdown)
}

// ParseAnalysisJSON extracts and parses the JSON analysis from Claude's response.
// Handles responses where JSON is wrapped in markdown code fences.
func ParseAnalysisJSON(raw string) AnalysisResult {
	// Try to extract JSON from ```json ... ``` fences first
	if idx := strings.Index(raw, "```json"); idx >= 0 {
		start := idx + len("```json")
		end := strings.Index(raw[start:], "```")
		if end >= 0 {
			raw = strings.TrimSpace(raw[start : start+end])
		}
	} else if idx := strings.Index(raw, "{"); idx >= 0 {
		// Try bare JSON object
		end := strings.LastIndex(raw, "}")
		if end > idx {
			raw = raw[idx : end+1]
		}
	}

	var result AnalysisResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return AnalysisResult{
			Ready:   false,
			Missing: []string{"Unable to parse architecture analysis — Claude response was not valid JSON"},
		}
	}
	return result
}

// BuildQuestions converts missing items and inconsistencies into user-facing questions.
func BuildQuestions(missing, inconsistencies []string) []string {
	var questions []string
	questions = append(questions, missing...)
	for _, inc := range inconsistencies {
		questions = append(questions, "Please clarify: "+inc)
	}
	return questions
}

// BuildClarifyPrompt constructs the follow-up prompt after the user answers questions.
func BuildClarifyPrompt(questions, answers []string) string {
	var sb strings.Builder
	sb.WriteString("The following clarifications were provided by the user:\n\n")
	for i, q := range questions {
		a := "(no answer provided)"
		if i < len(answers) && answers[i] != "" {
			a = answers[i]
		}
		sb.WriteString(fmt.Sprintf("Q: %s\nA: %s\n\n", q, a))
	}
	sb.WriteString(`Based on these answers, re-analyze the architecture and respond with ONLY valid JSON:

{
  "ready": true,
  "missing": ["any remaining gaps — empty array if none"],
  "inconsistencies": ["any remaining conflicts — empty array if none"],
  "summary": "updated architecture description incorporating the answers"
}`)
	return sb.String()
}
