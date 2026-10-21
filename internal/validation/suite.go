package validation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"tf-architect/internal/fsutil"
)

type ValidationResult struct {
	Stage   string
	Passed  bool
	Skipped bool // tool not installed; counts as passed
	Output  string
	Errors  []string
}

type Suite struct {
	Dir string
	// Env is appended to the environment of every validator subprocess
	// (e.g. TF_DATA_DIR when validating a copy of the project).
	Env []string
}

func New(dir string) *Suite {
	return &Suite{Dir: dir}
}

// Run executes all available validators in order. Stops at the first critical failure.
func (s *Suite) Run() ([]ValidationResult, error) {
	var results []ValidationResult

	if _, err := exec.LookPath("terraform"); err != nil {
		results = append(results, ValidationResult{
			Stage:   "terraform init/validate",
			Passed:  true,
			Skipped: true,
			Output:  "terraform not installed — skipped",
		})
	} else {
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

func (s *Suite) command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	if len(s.Env) > 0 {
		cmd.Env = append(os.Environ(), s.Env...)
	}
	return cmd
}

func (s *Suite) runTerraformInit() ValidationResult {
	var out bytes.Buffer
	cmd := s.command("terraform", "init", "-backend=false", "-no-color", "-input=false")
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
	cmd := s.command("terraform", "validate", "-no-color", "-json")
	cmd.Dir = s.Dir
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()

	var tfResult struct {
		Valid       bool `json:"valid"`
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
	cmd := s.command("tflint", "--format", "json", "--recursive")
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
	cmd := s.command("checkov",
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
	Severity string
	Message  string
	Violated func(content string) bool
}

func contains(re string) func(string) bool { return regexp.MustCompile(re).MatchString }

func lacks(re string) func(string) bool {
	match := regexp.MustCompile(re).MatchString
	return func(content string) bool { return !match(content) }
}

// Ingress open to the world, in its three Terraform spellings. Egress to
// 0.0.0.0/0 is normal and must not match.
var (
	openCIDR          = `"(?:0\.0\.0\.0/0|::/0)"`
	inlineIngressRe   = regexp.MustCompile(`ingress\s*\{[^}]*(?:ipv6_)?cidr_blocks\s*=\s*\[[^\]]*` + openCIDR)
	sgRuleBlockRe     = regexp.MustCompile(`resource\s+"aws_security_group_rule"\s+"[^"]+"\s*\{[^}]*\}`)
	sgRuleIngressRe   = regexp.MustCompile(`type\s*=\s*"ingress"`)
	sgRuleOpenRe      = regexp.MustCompile(`cidr_blocks\s*=\s*\[[^\]]*` + openCIDR)
	ingressRuleOpenRe = regexp.MustCompile(`resource\s+"aws_vpc_security_group_ingress_rule"\s+"[^"]+"\s*\{[^}]*cidr_ipv[46]\s*=\s*` + openCIDR)
)

func hasOpenIngress(content string) bool {
	if inlineIngressRe.MatchString(content) || ingressRuleOpenRe.MatchString(content) {
		return true
	}
	for _, block := range sgRuleBlockRe.FindAllString(content, -1) {
		if sgRuleIngressRe.MatchString(block) && sgRuleOpenRe.MatchString(block) {
			return true
		}
	}
	return false
}

var goldenRules = []ruleCheck{
	{
		Name:     "no-0.0.0.0-ingress",
		Severity: "error",
		Message:  "Security group with ingress 0.0.0.0/0 — golden rule violation",
		Violated: hasOpenIngress,
	},
	{
		Name:     "no-hardcoded-account-id",
		Severity: "warning",
		Message:  "Possible hardcoded AWS account ID — use data.aws_caller_identity",
		Violated: contains(`"\d{12}"`),
	},
	{
		Name:     "no-wildcard-iam-action",
		Severity: "error",
		Message:  "IAM policy with Action:* — least privilege violation",
		Violated: contains(`"Action"\s*:\s*"\*"`),
	},
	{
		Name:     "no-plaintext-secret",
		Severity: "error",
		Message:  "Possible plaintext secret — use aws_secretsmanager or sensitive variable",
		Violated: contains(`(?i)(password|secret|token|api_key)\s*=\s*"[^$][^{]`),
	},
	{
		Name:     "has-required-tags",
		Severity: "warning",
		Message:  "No default_tags detected — golden rule: all resources must be tagged",
		Violated: lacks(`default_tags`),
	},
	{
		Name:     "has-backend-config",
		Severity: "error",
		Message:  "No S3 backend configured — local state violates golden rules",
		Violated: lacks(`backend\s+"s3"`),
	},
	{
		Name:     "has-required-version",
		Severity: "warning",
		Message:  "terraform.required_version not found",
		Violated: lacks(`required_version`),
	},
	{
		Name:     "has-provider-version",
		Severity: "warning",
		Message:  "required_providers not found — provider versions unpinned",
		Violated: lacks(`required_providers`),
	},
}

// RunGoldenRulesAudit is exported so tests can call it directly.
func (s *Suite) RunGoldenRulesAudit() ValidationResult {
	files, _ := fsutil.TFFiles(s.Dir)
	content := concatenateTFFiles(files)

	var errs, warnings []string
	for _, rule := range goldenRules {
		if !rule.Violated(content) {
			continue
		}
		msg := fmt.Sprintf("[%s] %s: %s", strings.ToUpper(rule.Severity), rule.Name, rule.Message)
		if rule.Severity == "error" {
			errs = append(errs, msg)
		} else {
			warnings = append(warnings, msg)
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
