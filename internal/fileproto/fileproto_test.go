package fileproto

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"tf-architect/internal/security"
)

func TestParseBasic(t *testing.T) {
	resp := "===FILE: main.tf===\nresource \"a\" \"b\" {}\n===END FILE===\n" +
		"===FILE: modules/x/variables.tf===\nvariable \"v\" {}\n===END FILE===\n" +
		"===DELETE: old.tf===\n"
	ch, warns, err := Parse(resp)
	if err != nil || len(warns) != 0 {
		t.Fatalf("err=%v warns=%v", err, warns)
	}
	want := []Change{
		{Path: "main.tf", Content: "resource \"a\" \"b\" {}\n"},
		{Path: "modules/x/variables.tf", Content: "variable \"v\" {}\n"},
		{Path: "old.tf", Delete: true},
	}
	if !reflect.DeepEqual(ch, want) {
		t.Fatalf("got %#v", ch)
	}
}

func TestParseNoChanges(t *testing.T) {
	ch, _, err := Parse("NO_CHANGES")
	if err != nil || len(ch) != 0 {
		t.Fatalf("ch=%v err=%v", ch, err)
	}
	ch, _, err = Parse("")
	if err != nil || len(ch) != 0 {
		t.Fatalf("empty: ch=%v err=%v", ch, err)
	}
	ch, _, err = Parse("I could not do that, sorry.")
	if err != nil || len(ch) != 0 {
		t.Fatalf("prose: ch=%v err=%v", ch, err)
	}
}

func TestParseTruncated(t *testing.T) {
	_, _, err := Parse("===FILE: a.tf===\nfoo\n")
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err=%v", err)
	}
}

func TestParseTolerant(t *testing.T) {
	resp := "Here you go:\r\n===FILE: a.tf===\r\n```hcl\r\nfoo {}\r\n```\r\n===END FILE===\r\nHope it helps.\r\n"
	ch, _, err := Parse(resp)
	if err != nil || len(ch) != 1 {
		t.Fatalf("ch=%v err=%v", ch, err)
	}
	if strings.ContainsAny(ch[0].Content, "\r") || strings.Contains(ch[0].Content, "```") {
		t.Fatalf("content not clean: %q", ch[0].Content)
	}
	if ch[0].Content != "foo {}\n" {
		t.Fatalf("content=%q", ch[0].Content)
	}
}

func TestParseDuplicatePathLastWins(t *testing.T) {
	resp := "===FILE: a.tf===\none\n===END FILE===\n===FILE: a.tf===\ntwo\n===END FILE===\n"
	ch, warns, err := Parse(resp)
	if err != nil || len(ch) != 1 || ch[0].Content != "two\n" || len(warns) != 1 {
		t.Fatalf("ch=%v warns=%v err=%v", ch, warns, err)
	}
}

func TestValidatePathRejects(t *testing.T) {
	for _, p := range []string{"/etc/x.tf", "../x.tf", "a/../../x.tf", ".terraform/m.tf", ".git/config.tf", ".Git/config.tf", ".GIT/x.tf", ".Terraform/x.tf", "a/.TF-Architect/s.json", ".tf-architect/s.json", "x.exe", ""} {
		err := ValidatePath(p)
		var se *security.Error
		if !errors.As(err, &se) {
			t.Errorf("%q: want *security.Error, got %v", p, err)
		}
	}
	if err := ValidatePath("modules/vpc/main.tf"); err != nil {
		t.Fatal(err)
	}
}

func TestApplyTransformAndDelete(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old.tf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Apply(dir, []Change{
		{Path: "mod/a.tf", Content: "abc"},
		{Path: "old.tf", Delete: true},
		{Path: "missing.tf", Delete: true},
	}, strings.ToUpper)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "mod", "a.tf"))
	if err != nil || string(b) != "ABC" {
		t.Fatalf("b=%q err=%v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.tf")); !os.IsNotExist(err) {
		t.Fatalf("old.tf still exists: %v", err)
	}
	if err := Apply(dir, []Change{{Path: "../evil.tf", Content: "x"}}, nil); err == nil {
		t.Fatal("unsafe path accepted")
	}
}

func TestReadProjectBudget(t *testing.T) {
	dir := t.TempDir()
	write := func(p, c string) {
		full := filepath.Join(dir, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.tf", "1234567890")
	write("mod/v.tf", "1234567890")
	write(".terraform/p.tf", "hidden")
	write("x.bin", "bin")
	files, err := ReadProject(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files["main.tf"] == "" || files["mod/v.tf"] == "" {
		t.Fatalf("files=%v", files)
	}
	_, err = ReadProject(dir, 15)
	var se *security.Error
	if !errors.As(err, &se) {
		t.Fatalf("want *security.Error, got %v", err)
	}
}

func TestRenderRoundTrip(t *testing.T) {
	m := map[string]string{"b.tf": "bbb\n", "a/c.md": "# hi\n\ntext\n"}
	ch, _, err := Parse(Render(m))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range ch {
		got[c.Path] = c.Content
	}
	if !reflect.DeepEqual(got, m) {
		t.Fatalf("got %v", got)
	}
}

func TestApplyRefusesSymlinks(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "victim.tf")
	if err := os.WriteFile(target, []byte("orig"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Skip("symlinks unsupported")
	}
	if err := os.Symlink(target, filepath.Join(dir, "main.tf")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"link/new.tf", "link/victim.tf", "main.tf"} {
		err := Apply(dir, []Change{{Path: p, Content: "pwn"}}, nil)
		var se *security.Error
		if !errors.As(err, &se) {
			t.Errorf("%s: want *security.Error, got %v", p, err)
		}
	}
	if err := Apply(dir, []Change{{Path: "main.tf", Delete: true}}, nil); err == nil {
		t.Error("delete through symlink accepted")
	}
	if b, _ := os.ReadFile(target); string(b) != "orig" {
		t.Fatalf("outside file modified: %q", b)
	}
	if _, err := os.Stat(filepath.Join(outside, "new.tf")); !os.IsNotExist(err) {
		t.Fatal("file created outside project")
	}
}
