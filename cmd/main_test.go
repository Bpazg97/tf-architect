package main

import (
	"reflect"
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
