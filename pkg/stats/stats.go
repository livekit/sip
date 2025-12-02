package stats

import (
	"math"
	"sync/atomic"
)

type statAtomic struct {
	overflow   uint64
	count      uint64
	sum        uint64
	squaresSum uint64
	min        uint64
	max        uint64
}

type StatSnapshot struct {
	Overflow   uint64
	Count      uint64
	Sum        uint64
	SquaresSum uint64
	Min        uint64
	Average    float64
	Max        uint64
	Variance   float64
}

func NewStatAtomic() *statAtomic {
	return &statAtomic{
		overflow:   0,
		count:      0,
		sum:        0,
		squaresSum: 0,
		min:        math.MaxUint64,
		max:        0,
	}
}
func (s *statAtomic) Update(value uint64) {
	atomic.AddUint64(&s.count, 1) // new count
	atomic.AddUint64(&s.sum, value)
	for { // Update max
		max := atomic.LoadUint64(&s.max)
		if value <= max {
			break
		}
		if atomic.CompareAndSwapUint64(&s.max, max, value) {
			break
		}
	}
	for { // Update min
		min := atomic.LoadUint64(&s.min)
		if value >= min {
			break
		}
		if atomic.CompareAndSwapUint64(&s.min, min, value) {
			break
		}
	}
	square := value * value
	newsquaresSum := atomic.AddUint64(&s.squaresSum, square)
	if newsquaresSum < square {
		// squaresSum will be the first to go by definition. Everything else may follow.
		atomic.StoreUint64(&s.overflow, 1)
	}
}

// Can be inaccurate if updated concurrently with function call.
// Caller should lock to ensure accuracy.
func (s *statAtomic) Snapshot() *StatSnapshot {
	snapshot := StatSnapshot{
		Overflow:   atomic.LoadUint64(&s.overflow),
		Count:      atomic.LoadUint64(&s.count),
		Sum:        atomic.LoadUint64(&s.sum),
		SquaresSum: atomic.LoadUint64(&s.squaresSum),
		Min:        atomic.LoadUint64(&s.min),
		Max:        atomic.LoadUint64(&s.max),
	}
	if snapshot.Count == 0 {
		snapshot.Min = 0
		snapshot.Average = 0
		snapshot.Variance = 0
	} else {
		snapshot.Average = float64(snapshot.Sum) / float64(snapshot.Count)
		snapshot.Variance = (float64(snapshot.SquaresSum) / float64(snapshot.Count)) - (snapshot.Average * snapshot.Average)
	}
	return &snapshot
}
