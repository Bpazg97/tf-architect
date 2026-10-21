package security

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrorsAs(t *testing.T) {
	wrapped := fmt.Errorf("x: %w", New("what", "why", "fix"))
	var se *Error
	if !errors.As(wrapped, &se) {
		t.Fatal("errors.As did not match *Error")
	}
	if se.What != "what" || se.Why != "why" || se.Fix != "fix" {
		t.Errorf("fields = %+v", se)
	}
	if got := se.Error(); got != "security stop: what" {
		t.Errorf("Error() = %q", got)
	}
}
