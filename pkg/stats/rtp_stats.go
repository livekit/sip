package stats

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/media-sdk"
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

// Implements:
// - rtp.Handler
// - rtp.HandlerCloser
// - rtp.WriteStream
// - TODO: media.WriteCloser[T]
type RTPStats struct {
	packetSize    *statAtomic // Contains count as well
	deltaMS       *statAtomic // PACING indicator ; difference in milliseconds between last packet and current
	deltaSeq      *statAtomic // LOSS / OUT OF ORDER indicator ; positive difference in rtp.sequenceNumber
	deltaTS       *statAtomic // LOSS / OUT OF ORDER indicator ; positive difference in rtp.Timestamp
	seqOutOfOrder *statAtomic // OUT OF ORDER indicator ; negative distance from most recent packet, in rtp.sequenceNumber
	tsOutOfOrder  *statAtomic // OUT OF ORDER indicator ; positive difference in rtp.Timestamp
	packetCount   uint64      // number of packets received since last reset

	totalPacketCount uint64 // number of total packets received, persists across resets
	resetCount       uint64 // number of resets detected, persists across resets
}

func NewRTPStats() *RTPStats {
	return &RTPStats{
		packetSize:    NewStatAtomic(),
		deltaMS:       NewStatAtomic(),
		deltaSeq:      NewStatAtomic(),
		deltaTS:       NewStatAtomic(),
		seqOutOfOrder: NewStatAtomic(),
		tsOutOfOrder:  NewStatAtomic(),
	}
}

type RtpStats struct { // Complies with rtp.Handler and rtp.HandlerCloser interfaces
	h    rtp.Handler
	hc   rtp.HandlerCloser
	w    rtp.WriteStream
	name string
	log  logger.Logger

	mu   sync.Mutex
	data *RTPStats

	latestReceived  time.Time // timestamp of latest packet received
	latestSSRC      uint32    // SSRC of latest packet received
	latestSequence  uint16    // sequence number of latest packet received
	latestTimestamp uint32    // timestamp of latest packet received
}

type RTPStatsOptions func(*RtpStats)

func WithWriteStream(w rtp.WriteStream) RTPStatsOptions {
	return func(s *RtpStats) {
		s.w = w
	}
}

func WithHandler(h rtp.Handler) RTPStatsOptions {
	return func(s *RtpStats) {
		s.h = h
	}
}

func WithHandlerCloser(hc rtp.HandlerCloser) RTPStatsOptions {
	return func(s *RtpStats) {
		s.hc = hc
	}
}

func NewHandlerStats(name string, log logger.Logger, data *RTPStats, opts ...RTPStatsOptions) *RtpStats {
	if data == nil {
		data = NewRTPStats()
	}
	stats := &RtpStats{
		name: name,
		log:  log,
		data: data,
	}
	for _, opt := range opts {
		opt(stats)
	}
	if stats.w == nil && stats.h == nil && stats.hc == nil {
		panic("no handler, handler closer, or write stream provided")
	}
	return stats
}

func (s *RtpStats) String() string {
	return "RTPStats"
}

func (s *RtpStats) isReset(h *rtp.Header) bool {
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

func (s *RtpStats) LogStats(reason string) {
	s.log.Infow("rtp stats",
		"name", s.name,
		"reason", reason,
		"packetSize", s.data.packetSize.Snapshot(),
		"deltaMS", s.data.deltaMS.Snapshot(),
		"deltaSeq", s.data.deltaSeq.Snapshot(),
		"deltaTS", s.data.deltaTS.Snapshot(),
		"seqOutOfOrder", s.data.seqOutOfOrder.Snapshot(),
		"tsOutOfOrder", s.data.tsOutOfOrder.Snapshot(),
		"packetCount", s.data.totalPacketCount,
		"resetCount", s.data.resetCount,
	)
}

func (s *RtpStats) Reset() {
	s.data.packetSize = NewStatAtomic()
	s.data.deltaMS = NewStatAtomic()
	s.data.deltaSeq = NewStatAtomic()
	s.data.deltaTS = NewStatAtomic()
	s.data.seqOutOfOrder = NewStatAtomic()
	s.data.tsOutOfOrder = NewStatAtomic()
	s.data.packetCount = 0
	s.latestReceived = time.Time{}
}

func (s *RtpStats) update(h *rtp.Header, payloadSize uint64) {
	newCount := atomic.AddUint64(&s.data.packetCount, 1)
	atomic.AddUint64(&s.data.totalPacketCount, 1)
	s.data.packetSize.Update(payloadSize)
	if newCount != 1 {
		msSinceLast := time.Since(s.latestReceived).Milliseconds()
		s.data.deltaMS.Update(uint64(msSinceLast))

		deltaSeq := Diff16(h.SequenceNumber, s.latestSequence)
		if deltaSeq >= 0 {
			s.data.deltaSeq.Update(uint64(deltaSeq))
		} else {
			s.data.seqOutOfOrder.Update(uint64(-deltaSeq))
		}

		deltaTS := Diff32(h.Timestamp, s.latestTimestamp)
		if deltaTS >= 0 {
			s.data.deltaTS.Update(uint64(deltaTS))
		} else {
			s.data.tsOutOfOrder.Update(uint64(-deltaTS))
		}
	}
	s.latestReceived = time.Now()
	s.latestSSRC = h.SSRC
	s.latestSequence = h.SequenceNumber
	s.latestTimestamp = h.Timestamp
}

func (s *RtpStats) processPacket(h *rtp.Header, payload []byte) error {
	if s.isReset(h) {
		s.mu.Lock()
		if s.isReset(h) {
			if atomic.LoadUint64(&s.data.packetCount) > 0 {
				s.data.resetCount++ // Locked, so not atomic
				s.LogStats("stream reset")
				s.Reset()
			}
		}
		s.mu.Unlock()
	}
	s.update(h, uint64(len(payload)))
	return nil
}

func (s *RtpStats) HandleRTP(h *rtp.Header, payload []byte) error {
	if err := s.processPacket(h, payload); err != nil {
		return err
	}
	if s.h != nil {
		return s.h.HandleRTP(h, payload)
	}
	if s.hc != nil {
		return s.hc.HandleRTP(h, payload)
	}
	return errors.New("no handler or handler closer provided")
}

func (s *RtpStats) WriteRTP(h *rtp.Header, payload []byte) (int, error) {
	if err := s.processPacket(h, payload); err != nil {
		return 0, err
	}
	if s.w == nil {
		return 0, errors.New("no write stream provided")
	}
	return s.w.WriteRTP(h, payload)
}

func (s *RtpStats) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LogStats("stream closing")
	if s.hc != nil {
		s.hc.Close()
	}
}

func NewRtpStatsWriteCloser[T ~[]byte](w media.WriteCloser[T], dur time.Duration, stats *RtpStats) media.WriteCloser[T] {
	writer := &RtpStatsWriteCloser[T]{w: w, dur: dur, stats: stats}
	return writer
}

type RtpStatsWriteCloser[T ~[]byte] struct {
	stats *RtpStats
	w     media.WriteCloser[T]
	dur   time.Duration
	tsHop uint32
	seq   uint32
	ts    uint32
}

func (s *RtpStatsWriteCloser[T]) String() string {
	return fmt.Sprintf("RtpStatsWriteCloser(%s)", s.w.String())
}

func (s *RtpStatsWriteCloser[T]) SampleRate() int {
	return s.w.SampleRate()
}

func (s *RtpStatsWriteCloser[T]) WriteSample(sample T) error {
	// For now, fake out the rtp header.
	seq := uint16(atomic.AddUint32(&s.seq, 1))
	if s.tsHop == 0 {
		s.tsHop = uint32(s.w.SampleRate()) * uint32(s.dur.Milliseconds()) / 1000
	}
	ts := atomic.AddUint32(&s.ts, s.tsHop)
	h := &rtp.Header{
		SequenceNumber: seq,
		Timestamp:      ts,
		SSRC:           0,
	}
	s.stats.update(h, uint64(len(sample)))
	return s.w.WriteSample(sample)
}

func (s *RtpStatsWriteCloser[T]) Close() error {
	s.stats.Close()
	return s.w.Close()
}
