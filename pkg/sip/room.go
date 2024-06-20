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
	"slices"
	"sync/atomic"

	"github.com/frostbyte73/core"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/opus"
	"github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/pkg/mixer"
)

type Participant struct {
	ID       string
	RoomName string
	Identity string
	Name     string
	Metadata string
}

type Room struct {
	log     logger.Logger
	room    *lksdk.Room
	mix     *mixer.Mixer
	out     *media.SwitchWriter[media.PCM16Sample]
	p       Participant
	ready   atomic.Bool
	stopped core.Fuse
}

type lkRoomConfig struct {
	roomName string
	identity string
	name     string
	meta     string
	wsUrl    string
	token    string
}

func NewRoom(log logger.Logger) *Room {
	r := &Room{log: log, out: media.NewSwitchWriter[media.PCM16Sample](RoomSampleRate)}
	r.mix = mixer.NewMixer(r.out, rtp.DefFrameDur)
	return r
}

func (r *Room) Closed() <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.stopped.Watch()
}

func (r *Room) Connect(conf *config.Config, roomName, identity, name, meta, wsUrl, token string) error {
	var (
		err  error
		room *lksdk.Room
	)
	r.p = Participant{
		RoomName: roomName,
		Identity: identity,
		Name:     name,
		Metadata: meta,
	}
	handleTrackPublished := func(publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
		if publication.Kind() == lksdk.TrackKindAudio {
			if err := publication.SetSubscribed(true); err != nil {
				r.log.Errorw("cannot subscribe to the track", err, "trackID", publication.SID())
			}
		}
	}
	roomCallback := &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackPublished: handleTrackPublished,
			OnTrackSubscribed: func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				mTrack := r.NewTrack()
				defer mTrack.Close()

				odec, err := opus.Decode(mTrack, channels)
				if err != nil {
					return
				}
				h := rtp.NewMediaStreamIn[opus.Sample](odec)
				_ = rtp.HandleLoop(track, h)
			},
		},
		OnDisconnected: func() {
			r.stopped.Break()
		},
	}

	if wsUrl == "" || token == "" {
		room, err = lksdk.ConnectToRoom(conf.WsUrl,
			lksdk.ConnectInfo{
				APIKey:              conf.ApiKey,
				APISecret:           conf.ApiSecret,
				RoomName:            roomName,
				ParticipantIdentity: identity,
				ParticipantName:     name,
				ParticipantMetadata: meta,
				ParticipantKind:     lksdk.ParticipantSIP,
			}, roomCallback, lksdk.WithAutoSubscribe(false))
	} else {
		room, err = lksdk.ConnectToRoomWithToken(wsUrl, token, roomCallback, lksdk.WithAutoSubscribe(false))
	}

	if err != nil {
		return err
	}
	r.room = room
	r.p.ID = r.room.LocalParticipant.SID()
	r.p.Identity = r.room.LocalParticipant.Identity()
	r.ready.Store(true)

	// since TrackPublished isn't fired for tracks published *before* the participant is in the room, we'll
	// need to handle them manually
	for _, rp := range room.GetRemoteParticipants() {
		for _, pub := range rp.TrackPublications() {
			if remotePub, ok := pub.(*lksdk.RemoteTrackPublication); ok {
				handleTrackPublished(remotePub, rp)
			}
		}
	}
	return nil
}

func (r *Room) Output() media.Writer[media.PCM16Sample] {
	return r.out.Get()
}

func (r *Room) SetOutput(out media.Writer[media.PCM16Sample]) {
	if r == nil {
		return
	}
	if out == nil {
		r.out.Set(nil)
		return
	}
	r.out.Set(media.ResampleWriter(out, r.mix.SampleRate()))
}

func (r *Room) Close() error {
	r.ready.Store(false)
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

func (r *Room) Participant() Participant {
	if r == nil {
		return Participant{}
	}
	return r.p
}

func (r *Room) NewParticipantTrack(sampleRate int) (media.Writer[media.PCM16Sample], error) {
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion")
	if err != nil {
		return nil, err
	}
	p := r.room.LocalParticipant
	if _, err = p.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name: p.Identity(),
	}); err != nil {
		return nil, err
	}
	ow := media.FromSampleWriter[opus.Sample](track, sampleRate, rtp.DefFrameDur)
	pw, err := opus.Encode(ow, channels)
	if err != nil {
		return nil, err
	}
	return pw, nil
}

func (r *Room) SendData(data lksdk.DataPacket, opts ...lksdk.DataPublishOption) error {
	if r == nil || !r.ready.Load() {
		return nil
	}
	return r.room.LocalParticipant.PublishDataPacket(data, opts...)
}

func (r *Room) NewTrack() *Track {
	inp := r.mix.NewInput()
	return &Track{mix: r.mix, inp: inp}
}

type Track struct {
	mix *mixer.Mixer
	inp *mixer.Input
}

func (t *Track) Close() error {
	t.mix.RemoveInput(t.inp)
	return nil
}

func (t *Track) PlayAudio(ctx context.Context, sampleRate int, frames []media.PCM16Sample) {
	if t.SampleRate() != sampleRate {
		frames = slices.Clone(frames)
		for i := range frames {
			frames[i] = media.Resample(nil, t.SampleRate(), frames[i], sampleRate)
		}
	}
	_ = media.PlayAudio[media.PCM16Sample](ctx, t, rtp.DefFrameDur, frames)
}

func (t *Track) SampleRate() int {
	return t.mix.SampleRate()
}

func (t *Track) WriteSample(pcm media.PCM16Sample) error {
	return t.inp.WriteSample(pcm)
}
