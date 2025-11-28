package stats

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/logger"
)

// DiffNN returns the signed difference between cur and prev, accounting for wrap-around in a set integer size

func Diff32(cur, prev uint32) int32 {
	return int32(cur - prev)
}

func Diff16(cur, prev uint16) int16 {
	return int16(cur - prev)
}

type RTPStats struct { // Complies with rtp.Handler interface
	h   rtp.Handler
	log logger.Logger

	mu            sync.Mutex
	packetSize    *statAtomic // Contains count as well
	deltaMS       *statAtomic // PACING indicator ; difference in milliseconds between last packet and current
	deltaSeq      *statAtomic // LOSS / OUT OF ORDER indicator ; positive difference in rtp.sequenceNumber
	deltaTS       *statAtomic // LOSS / OUT OF ORDER indicator ; positive difference in rtp.Timestamp
	seqOutOfOrder *statAtomic // OUT OF ORDER indicator ; negative distance from most recent packet, in rtp.sequenceNumber
	tsOutOfOrder  *statAtomic // OUT OF ORDER indicator ; positive difference in rtp.Timestamp

	packetCount uint64 // number of total packets received, persists across resets
	resetCount  uint64 // number of resets detected, persists across resets

	latestReceived  time.Time // timestamp of latest packet received
	latestSSRC      uint32    // SSRC of latest packet received
	latestSequence  uint16    // sequence number of latest packet received
	latestTimestamp uint32    // timestamp of latest packet received
}

func NewRTPStats(h rtp.Handler, log logger.Logger) *RTPStats {
	return &RTPStats{
		h:             h,
		log:           log,
		packetSize:    NewStatAtomic(),
		deltaMS:       NewStatAtomic(),
		deltaSeq:      NewStatAtomic(),
		deltaTS:       NewStatAtomic(),
		seqOutOfOrder: NewStatAtomic(),
		tsOutOfOrder:  NewStatAtomic(),
		packetCount:   0,
		resetCount:    0,
		latestSSRC:    0,
	}
}

func (s *RTPStats) String() string {
	return "RTPStats"
}

func (s *RTPStats) isReset(h *rtp.Header) bool {
	const maxSeqDiff = (math.MaxUint16 / 4) // 16384, about 5.5 minutes at 20ms packets

	// for now, we're assuming 20ms packets and a maximum of 48000sample rate
	if s.latestSSRC != h.SSRC {
		return true
	}
	seqDiff := Diff16(h.SequenceNumber, s.latestSequence)
	if seqDiff < -maxSeqDiff || seqDiff > maxSeqDiff {
		return true
	}
	// Ignoring TS differences, varies wildly between 8k and 48k sample rates
	return false
}

func (s *RTPStats) LogStats(reason string) {
	s.log.Infow("rtp stats",
		"reason", reason,
		"packetSize", s.packetSize.Snapshot(),
		"deltaMS", s.deltaMS.Snapshot(),
		"deltaSeq", s.deltaSeq.Snapshot(),
		"deltaTS", s.deltaTS.Snapshot(),
		"seqOutOfOrder", s.seqOutOfOrder.Snapshot(),
		"tsOutOfOrder", s.tsOutOfOrder.Snapshot(),
		"packetCount", s.packetCount,
		"resetCount", s.resetCount,
	)
}

func (s *RTPStats) Reset() {
	s.packetSize = NewStatAtomic()
	s.deltaMS = NewStatAtomic()
	s.deltaSeq = NewStatAtomic()
	s.deltaTS = NewStatAtomic()
	s.seqOutOfOrder = NewStatAtomic()
	s.tsOutOfOrder = NewStatAtomic()
	s.latestReceived = time.Time{}
}

func (s *RTPStats) Update(h *rtp.Header, payload []byte) {
	newCount := atomic.AddUint64(&s.packetCount, 1)
	s.packetSize.Update(uint64(len(payload)))
	if newCount != 1 {
		msSinceLast := time.Since(s.latestReceived).Milliseconds()
		s.deltaMS.Update(uint64(msSinceLast))

		deltaSeq := Diff16(h.SequenceNumber, s.latestSequence)
		if deltaSeq >= 0 {
			s.deltaSeq.Update(uint64(deltaSeq))
		} else {
			s.seqOutOfOrder.Update(uint64(-deltaSeq))
		}

		deltaTS := Diff32(h.Timestamp, s.latestTimestamp)
		if deltaTS >= 0 {
			s.deltaTS.Update(uint64(deltaTS))
		} else {
			s.tsOutOfOrder.Update(uint64(-deltaTS))
		}
	}
	s.latestReceived = time.Now()
	s.latestSSRC = h.SSRC
	s.latestSequence = h.SequenceNumber
	s.latestTimestamp = h.Timestamp
}

func (s *RTPStats) HandleRTP(h *rtp.Header, payload []byte) error {
	if s.isReset(h) {
		s.mu.Lock()
		if s.isReset(h) {
			s.resetCount++ // Locked, so not atomic
			s.LogStats("stream reset")
			s.Reset()
		}
		s.mu.Unlock()
	}
	return s.h.HandleRTP(h, payload)
}
