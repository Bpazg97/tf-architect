package docs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeClient struct {
	resp   string
	prompt string
}

func (f *fakeClient) Query(_ context.Context, prompt string) (string, error) {
	f.prompt = prompt
	return f.resp, nil
}

const allSections = `===BEGIN_ROOT_README===
Readme CIDR_PRIVATE_001
===END_ROOT_README===
===BEGIN_ARCHITECTURE===
Net CIDR_PRIVATE_001
===END_ARCHITECTURE===
===BEGIN_RUNBOOK===
Runbook CIDR_PRIVATE_001
===END_RUNBOOK===
===BEGIN_VARIABLES===
Vars CIDR_PRIVATE_001
===END_VARIABLES===
===BEGIN_PROJECT_AGENTS===
### Layout
VPC CIDR_PRIVATE_001
===END_PROJECT_AGENTS===`

func unmaskCIDR(s string) string { return strings.ReplaceAll(s, "CIDR_PRIVATE_001", "10.0.0.0/16") }

func run(t *testing.T, c *fakeClient, dir string) {
	t.Helper()
	err := GenerateArchitecturalDocs(context.Background(), c, SessionData{DocPath: "arch.md"}, dir, "BASE RULES", "PROJECT FILES", unmaskCIDR, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDocsWritesAgentsMD(t *testing.T) {
	dir := t.TempDir()
	run(t, &fakeClient{resp: allSections}, dir)

	b, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	ag := string(b)
	if !strings.HasPrefix(ag, "> This file contains real infrastructure values.") {
		t.Errorf("missing header:\n%s", ag)
	}
	if !strings.Contains(ag, "BASE RULES") || !strings.Contains(ag, "## Project") || !strings.Contains(ag, "10.0.0.0/16") {
		t.Errorf("AGENTS.md incomplete:\n%s", ag)
	}
	filepath.Walk(dir, func(p string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() {
			if c, _ := os.ReadFile(p); strings.Contains(string(c), "CIDR_PRIVATE_001") {
				t.Errorf("%s holds a placeholder", p)
			}
		}
		return nil
	})
}

func TestDocsAgentsMDWithoutProjectSection(t *testing.T) {
	dir := t.TempDir()
	var warned bool
	c := &fakeClient{resp: "===BEGIN_ARCHITECTURE===\nA\n===END_ARCHITECTURE==="}
	err := GenerateArchitecturalDocs(context.Background(), c, SessionData{}, dir, "BASE RULES", "P", unmaskCIDR, func(m string) { warned = warned || strings.Contains(m, "⚠") })
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if !strings.Contains(string(b), "BASE RULES") || strings.Contains(string(b), "## Project") {
		t.Errorf("unexpected AGENTS.md:\n%s", b)
	}
	if !warned {
		t.Error("no warning for the missing section")
	}
}

func TestDocsPromptHasTaskHeaderAndProject(t *testing.T) {
	c := &fakeClient{resp: allSections}
	run(t, c, t.TempDir())
	if !strings.HasPrefix(c.prompt, "## TASK: DOCS") || !strings.Contains(c.prompt, "PROJECT FILES") {
		t.Errorf("prompt lacks header or project:\n%s", c.prompt)
	}
}

func TestChangelogNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	cl := filepath.Join(dir, "docs", "CHANGELOG.md")
	if err := os.MkdirAll(filepath.Dir(cl), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cl, []byte("mine"), 0644); err != nil {
		t.Fatal(err)
	}
	run(t, &fakeClient{resp: allSections}, dir)
	if b, _ := os.ReadFile(cl); string(b) != "mine" {
		t.Errorf("changelog overwritten: %s", b)
	}
}
