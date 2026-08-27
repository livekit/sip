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

package lksdk

import (
	"github.com/livekit/protocol/livekit"
)

type TranscriptionSegment struct {
	ID        string
	Text      string
	Language  string
	StartTime uint64
	EndTime   uint64
	Final     bool
}

func ExtractTranscriptionSegments(transcription *livekit.Transcription) []*TranscriptionSegment {
	var segments []*TranscriptionSegment
	if transcription == nil {
		return segments
	}
	segments = make([]*TranscriptionSegment, len(transcription.Segments))
	for i := range transcription.Segments {
		segments[i] = &TranscriptionSegment{
			ID:        transcription.Segments[i].Id,
			Text:      transcription.Segments[i].Text,
			Language:  transcription.Segments[i].Language,
			StartTime: transcription.Segments[i].StartTime,
			EndTime:   transcription.Segments[i].EndTime,
			Final:     transcription.Segments[i].Final,
		}
	}
	return segments
}
