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
	"errors"
	"sync/atomic"
	"time"

	"github.com/frostbyte73/core"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/livekit/protocol/utils/hwstats"

	"github.com/livekit/sip/pkg/config"
)

// Durations are in seconds
var (
	// durBucketsOp lists histogram buckets for relatively short operations like SIP INVITE.
	durBucketsOp = []float64{
		0.1, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 3 * 60,
	}
	// durBucketsLong lists histogram buckets for long operations like call/session durations.
	durBucketsLong = []float64{
		1, 10, 60, 10 * 60, 30 * 60, 3600, 6 * 3600, 12 * 3600, 24 * 3600,
	}
	sizeBuckets = []float64{
		100, 250, 500, 750, 1000, 1250, 1500,
	}
)

type CallDir bool

func (d CallDir) String() string {
	if d == Inbound {
		return "in"
	}
	return "out"
}

const (
	Inbound  = CallDir(false)
	Outbound = CallDir(true)
)

type Monitor struct {
	nodeID string

	inviteReqRaw       prometheus.Counter
	inviteReq          *prometheus.CounterVec
	inviteAccept       *prometheus.CounterVec
	inviteErr          *prometheus.CounterVec
	callsActive        *prometheus.GaugeVec
	callsTerminated    *prometheus.CounterVec
	packetsRTP         *prometheus.CounterVec
	durSession         *prometheus.HistogramVec
	durCall            *prometheus.HistogramVec
	durJoin            *prometheus.HistogramVec
	durCheck           *prometheus.HistogramVec
	cpuLoad            prometheus.Gauge
	sdpSize            *prometheus.HistogramVec
	nodeAvailable      prometheus.GaugeFunc
	transfersTotal     *prometheus.CounterVec
	transfersSucceeded *prometheus.CounterVec
	transfersFailed    *prometheus.CounterVec
	transfersActive    *prometheus.GaugeVec

	cpu            *hwstats.CPUStats
	maxUtilization float64

	metrics  []prometheus.Collector
	started  core.Fuse
	shutdown core.Fuse
}

func NewMonitor(conf *config.Config) (*Monitor, error) {
	m := &Monitor{
		nodeID:         conf.NodeID,
		maxUtilization: conf.MaxCpuUtilization,
	}
	cpu, err := hwstats.NewCPUStats(func(idle float64) {
		if m.started.IsBroken() {
			m.cpuLoad.Set(1 - idle/m.cpu.NumCPU())
		}
	})
	if err != nil {
		return nil, err
	}
	m.cpu = cpu
	return m, nil
}

func mustRegister[T prometheus.Collector](m *Monitor, c T) T {
	err := prometheus.Register(c)
	if err != nil {
		var e prometheus.AlreadyRegisteredError
		if errors.As(err, &e) {
			return e.ExistingCollector.(T)
		} else {
			panic(err)
		}
	}
	m.metrics = append(m.metrics, c)
	return c
}

func (m *Monitor) Start(conf *config.Config) error {
	prometheus.Unregister(collectors.NewGoCollector())
	mustRegister(m, collectors.NewGoCollector(collectors.WithGoCollectorRuntimeMetrics(collectors.MetricsAll)))

	m.inviteReqRaw = mustRegister(m, prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "invite_requests_raw",
		Help:        "Number of unvalidated SIP INVITE requests received",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}))

	m.inviteReq = mustRegister(m, prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "invite_requests",
		Help:        "Number of valid SIP INVITE requests received",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"dir"}))

	m.inviteAccept = mustRegister(m, prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "invite_accepted",
		Help:        "Number of accepted SIP INVITE requests (that matched a trunk and passed auth)",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"dir", "to"}))

	m.inviteErr = mustRegister(m, prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "invite_error",
		Help:        "Number of rejected SIP INVITE requests",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"dir", "to", "reason"}))

	m.callsActive = mustRegister(m, prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "calls_active",
		Help:        "Number of currently active SIP calls",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"dir", "to"}))

	m.callsTerminated = mustRegister(m, prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "calls_terminated",
		Help:        "Number of calls terminated by SIP bridge",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"dir", "to", "reason"}))

	m.packetsRTP = mustRegister(m, prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "packets_rtp",
		Help:        "Number of RTP packets sent or received by SIP bridge",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"dir", "to", "op", "payload"}))

	m.durSession = mustRegister(m, prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "dur_session_sec",
		Help:        "SIP session duration (from INVITE to closed)",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
		Buckets:     durBucketsLong,
	}, []string{"dir"}))

	m.durCall = mustRegister(m, prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "dur_call_sec",
		Help:        "SIP call duration (from successful pin to closed)",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
		Buckets:     durBucketsLong,
	}, []string{"dir"}))

	m.durCheck = mustRegister(m, prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "dur_check_sec",
		Help:        "SIP call check duration (from INVITE to an initial dispatch response)",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
		Buckets:     durBucketsOp,
	}, []string{"dir"}))

	m.durJoin = mustRegister(m, prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "dur_join_sec",
		Help:        "SIP room join duration (from INVITE to mixed room audio)",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
		Buckets:     durBucketsOp,
	}, []string{"dir"}))

	m.sdpSize = mustRegister(m, prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "sdp_size_bytes",
		Help:        "SDP size in bytes",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
		Buckets:     sizeBuckets,
	}, []string{"type"}))

	m.nodeAvailable = mustRegister(m, prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "available",
		Help:        "Whether node can accept new requests",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, func() float64 {
		if m.Health() == HealthOK {
			return 1
		}
		return 0
	}))

	m.cpuLoad = mustRegister(m, prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   "livekit",
		Subsystem:   "node",
		Name:        "cpu_load",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID, "node_type": "SIP"},
	}))

	m.transfersTotal = mustRegister(m, prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "transfers_total",
		Help:        "Total number of SIP transfer attempts",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"dir"}))

	m.transfersSucceeded = mustRegister(m, prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "transfers_succeeded_total",
		Help:        "Total number of successful SIP transfers",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"dir"}))

	m.transfersFailed = mustRegister(m, prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "transfers_failed_total",
		Help:        "Total number of failed SIP transfers",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"dir", "reason"}))

	m.transfersActive = mustRegister(m, prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "transfers_active",
		Help:        "Number of currently active SIP transfers",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"dir"}))

	m.started.Break()

	return nil
}

func (m *Monitor) Shutdown() {
	m.shutdown.Break()
}

func (m *Monitor) Stop() {
	for _, c := range m.metrics {
		prometheus.Unregister(c)
	}
	m.metrics = nil
}

//go:generate stringer -type HealthStatus -trimprefix Health

type HealthStatus int

const (
	HealthOK HealthStatus = iota
	HealthNotStarted
	HealthStopped
	HealthUnderLoad
	HealthDisabled
)

func (m *Monitor) Health() HealthStatus {
	if !m.started.IsBroken() {
		return HealthNotStarted
	}
	if m.shutdown.IsBroken() {
		return HealthStopped
	}
	if m.cpu.GetCPUIdle() < m.cpu.NumCPU()*(1-m.maxUtilization) {
		return HealthUnderLoad
	}
	return HealthOK
}

func (m *Monitor) IdleCPU() float64 {
	return m.cpu.GetCPUIdle()
}

func (m *Monitor) InviteReqRaw(dir CallDir) {
	m.inviteReqRaw.Inc()
}

func (m *Monitor) NewCall(dir CallDir, fromHost, toHost string) *CallMonitor {
	return &CallMonitor{
		m:        m,
		dir:      dir,
		fromHost: fromHost,
		toHost:   toHost,
	}
}

type CallMonitor struct {
	m          *Monitor
	dir        CallDir
	fromHost   string
	toHost     string
	started    atomic.Bool
	terminated atomic.Bool
}

func (c *CallMonitor) labelsShort(l prometheus.Labels) prometheus.Labels {
	out := prometheus.Labels{"dir": c.dir.String()}
	for k, v := range l {
		out[k] = v
	}
	return out
}

func (c *CallMonitor) labels(l prometheus.Labels) prometheus.Labels {
	out := prometheus.Labels{"dir": c.dir.String(), "to": c.toHost}
	for k, v := range l {
		out[k] = v
	}
	return out
}

func (c *CallMonitor) InviteReq() {
	c.m.inviteReq.With(c.labelsShort(nil)).Inc()
}

func (c *CallMonitor) InviteAccept() {
	c.m.inviteAccept.With(c.labels(nil)).Inc()
}

func (c *CallMonitor) InviteErrorShort(reason string) {
	c.m.inviteErr.With(c.labelsShort(prometheus.Labels{"reason": reason, "to": "unknown"})).Inc()
}

func (c *CallMonitor) InviteError(reason string) {
	c.m.inviteErr.With(c.labels(prometheus.Labels{"reason": reason})).Inc()
}

func (c *CallMonitor) CallStart() {
	if !c.started.CompareAndSwap(false, true) {
		return
	}
	c.m.callsActive.With(c.labels(nil)).Inc()
}

func (c *CallMonitor) CallEnd() {
	if !c.started.CompareAndSwap(true, false) {
		return
	}
	c.m.callsActive.With(c.labels(nil)).Dec()
}

func (c *CallMonitor) CallTerminate(reason string) {
	if !c.terminated.CompareAndSwap(false, true) {
		return
	}
	c.m.callsTerminated.With(c.labels(prometheus.Labels{"reason": reason})).Inc()
}

func (c *CallMonitor) RTPPacketSend(payloadType string) {
	c.m.packetsRTP.With(c.labels(prometheus.Labels{"op": "send", "payload": payloadType})).Inc()
}

func (c *CallMonitor) RTPPacketRecv(payloadType string) {
	c.m.packetsRTP.With(c.labels(prometheus.Labels{"op": "recv", "payload": payloadType})).Inc()
}

func (c *CallMonitor) SessionDur() func() time.Duration {
	return prometheus.NewTimer(c.m.durSession.With(c.labelsShort(nil))).ObserveDuration
}

func (c *CallMonitor) CallDur() func() time.Duration {
	return prometheus.NewTimer(c.m.durCall.With(c.labelsShort(nil))).ObserveDuration
}

func (c *CallMonitor) CheckDur() prometheus.Observer {
	return c.m.durCheck.With(c.labelsShort(nil))
}

func (c *CallMonitor) JoinDur() func() time.Duration {
	return prometheus.NewTimer(c.m.durJoin.With(c.labelsShort(nil))).ObserveDuration
}

func (c *CallMonitor) SDPSize(sz int, isOffer bool) {
	typ := "answer"
	if isOffer {
		typ = "offer"
	}
	c.m.sdpSize.WithLabelValues(typ).Observe(float64(sz))
}

func (m *Monitor) TransferStarted(dir CallDir) {
	m.transfersTotal.WithLabelValues(dir.String()).Inc()
	m.transfersActive.WithLabelValues(dir.String()).Inc()
}

func (m *Monitor) TransferSucceeded(dir CallDir) {
	m.transfersSucceeded.WithLabelValues(dir.String()).Inc()
	m.transfersActive.WithLabelValues(dir.String()).Dec()
}

func (m *Monitor) TransferFailed(dir CallDir, reason string, changeActive bool) {
	m.transfersFailed.WithLabelValues(dir.String(), reason).Inc()
	if changeActive {
		m.transfersActive.WithLabelValues(dir.String()).Dec()
	}
}
