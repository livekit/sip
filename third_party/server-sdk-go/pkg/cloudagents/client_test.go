package cloudagents

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetAgentsURL(t *testing.T) {
	tests := []struct {
		name         string
		projectURL   string
		serverRegion string
		expected     string
	}{
		{
			name:       "standard https project URL",
			projectURL: "https://my-project.livekit.cloud",
			expected:   "https://agents.livekit.cloud",
		},
		{
			name:       "wss project URL",
			projectURL: "wss://my-project.livekit.cloud",
			expected:   "https://agents.livekit.cloud",
		},
		{
			name:       "subdomain containing localhost",
			projectURL: "wss://andrewnitu-localhost-project.livekit.cloud",
			expected:   "https://agents.livekit.cloud",
		},
		{
			name:       "subdomain containing 127",
			projectURL: "https://test-127-app.livekit.cloud",
			expected:   "https://agents.livekit.cloud",
		},
		{
			name:         "with server region",
			projectURL:   "https://my-project.livekit.cloud",
			serverRegion: "osbxash1a",
			expected:     "https://osbxash1a.agents.livekit.cloud",
		},
		{
			name:         "wss with server region",
			projectURL:   "wss://my-project.livekit.cloud",
			serverRegion: "osbxmum1a",
			expected:     "https://osbxmum1a.agents.livekit.cloud",
		},
		{
			name:       "localhost development URL",
			projectURL: "http://localhost:7880",
			expected:   "http://localhost:7880",
		},
		{
			name:       "127.0.0.1 development URL",
			projectURL: "http://127.0.0.1:7880",
			expected:   "http://127.0.0.1:7880",
		},
		{
			name:       "staging project URL",
			projectURL: "https://my-project.staging.livekit.cloud",
			expected:   "https://agents.staging.livekit.cloud",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{projectURL: tt.projectURL}
			result := c.getAgentsURL(tt.serverRegion)
			require.Equal(t, tt.expected, result)
		})
	}
}
