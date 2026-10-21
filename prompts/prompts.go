// Package prompts embeds the default system prompt into the binary.
package prompts

import _ "embed"

// System is the default system prompt (golden rules).
//
//go:embed system.md
var System string
