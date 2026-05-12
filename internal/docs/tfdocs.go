package docs

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const tfDocsConfig = `formatter: "markdown table"

recursive:
  enabled: true
  path: modules
  include-main: true

sections:
  show:
    - requirements
    - providers
    - modules
    - resources
    - inputs
    - outputs

output:
  file: README.md
  mode: inject
  template: |-
    <!-- BEGIN_TF_DOCS -->
    {{ .Content }}
    <!-- END_TF_DOCS -->

sort:
  enabled: true
  by: name

settings:
  anchor: true
  default: true
  description: true
  hide-empty: true
  indent: 2
  required: true
  sensitive: true
  type: true
`

// RunTFDocs writes .terraform-docs.yml and runs terraform-docs against outputDir.
func RunTFDocs(outputDir string) (string, error) {
	if _, err := exec.LookPath("terraform-docs"); err != nil {
		return "", fmt.Errorf("terraform-docs not found — install from https://terraform-docs.io")
	}

	configPath := filepath.Join(outputDir, ".terraform-docs.yml")
	if err := os.WriteFile(configPath, []byte(tfDocsConfig), 0644); err != nil {
		return "", fmt.Errorf("writing .terraform-docs.yml: %w", err)
	}
	// .terraform-docs.yml is intentionally left in the output dir — users may
	// inspect or customize it for subsequent manual runs.

	if err := ensureREADMEMarkers(outputDir); err != nil {
		return "", fmt.Errorf("ensuring README markers: %w", err)
	}

	var out bytes.Buffer
	cmd := exec.Command("terraform-docs", ".")
	cmd.Dir = outputDir
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("terraform-docs failed: %w\n%s", err, out.String())
	}
	return out.String(), nil
}

// ensureREADMEMarkers walks all dirs with .tf files and creates stub READMEs
// with BEGIN_TF_DOCS/END_TF_DOCS markers if they don't already exist.
func ensureREADMEMarkers(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if base == ".terraform" || base == ".tf-architect" || base == ".git" {
			return filepath.SkipDir
		}

		hasTF, err := dirHasTFFiles(path)
		if err != nil {
			return err
		}
		if !hasTF {
			return nil
		}

		readmePath := filepath.Join(path, "README.md")
		if _, err := os.Stat(readmePath); os.IsNotExist(err) {
			moduleName := filepath.Base(path)
			stub := fmt.Sprintf("# %s\n\n<!-- BEGIN_TF_DOCS -->\n<!-- END_TF_DOCS -->\n",
				strings.ToTitle(moduleName))
			return os.WriteFile(readmePath, []byte(stub), 0644)
		}
		return nil
	})
}

func dirHasTFFiles(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".tf") {
			return true, nil
		}
	}
	return false, nil
}
