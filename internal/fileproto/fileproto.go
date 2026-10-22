// Package fileproto implements the text protocol in which AI models return
// files. Models have no tools, so everything they write passes through Parse,
// ValidatePath and Apply, which is where path safety is enforced.
package fileproto

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"tf-architect/internal/fsutil"
	"tf-architect/internal/security"
)

const (
	// NoChanges is the response of a review with nothing to change.
	NoChanges = "NO_CHANGES"
	// DefaultBudget is the max project size (bytes) sent in one prompt.
	DefaultBudget = 400_000

	endMarker = "===END FILE==="
)

// ErrTruncated means the response ended inside a FILE block.
var ErrTruncated = errors.New("response truncated: FILE block not closed")

// Change is one file write or deletion requested by the model.
type Change struct {
	Path    string
	Content string
	Delete  bool
}

var allowedExt = map[string]bool{
	".tf": true, ".tfvars": true, ".md": true, ".json": true,
	".tpl": true, ".yaml": true, ".yml": true, ".sh": true,
}

var forbiddenSegments = map[string]bool{
	"..": true, ".terraform": true, ".git": true, ".tf-architect": true,
}

// marker returns the path of a "===<kind>: path===" line, if line is one.
func marker(line, kind string) (string, bool) {
	prefix := "===" + kind + ":"
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "===") || len(line) < len(prefix)+3 {
		return "", false
	}
	return strings.TrimSpace(line[len(prefix) : len(line)-3]), true
}

// Parse extracts changes from a model response. Prose around blocks is
// ignored; a response with no blocks yields zero changes and no error.
// Paths are returned as written: Apply validates them.
func Parse(resp string) (changes []Change, warnings []string, err error) {
	index := map[string]int{}
	add := func(c Change) {
		if i, ok := index[c.Path]; ok {
			warnings = append(warnings, fmt.Sprintf("duplicate path %q: last block wins", c.Path))
			changes[i] = c
			return
		}
		index[c.Path] = len(changes)
		changes = append(changes, c)
	}

	var cur []string
	var curPath string
	inBlock := false
	for _, raw := range strings.Split(resp, "\n") {
		raw = strings.TrimSuffix(raw, "\r")
		if inBlock {
			if strings.TrimSpace(raw) == endMarker {
				add(Change{Path: curPath, Content: blockContent(cur)})
				inBlock = false
				continue
			}
			cur = append(cur, raw)
			continue
		}
		line := strings.TrimSpace(raw)
		if p, ok := marker(line, "FILE"); ok {
			inBlock, curPath, cur = true, p, nil
		} else if p, ok := marker(line, "DELETE"); ok {
			add(Change{Path: p, Delete: true})
		}
	}
	if inBlock {
		return nil, warnings, ErrTruncated
	}
	return changes, warnings, nil
}

// blockContent strips one pair of code fences and joins lines with a
// trailing newline.
func blockContent(lines []string) string {
	if n := len(lines); n >= 2 && strings.HasPrefix(lines[0], "```") && lines[n-1] == "```" {
		lines = lines[1 : n-1]
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// cleanPath normalizes p to a forward-slash relative path.
func cleanPath(p string) string {
	return filepath.ToSlash(filepath.Clean(p))
}

// ValidatePath reports a *security.Error unless p is a relative path inside
// the project with an allowed extension and no forbidden segment.
func ValidatePath(p string) error {
	unsafe := func(why string) error {
		return security.New(
			fmt.Sprintf("The AI returned an unsafe file path: %q.", p),
			why,
			"No file was written. Re-run the command; if it keeps happening, try another model.",
		)
	}
	if strings.TrimSpace(p) == "" {
		return unsafe("Empty file paths are not allowed.")
	}
	if strings.IndexFunc(p, isControl) >= 0 {
		return unsafe("Control characters in a path can rewrite what the terminal shows.")
	}
	norm := strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(norm, "/") || filepath.IsAbs(p) || filepath.VolumeName(p) != "" || (len(norm) > 1 && norm[1] == ':') {
		return unsafe("Absolute paths could write outside your project.")
	}
	// Check raw segments: Clean would silently resolve "a/../b".
	for _, seg := range strings.Split(norm, "/") {
		if forbiddenSegments[strings.ToLower(seg)] {
			return unsafe(fmt.Sprintf("The path segment %q is not allowed.", seg))
		}
	}
	c := cleanPath(p)
	if c == "." || !allowedExt[strings.ToLower(filepath.Ext(c))] {
		return unsafe("Only .tf, .tfvars, .md, .json, .tpl, .yaml, .yml and .sh files are allowed.")
	}
	return nil
}

// isControl reports C0 controls, DEL and C1 controls: runes that terminals
// interpret instead of printing.
func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f
}

// Printable escapes control characters (except newline and tab) in text
// that came from the model or a document, so it cannot drive the terminal
// it is shown on.
func Printable(s string) string {
	if strings.IndexFunc(s, func(r rune) bool { return isControl(r) && r != '\n' && r != '\t' }) < 0 {
		return s
	}
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t' || !isControl(r):
			sb.WriteRune(r)
		case r < 0x80:
			fmt.Fprintf(&sb, `\x%02x`, r)
		default:
			fmt.Fprintf(&sb, `\u%04x`, r)
		}
	}
	return sb.String()
}

// Apply writes (or deletes) the changes under dir. Every path is validated
// before anything is touched. transform, if non-nil, is applied to content
// before writing.
func Apply(dir string, changes []Change, transform func(string) string) error {
	for _, c := range changes {
		if err := ValidatePath(c.Path); err != nil {
			return err
		}
		if err := refuseSymlinks(dir, c.Path); err != nil {
			return err
		}
	}
	for _, c := range changes {
		full := filepath.Join(dir, filepath.FromSlash(cleanPath(c.Path)))
		if c.Delete {
			if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		content := c.Content
		if transform != nil {
			content = transform(content)
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// refuseSymlinks fails if any existing component of p under dir is a symlink,
// since writing through one could escape the project directory.
func refuseSymlinks(dir, p string) error {
	cur := dir
	for _, seg := range strings.Split(cleanPath(p), "/") {
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return security.New(
				fmt.Sprintf("The AI chose a path that goes through a symbolic link: %q.", p),
				"Writing through a symlink could modify files outside your project.",
				"Remove the symlink from the project directory, then re-run the command.",
			)
		}
	}
	return nil
}

// ReadProject returns the allowed-extension files under dir keyed by
// slash-separated relative path. It returns a *security.Error when their
// total size exceeds budget (DefaultBudget if budget <= 0).
func ReadProject(dir string, budget int) (map[string]string, error) {
	if budget <= 0 {
		budget = DefaultBudget
	}
	files := map[string]string{}
	total := 0
	err := fsutil.WalkFiles(dir, func(path string, info os.FileInfo) error {
		if !allowedExt[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		overBudget := func() error {
			return security.New(
				fmt.Sprintf("The project is larger than the %d byte limit for one AI request.", budget),
				"Sending everything would exceed the size the tool can safely mask and review.",
				"Remove generated or unrelated files from the project directory, or split it into smaller projects.",
			)
		}
		// Checked before reading, so a huge file is never loaded.
		if info.Size() > int64(budget-total) {
			return overBudget()
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		total += len(b)
		if total > budget {
			return overBudget()
		}
		files[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// Render formats files as protocol blocks sorted by path.
func Render(files map[string]string) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var sb strings.Builder
	for _, p := range paths {
		c := files[p]
		if c != "" && !strings.HasSuffix(c, "\n") {
			c += "\n"
		}
		fmt.Fprintf(&sb, "===FILE: %s===\n%s%s\n", p, c, endMarker)
	}
	return sb.String()
}
