package orchestrator

import (
	"strings"
	"testing"

	"tf-architect/prompts"
)

func TestWithMaskingNote(t *testing.T) {
	custom := withMaskingNote("my own prompt", false)
	if strings.Count(custom, "## MASKED VALUES") != 1 {
		t.Error("custom prompt must get the masking note appended once")
	}
	def := withMaskingNote(prompts.Agents, false)
	if def != prompts.Agents {
		t.Error("default AGENTS.md already has the section and must not be duplicated")
	}
	if got := withMaskingNote("my own prompt", true); got != "my own prompt" {
		t.Error("noMask must leave the prompt untouched")
	}
}
