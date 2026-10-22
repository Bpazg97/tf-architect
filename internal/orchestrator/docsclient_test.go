package orchestrator

import (
	"context"
	"errors"
	"testing"
)

// seqProvider fails every call with the next error of errs.
type seqProvider struct{ errs []error }

func (s *seqProvider) Name() string { return "seq" }

func (s *seqProvider) Complete(context.Context, string, string) (string, error) {
	err := s.errs[0]
	s.errs = s.errs[1:]
	return "", err
}

func TestDocsClientRecordsFirstError(t *testing.T) {
	first, second := errors.New("first"), errors.New("second")
	o := New(t.TempDir(), t.TempDir(), "", &seqProvider{errs: []error{first, second}}, true)
	dc := &docsClient{o: o}
	for _, want := range []error{first, second} {
		if _, err := dc.Query(context.Background(), "p"); !errors.Is(err, want) {
			t.Fatalf("Query returned %v, want %v", err, want)
		}
	}
	if !errors.Is(dc.err, first) {
		t.Errorf("recorded %v, want the first error", dc.err)
	}
}
