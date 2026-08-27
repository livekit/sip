// Copyright 2026 LiveKit, Inc.
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

package synchronizer

import (
	"time"

	"github.com/pion/rtcp"

	"github.com/livekit/media-sdk/jitter"
)

// Sync is the top-level synchronization interface.
// Implemented by both Synchronizer (legacy) and SyncEngine (new).
type Sync interface {
	AddTrack(track TrackRemote, participantID string) TrackSync
	RemoveTrack(trackID string)
	OnRTCP(packet rtcp.Packet)
	End()
	GetStartedAt() int64
	GetEndedAt() int64
	SetMediaRunningTime(mediaRunningTime func() (time.Duration, bool))
}

// TrackSync is the per-track synchronization interface.
// Implemented by both TrackSynchronizer (legacy) and syncEngineTrack (new).
type TrackSync interface {
	PrimeForStart(pkt jitter.ExtPacket) ([]jitter.ExtPacket, int, bool)
	GetPTS(pkt jitter.ExtPacket) (time.Duration, error)
	OnSenderReport(f func(drift time.Duration))
	LastPTSAdjusted() time.Duration
	Close()
}
