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
	"sync"

	"github.com/pion/rtp"
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

// HandleRTP selects a Handler based on payload type.
// Types can be registered with Register. If no handler is set, a default one will be used.
func (m *Mux) HandleRTP(p *rtp.Packet) error {
	if m == nil {
		return nil
	}
	var h Handler
	m.mu.RLock()
	if p.PayloadType < byte(len(m.static)) {
		h = m.static[p.PayloadType]
	} else {
		h = m.dynamic[p.PayloadType]
	}
	if h == nil {
		h = m.def
	}
	m.mu.RUnlock()
	if h == nil {
		return nil
	}
	return h.HandleRTP(p)
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
