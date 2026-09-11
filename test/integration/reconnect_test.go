package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/sip/pkg/siptest"
	"github.com/livekit/sip/test/lktest"
)

const (
	reconnectAudioTimeout = 20 * time.Second

	// The SDK retries for about a minute before it gives up and ends the call, so
	// wait past that. Failing earlier would just be impatience, not a defect.
	reconnectStateTimeout = 75 * time.Second
)

// TestSIPRoomReconnect covers what happens to a live call when the SIP service
// loses its signal connection to LiveKit and gets it back.
//
// Blocking the resume is what makes the interesting case reachable. The SDK always
// tries to resume first, and a resume restores subscriptions on its own. Only once
// a resume fails does it escalate to a full reconnect, where the subscriptions are
// ours to restore.
func TestSIPRoomReconnect(t *testing.T) {
	lk := runLiveKit(t)
	proxy := newSignalProxy(t, lk.WsUrl)
	srv := runSIPServerWithWsURL(t, lk, proxy.URL())

	t.Run("escalated reconnect", func(t *testing.T) {
		const roomName = "test-reconnect"
		room, cli, sid := startCall(t, lk, srv, roomName, "reconnect-cli", "+000000001")

		proxy.BlockResume(true)
		require.NotZero(t, proxy.Cut(), "expected a live signal connection to cut")

		// Confirms the escalation was forced rather than the SDK happening to
		// take a path we did not choose.
		require.Eventually(t, func() bool { return proxy.ResumesBlocked() > 0 },
			15*time.Second, 100*time.Millisecond,
			"no resume was attempted, so the reconnect path was never exercised")
		proxy.BlockResume(false)

		// A reconnect mints a new SID, a resume keeps it.
		newSID := waitSIPParticipant(t, lk, roomName, func(cur string) bool { return cur != sid },
			reconnectStateTimeout, "SIP participant SID never changed, so this was not a full reconnect")
		t.Log("SIP participant rejoined:", sid, "->", newSID)

		// Without re-subscribing, the call stays up with no inbound room audio.
		requireAudio(t, room, cli, "after the reconnect")
	})

	// A resume keeps the session and the SDK replays subscriptions itself, so
	// nothing on our side has to run.
	t.Run("resume", func(t *testing.T) {
		const roomName = "test-resume"
		room, cli, sid := startCall(t, lk, srv, roomName, "resume-cli", "+000000002")

		blocked := proxy.ResumesBlocked()
		require.NotZero(t, proxy.Cut(), "expected a live signal connection to cut")

		require.Eventually(t, func() bool { return proxy.Conns() > 0 },
			reconnectStateTimeout, 100*time.Millisecond,
			"signal connection was never re-established")

		newSID := waitSIPParticipant(t, lk, roomName, nil,
			reconnectStateTimeout, "SIP participant left the room")
		require.Equal(t, sid, newSID, "SID changed, so the SDK did a full reconnect rather than a resume")
		require.Equal(t, blocked, proxy.ResumesBlocked(), "proxy should not have blocked anything here")

		requireAudio(t, room, cli, "after the resume")
	})
}

// startCall puts a LiveKit participant and a SIP caller in the same room and
// returns once audio flows between them.
func startCall(t *testing.T, lk *LiveKit, srv *SIPServer, roomName, clientID, trunkNumber string) (*lktest.Participant, *siptest.Client, string) {
	t.Helper()

	nc := createTrunkAndDirect(t, srv, roomName, trunkNumber)

	room := lk.ConnectParticipant(t, roomName, "room-participant", nil)
	cli := runClient(t, nc, srv.IP, clientID, clientNumber, false, nil, nil, nil, nil)

	// Keep the caller audible for the whole test. The SIP service drops a call
	// whose media goes quiet, and an outage can last longer than that timeout.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = cli.SendSilence(ctx) }()

	sid := waitSIPParticipant(t, lk, roomName, nil,
		participantsJoinTimeout, "SIP participant never joined the room")
	requireAudio(t, room, cli, "before the outage")
	return room, cli, sid
}

// createTrunkAndDirect is CreateTrunkAndDirect with the dispatch rule scoped to
// its own trunk. The subtests share a LiveKit server, and two unscoped direct
// rules with no PIN collide.
func createTrunkAndDirect(t *testing.T, srv *SIPServer, roomName, trunkNumber string) *NumberConfig {
	t.Helper()

	trunkID := srv.CreateTrunkIn(t, &livekit.SIPInboundTrunkInfo{
		Numbers: []string{trunkNumber},
	})
	dr, err := srv.Client.CreateSIPDispatchRule(context.Background(), &livekit.CreateSIPDispatchRuleRequest{
		Name:     roomName,
		TrunkIds: []string{trunkID},
		Rule: &livekit.SIPDispatchRule{
			Rule: &livekit.SIPDispatchRule_DispatchRuleDirect{
				DispatchRuleDirect: &livekit.SIPDispatchRuleDirect{RoomName: roomName},
			},
		},
	})
	require.NoError(t, err)
	t.Log("New dispatch rule (direct):", dr.SipDispatchRuleId)

	return &NumberConfig{SIP: srv, TrunkID: trunkID, RuleID: dr.SipDispatchRuleId, Number: trunkNumber}
}

// requireAudio checks that audio flows both ways between the room and the SIP leg.
// phase names the moment being checked, so a failure says which one.
func requireAudio(t *testing.T, room *lktest.Participant, cli *siptest.Client, phase string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), reconnectAudioTimeout)
	defer cancel()
	t.Log("checking audio", phase)
	lktest.CheckAudioForParticipants(t, ctx, room, cli)
}

// waitSIPParticipant polls until the room holds exactly one SIP participant whose
// SID satisfies cond, and returns that SID. A nil cond accepts any SID. A reconnect
// can briefly leave the previous participant behind, so wait for the room to settle
// on one.
func waitSIPParticipant(t *testing.T, lk *LiveKit, room string, cond func(sid string) bool, timeout time.Duration, msg string) string {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		var sids []string
		for _, p := range lk.RoomParticipants(t, room) {
			if p.Kind == livekit.ParticipantInfo_SIP {
				sids = append(sids, p.Sid)
			}
		}
		if len(sids) == 1 && (cond == nil || cond(sids[0])) {
			return sids[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (SIP participants: %v)", msg, sids)
			return ""
		}
		time.Sleep(250 * time.Millisecond)
	}
}
