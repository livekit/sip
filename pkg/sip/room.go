// Copyright 2023 LiveKit, Inc.
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

package sip

import (
	"context"

	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/opus"
	"github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/pkg/mixer"
)

type Room interface {
	Connect(conf *config.Config, roomName, identity, wsUrl, token string) error
	Output() media.Writer[media.PCM16Sample]
	SetOutput(out media.Writer[media.PCM16Sample])
	NewParticipant() (media.Writer[media.PCM16Sample], error)
	NewTrack() Track
	Close() error
}

type LkRoom struct {
	room     *lksdk.Room
	mix      *mixer.Mixer
	out      media.SwitchWriter[media.PCM16Sample]
	identity string
}

type lkRoomConfig struct {
	roomName string
	identity string
}

func NewLkRoom() *LkRoom {
	r := &LkRoom{}
	r.mix = mixer.NewMixer(&r.out, sampleRate)
	return r
}

func (r *LkRoom) Connect(conf *config.Config, roomName, identity, wsUrl, token string) error {
	var (
		err  error
		room *lksdk.Room
	)
	r.identity = identity
	roomCallback := &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				if track.Kind() != webrtc.RTPCodecTypeAudio {
					if err := pub.SetSubscribed(false); err != nil {
						logger.Errorw("Cannot unsubscribe from the track", err)
					}
					return
				}

				mtrack := r.NewTrack()
				defer mtrack.Close()

				odec, err := opus.Decode(mtrack, sampleRate, channels)
				if err != nil {
					return
				}
				h := rtp.NewMediaStreamIn[opus.Sample](odec)
				_ = rtp.HandleLoop(track, h)
			},
		},
	}

	if wsUrl == "" || token == "" {
		room, err = lksdk.ConnectToRoom(conf.WsUrl,
			lksdk.ConnectInfo{
				APIKey:              conf.ApiKey,
				APISecret:           conf.ApiSecret,
				RoomName:            roomName,
				ParticipantIdentity: identity,
			}, roomCallback)
	} else {
		room, err = lksdk.ConnectToRoomWithToken(wsUrl, token, roomCallback)
	}

	if err != nil {
		return err
	}
	r.room = room
	return nil
}

func ConnectToRoom(conf *config.Config, roomName string, identity string) (*LkRoom, error) {
	r := NewLkRoom()
	if err := r.Connect(conf, roomName, identity, "", ""); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *LkRoom) Output() media.Writer[media.PCM16Sample] {
	return r.out.Get()
}

func (r *LkRoom) SetOutput(out media.Writer[media.PCM16Sample]) {
	if r == nil {
		return
	}
	r.out.Set(out)
}

func (r *LkRoom) Close() error {
	if r.room != nil {
		r.room.Disconnect()
		r.room = nil
	}
	if r.mix != nil {
		r.mix.Stop()
		r.mix = nil
	}
	return nil
}

func (r *LkRoom) NewParticipant() (media.Writer[media.PCM16Sample], error) {
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion")
	if err != nil {
		return nil, err
	}
	if _, err = r.room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name: r.identity,
	}); err != nil {
		return nil, err
	}
	ow := media.FromSampleWriter[opus.Sample](track, sampleDur)
	pw, err := opus.Encode(ow, sampleRate, channels)
	if err != nil {
		return nil, err
	}
	return pw, nil
}

func (r *LkRoom) NewTrack() Track {
	inp := r.mix.AddInput()
	return &LkTrack{room: r, inp: inp}
}

type Track interface {
	Close() error
	PlayAudio(ctx context.Context, frames []media.PCM16Sample)
	WriteSample(pcm media.PCM16Sample) error
}

type LkTrack struct {
	room *LkRoom
	inp  *mixer.Input
}

func (t *LkTrack) Close() error {
	t.room.mix.RemoveInput(t.inp)
	return nil
}

func (t *LkTrack) PlayAudio(ctx context.Context, frames []media.PCM16Sample) {
	_ = media.PlayAudio[media.PCM16Sample](ctx, t, sampleDur, frames)
}

func (t *LkTrack) WriteSample(pcm media.PCM16Sample) error {
	return t.inp.WriteSample(pcm)
}
