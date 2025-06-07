package sip

import (
	"testing"

	"github.com/livekit/sipgo/sip"
	"github.com/stretchr/testify/require"
)

func TestSIPInbound_SetFromUser(t *testing.T) {
	testCases := []struct {
		name     string
		initial  *sip.FromHeader
		newUser  string
		expected string
	}{
		{
			name: "sets user when From header exists",
			initial: &sip.FromHeader{
				Address: sip.Uri{
					User: "olduser",
				},
			},
			newUser:  "newuser",
			expected: "newuser",
		},
		{
			name:     "handles nil From header",
			initial:  nil,
			newUser:  "newuser",
			expected: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			inbound := &sipInbound{
				from: tc.initial,
			}
			inbound.SetFromUser(tc.newUser)

			if tc.initial == nil {
				require.Equal(t, "", inbound.From().User)
			} else {
				require.Equal(t, tc.expected, inbound.From().User)
			}
		})
	}
}

func TestSIPInbound_SetToUser(t *testing.T) {
	testCases := []struct {
		name     string
		initial  *sip.ToHeader
		newUser  string
		expected string
	}{
		{
			name: "sets user when To header exists",
			initial: &sip.ToHeader{
				Address: sip.Uri{
					User: "olduser",
				},
			},
			newUser:  "newuser",
			expected: "newuser",
		},
		{
			name:     "handles nil To header",
			initial:  nil,
			newUser:  "newuser",
			expected: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			inbound := &sipInbound{
				to: tc.initial,
			}
			inbound.SetToUser(tc.newUser)

			if tc.initial == nil {
				require.Equal(t, "", inbound.To().User)
			} else {
				require.Equal(t, tc.expected, inbound.To().User)
			}
		})
	}
}
