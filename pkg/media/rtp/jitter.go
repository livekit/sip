// Copyright 2024 LiveKit, Inc.
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
	"time"

	"github.com/pion/rtp"

	"github.com/livekit/server-sdk-go/v2/pkg/jitter"
)

const (
	jitterMaxLatency = 60 * time.Millisecond // should match mixer's target buffer size
)

func HandleJitter(clockRate int, h Handler) Handler {
	return &jitterHandler{
		h:   h,
		buf: jitter.NewBuffer(audioDepacketizer{}, uint32(clockRate), jitterMaxLatency),
	}
}

type jitterHandler struct {
	h   Handler
	buf *jitter.Buffer
}

func (r *jitterHandler) HandleRTP(h *rtp.Header, payload []byte) error {
	r.buf.Push(&rtp.Packet{Header: *h, Payload: payload})
	var last error
	for _, p := range r.buf.Pop(false) {
		if err := r.h.HandleRTP(&p.Header, p.Payload); err != nil {
			last = err
		}
	}
	return last
}

type audioDepacketizer struct{}

func (d audioDepacketizer) Unmarshal(packet []byte) ([]byte, error) {
	return packet, nil
}

func (d audioDepacketizer) IsPartitionHead(payload []byte) bool {
	return true
}

func (d audioDepacketizer) IsPartitionTail(marker bool, payload []byte) bool {
	return true
}
