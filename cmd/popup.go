package main

import (
	"strings"
	"unicode/utf8"

	"tf-architect/internal/security"
)

const popupWidth = 72

// renderPopup draws a security stop as a boxed terminal pop-up. Color is
// applied per line so the box keeps equal display width once ANSI codes
// are stripped.
func renderPopup(e *security.Error) string {
	inner := popupWidth - 4 // "│ " + text + " │"
	var lines []string      // already padded to inner width
	add := func(s string) {
		lines = append(lines, s+strings.Repeat(" ", inner-utf8.RuneCountInString(s)))
	}
	section := func(label, text string) {
		prefix := label + " "
		indent := strings.Repeat(" ", len(prefix))
		for i, l := range wrapWords(text, inner-len(prefix)) {
			if i == 0 {
				add(prefix + l)
			} else {
				add(indent + l)
			}
		}
	}

	add("SECURITY STOP")
	add("")
	section("What:", e.What)
	section("Why:", e.Why)
	section("Fix:", e.Fix)
	add("")
	for _, l := range wrapWords("Nothing sensitive was sent to the AI provider.", inner) {
		add(l)
	}

	var b strings.Builder
	line := func(s string) { b.WriteString(paint(ansiBold+";"+ansiRed, s) + "\n") }
	line("┌" + strings.Repeat("─", popupWidth-2) + "┐")
	for _, l := range lines {
		line("│ " + l + " │")
	}
	line("└" + strings.Repeat("─", popupWidth-2) + "┘")
	return b.String()
}

// wrapWords greedily wraps text to width runes; over-long words are split.
func wrapWords(text string, width int) []string {
	var out []string
	cur := ""
	flush := func() {
		if cur != "" {
			out = append(out, cur)
			cur = ""
		}
	}
	for _, w := range strings.Fields(text) {
		for utf8.RuneCountInString(w) > width {
			r := []rune(w)
			flush()
			out = append(out, string(r[:width]))
			w = string(r[width:])
		}
		switch {
		case cur == "":
			cur = w
		case utf8.RuneCountInString(cur)+1+utf8.RuneCountInString(w) <= width:
			cur += " " + w
		default:
			flush()
			cur = w
		}
	}
	flush()
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}
