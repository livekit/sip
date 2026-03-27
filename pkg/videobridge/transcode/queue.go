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

package transcode

import (
	"container/heap"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/videobridge/codec"
	"github.com/livekit/sip/pkg/videobridge/stats"
)

// Priority levels for transcode jobs.
type Priority int

const (
	PriorityLow    Priority = 0
	PriorityNormal Priority = 1
	PriorityHigh   Priority = 2 // e.g., keyframes
)

// Job represents a unit of work for the transcode queue.
type Job struct {
	SessionID string
	NAL       codec.NALUnit
	Timestamp uint32
	Priority  Priority
	EnqueueAt time.Time
	index     int // heap index
}

// Queue is a priority-based job queue for transcode requests.
// It decouples NAL ingestion from the transcoder subprocess,
// preventing slow transcoders from blocking the RTP read loop.
type Queue struct {
	log  logger.Logger
	conf QueueConfig

	mu   sync.Mutex
	cond *sync.Cond
	jobs jobHeap

	// Workers pull from the queue
	workerFn func(job *Job) error

	// Stats
	enqueued  atomic.Uint64
	dequeued  atomic.Uint64
	dropped   atomic.Uint64
	queueSize atomic.Int64

	closed atomic.Bool
}

// QueueConfig configures the transcode queue.
type QueueConfig struct {
	// MaxSize is the maximum number of pending jobs. 0 = unlimited.
	MaxSize int
	// Workers is the number of concurrent worker goroutines.
	Workers int
	// DropPolicy: if true, drop lowest-priority jobs when queue is full.
	DropOnFull bool
}

// NewQueue creates a new priority-based transcode queue.
func NewQueue(log logger.Logger, conf QueueConfig) *Queue {
	if conf.Workers <= 0 {
		conf.Workers = 1
	}
	if conf.MaxSize <= 0 {
		conf.MaxSize = 120 // ~4 seconds at 30fps
	}

	q := &Queue{
		log:  log,
		conf: conf,
		jobs: make(jobHeap, 0, conf.MaxSize),
	}
	q.cond = sync.NewCond(&q.mu)
	heap.Init(&q.jobs)

	return q
}

// SetWorkerFunc sets the function called for each dequeued job.
func (q *Queue) SetWorkerFunc(fn func(job *Job) error) {
	q.workerFn = fn
}

// Start launches worker goroutines that process jobs from the queue.
func (q *Queue) Start() {
	for i := 0; i < q.conf.Workers; i++ {
		go q.workerLoop(i)
	}
	q.log.Infow("transcode queue started", "workers", q.conf.Workers, "maxSize", q.conf.MaxSize)
}

// Enqueue adds a transcode job to the queue.
// Returns false if the job was dropped due to queue being full.
func (q *Queue) Enqueue(job *Job) bool {
	if q.closed.Load() {
		return false
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	// Check queue capacity
	if q.conf.MaxSize > 0 && q.jobs.Len() >= q.conf.MaxSize {
		if q.conf.DropOnFull {
			// Drop lowest priority job (bottom of heap = index 0 after we peek)
			if q.jobs.Len() > 0 && job.Priority > q.jobs[0].Priority {
				// New job has higher priority: drop the lowest
				dropped := heap.Pop(&q.jobs).(*Job)
				q.dropped.Add(1)
				q.queueSize.Add(-1)
				stats.SessionErrors.WithLabelValues("transcode_job_dropped").Inc()
				q.log.Debugw("dropped low-priority job",
					"droppedSession", dropped.SessionID,
					"newSession", job.SessionID,
				)
			} else {
				// New job is lowest priority: drop it
				q.dropped.Add(1)
				stats.SessionErrors.WithLabelValues("transcode_job_dropped").Inc()
				return false
			}
		} else {
			q.dropped.Add(1)
			stats.SessionErrors.WithLabelValues("transcode_queue_full").Inc()
			return false
		}
	}

	job.EnqueueAt = time.Now()
	heap.Push(&q.jobs, job)
	q.enqueued.Add(1)
	q.queueSize.Add(1)
	q.cond.Signal() // wake one worker

	return true
}

// Close shuts down the queue and wakes all workers.
func (q *Queue) Close() {
	q.closed.Store(true)
	q.cond.Broadcast() // wake all workers to exit
	q.log.Infow("transcode queue closed",
		"enqueued", q.enqueued.Load(),
		"dequeued", q.dequeued.Load(),
		"dropped", q.dropped.Load(),
	)
}

// Stats returns queue statistics.
func (q *Queue) Stats() QueueStats {
	return QueueStats{
		Enqueued:  q.enqueued.Load(),
		Dequeued:  q.dequeued.Load(),
		Dropped:   q.dropped.Load(),
		QueueSize: q.queueSize.Load(),
	}
}

// QueueStats holds queue statistics.
type QueueStats struct {
	Enqueued  uint64 `json:"enqueued"`
	Dequeued  uint64 `json:"dequeued"`
	Dropped   uint64 `json:"dropped"`
	QueueSize int64  `json:"queue_size"`
}

func (q *Queue) workerLoop(id int) {
	for {
		q.mu.Lock()
		for q.jobs.Len() == 0 && !q.closed.Load() {
			q.cond.Wait()
		}
		if q.closed.Load() && q.jobs.Len() == 0 {
			q.mu.Unlock()
			return
		}

		job := heap.Pop(&q.jobs).(*Job)
		q.mu.Unlock()

		q.dequeued.Add(1)
		q.queueSize.Add(-1)

		// Track queue wait time
		waitMs := time.Since(job.EnqueueAt).Milliseconds()
		stats.TranscodeLatencyMs.Observe(float64(waitMs))

		if q.workerFn != nil {
			if err := q.workerFn(job); err != nil {
				q.log.Debugw("transcode worker error",
					"worker", id,
					"session", job.SessionID,
					"error", err,
				)
			}
		}
	}
}

// jobHeap implements heap.Interface for priority-based job scheduling.
// Higher priority jobs are dequeued first. Equal priority → FIFO (earlier enqueue time first).
type jobHeap []*Job

func (h jobHeap) Len() int { return len(h) }

func (h jobHeap) Less(i, j int) bool {
	if h[i].Priority != h[j].Priority {
		return h[i].Priority > h[j].Priority // higher priority first
	}
	return h[i].EnqueueAt.Before(h[j].EnqueueAt) // FIFO for same priority
}

func (h jobHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *jobHeap) Push(x interface{}) {
	n := len(*h)
	job := x.(*Job)
	job.index = n
	*h = append(*h, job)
}

func (h *jobHeap) Pop() interface{} {
	old := *h
	n := len(old)
	job := old[n-1]
	old[n-1] = nil
	job.index = -1
	*h = old[:n-1]
	return job
}

// Ensure interface compliance
var _ fmt.Stringer = Priority(0)

func (p Priority) String() string {
	switch p {
	case PriorityLow:
		return "low"
	case PriorityNormal:
		return "normal"
	case PriorityHigh:
		return "high"
	default:
		return fmt.Sprintf("priority(%d)", int(p))
	}
}
