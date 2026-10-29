// Package prompts embeds the provider-neutral AGENTS.md into the binary.
package prompts

import _ "embed"

// Agents is the provider-neutral prompt: the system prompt for every provider
// and the base of the AGENTS.md written into generated projects.
//
//go:embed AGENTS.md
var Agents string
