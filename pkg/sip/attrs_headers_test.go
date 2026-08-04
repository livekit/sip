// Copyright 2026 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sip

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression for livekit/sip#404: when the agent deletes the room before SIP
// sends BYE, LocalParticipant is gone. attributes_to_headers must still map
// from the last cached participant attributes.
func TestFillHeadersUsesCachedAttrsWhenRoomNil(t *testing.T) {
	call := &inboundCall{
		attrsToHdr: map[string]string{
			"sip.custom": "X-Custom-Header",
		},
	}
	call.storeParticipantAttrs(map[string]string{
		"sip.custom": "value-from-cache",
		"other":      "ignored",
	})
	call.lkRoom = nil
	cc := &sipInbound{call: call}

	headers := cc.fillHeaders(nil)
	require.Equal(t, map[string]string{"X-Custom-Header": "value-from-cache"}, headers)

	// No mapping configured → leave headers untouched.
	call.attrsToHdr = nil
	require.Nil(t, cc.fillHeaders(nil))

	// Mapping configured but cache empty → leave headers untouched.
	call.attrsToHdr = map[string]string{"sip.custom": "X-Custom-Header"}
	call.attrsMu.Lock()
	call.cachedAttrs = nil
	call.attrsMu.Unlock()
	require.Nil(t, cc.fillHeaders(nil))
}

func TestOutboundSetAttrsToHeadersUsesCachedAttrsWhenRoomNil(t *testing.T) {
	call := &outboundCall{
		sipConf: sipOutboundConfig{
			attrsToHeaders: map[string]string{
				"sip.custom": "X-Custom-Header",
			},
		},
	}
	call.storeParticipantAttrs(map[string]string{
		"sip.custom": "outbound-cache",
	})
	call.lkRoom = nil

	headers := call.setAttrsToHeaders(nil)
	require.Equal(t, map[string]string{"X-Custom-Header": "outbound-cache"}, headers)
}

func TestAttrsToHeaders(t *testing.T) {
	attrs := map[string]string{"a": "1", "b": "2"}
	mapping := map[string]string{"a": "X-A", "missing": "X-Missing"}
	headers := AttrsToHeaders(attrs, mapping, map[string]string{"Keep": "yes"})
	require.Equal(t, map[string]string{
		"Keep": "yes",
		"X-A":  "1",
	}, headers)
}
