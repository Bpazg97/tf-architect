package main

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"tf-architect/internal/security"
)

func TestRenderPopup(t *testing.T) {
	old := colorEnabled
	defer func() { colorEnabled = old }()

	e := security.New("Masking failed for a file", strings.Repeat("long reason ", 12), "Run again with a valid configuration")
	for _, color := range []bool{false, true} {
		colorEnabled = color
		out := renderPopup(e)
		for _, want := range []string{e.What, "Why:", "Fix:", "SECURITY STOP", "Nothing sensitive was sent to the AI provider."} {
			if !strings.Contains(out, want) {
				t.Errorf("color=%v: missing %q", color, want)
			}
		}
		if !color && strings.Contains(out, "\033") {
			t.Error("ANSI codes present with color disabled")
		}
		plain := out
		if color {
			plain = ansiRe.ReplaceAllString(out, "")
		}
		lines := strings.Split(strings.TrimRight(plain, "\n"), "\n")
		w := utf8.RuneCountInString(lines[0])
		for _, l := range lines {
			if utf8.RuneCountInString(l) != w {
				t.Errorf("color=%v: width %d != %d for %q", color, utf8.RuneCountInString(l), w, l)
			}
		}
	}
}

var ansiRe = regexp.MustCompile(`\033\[[0-9;]*m`)
