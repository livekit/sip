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
	"github.com/frostbyte73/core"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/livekit/sip/pkg/config"
)

type Monitor struct {
	inviteReqRaw prometheus.Counter
	inviteReq    *prometheus.CounterVec
	inviteAccept *prometheus.CounterVec
	inviteErr    *prometheus.CounterVec
	callsActive  *prometheus.GaugeVec

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
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	})

	m.inviteReq = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "invite_requests",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"type", "from", "to"})

	m.inviteAccept = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "invite_accepted",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"type", "from", "to"})

	m.inviteErr = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "invite_error",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"type", "from", "to", "reason"})

	m.callsActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   "livekit",
		Subsystem:   "sip",
		Name:        "calls_active",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"type", "from", "to"})

	prometheus.MustRegister(m.inviteReqRaw, m.inviteReq, m.inviteAccept, m.inviteErr, m.callsActive)

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
}

func (m *Monitor) CanAccept() bool {
	if !m.started.IsBroken() || m.shutdown.IsBroken() {
		return false
	}

	return true
}

func (m *Monitor) InviteReqRaw(outbound bool) {
	m.inviteReqRaw.Inc()
}

func callType(outbound bool) string {
	typ := "inbound"
	if outbound {
		typ = "outbound"
	}
	return typ
}

func (m *Monitor) InviteReq(outbound bool, from, to string) {
	m.inviteReq.With(prometheus.Labels{"type": callType(outbound), "from": from, "to": to}).Inc()
}

func (m *Monitor) InviteAccept(outbound bool, from, to string) {
	m.inviteAccept.With(prometheus.Labels{"type": callType(outbound), "from": from, "to": to}).Inc()
}

func (m *Monitor) InviteError(outbound bool, from, to string, reason string) {
	m.inviteErr.With(prometheus.Labels{"type": callType(outbound), "from": from, "to": to, "reason": reason}).Inc()
}

func (m *Monitor) CallStart(outbound bool, from, to string) {
	m.callsActive.With(prometheus.Labels{"type": callType(outbound), "from": from, "to": to}).Inc()
}

func (m *Monitor) CallEnd(outbound bool, from, to string) {
	m.callsActive.With(prometheus.Labels{"type": callType(outbound), "from": from, "to": to}).Dec()
}
