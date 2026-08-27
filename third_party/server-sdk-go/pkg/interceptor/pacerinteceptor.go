package interceptor

import (
	"sync"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"

	"github.com/livekit/mediatransportutil/pkg/pacer"
)

type PacerInterceptorFactory struct {
	pacer pacer.Factory
	pool  *PacketPool
}

func NewPacerInterceptorFactory(pacer pacer.Factory) *PacerInterceptorFactory {
	return &PacerInterceptorFactory{
		pacer: pacer,
		pool:  NewPacketPool(500, 1500),
	}
}

func (p *PacerInterceptorFactory) NewInterceptor(id string) (interceptor.Interceptor, error) {
	pacer, err := p.pacer.NewPacer()
	if err != nil {
		return nil, err
	}
	return &PacerInterceptor{
		pacer: pacer,
		pool:  p.pool,
	}, nil
}

// PacerInterceptor is an interceptor that paces outgoing packets to avoid congestion on the bursty traffic of huge frames.
type PacerInterceptor struct {
	interceptor.NoOp

	pool  *PacketPool
	pacer pacer.Pacer
}

// BindRTCPWriter lets you modify any outgoing RTCP packets. It is called once per PeerConnection. The returned method
// will be called once per packet batch.
func (pi *PacerInterceptor) BindRTCPWriter(writer interceptor.RTCPWriter) interceptor.RTCPWriter {
	pi.pacer.Start()
	return writer
}

func (pi *PacerInterceptor) BindLocalStream(stream *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	var (
		lock         sync.Mutex
		lastSeq      uint16
		firstPktSeen bool
	)
	pacerWriter := func(header *rtp.Header, payload []byte) (int, error) {
		return writer.Write(header, payload, interceptor.Attributes{})
	}
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		// send packets immediately if it's retransmit to reduce frame freeze
		var isRetransmit bool
		seq := header.SequenceNumber
		lock.Lock()
		if firstPktSeen {
			if diff := seq - lastSeq; diff == 0 || diff > 0x8000 {
				isRetransmit = true
			} else {
				lastSeq = seq
			}
		} else {
			firstPktSeen = true
			lastSeq = seq
		}
		lock.Unlock()
		if isRetransmit {
			return writer.Write(header, payload, attributes)
		}

		pktSize := len(payload) + header.MarshalSize()
		poolEntity, pool := pi.pool.Get(pktSize)
		buf := *poolEntity
		n, err := header.MarshalTo(buf)
		if err != nil {
			if pool != nil {
				pool.Put(poolEntity)
			}
			return 0, err
		}
		copy(buf[n:], payload)
		var headCopy rtp.Header
		headCopy.Unmarshal(buf)

		pkt := &pacer.Packet{
			Header:     &headCopy,
			Payload:    buf[n:pktSize],
			Writer:     pacerWriter,
			Pool:       pool,
			PoolEntity: poolEntity,
		}
		pi.pacer.Enqueue(pkt)
		return pktSize, nil
	})

}

func (pi *PacerInterceptor) Close() error {
	pi.pacer.Stop()
	return nil
}
