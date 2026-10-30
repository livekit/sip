package sip

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAttrsToHeaders(t *testing.T) {
	cases := []struct {
		name      string
		attrs     map[string]string
		attrToHdr map[string]string
		headers   map[string]string
		exp       map[string]string
	}{
		{
			name:      "no mapping",
			attrs:     map[string]string{"a": "1"},
			attrToHdr: nil,
			headers:   nil,
			exp:       nil,
		},
		{
			name:      "valid value",
			attrs:     map[string]string{"a": "value"},
			attrToHdr: map[string]string{"a": "X-A"},
			exp:       map[string]string{"X-A": "value"},
		},
		{
			name:      "missing attribute",
			attrs:     map[string]string{},
			attrToHdr: map[string]string{"a": "X-A"},
			exp:       map[string]string{},
		},
		{
			name:      "keeps existing headers",
			attrs:     map[string]string{"a": "value"},
			attrToHdr: map[string]string{"a": "X-A"},
			headers:   map[string]string{"X-B": "other"},
			exp:       map[string]string{"X-A": "value", "X-B": "other"},
		},
		{
			name:      "drops CRLF injection",
			attrs:     map[string]string{"a": "value\r\nX-Injected: value", "b": "ok"},
			attrToHdr: map[string]string{"a": "X-A", "b": "X-B"},
			exp:       map[string]string{"X-B": "ok"},
		},
		{
			name:      "drops control characters",
			attrs:     map[string]string{"a": "val\x00ue"},
			attrToHdr: map[string]string{"a": "X-A"},
			exp:       map[string]string{},
		},
		{
			name:      "drops too long value",
			attrs:     map[string]string{"a": strings.Repeat("a", 1025)},
			attrToHdr: map[string]string{"a": "X-A"},
			exp:       map[string]string{},
		},
		{
			name:      "invalid value does not overwrite existing header",
			attrs:     map[string]string{"a": "bad\nvalue"},
			attrToHdr: map[string]string{"a": "X-A"},
			headers:   map[string]string{"X-A": "original"},
			exp:       map[string]string{"X-A": "original"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := AttrsToHeaders(c.attrs, c.attrToHdr, c.headers)
			require.Equal(t, c.exp, got)
		})
	}
}
