// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package stats

import (
	"time"

	"github.com/frostbyte73/core"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/livekit/sip/pkg/config"
)

var durBuckets = []float64{
	// TODO
}

type CallDir bool

func (d CallDir) String() string {
	if d == Inbound {
		return "inbound"
	}
	return "outbound"
}

const (
	Inbound  = CallDir(false)
	Outbound = CallDir(true)
)

type Monitor struct {
	inviteReqRaw    prometheus.Counter
	inviteReq       *prometheus.CounterVec
	inviteAccept    *prometheus.CounterVec
	inviteErrShort  *prometheus.CounterVec
	inviteErr       *prometheus.CounterVec
	callsActive     *prometheus.GaugeVec
	callsTerminated *prometheus.CounterVec
	packetsRTP      *prometheus.CounterVec
	durSession      *prometheus.HistogramVec
	durCall         *prometheus.HistogramVec
	durJoin         *prometheus.HistogramVec

	started  core.Fuse
	shutdown core.Fuse
}

func NewMonitor() *Monitor {
	return &Monitor{
		started:  core.NewFuse(),
		shutdown: core.NewFuse(),
	}
}

func (m *Monitor) Start(conf *config.Config) error {
	m.inviteReqRaw = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "invite_requests_raw",
		Help:        "Number of unvalidated SIP INVITE requests received",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	})

	m.inviteReq = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "invite_requests",
		Help:        "Number of valid SIP INVITE requests received",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"dir", "from", "to"})

	m.inviteAccept = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "invite_accepted",
		Help:        "Number of accepted SIP INVITE requests (that matched a trunk and passed auth)",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"dir", "from", "to"})

	m.inviteErr = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "invite_error",
		Help:        "Number of rejected SIP INVITE requests",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"dir", "from", "to", "reason"})
	m.inviteErrShort = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "invite_error",
		Help:        "Number of rejected SIP INVITE requests",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"dir", "reason"})

	m.callsActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "calls_active",
		Help:        "Number of currently active SIP calls",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"dir", "from", "to"})

	m.callsTerminated = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "calls_terminated",
		Help:        "Number of calls terminated by SIP bridge",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"dir", "from", "to", "reason"})

	m.packetsRTP = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "packets_rtp",
		Help:        "Number of RTP packets sent or received by SIP bridge",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"dir", "from", "to", "op", "payload"})

	m.durSession = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "dur_session_sec",
		Help:        "SIP session duration (from INVITE to closed)",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
		Buckets:     durBuckets,
	}, []string{"dir", "from", "to"})

	m.durCall = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "dur_call_sec",
		Help:        "SIP call duration (from successful pin to closed)",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
		Buckets:     durBuckets,
	}, []string{"dir", "from", "to"})

	m.durJoin = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "dur_join_sec",
		Help:        "SIP room join duration (from INVITE to mixed room audio)",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
		Buckets:     durBuckets,
	}, []string{"dir", "from", "to"})

	prometheus.MustRegister(
		m.inviteReqRaw, m.inviteReq, m.inviteAccept, m.inviteErr, m.callsActive, m.callsTerminated,
		m.packetsRTP, m.durSession, m.durCall, m.durJoin,
	)

	m.started.Break()

	return nil
}

func (m *Monitor) Shutdown() {
	m.shutdown.Break()
}

func (m *Monitor) Stop() {
	prometheus.Unregister(m.inviteReqRaw)
	prometheus.Unregister(m.inviteReq)
	prometheus.Unregister(m.inviteAccept)
	prometheus.Unregister(m.inviteErr)
	prometheus.Unregister(m.callsActive)
	prometheus.Unregister(m.callsTerminated)
	prometheus.Unregister(m.packetsRTP)
	prometheus.Unregister(m.durSession)
	prometheus.Unregister(m.durCall)
	prometheus.Unregister(m.durJoin)
}

func (m *Monitor) CanAccept() bool {
	if !m.started.IsBroken() || m.shutdown.IsBroken() {
		return false
	}

	return true
}

func (m *Monitor) InviteReqRaw(dir CallDir) {
	m.inviteReqRaw.Inc()
}

func (m *Monitor) NewCall(dir CallDir, from, to string) *CallMonitor {
	return &CallMonitor{
		m:    m,
		dir:  dir,
		from: from,
		to:   to,
	}
}

type CallMonitor struct {
	m        *Monitor
	dir      CallDir
	from, to string
}

func (c *CallMonitor) labelsShort(l prometheus.Labels) prometheus.Labels {
	out := prometheus.Labels{"dir": c.dir.String()}
	for k, v := range l {
		out[k] = v
	}
	return out
}

func (c *CallMonitor) labels(l prometheus.Labels) prometheus.Labels {
	out := prometheus.Labels{"dir": c.dir.String(), "from": c.from, "to": c.to}
	for k, v := range l {
		out[k] = v
	}
	return out
}

func (c *CallMonitor) InviteReq() {
	c.m.inviteReq.With(c.labels(nil)).Inc()
}

func (c *CallMonitor) InviteAccept() {
	c.m.inviteAccept.With(c.labels(nil)).Inc()
}

func (c *CallMonitor) InviteErrorShort(reason string) {
	c.m.inviteErrShort.With(c.labelsShort(prometheus.Labels{"reason": reason})).Inc()
}

func (c *CallMonitor) InviteError(reason string) {
	c.m.inviteErr.With(c.labels(prometheus.Labels{"reason": reason})).Inc()
}

func (c *CallMonitor) CallStart() {
	c.m.callsActive.With(c.labels(nil)).Inc()
}

func (c *CallMonitor) CallEnd() {
	c.m.callsActive.With(c.labels(nil)).Dec()
}

func (c *CallMonitor) CallTerminate(reason string) {
	c.m.callsTerminated.With(c.labels(prometheus.Labels{"reason": reason})).Inc()
}

func (c *CallMonitor) RTPPacketSend(payloadType string) {
	c.m.packetsRTP.With(c.labels(prometheus.Labels{"op": "send", "payload": payloadType})).Inc()
}

func (c *CallMonitor) RTPPacketRecv(payloadType string) {
	c.m.packetsRTP.With(c.labels(prometheus.Labels{"op": "recv", "payload": payloadType})).Inc()
}

func (c *CallMonitor) SessionDur() func() time.Duration {
	return prometheus.NewTimer(c.m.durSession.With(c.labels(nil))).ObserveDuration
}

func (c *CallMonitor) CallDur() func() time.Duration {
	return prometheus.NewTimer(c.m.durCall.With(c.labels(nil))).ObserveDuration
}

func (c *CallMonitor) JoinDur() func() time.Duration {
	return prometheus.NewTimer(c.m.durJoin.With(c.labels(nil))).ObserveDuration
}
