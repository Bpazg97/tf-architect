// Package security defines the typed error returned whenever tf-architect
// aborts fail-closed to keep sensitive data away from an AI provider.
package security

// Error is a security stop: the run was aborted on purpose, with an
// explanation the user can act on.
type Error struct {
	What string // what happened
	Why  string // why it is a security problem
	Fix  string // how the user can resolve it
}

// New builds a security stop error.
func New(what, why, fix string) *Error {
	return &Error{What: what, Why: why, Fix: fix}
}

func (e *Error) Error() string { return "security stop: " + e.What }
