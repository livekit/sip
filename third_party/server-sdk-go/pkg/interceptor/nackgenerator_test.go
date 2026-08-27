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
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func TestGeneratorInterceptor(t *testing.T) {
	f := &NackGeneratorInterceptorFactory{}

	i, err := f.NewInterceptor("")
	require.NoError(t, err)

	stream := NewMockStream(&interceptor.StreamInfo{
		SSRC:         1,
		RTCPFeedback: []interceptor.RTCPFeedback{{Type: "nack"}},
	}, i)
	defer func() {
		require.NoError(t, stream.Close())
	}()

	for _, seqNum := range []uint16{10, 11, 12, 14, 16, 18} {
		stream.ReceiveRTP(&rtp.Packet{Header: rtp.Header{SequenceNumber: seqNum}})

		select {
		case r := <-stream.ReadRTP():
			require.NoError(t, r.Err)
			require.Equal(t, seqNum, r.Packet.SequenceNumber)
		case <-time.After(10 * time.Millisecond):
			t.Fatal("receiver rtp packet not found")
		}
	}
	i.(*NackGeneratorInterceptor).SetRTT(20)
	time.Sleep(100 * time.Millisecond)
	stream.ReceiveRTP(&rtp.Packet{Header: rtp.Header{SequenceNumber: 19}})

	select {
	case pkts := <-stream.WrittenRTCP():
		require.Equal(t, 1, len(pkts), "single packet RTCP Compound Packet expected")

		p, ok := pkts[0].(*rtcp.TransportLayerNack)
		require.True(t, ok, "TransportLayerNack rtcp packet expected, found: %T", pkts[0])

		require.Equal(t, uint16(13), p.Nacks[0].PacketID)
		require.Equal(t, rtcp.PacketBitmap(0b1010), p.Nacks[0].LostPackets) // lost 13,15,17
	case <-time.After(1000000 * time.Millisecond):
		t.Fatal("written rtcp packet not found")
	}
}
