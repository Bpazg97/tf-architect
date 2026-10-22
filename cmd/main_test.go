package main

import (
	"bufio"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestParseCustomSensitive(t *testing.T) {
	cases := []struct {
		in            string
		values, short []string
	}{
		{"null\n", nil, nil},
		{"\n", nil, nil},
		{"NULL", nil, nil},
		{" Phoenix , acme-corp,,null, Phoenix\n", []string{"Phoenix", "acme-corp"}, nil},
		{"ab,clientx", []string{"clientx"}, []string{"ab"}},
	}
	for _, c := range cases {
		values, short := parseCustomSensitive(c.in)
		if !reflect.DeepEqual(values, c.values) || !reflect.DeepEqual(short, c.short) {
			t.Errorf("parseCustomSensitive(%q) = %v, %v; want %v, %v", c.in, values, short, c.values, c.short)
		}
	}
}

func TestAskCustomSensitiveReadsInjectedInput(t *testing.T) {
	oldIn, oldTTY := stdin, stdinIsTerminal
	t.Cleanup(func() { stdin, stdinIsTerminal = oldIn, oldTTY })
	stdin = bufio.NewReader(strings.NewReader("Phoenix, ab\n"))
	stdinIsTerminal = func() bool { return true }
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	oldOut := os.Stdout
	os.Stdout = devNull
	t.Cleanup(func() { os.Stdout = oldOut; devNull.Close() })
	if got := askCustomSensitive(); !reflect.DeepEqual(got, []string{"Phoenix"}) {
		t.Errorf("askCustomSensitive = %v", got)
	}
	stdinIsTerminal = func() bool { return false }
	if got := askCustomSensitive(); got != nil {
		t.Errorf("non-interactive run must get no values, got %v", got)
	}
}
