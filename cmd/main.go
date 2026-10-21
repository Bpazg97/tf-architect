package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"tf-architect/internal/masking"
	"tf-architect/internal/orchestrator"
	"tf-architect/internal/state"
	"tf-architect/internal/validation"
	"tf-architect/prompts"
)

var (
	outputDir  string
	forceFlag  bool
	systemFile string
	noMaskFlag bool
	version    = "dev"
)

// ── TUI styling ──────────────────────────────────────────────────────────────
// Minimal ANSI styling. Disabled when stdout is not a TTY or NO_COLOR is set.

var colorEnabled = os.Getenv("NO_COLOR") == "" && isTerminal(os.Stdout)

// stdin is shared by every prompt: separate bufio readers on os.Stdin would
// each buffer input the others never see.
var stdin = bufio.NewReader(os.Stdin)

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

const (
	ansiBold   = "1"
	ansiDim    = "2"
	ansiRed    = "31"
	ansiGreen  = "32"
	ansiYellow = "33"
	ansiCyan   = "36"
)

func paint(code, s string) string {
	if !colorEnabled {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

// styleStatus colorizes orchestrator status lines by their leading marker.
func styleStatus(msg string) string {
	t := strings.TrimSpace(msg)
	switch {
	case strings.HasPrefix(t, "✓"):
		return paint(ansiGreen, msg)
	case strings.HasPrefix(t, "✗"):
		return paint(ansiRed, msg)
	case strings.HasPrefix(t, "Warning"), strings.HasPrefix(t, "⚠"):
		return paint(ansiYellow, msg)
	case strings.HasPrefix(t, "──"):
		return paint(ansiDim, msg)
	case strings.HasPrefix(t, "Mode:"):
		return paint(ansiBold, msg)
	default:
		return msg
	}
}

func main() {
	root := &cobra.Command{
		Use:     "tf-architect",
		Version: version,
		// Runtime errors are not usage errors: print the error only.
		SilenceUsage: true,
		Short:        "Generate and validate production Terraform IaC from architecture documents",
		Long: `tf-architect turns architecture documents (PDF, DOCX, MD, TXT) into
production-grade AWS Terraform — and validates existing Terraform projects.

Typical workflow:

  1. Generate IaC from a document:
       tf-architect run architecture.pdf

  2. Validate the generated (or any existing) Terraform directory:
       tf-architect validate ./architecture-tf

  3. Check or resume an interrupted session:
       tf-architect status architecture.pdf

  4. Start fresh if needed:
       tf-architect reset architecture.pdf

Validation checks (in order):
  • terraform init    — initialises providers (no backend)
  • terraform validate — checks HCL syntax and references
  • tflint            — linting rules (skipped if not installed)
  • checkov           — security/compliance scan (skipped if not installed)
  • golden-rules-audit — enforces project conventions (S3 backend, tags,
                         no open ingress, no plaintext secrets, etc.)

Sessions are persisted: interrupted runs resume from the last checkpoint.`,
	}

	runCmd := &cobra.Command{
		Use:   "run <document>",
		Short: "Generate Terraform IaC from an architecture document",
		Long: `Reads an architecture document and generates production-grade AWS Terraform.

Claude analyses the document, asks clarifying questions when information is
missing, then generates each Terraform module. After generation it runs the
full validation suite and produces terraform-docs documentation.

Supported formats: PDF, DOCX, MD, TXT`,
		Example: `  # Generate into the default output directory (<docname>-tf/)
  tf-architect run architecture.pdf

  # Specify a custom output directory
  tf-architect run architecture.pdf --output ./infra/terraform

  # Force a full re-run (discard saved session)
  tf-architect run architecture.pdf --force

  # Use a custom system prompt / golden rules file
  tf-architect run architecture.pdf --system-file ./my-rules.md`,
		Args: cobra.ExactArgs(1),
		RunE: runGenerate,
	}
	runCmd.Flags().StringVarP(&outputDir, "output", "o", "", "Output directory for Terraform files (default: <docname>-tf/)")
	runCmd.Flags().BoolVarP(&forceFlag, "force", "f", false, "Force full re-run, ignoring saved session state")
	runCmd.Flags().StringVar(&systemFile, "system-file", "", "Path to custom system prompt file (default: built-in golden rules)")
	runCmd.Flags().BoolVar(&noMaskFlag, "no-mask", false, "Disable sensitive value masking (debug only — never use in production)")

	statusCmd := &cobra.Command{
		Use:   "status <document>",
		Short: "Show the session status for a document",
		Long: `Displays the current session state for a document: phase, last checkpoint,
generated files, and the result of the most recent validation run.`,
		Example: `  tf-architect status architecture.pdf`,
		Args:    cobra.ExactArgs(1),
		RunE:    runStatus,
	}

	resetCmd := &cobra.Command{
		Use:   "reset <document>",
		Short: "Delete the saved session state for a document",
		Long: `Removes the persisted session so the next 'run' starts from scratch.
Useful when you want to regenerate everything or the session is corrupted.`,
		Example: `  tf-architect reset architecture.pdf`,
		Args:    cobra.ExactArgs(1),
		RunE:    runReset,
	}

	validateCmd := &cobra.Command{
		Use:   "validate <terraform-dir>",
		Short: "Validate an existing Terraform project directory",
		Long: `Runs the full validation suite against any Terraform directory —
not just projects generated by tf-architect.

Checks performed (in order):
  1. terraform init     — downloads providers (no remote backend)
  2. terraform validate — syntax and reference errors
  3. tflint             — best-practice linting (skipped if not installed)
  4. checkov            — security and compliance policies (skipped if not installed)
  5. golden-rules-audit — project conventions:
       • S3 backend configured
       • required_version and required_providers pinned
       • default_tags present
       • no 0.0.0.0/0 ingress rules
       • no wildcard IAM actions (Action: *)
       • no plaintext secrets (password/token/api_key = "...")
       • no hardcoded AWS account IDs

The command exits with a non-zero status if any check fails.`,
		Example: `  # Validate a generated project
  tf-architect validate ./architecture-tf

  # Validate any existing Terraform directory
  tf-architect validate ~/projects/my-infra/terraform`,
		Args: cobra.ExactArgs(1),
		RunE: runValidate,
	}

	root.AddCommand(runCmd, statusCmd, resetCmd, validateCmd)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func runGenerate(cmd *cobra.Command, args []string) error {
	docPath, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("resolving document path: %w", err)
	}

	if _, err := os.Stat(docPath); os.IsNotExist(err) {
		return fmt.Errorf("document not found: %s", docPath)
	}

	workDir, err := os.Getwd()
	if err != nil {
		return err
	}

	if outputDir == "" {
		base := strings.TrimSuffix(filepath.Base(docPath), filepath.Ext(docPath))
		outputDir = filepath.Join(workDir, base+"-tf")
	}
	if outputDir, err = filepath.Abs(outputDir); err != nil {
		return fmt.Errorf("resolving output path: %w", err)
	}

	// Force re-run: delete every saved session for this document
	if forceFlag {
		n, err := state.NewManager(workDir).DeleteByDocPath(docPath)
		if err != nil {
			return fmt.Errorf("clearing sessions: %w", err)
		}
		if n > 0 {
			fmt.Printf("Cleared session for %s\n", filepath.Base(docPath))
		}
	}

	systemPrompt := prompts.System
	if systemFile != "" {
		data, err := os.ReadFile(systemFile)
		if err != nil {
			return fmt.Errorf("reading system file: %w", err)
		}
		systemPrompt = string(data)
	}

	orch := orchestrator.New(workDir, outputDir, systemPrompt, noMaskFlag)

	fmt.Println(paint(ansiBold, "tf-architect ") + paint(ansiDim, version))
	fmt.Println(paint(ansiDim, strings.Repeat("─", 45)))

	if !noMaskFlag {
		orch.CustomSensitive = askCustomSensitive()
	}

	orch.OnStatus = func(msg string) {
		fmt.Println(styleStatus(msg))
	}

	orch.OnChunk = func(chunk string) {
		fmt.Print(paint(ansiDim, chunk))
	}

	orch.OnQuestion = func(questions []string) []string {
		fmt.Println("\n" + paint(ansiCyan, "── Clarification needed ─────────────────────"))
		answers := make([]string, len(questions))
		for i, q := range questions {
			fmt.Printf("\n%s %s\n%s ",
				paint(ansiCyan, fmt.Sprintf("[%d/%d]", i+1, len(questions))),
				paint(ansiBold, q),
				paint(ansiCyan, ">"))
			answer, _ := stdin.ReadString('\n')
			answers[i] = strings.TrimSpace(answer)
		}
		fmt.Println(paint(ansiCyan, "─────────────────────────────────────────────"))
		return answers
	}

	stop := handleInterrupt(orch.Interrupt)
	defer stop()
	return orch.Run(docPath)
}

const customDisclaimer = `Automatic masking covers ARNs, account IDs, IPs/CIDRs, AWS resource IDs,
hostnames, emails, bucket names and common tag values. List anything else you
consider sensitive (client or project names, internal codenames, ...). Values
are masked wherever they appear, ignoring case.

DISCLAIMER: values you do not list may be sent to Claude. The author of
tf-architect accepts no responsibility for data exposed through omission or
negligence when providing this list.`

// askCustomSensitive asks for extra sensitive values as value1,value2,value3.
// "null" (the default) or an empty answer means none. Non-interactive runs
// get none.
func askCustomSensitive() []string {
	if !isTerminal(os.Stdin) {
		fmt.Println(paint(ansiDim, "Custom sensitive values: none (non-interactive input)"))
		return nil
	}
	fmt.Println("\n" + paint(ansiCyan, "── Custom sensitive values ──────────────────"))
	fmt.Println(customDisclaimer)
	fmt.Printf("\n%s %s ", paint(ansiBold, "Values (value1,value2,value3)"), paint(ansiDim, "[null]"))
	line, _ := stdin.ReadString('\n')
	values, short := parseCustomSensitive(line)
	for _, v := range short {
		fmt.Println(paint(ansiYellow, fmt.Sprintf("  ⚠ ignored %q: shorter than %d characters", v, masking.MinCustomLen)))
	}
	if len(values) == 0 {
		fmt.Println(paint(ansiDim, "  No custom values."))
	} else {
		fmt.Println(paint(ansiGreen, fmt.Sprintf("  ✓ custom values to mask: %d", len(values))))
	}
	fmt.Println(paint(ansiCyan, "─────────────────────────────────────────────"))
	return values
}

// parseCustomSensitive splits a comma-separated answer, dropping blanks and
// "null". Values too short to mask safely are returned separately.
func parseCustomSensitive(line string) (values, short []string) {
	seen := make(map[string]bool)
	for _, v := range strings.Split(line, ",") {
		v = strings.TrimSpace(v)
		if v == "" || strings.EqualFold(v, "null") || seen[v] {
			continue
		}
		seen[v] = true
		if len(v) < masking.MinCustomLen {
			short = append(short, v)
			continue
		}
		values = append(values, v)
	}
	return values, short
}

// handleInterrupt keeps the first Ctrl-C from killing the process outright:
// onFirst stops further Claude calls, the running subprocess receives the
// signal too and exits, so Run returns normally and de-masks the output
// files. A second Ctrl-C exits immediately (files then stay masked until the
// next run).
func handleInterrupt(onFirst func()) (stop func()) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		if _, ok := <-ch; !ok {
			return
		}
		onFirst()
		fmt.Fprintln(os.Stderr, paint(ansiYellow, "\nInterrupt received — cleaning up (press Ctrl-C again to force quit)"))
		if _, ok := <-ch; ok {
			os.Exit(130)
		}
	}()
	return func() {
		signal.Stop(ch)
		close(ch)
	}
}

func runStatus(cmd *cobra.Command, args []string) error {
	docPath, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("resolving document path: %w", err)
	}
	workDir, err := os.Getwd()
	if err != nil {
		return err
	}

	sess, err := state.NewManager(workDir).FindByDocPath(docPath)
	if err != nil {
		return err
	}
	if sess == nil {
		fmt.Printf("No session found for %s\n", filepath.Base(docPath))
		return nil
	}

	fmt.Printf("Document: %s\n", sess.DocPath)
	docHash := sess.DocHash
	if len(docHash) > 12 {
		docHash = docHash[:12] + "..."
	}
	fmt.Printf("Hash:     %s\n", docHash)
	fmt.Printf("Phase:    %s\n", paint(ansiBold, string(sess.CurrentPhase)))
	fmt.Printf("Created:  %s\n", sess.CreatedAt.Format("2006-01-02 15:04"))
	fmt.Printf("Updated:  %s\n", sess.UpdatedAt.Format("2006-01-02 15:04"))
	if sess.LastCheckpoint.GenerateStep != "" {
		fmt.Printf("Last step: %s\n", sess.LastCheckpoint.GenerateStep)
	}
	if len(sess.GeneratedFiles) > 0 {
		fmt.Printf("Generated: %d Terraform files\n", len(sess.GeneratedFiles))
	}
	if len(sess.ValidationRuns) > 0 {
		last := sess.ValidationRuns[len(sess.ValidationRuns)-1]
		result := paint(ansiGreen, "PASSED")
		if !last.Passed {
			result = paint(ansiRed, "FAILED")
		}
		fmt.Printf("Last validation: %s (%s)\n", result, last.RunAt.Format("15:04"))
	}
	return nil
}

func runReset(cmd *cobra.Command, args []string) error {
	docPath, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("resolving document path: %w", err)
	}
	workDir, err := os.Getwd()
	if err != nil {
		return err
	}

	n, err := state.NewManager(workDir).DeleteByDocPath(docPath)
	if err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	if n == 0 {
		fmt.Printf("No session found for %s\n", filepath.Base(docPath))
		return nil
	}
	fmt.Printf("Session for %s cleared.\n", filepath.Base(docPath))
	return nil
}

func runValidate(cmd *cobra.Command, args []string) error {
	dir, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("resolving path: %w", err)
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("directory not found: %s", dir)
	}

	fmt.Printf("Validating: %s\n\n", dir)
	suite := validation.New(dir)
	results, runErr := suite.Run()

	passed, failed := 0, 0
	for _, r := range results {
		icon := paint(ansiGreen, "✓")
		switch {
		case r.Skipped:
			icon = paint(ansiDim, "–")
		case !r.Passed:
			icon = paint(ansiRed, "✗")
			failed++
		default:
			passed++
		}
		fmt.Printf("  %s  %s\n", icon, r.Stage)
		if r.Skipped {
			fmt.Printf("       %s\n", paint(ansiDim, r.Output))
		}
		for _, e := range r.Errors {
			fmt.Printf("       %s\n", paint(ansiRed, e))
		}
		if !r.Passed && r.Output != "" && len(r.Errors) == 0 {
			// print raw output for stages without structured errors (e.g. tflint)
			lines := strings.SplitN(strings.TrimSpace(r.Output), "\n", 6)
			for _, l := range lines {
				fmt.Printf("       %s\n", l)
			}
		}
	}

	failedText := fmt.Sprintf("%d failed", failed)
	if failed > 0 {
		failedText = paint(ansiRed, failedText)
	}
	fmt.Printf("\n%s, %s\n", paint(ansiGreen, fmt.Sprintf("%d passed", passed)), failedText)
	return runErr
}
