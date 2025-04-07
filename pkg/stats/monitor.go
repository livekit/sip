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
	// 列出用于相对较短操作（如SIP INVITE）的直方图桶。
	durBucketsOp = []float64{
		0.1, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 3 * 60,
	}
	// durBucketsLong lists histogram buckets for long operations like call/session durations.
	// 列出用于长时间操作（如呼叫/会话持续时间）的直方图桶。
	durBucketsLong = []float64{
		1, 10, 60, 10 * 60, 30 * 60, 3600, 6 * 3600, 12 * 3600, 24 * 3600,
	}
	// sizeBuckets lists histogram buckets for SDP size.
	// 列出用于SDP大小的直方图桶。
	sizeBuckets = []float64{
		100, 250, 500, 750, 1000, 1250, 1500,
	}
)

// CallDir 表示呼叫方向。
type CallDir bool

// String 返回呼叫方向的字符串表示。
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
	nodeID string // 节点ID

	inviteReqRaw    prometheus.Counter       // 未验证的SIP INVITE请求计数器
	inviteReq       *prometheus.CounterVec   // 有效SIP INVITE请求计数器向量
	inviteAccept    *prometheus.CounterVec   // 接受的SIP INVITE请求计数器向量
	inviteErr       *prometheus.CounterVec   // 拒绝的SIP INVITE请求计数器向量
	callsActive     *prometheus.GaugeVec     // 当前活跃的SIP呼叫计数器向量
	callsTerminated *prometheus.CounterVec   // 终止的SIP呼叫计数器向量
	packetsRTP      *prometheus.CounterVec   // RTP数据包发送/接收计数器向量
	durSession      *prometheus.HistogramVec // SIP会话持续时间直方图向量
	durCall         *prometheus.HistogramVec // SIP呼叫持续时间直方图向量
	durJoin         *prometheus.HistogramVec // SIP房间加入持续时间直方图向量
	cpuLoad         prometheus.Gauge         // CPU负载计数器
	sdpSize         *prometheus.HistogramVec // SDP大小直方图向量
	nodeAvailable   prometheus.GaugeFunc     // 节点可用性计数器函数

	cpu            *hwstats.CPUStats // CPU统计信息
	maxUtilization float64           // 最大CPU利用率

	metrics  []prometheus.Collector // 指标列表
	started  core.Fuse              // 启动熔断器
	shutdown core.Fuse              // 关闭熔断器
}

// NewMonitor 创建一个新的监控实例。
func NewMonitor(conf *config.Config) (*Monitor, error) {
	m := &Monitor{
		nodeID:         conf.NodeID,            // 节点ID
		maxUtilization: conf.MaxCpuUtilization, // 最大CPU利用率
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

// Start 启动监控实例。
func (m *Monitor) Start(conf *config.Config) error {
	// 注销Go收集器
	prometheus.Unregister(collectors.NewGoCollector())
	// 注册Go收集器
	mustRegister(m, collectors.NewGoCollector(collectors.WithGoCollectorRuntimeMetrics(collectors.MetricsAll)))

	// 注册未验证的SIP INVITE请求计数器
	m.inviteReqRaw = mustRegister(m, prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "invite_requests_raw",
		Help:        "Number of unvalidated SIP INVITE requests received",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}))

	// 注册有效的SIP INVITE请求计数器向量
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

	// 注册节点可用性计数器函数
	m.nodeAvailable = mustRegister(m, prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "available",
		Help:        "Whether node can accept new requests",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, func() float64 {
		c := m.CanAccept()
		if c {
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

func (m *Monitor) CanAccept() bool {
	if !m.started.IsBroken() ||
		m.shutdown.IsBroken() ||
		m.cpu.GetCPUIdle() < m.cpu.NumCPU()*(1-m.maxUtilization) {
		return false
	}

	return true
}

func (m *Monitor) IdleCPU() float64 {
	return m.cpu.GetCPUIdle()
}

// InviteReqRaw 记录未验证的SIP INVITE请求
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
