package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"tf-architect/internal/llm"
	"tf-architect/internal/masking"
	"tf-architect/internal/orchestrator"
	"tf-architect/internal/security"
	"tf-architect/internal/state"
	"tf-architect/internal/validation"
	"tf-architect/prompts"
)

var (
	outputDir  string
	forceFlag  bool
	agentsFile string
	noMaskFlag bool
	version    = "dev"

	providerFlag  string
	modelFlag     string
	baseURLFlag   string
	yesCustomFlag bool
	claudeBin     string // test hook: path of the claude binary
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
		// Runtime errors are not usage errors; main prints them itself
		// (security stops as a pop-up), so cobra must not print them too.
		SilenceUsage:  true,
		SilenceErrors: true,
		Short:         "Generate and validate production Terraform IaC from architecture documents",
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

The AI model analyses the document, asks clarifying questions when information
is missing, then generates each Terraform module. After generation it runs the
best-effort validation suite and produces documentation. Sensitive values are
masked before anything is sent to the provider.

Providers (--provider): claude (default, uses your Claude Code login),
anthropic, openai, gemini, openai-compatible. API keys come only from the
environment: ANTHROPIC_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY, TFA_API_KEY.

Exit codes: 0 ok, 1 error, 2 security stop (nothing sensitive was sent).

Supported formats: PDF, DOCX, MD, TXT`,
		Example: `  # Generate into the default output directory (<docname>-tf/)
  tf-architect run architecture.pdf

  # Specify a custom output directory
  tf-architect run architecture.pdf --output ./infra/terraform

  # Force a full re-run (discard saved session)
  tf-architect run architecture.pdf --force

  # Use a custom agents prompt / golden rules file
  tf-architect run architecture.pdf --agents-file ./my-rules.md

  # Use another provider
  OPENAI_API_KEY=... tf-architect run architecture.pdf --provider openai --model <model>

  # Self-hosted OpenAI-compatible endpoint
  TFA_API_KEY=... tf-architect run architecture.pdf --provider openai-compatible \
      --base-url https://llm.internal.example/v1 --model <model>`,
		Args: cobra.ExactArgs(1),
		RunE: runGenerate,
	}
	runCmd.Flags().StringVarP(&outputDir, "output", "o", "", "Output directory for Terraform files (default: <docname>-tf/)")
	runCmd.Flags().BoolVarP(&forceFlag, "force", "f", false, "Force full re-run, ignoring saved session state")
	runCmd.Flags().StringVar(&agentsFile, "agents-file", "", "Path to custom agents prompt file (default: built-in golden rules)")
	runCmd.Flags().StringVar(&providerFlag, "provider", "claude", "AI provider: claude, anthropic, openai, gemini, openai-compatible")
	runCmd.Flags().StringVar(&modelFlag, "model", "", "Model name (required for openai, gemini and openai-compatible)")
	runCmd.Flags().StringVar(&baseURLFlag, "base-url", "", "Override the provider endpoint (HTTPS, or http on localhost); required for openai-compatible")
	runCmd.Flags().BoolVar(&yesCustomFlag, "yes-custom-endpoint", false, "Accept a non-local --base-url without asking (for non-interactive runs)")
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
	os.Exit(reportError(os.Stderr, root.Execute()))
}

// reportError prints err and returns the process exit code: a security stop
// (anywhere in the chain) is shown as a pop-up and exits 2, any other error
// prints its message and exits 1.
func reportError(w io.Writer, err error) int {
	if err == nil {
		return 0
	}
	var se *security.Error
	if errors.As(err, &se) {
		fmt.Fprint(w, renderPopup(se))
		return 2
	}
	fmt.Fprintln(w, "Error: "+err.Error())
	return 1
}

// confirmCustomEndpoint makes sure a non-local --base-url is a conscious
// choice before any provider call: prompts are sent there. Without the flag
// and without a terminal to ask on, it stops.
func confirmCustomEndpoint(baseURL string, yes, interactive bool, in *bufio.Reader, out io.Writer) error {
	if baseURL == "" {
		return nil
	}
	local, err := llm.IsLocalURL(baseURL)
	if err != nil {
		return fmt.Errorf("--base-url: %w", err)
	}
	if local || yes {
		return nil
	}
	stop := func(what string) error {
		return security.New(what,
			"A custom endpoint receives your masked prompts and API key; sending them to an unintended host would expose them.",
			"Check the URL, then re-run with --yes-custom-endpoint (or answer y when asked) to trust it.")
	}
	if !interactive {
		return stop("Custom endpoint " + baseURL + " was not confirmed (non-interactive run).")
	}
	fmt.Fprintln(out, paint(ansiYellow, "┌─ Custom endpoint ─────────────────────────────────────────"))
	fmt.Fprintln(out, paint(ansiYellow, "│ ")+"Prompts and your API key will be sent to:")
	fmt.Fprintln(out, paint(ansiYellow, "│ ")+paint(ansiBold, baseURL))
	fmt.Fprintln(out, paint(ansiYellow, "└───────────────────────────────────────────────────────────"))
	fmt.Fprint(out, "Continue? [y/N] ")
	line, _ := in.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return stop("Custom endpoint " + baseURL + " was declined.")
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

	agents := prompts.Agents
	if agentsFile != "" {
		data, err := os.ReadFile(agentsFile)
		if err != nil {
			return fmt.Errorf("reading agents file: %w", err)
		}
		agents = string(data)
	}

	// Endpoint confirmation and provider construction happen before any
	// provider call.
	if err := confirmCustomEndpoint(baseURLFlag, yesCustomFlag, isTerminal(os.Stdin), stdin, os.Stdout); err != nil {
		return err
	}
	provider, err := llm.New(llm.Config{
		Provider:  providerFlag,
		Model:     modelFlag,
		BaseURL:   baseURLFlag,
		ClaudeBin: claudeBin,
	})
	if err != nil {
		return err
	}

	orch := orchestrator.New(workDir, outputDir, agents, provider, noMaskFlag)

	fmt.Println(paint(ansiBold, "tf-architect ") + paint(ansiDim, version))
	fmt.Println(paint(ansiDim, strings.Repeat("─", 45)))

	if !noMaskFlag {
		orch.CustomSensitive = askCustomSensitive()
	}

	orch.OnStatus = func(msg string) {
		fmt.Println(styleStatus(msg))
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

DISCLAIMER: values you do not list may be sent to the AI provider. The author of
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
// onFirst cancels the in-flight request and stops further ones, so Run
// returns normally with its state saved. A second Ctrl-C exits immediately.
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
