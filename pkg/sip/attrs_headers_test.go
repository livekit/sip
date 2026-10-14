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

// snapshotParticipantAttrs skips a read that returns no attributes, so a
// teardown-time empty read does not wipe the cached values that BYE's
// attributes_to_headers relies on (livekit/sip#404). This is exercised on
// the lkRoom == nil path here: the function returns early without touching
// the cache. The "room up, attributes empty" path is the same guard, but
// needs a live lksdk participant so it is left to integration.
func TestSnapshotParticipantAttrsDoesNotWipeCacheOnEmptyRead(t *testing.T) {
	for _, setup := range []func() *inboundCall{
		func() *inboundCall {
			c := &inboundCall{}
			c.storeParticipantAttrs(map[string]string{"sip.custom": "seeded"})
			c.snapshotParticipantAttrs() // lkRoom nil → early return, cache intact
			return c
		},
	} {
		c := setup()
		c.attrsMu.Lock()
		got := c.cachedAttrs
		c.attrsMu.Unlock()
		require.Equal(t, map[string]string{"sip.custom": "seeded"}, got,
			"empty/nil room read must not wipe cached attrs")
	}
}

func TestOutboundSnapshotParticipantAttrsDoesNotWipeCacheOnEmptyRead(t *testing.T) {
	c := &outboundCall{}
	c.storeParticipantAttrs(map[string]string{"sip.custom": "outbound-seeded"})
	c.snapshotParticipantAttrs() // lkRoom nil → early return, cache intact
	c.attrsMu.Lock()
	got := c.cachedAttrs
	c.attrsMu.Unlock()
	require.Equal(t, map[string]string{"sip.custom": "outbound-seeded"}, got,
		"empty/nil room read must not wipe cached attrs")
}
