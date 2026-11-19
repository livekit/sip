package sip

import (
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
	"github.com/livekit/sipgo/sip"
	"github.com/stretchr/testify/require"
)

type registrationStopManager struct {
	CallManager
	beforeStop func()
}

func (m *registrationStopManager) Stop() {
	m.beforeStop()
	m.CallManager.Stop()
}

// Exercise the real service/manager/listener seam, not just a standalone registrant.
func TestServiceRegistrationLifecycle(t *testing.T) {
	for _, drainFirst := range []bool{false, true} {
		t.Run("drain first "+strconv.FormatBool(drainFirst), func(t *testing.T) {
			var withdrawals atomic.Int32
			reg := newTestRegistrar(t, func(req *sip.Request) *sip.Response {
				resp := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
				if req.Method == sip.REGISTER {
					if req.GetHeader("Expires").Value() == "0" {
						withdrawals.Add(1)
					}
					resp.AppendHeader(&sip.ContactHeader{Address: cloneURI(req.Contact().Address), Params: sip.HeaderParams{{K: "expires", V: req.GetHeader("Expires").Value()}}})
				}
				return resp
			})
			host, portText, err := net.SplitHostPort(freeAddr(t))
			require.NoError(t, err)
			port, err := strconv.Atoi(portText)
			require.NoError(t, err)
			conf := &config.Config{
				SIPPort: port, SIPPortListen: port, ListenIP: host,
				RTPPort:           rtcconfig.PortRange{Start: testPortRTPMin, End: testPortRTPMax},
				MaxCpuUtilization: 0.9,
				SIPRegistrations: []config.SIPRegistrationConfig{{
					Registrar: reg.addr, Username: testRegUser, Password: testRegPass,
					Expiry: time.Minute, Keepalive: -1,
				}},
			}
			mon, err := stats.NewMonitor(conf)
			require.NoError(t, err)
			log := logger.NewTestLogger(t)
			manager, err := NewCallManager("", conf, log, mon, newTestNonces(t), func(string, *rpc.SIPCallObservability, *livekit.SIPCallInfo) StateHandler {
				return NewRPCStateHandler(nil)
			})
			require.NoError(t, err)
			managerStopped := false
			var withdrawalsAtManagerStop int32
			wrapped := &registrationStopManager{CallManager: manager, beforeStop: func() {
				managerStopped = true
				withdrawalsAtManagerStop = withdrawals.Load()
			}}
			svc, err := NewService(conf, mon, log, wrapped)
			require.NoError(t, err)
			var stopOnce sync.Once
			stop := func() { stopOnce.Do(svc.Stop) }
			t.Cleanup(stop)
			svc.SetHandler(TestHandler{})
			require.NoError(t, svc.Start())
			require.Len(t, svc.registrants, 1)
			require.Eventually(t, func() bool { return svc.registrants[0].State().Registered }, time.Second, 5*time.Millisecond)
			_, sourcePort, err := net.SplitHostPort(reg.received()[0].Source())
			require.NoError(t, err)
			require.Equal(t, portText, sourcePort, "REGISTER must use the service's listening socket")
			if drainFirst {
				svc.StopRegistrations()
				require.False(t, managerStopped, "withdrawal must leave the manager available to drain existing calls")
				require.Equal(t, ActiveCalls{}, svc.ActiveCalls())
			}
			stop()
			require.True(t, managerStopped)
			require.EqualValues(t, 1, withdrawalsAtManagerStop, "withdraw before stopping the call manager and signaling transport")
			require.EqualValues(t, 1, withdrawals.Load(), "drain followed by Stop must not withdraw twice")
		})
	}
}
