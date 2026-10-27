package sip

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/icholy/digest"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// Regression cases promoted from four externally executed red proofs on PR774.
// These require repaired behavior without changing the candidate's transport/auth path.
func TestGrantedExpiryPrefersRewrittenContactLifetime(t *testing.T) {
	contact := sip.Uri{Scheme: "sip", User: "reguser", Host: "127.0.0.1", Port: 15060}
	req := sip.NewRequest(sip.REGISTER, sip.Uri{Scheme: "sip", Host: "127.0.0.1"})
	req.AppendHeader(&sip.FromHeader{Params: sip.NewParams()})
	req.AppendHeader(&sip.ToHeader{Params: sip.NewParams()})
	resp := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	rewritten := contact
	rewritten.Host = "192.0.2.10"
	rewritten.Port = 45060
	resp.AppendHeader(&sip.ContactHeader{Address: rewritten, Params: sip.HeaderParams{{K: "expires", V: "60"}}})
	resp.AppendHeader(sip.NewHeader("Expires", "600"))
	require.Equal(t, 60*time.Second, grantedExpiry(resp, &contact, 600*time.Second), "rewritten binding lifetime must not be overridden by larger global Expires")
}

func TestRegistrantUnregistersUnconfirmedBinding(t *testing.T) {
	var nonzero, withdrawals atomic.Int32
	reg := newTestRegistrar(t, func(req *sip.Request) *sip.Response {
		if req.Method != sip.REGISTER {
			return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		}
		if req.GetHeader("Expires").Value() == "0" {
			withdrawals.Add(1)
			return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		}
		nonzero.Add(1) // mock registrar applied binding; deliberately lose the 200 response
		return nil
	})
	r, _ := newTestRegistrant(t, reg.addr, config.SIPRegistrationConfig{})
	r.Start()
	t.Cleanup(r.Stop)
	require.Eventually(t, func() bool { return nonzero.Load() > 0 }, time.Second, 10*time.Millisecond)
	r.Stop()
	require.Eventually(t, func() bool { return withdrawals.Load() > 0 }, 300*time.Millisecond, 10*time.Millisecond, "applied-but-unacknowledged binding must get Contact-specific unregister")
	reqs := reg.received()
	first, last := reqs[0], reqs[len(reqs)-1]
	require.NotNil(t, last.Contact())
	require.Equal(t, first.Contact().Address.String(), last.Contact().Address.String())
	require.Equal(t, first.CallID().Value(), last.CallID().Value())
	require.Greater(t, last.CSeq().SeqNo, first.CSeq().SeqNo)
	require.False(t, r.State().Registered)
}

func TestRegistrantKeepsPingingDuringBlockedRefresh(t *testing.T) {
	var registrations, options atomic.Int32
	reg := newTestRegistrar(t, func(req *sip.Request) *sip.Response {
		if req.Method == sip.OPTIONS {
			options.Add(1)
			return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		}
		if req.GetHeader("Expires").Value() == "0" {
			return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		}
		if registrations.Add(1) > 1 {
			return nil
		} // dropped refresh response, not an immediate error
		resp := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		resp.AppendHeader(sip.NewHeader("Expires", "4"))
		return resp
	})
	r, _ := newTestRegistrant(t, reg.addr, config.SIPRegistrationConfig{Keepalive: 40 * time.Millisecond})
	r.Start()
	t.Cleanup(r.Stop)
	require.Eventually(t, func() bool { return registrations.Load() >= 2 }, 3*time.Second, 10*time.Millisecond)
	before := options.Load()
	require.Eventually(t, func() bool { return options.Load() > before },
		300*time.Millisecond, 5*time.Millisecond, "OPTIONS must continue while a refresh waits on response")
}

func TestRegistrantsStopTogether(t *testing.T) {
	var withdrawing atomic.Bool
	first := newTestRegistrar(t, func(req *sip.Request) *sip.Response {
		if req.GetHeader("Expires").Value() == "0" {
			withdrawing.Store(true)
			return nil
		}
		return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	})
	second := newTestRegistrar(t, func(req *sip.Request) *sip.Response {
		if req.GetHeader("Expires").Value() == "0" {
			return nil
		}
		return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	})
	r1, _ := newTestRegistrant(t, first.addr, config.SIPRegistrationConfig{})
	r2, _ := newTestRegistrant(t, second.addr, config.SIPRegistrationConfig{})
	r1.Start()
	r2.Start()
	require.Eventually(t, func() bool { return r1.State().Registered && r2.State().Registered }, time.Second, 10*time.Millisecond)
	s := &Service{registrants: []*registrant{r1, r2}}
	done := make(chan struct{})
	started := time.Now()
	go func() { s.StopRegistrations(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
			require.Less(t, time.Since(started), unregisterTimeout+time.Second,
				"parallel withdrawals must share one global budget, not two serial budgets")
		case <-time.After(2*unregisterTimeout + time.Second):
			t.Fatal("service withdrawal failed to terminate")
		}
	})
	require.Eventually(t, func() bool { return withdrawing.Load() }, time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { return r2.stop.IsBroken() }, 200*time.Millisecond, 10*time.Millisecond, "second account must stop promptly, not wait for first account's unregister timeout")
}

// An unanswered refresh must not leave the gauge/state true after granted expiry.
func TestRegistrantLapsesDuringBlockedRefresh(t *testing.T) {
	var requests atomic.Int32
	reg := newTestRegistrar(t, func(req *sip.Request) *sip.Response {
		if req.Method == sip.OPTIONS || req.GetHeader("Expires").Value() == "0" {
			return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		}
		if requests.Add(1) > 1 {
			return nil
		}
		resp := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		resp.AppendHeader(sip.NewHeader("Expires", "3"))
		return resp
	})
	r, _ := newTestRegistrant(t, reg.addr, config.SIPRegistrationConfig{Keepalive: 20 * time.Millisecond})
	r.Start()
	t.Cleanup(r.Stop)
	require.Eventually(t, func() bool { return r.State().Registered }, time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool { return requests.Load() > 1 }, 2*time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool { return !r.State().Registered }, 2*time.Second, 5*time.Millisecond,
		"expiry must be processed while refresh is still waiting for its 10s timeout")
}

func TestGrantedExpiryRewrittenZeroAndMultipleContacts(t *testing.T) {
	contact := sip.Uri{Scheme: "sip", User: "reguser", Host: "127.0.0.1", Port: 15060}
	for _, tt := range []struct {
		name, expiry string
		want         time.Duration
	}{
		{"shortest positive", "60", 60 * time.Second}, {"removed rewritten binding", "0", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := sip.NewRequest(sip.REGISTER, sip.Uri{Scheme: "sip", Host: "127.0.0.1"})
			req.AppendHeader(&sip.FromHeader{Params: sip.NewParams()})
			req.AppendHeader(&sip.ToHeader{Params: sip.NewParams()})
			resp := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
			rewritten := contact
			rewritten.Host = "192.0.2.10"
			rewritten.Port = 45060
			resp.AppendHeader(&sip.ContactHeader{Address: rewritten, Params: sip.HeaderParams{{K: "expires", V: tt.expiry}}})
			other := contact
			other.User = "other"
			resp.AppendHeader(&sip.ContactHeader{Address: other, Params: sip.HeaderParams{{K: "expires", V: "600"}}})
			resp.AppendHeader(sip.NewHeader("Expires", "600"))
			require.Equal(t, tt.want, grantedExpiry(resp, &contact, 600*time.Second))
		})
	}
}

// REGISTER's cached Digest and monotonically increasing CSeq survive a lost 200.
func TestRegistrantWithdrawsUnconfirmedAuthenticatedBinding(t *testing.T) {
	var applied atomic.Bool
	reg := newTestRegistrar(t, func(req *sip.Request) *sip.Response {
		if req.GetHeader("Authorization") == nil {
			resp := sip.NewResponseFromRequest(req, sip.StatusUnauthorized, "Unauthorized", nil)
			resp.AppendHeader(sip.NewHeader("WWW-Authenticate", `Digest realm="registrar.test", nonce="unconfirmed-nonce", algorithm=MD5`))
			return resp
		}
		if req.GetHeader("Expires").Value() == "0" {
			return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		}
		applied.Store(true)
		return nil // authenticated binding applied; success response lost
	})
	r, _ := newTestRegistrant(t, reg.addr, config.SIPRegistrationConfig{Password: testRegPass})
	r.Start()
	t.Cleanup(r.Stop)
	require.Eventually(t, applied.Load, time.Second, 5*time.Millisecond)
	r.Stop()
	reqs := reg.received()
	require.GreaterOrEqual(t, len(reqs), 3)
	first, last := reqs[0], reqs[len(reqs)-1]
	require.Equal(t, "0", last.GetHeader("Expires").Value())
	require.Equal(t, first.CallID().Value(), last.CallID().Value())
	require.Equal(t, first.Contact().Address.String(), last.Contact().Address.String())
	require.Greater(t, last.CSeq().SeqNo, reqs[len(reqs)-2].CSeq().SeqNo)
	chal, err := digest.ParseChallenge(`Digest realm="registrar.test", nonce="unconfirmed-nonce", algorithm=MD5`)
	require.NoError(t, err)
	checkCredentials(t, last, chal, testRegUser, testRegPass)
	select {
	case <-r.done:
	default:
		t.Fatal("Stop returned without joining registration owner")
	}
}

func TestRegistrantConcurrentStopJoinsWorkers(t *testing.T) {
	reg := newTestRegistrar(t, func(req *sip.Request) *sip.Response { return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil) })
	r, _ := newTestRegistrant(t, reg.addr, config.SIPRegistrationConfig{Keepalive: time.Millisecond})
	r.Start()
	r.Start() // idempotent; cannot create a second owner
	require.Eventually(t, func() bool { return r.State().Registered }, time.Second, time.Millisecond)
	done := make(chan struct{}, 2)
	go func() { r.Stop(); done <- struct{}{} }()
	go func() { r.Stop(); done <- struct{}{} }()
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("concurrent Stop failed to join")
		}
	}
	require.False(t, r.State().Registered)
	var withdrawals int
	for _, req := range reg.received() {
		if req.Method == sip.REGISTER && req.GetHeader("Expires").Value() == "0" {
			withdrawals++
		}
	}
	require.Equal(t, 1, withdrawals, "one owner must withdraw only once")
}
