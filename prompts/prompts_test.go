package prompts

import (
	"strings"
	"testing"
)

func TestAgentsProviderNeutral(t *testing.T) {
	for _, want := range []string{"GOLDEN RULES", "===FILE:", "===END FILE===", "===DELETE:", "NO_CHANGES", "the end user", "## MASKED VALUES"} {
		if !strings.Contains(Agents, want) {
			t.Errorf("Agents is missing %q", want)
		}
	}
	for _, bad := range []string{"Write tool", "Bash", "Claude", "STEP_COMPLETE"} {
		if strings.Contains(Agents, bad) {
			t.Errorf("Agents must not contain %q", bad)
		}
	}
}
