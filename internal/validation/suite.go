package validation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type ValidationResult struct {
	Stage  string
	Passed bool
	Output string
	Errors []string
}

type Suite struct {
	Dir string
}

func New(dir string) *Suite {
	return &Suite{Dir: dir}
}

// Run executes all available validators in order. Stops at the first critical failure.
func (s *Suite) Run() ([]ValidationResult, error) {
	var results []ValidationResult

	r := s.runTerraformInit()
	results = append(results, r)
	if !r.Passed {
		return results, fmt.Errorf("terraform init failed — cannot proceed")
	}

	r = s.runTerraformValidate()
	results = append(results, r)
	if !r.Passed {
		return results, fmt.Errorf("terraform validate failed")
	}

	if _, err := exec.LookPath("tflint"); err == nil {
		results = append(results, s.runTFLint())
	}

	if _, err := exec.LookPath("checkov"); err == nil {
		results = append(results, s.runCheckov())
	}

	results = append(results, s.RunGoldenRulesAudit())

	for _, r := range results {
		if !r.Passed {
			return results, fmt.Errorf("validation failed at stage: %s", r.Stage)
		}
	}
	return results, nil
}

func (s *Suite) runTerraformInit() ValidationResult {
	var out bytes.Buffer
	cmd := exec.Command("terraform", "init", "-backend=false", "-no-color", "-input=false")
	cmd.Dir = s.Dir
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return ValidationResult{
		Stage:  "terraform init",
		Passed: err == nil,
		Output: out.String(),
	}
}

func (s *Suite) runTerraformValidate() ValidationResult {
	var out bytes.Buffer
	cmd := exec.Command("terraform", "validate", "-no-color", "-json")
	cmd.Dir = s.Dir
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()

	var tfResult struct {
		Valid        bool `json:"valid"`
		Diagnostics []struct {
			Severity string `json:"severity"`
			Summary  string `json:"summary"`
			Detail   string `json:"detail"`
		} `json:"diagnostics"`
	}

	var errs []string
	if jsonErr := json.Unmarshal(out.Bytes(), &tfResult); jsonErr == nil {
		for _, d := range tfResult.Diagnostics {
			if d.Severity == "error" {
				errs = append(errs, fmt.Sprintf("%s: %s", d.Summary, d.Detail))
			}
		}
	}

	return ValidationResult{
		Stage:  "terraform validate",
		Passed: err == nil && len(errs) == 0,
		Output: out.String(),
		Errors: errs,
	}
}

func (s *Suite) runTFLint() ValidationResult {
	var out bytes.Buffer
	cmd := exec.Command("tflint", "--format", "json", "--recursive")
	cmd.Dir = s.Dir
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return ValidationResult{
		Stage:  "tflint",
		Passed: err == nil,
		Output: out.String(),
	}
}

func (s *Suite) runCheckov() ValidationResult {
	var out bytes.Buffer
	cmd := exec.Command("checkov",
		"-d", s.Dir,
		"--framework", "terraform",
		"--output", "json",
		"--quiet",
		"--skip-check", "CKV_AWS_144,CKV_AWS_18",
	)
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return ValidationResult{
		Stage:  "checkov",
		Passed: err == nil,
		Output: out.String(),
	}
}

type ruleCheck struct {
	Name     string
	Pattern  string
	IsRegex  bool
	Inverted bool
	Severity string
	Message  string
}

// RunGoldenRulesAudit is exported so tests can call it directly.
func (s *Suite) RunGoldenRulesAudit() ValidationResult {
	files, _ := collectTFFiles(s.Dir)
	content := concatenateTFFiles(files)

	rules := []ruleCheck{
		{
			Name:     "no-0.0.0.0-ingress",
			Pattern:  `cidr_blocks\s*=\s*\[?"0\.0\.0\.0/0"`,
			IsRegex:  true,
			Severity: "error",
			Message:  "Security group with ingress 0.0.0.0/0 — golden rule violation",
		},
		{
			Name:     "no-hardcoded-account-id",
			Pattern:  `"\d{12}"`,
			IsRegex:  true,
			Severity: "warning",
			Message:  "Possible hardcoded AWS account ID — use data.aws_caller_identity",
		},
		{
			Name:     "no-wildcard-iam-action",
			Pattern:  `"Action"\s*:\s*"\*"`,
			IsRegex:  true,
			Severity: "error",
			Message:  "IAM policy with Action:* — least privilege violation",
		},
		{
			Name:     "no-plaintext-secret",
			Pattern:  `(?i)(password|secret|token|api_key)\s*=\s*"[^$][^{]`,
			IsRegex:  true,
			Severity: "error",
			Message:  "Possible plaintext secret — use aws_secretsmanager or sensitive variable",
		},
		{
			Name:     "has-required-tags",
			Pattern:  "default_tags",
			IsRegex:  false,
			Inverted: true,
			Severity: "warning",
			Message:  "No default_tags detected — golden rule: all resources must be tagged",
		},
		{
			Name:     "has-backend-config",
			Pattern:  `backend\s+"s3"`,
			IsRegex:  true,
			Inverted: true,
			Severity: "error",
			Message:  "No S3 backend configured — local state violates golden rules",
		},
		{
			Name:     "has-required-version",
			Pattern:  "required_version",
			IsRegex:  false,
			Inverted: true,
			Severity: "warning",
			Message:  "terraform.required_version not found",
		},
		{
			Name:     "has-provider-version",
			Pattern:  "required_providers",
			IsRegex:  false,
			Inverted: true,
			Severity: "warning",
			Message:  "required_providers not found — provider versions unpinned",
		},
	}

	var errs, warnings []string
	for _, rule := range rules {
		var found bool
		if rule.IsRegex {
			re, err := regexp.Compile(rule.Pattern)
			if err == nil {
				found = re.MatchString(content)
			}
		} else {
			found = strings.Contains(content, rule.Pattern)
		}

		violated := found
		if rule.Inverted {
			violated = !found
		}

		if violated {
			msg := fmt.Sprintf("[%s] %s: %s", strings.ToUpper(rule.Severity), rule.Name, rule.Message)
			if rule.Severity == "error" {
				errs = append(errs, msg)
			} else {
				warnings = append(warnings, msg)
			}
		}
	}

	all := append(errs, warnings...)
	return ValidationResult{
		Stage:  "golden-rules-audit",
		Passed: len(errs) == 0,
		Output: strings.Join(all, "\n"),
		Errors: errs,
	}
}

func collectTFFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".tf") {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

func concatenateTFFiles(files []string) string {
	var sb strings.Builder
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err == nil {
			sb.Write(data)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}
