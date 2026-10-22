package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// AnalysisResult is the model's response to the architecture analysis prompt.
type AnalysisResult struct {
	Ready           bool     `json:"ready"`
	Missing         []string `json:"missing"`
	Inconsistencies []string `json:"inconsistencies"`
	Summary         string   `json:"summary"`
	// ParseFailed is set when the response was not valid JSON.
	ParseFailed bool `json:"-"`
}

// BuildAnalysisPrompt returns the prompt sent to the model for the initial analysis phase.
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

// extractJSON returns the JSON object in a model response: the body of a
// ```json fence, else the span from the first "{" to the last "}", else raw.
func extractJSON(raw string) string {
	if idx := strings.Index(raw, "```json"); idx >= 0 {
		start := idx + len("```json")
		end := strings.Index(raw[start:], "```")
		if end >= 0 {
			return strings.TrimSpace(raw[start : start+end])
		}
	} else if idx := strings.Index(raw, "{"); idx >= 0 {
		end := strings.LastIndex(raw, "}")
		if end > idx {
			return raw[idx : end+1]
		}
	}
	return raw
}

// ParseAnalysisJSON extracts and parses the JSON analysis from the model's response.
// Handles responses where JSON is wrapped in markdown code fences.
func ParseAnalysisJSON(raw string) AnalysisResult {
	raw = extractJSON(raw)

	var result AnalysisResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return AnalysisResult{
			Ready:       false,
			Missing:     []string{"Unable to parse architecture analysis — the model response was not valid JSON"},
			ParseFailed: true,
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

// BuildClarifyPrompt constructs the follow-up prompt after the user answers
// questions. Calls are stateless, so it carries the analysis being refined.
func BuildClarifyPrompt(analysisJSON string, questions, answers []string) string {
	var sb strings.Builder
	sb.WriteString("Your previous architecture analysis:\n")
	sb.WriteString(analysisJSON)
	sb.WriteString("\n\nThe following clarifications were provided by the user:\n\n")
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

// rootModule is the module holding the root files; it is always generated last.
const rootModule = "root"

var rootFiles = []string{"versions.tf", "providers.tf", "main.tf", "variables.tf", "outputs.tf", "data.tf"}

// ModulePlan is one unit of generation: a module and the files it holds.
type ModulePlan struct {
	Name  string   `json:"name"`
	Files []string `json:"files"`
}

// GenerationPlan lists the modules to generate, root last.
type GenerationPlan struct {
	Modules []ModulePlan `json:"modules"`
}

// moduleNameRe is what a module name may hold: it becomes a task header, a
// checkpoint and a status line, so no spaces, newlines or control characters.
var moduleNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ParseGenerationPlan parses the model's plan. A plan without modules, or
// with a module whose name is invalid, duplicated or a reserved checkpoint
// (review, adjust), is an error. The root module is moved or appended last.
func ParseGenerationPlan(raw string) (GenerationPlan, error) {
	var plan GenerationPlan
	if err := json.Unmarshal([]byte(extractJSON(raw)), &plan); err != nil {
		return GenerationPlan{}, fmt.Errorf("generation plan is not valid JSON: %w", err)
	}
	if len(plan.Modules) == 0 {
		return GenerationPlan{}, errors.New("generation plan has no modules")
	}
	var modules []ModulePlan
	root := ModulePlan{Name: rootModule, Files: rootFiles}
	seen := map[string]bool{}
	for _, m := range plan.Modules {
		if strings.TrimSpace(m.Name) == "" {
			return GenerationPlan{}, errors.New("generation plan has a module without a name")
		}
		if !moduleNameRe.MatchString(m.Name) {
			return GenerationPlan{}, fmt.Errorf("generation plan has an invalid module name %q (letters, digits, - and _ only)", m.Name)
		}
		if m.Name == reviewStep || m.Name == adjustStep {
			return GenerationPlan{}, fmt.Errorf("generation plan uses the reserved module name %q", m.Name)
		}
		if seen[m.Name] {
			return GenerationPlan{}, fmt.Errorf("generation plan lists module %q twice", m.Name)
		}
		seen[m.Name] = true
		if m.Name == rootModule {
			root = m
			continue
		}
		modules = append(modules, m)
	}
	plan.Modules = append(modules, root)
	return plan, nil
}

// BuildPlanPrompt asks for the module breakdown as JSON only.
func BuildPlanPrompt(markdown string) string {
	return fmt.Sprintf(`Plan the Terraform project for the analyzed architecture. Respond with ONLY valid JSON, no other text, in this shape:
{
  "modules": [
    {"name": "vpc", "files": ["modules/vpc/main.tf", "modules/vpc/variables.tf", "modules/vpc/outputs.tf"]}
  ]
}

List one entry per module, in dependency order, with the path of every file it needs, relative to the project root.
The last entry must be the module "root" with the root files (versions.tf, providers.tf, main.tf, variables.tf, outputs.tf, data.tf as needed) that wire the modules together.

Architecture document:
---
%s`, markdown)
}

// BuildModulePrompt asks for one module's files, given the plan and the
// project written so far.
func BuildModulePrompt(plan GenerationPlan, m ModulePlan, project, markdown string) string {
	planJSON, _ := json.Marshal(plan)
	return fmt.Sprintf(`Generate the Terraform files of module %q only. Return each of these files in full in the output protocol, with paths relative to the project root:
%s

Generation plan: %s

Files already written (keep names, variables and outputs consistent with them; do not repeat them):
%s

Architecture document:
---
%s`, m.Name, "- "+strings.Join(m.Files, "\n- "), planJSON, project, markdown)
}

// BuildReviewPrompt asks for a whole-project review.
func BuildReviewPrompt(project string) string {
	return fmt.Sprintf(`Review the whole Terraform project below for correctness: wiring between modules and the root, variable and output names and types, missing or unused references, provider and version constraints, and security best practices.
Return only the files that must change, in full, in the output protocol. If nothing needs to change, respond with exactly %s.

Current project files:
%s`, "NO_CHANGES", project)
}
