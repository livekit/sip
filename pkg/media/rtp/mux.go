// Copyright 2023 LiveKit, Inc.
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

package rtp

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/pion/rtp"
	"golang.org/x/exp/maps"
)

// NewMux creates an RTP handler mux that selects a Handler based on RTP payload type.
// See Mux.HandleRTP for details.
func NewMux(def Handler) *Mux {
	return &Mux{dynamic: make(map[byte]Handler), def: def}
}

type Mux struct {
	mu      sync.RWMutex
	static  [rtp.PayloadTypeFirstDynamic]Handler
	dynamic map[byte]Handler
	def     Handler
}

func (m *Mux) String() string {
	var buf strings.Builder
	buf.WriteString("Mux")
	for t, h := range m.static {
		if h == nil {
			continue
		}
		fmt.Fprintf(&buf, " (T=%d -> %s)", t, h.String())
	}
	dkeys := maps.Keys(m.dynamic)
	slices.Sort(dkeys)
	for _, t := range dkeys {
		h := m.dynamic[t]
		fmt.Fprintf(&buf, " (T=%d -> %s)", t, h.String())
	}
	if m.def != nil {
		fmt.Fprintf(&buf, " (Def -> %s)", m.def.String())
	}
	return buf.String()
}

// HandleRTP selects a Handler based on payload type.
// Types can be registered with Register. If no handler is set, a default one will be used.
func (m *Mux) HandleRTP(h *rtp.Header, payload []byte) error {
	if m == nil {
		return nil
	}
	var r Handler
	m.mu.RLock()
	if h.PayloadType < byte(len(m.static)) {
		r = m.static[h.PayloadType]
	} else {
		r = m.dynamic[h.PayloadType]
	}
	if r == nil {
		r = m.def
	}
	m.mu.RUnlock()
	if r == nil {
		return nil
	}
	return r.HandleRTP(h, payload)
}

// SetDefault sets a default RTP handler.
// Setting nil will drop packets with unregistered type.
func (m *Mux) SetDefault(h Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.def = h
}

// Register RTP handler for a payload type.
// Setting nil removes the handler.
func (m *Mux) Register(typ byte, h Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if typ < byte(len(m.static)) {
		m.static[typ] = h
	} else {
		if h == nil {
			delete(m.dynamic, typ)
		} else {
			m.dynamic[typ] = h
		}
	}
}
