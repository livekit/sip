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
	"context"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
)

type SampleProvider interface {
	NextSample(context.Context) (media.Sample, error)
	OnBind() error
	OnUnbind() error
	Close() error
}

type AudioSampleProvider interface {
	SampleProvider
	CurrentAudioLevel() uint8
}

// BaseSampleProvider provides empty implementations for OnBind and OnUnbind
type BaseSampleProvider struct {
}

func (p *BaseSampleProvider) OnBind() error {
	return nil
}

func (p *BaseSampleProvider) OnUnbind() error {
	return nil
}

func (p *BaseSampleProvider) Close() error {
	return nil
}

// NullSampleProvider is a media provider that provides null packets, it could meet a certain bitrate, if desired
type NullSampleProvider struct {
	BaseSampleProvider
	BytesPerSample uint32
	SampleDuration time.Duration
}

func NewNullSampleProvider(bitrate uint32) *NullSampleProvider {
	return &NullSampleProvider{
		SampleDuration: time.Second / 30,
		BytesPerSample: bitrate / 8 / 30,
	}
}

func (p *NullSampleProvider) NextSample(ctx context.Context) (media.Sample, error) {
	return media.Sample{
		Data:     make([]byte, p.BytesPerSample),
		Duration: p.SampleDuration,
	}, nil
}
