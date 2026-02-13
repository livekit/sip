package sip

import (
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
)

func TestTokenAttrsForJoin(t *testing.T) {
	t.Run("includes only sip scoped attrs", func(t *testing.T) {
		attrs := map[string]string{
			livekit.AttrSIPCallID:                          "call-id",
			livekit.AttrSIPHeaderPrefix + "x-rc-ctx":       "header-context",
			livekit.AttrSIPPrefix + "telnyx.callSessionID": "session-id",
			"projectID": "project-123",
		}

		got := tokenAttrsForJoin(attrs)

		require.Len(t, got, 3)
		require.Equal(t, "call-id", got[livekit.AttrSIPCallID])
		require.Equal(t, "header-context", got[livekit.AttrSIPHeaderPrefix+"x-rc-ctx"])
		require.Equal(t, "session-id", got[livekit.AttrSIPPrefix+"telnyx.callSessionID"])
		require.NotContains(t, got, "projectID")
	})

	t.Run("returns nil when attrs are empty", func(t *testing.T) {
		require.Nil(t, tokenAttrsForJoin(nil))
		require.Nil(t, tokenAttrsForJoin(map[string]string{}))
	})
}
