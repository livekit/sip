package lksdk

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

func TestFilterTURNServers(t *testing.T) {
	tests := []struct {
		name     string
		input    []*livekit.ICEServer
		expected []*livekit.ICEServer
	}{
		{
			name: "removes turn and turns URLs, keeps stun",
			input: []*livekit.ICEServer{
				{
					Urls: []string{
						"turn:ip-10-0-0-1.host.livekit.cloud:3478?transport=udp",
						"turns:oashburn1b.turn.livekit.cloud:443?transport=tcp",
						"stun:stun.l.google.com:19302",
					},
					Username:   "user",
					Credential: "pass",
				},
			},
			expected: []*livekit.ICEServer{
				{
					Urls:       []string{"stun:stun.l.google.com:19302"},
					Username:   "user",
					Credential: "pass",
				},
			},
		},
		{
			name: "case insensitive",
			input: []*livekit.ICEServer{
				{
					Urls:       []string{"TURN:host:3478", "TURNS:host:443", "STUN:host:3478"},
					Username:   "user",
					Credential: "pass",
				},
			},
			expected: []*livekit.ICEServer{
				{
					Urls:       []string{"STUN:host:3478"},
					Username:   "user",
					Credential: "pass",
				},
			},
		},
		{
			name: "drops server entirely when all URLs are turn",
			input: []*livekit.ICEServer{
				{
					Urls: []string{
						"turn:host:3478",
						"turns:host:443?transport=tcp",
					},
					Username:   "user",
					Credential: "pass",
				},
			},
			expected: nil,
		},
		{
			name:     "nil input",
			input:    nil,
			expected: nil,
		},
		{
			name: "multiple servers",
			input: []*livekit.ICEServer{
				{
					Urls:       []string{"turn:host:3478"},
					Username:   "user1",
					Credential: "pass1",
				},
				{
					Urls:       []string{"stun:stun.l.google.com:19302"},
					Username:   "",
					Credential: "",
				},
			},
			expected: []*livekit.ICEServer{
				{
					Urls:       []string{"stun:stun.l.google.com:19302"},
					Username:   "",
					Credential: "",
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := filterTURNServers(tc.input)
			require.Equal(t, tc.expected, result)
		})
	}
}
