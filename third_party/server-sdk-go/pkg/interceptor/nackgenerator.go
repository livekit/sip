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

package interceptor

import (
	"sync"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"go.uber.org/atomic"

	"github.com/livekit/mediatransportutil/pkg/nack"
)

type NackGeneratorInterceptorFactory struct {
	lock         sync.Mutex
	interceptors []*NackGeneratorInterceptor
}

// NewInterceptor constructs a new ReceiverInterceptor
func (g *NackGeneratorInterceptorFactory) NewInterceptor(id string) (interceptor.Interceptor, error) {
	i, err := NewNackGeneratorInterceptor()
	if err != nil {
		return nil, err
	}

	g.lock.Lock()
	g.interceptors = append(g.interceptors, i)
	g.lock.Unlock()
	return i, err
}

func (g *NackGeneratorInterceptorFactory) SetRTT(rtt uint32) {
	g.lock.Lock()
	defer g.lock.Unlock()

	for _, i := range g.interceptors {
		i.SetRTT(rtt)
	}
}

type NackGeneratorInterceptor struct {
	interceptor.NoOp
	lock       sync.Mutex
	writer     atomic.Value
	nackQueues map[uint32]nack.NackQueueInterface
}

func NewNackGeneratorInterceptor() (*NackGeneratorInterceptor, error) {
	n := &NackGeneratorInterceptor{
		nackQueues: make(map[uint32]nack.NackQueueInterface),
	}

	return n, nil
}

func (n *NackGeneratorInterceptor) SetRTT(rtt uint32) {
	n.lock.Lock()
	defer n.lock.Unlock()

	for _, q := range n.nackQueues {
		q.SetRTT(rtt)
	}
}

func (n *NackGeneratorInterceptor) BindRTCPWriter(writer interceptor.RTCPWriter) interceptor.RTCPWriter {
	n.writer.Store(writer)
	return writer
}

func (n *NackGeneratorInterceptor) BindRemoteStream(info *interceptor.StreamInfo, reader interceptor.RTPReader) interceptor.RTPReader {
	if !streamSupportNack(info) {
		return reader
	}

	nackQueue := nack.NewNACKQueueSafe(nack.NackQueueParamsDefault)
	n.lock.Lock()
	n.nackQueues[info.SSRC] = nackQueue
	n.lock.Unlock()
	nackQueue.SetRTT(70)
	var (
		firstReceived bool
		highestSeq    uint16
	)
	ssrc := info.SSRC

	return interceptor.RTPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		i, attr, err := reader.Read(b, a)
		if err != nil {
			return 0, nil, err
		}

		if attr == nil {
			attr = make(interceptor.Attributes)
		}
		header, err := attr.GetRTPHeader(b[:i])
		if err != nil {
			return 0, nil, err
		}

		if !firstReceived {
			firstReceived = true
			highestSeq = header.SequenceNumber
		} else {
			nackQueue.Remove(header.SequenceNumber)
			if diff := header.SequenceNumber - highestSeq; diff > 0 && diff < 0x8000 {
				for seq := highestSeq + 1; seq < header.SequenceNumber; seq++ {
					nackQueue.Push(seq)
				}
				highestSeq = header.SequenceNumber
			}
		}

		if nacks, _ := nackQueue.Pairs(); len(nacks) > 0 {
			pkts := []rtcp.Packet{&rtcp.TransportLayerNack{
				SenderSSRC: ssrc,
				MediaSSRC:  ssrc,
				Nacks:      nacks,
			}}
			if w := n.writer.Load(); w != nil {
				w.(interceptor.RTCPWriter).Write(pkts, nil)
			}
		}

		return i, attr, nil
	})
}

func (n *NackGeneratorInterceptor) UnbindRemoteStream(info *interceptor.StreamInfo) {
	n.lock.Lock()
	defer n.lock.Unlock()
	delete(n.nackQueues, info.SSRC)
}

func streamSupportNack(info *interceptor.StreamInfo) bool {
	for _, fb := range info.RTCPFeedback {
		if fb.Type == "nack" && fb.Parameter == "" {
			return true
		}
	}

	return false
}
