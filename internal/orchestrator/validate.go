package orchestrator

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tf-architect/internal/fsutil"
	"tf-architect/internal/masking"
	"tf-architect/internal/state"
	"tf-architect/internal/validation"
)

// maxStageOutput bounds raw validator output forwarded to auto-fix.
const maxStageOutput = 2000

// validate runs the suite on real values and returns a masked ValidationRun.
//
// Validators need real values (a placeholder is not a valid CIDR), but
// outputDir must stay masked while Claude can read it. So the suite runs on
// a de-masked temporary copy, sharing outputDir/.terraform via TF_DATA_DIR,
// and every error is masked before it is stored or sent.
func (o *Orchestrator) validate(label string) (state.ValidationRun, error) {
	dir, cleanup, err := o.validationDir()
	if err != nil {
		return state.ValidationRun{}, err
	}
	defer cleanup()

	suite := validation.New(dir)
	if dir != o.outputDir {
		suite.Env = []string{"TF_DATA_DIR=" + filepath.Join(o.outputDir, ".terraform")}
	}
	results, suiteErr := suite.Run()
	if dir != o.outputDir {
		// Keep the provider lock file terraform init produced.
		_ = copyFile(filepath.Join(dir, ".terraform.lock.hcl"), filepath.Join(o.outputDir, ".terraform.lock.hcl"))
	}

	run := state.ValidationRun{
		RunAt:   time.Now(),
		Passed:  suiteErr == nil,
		Results: make(map[string]bool),
	}
	for _, r := range results {
		run.Results[r.Stage] = r.Passed
		icon := "✓"
		switch {
		case r.Skipped:
			icon = "–"
		case !r.Passed:
			icon = "✗"
		}
		o.status("  %s%s %s", label, icon, r.Stage)

		errs := r.Errors
		if !r.Passed && len(errs) == 0 && r.Output != "" {
			// Stages without structured errors (tflint, checkov, init).
			errs = []string{r.Stage + ": " + truncate(strings.TrimSpace(r.Output), maxStageOutput)}
		}
		for _, e := range errs {
			// Paths point at the temporary copy; Claude works in outputDir.
			e = strings.ReplaceAll(e, dir+string(filepath.Separator), "")
			e = strings.ReplaceAll(e, dir, ".")
			masked, err := o.maskText(e)
			if err != nil {
				return state.ValidationRun{}, err
			}
			run.Errors = append(run.Errors, masked)
		}
	}
	return run, nil
}

// validationDir returns the directory validators run in: outputDir itself
// when masking is off, otherwise a de-masked temporary copy of it.
func (o *Orchestrator) validationDir() (string, func(), error) {
	if o.noMask || len(o.maskMap) == 0 {
		return o.outputDir, func() {}, nil
	}
	tmp, err := os.MkdirTemp("", "tf-architect-validate-*")
	if err != nil {
		return "", nil, fmt.Errorf("creating validation dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	if err := copyTree(o.outputDir, tmp); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("copying files for validation: %w", err)
	}
	if err := masking.Unmask(tmp, o.maskMap); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("de-masking validation copy: %w", err)
	}
	return tmp, cleanup, nil
}

// copyTree copies the project files under src into dst (caches excluded).
func copyTree(src, dst string) error {
	return fsutil.WalkFiles(src, func(path string, _ os.FileInfo) error {
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		return copyFile(path, filepath.Join(dst, rel))
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "... (truncated)"
}
