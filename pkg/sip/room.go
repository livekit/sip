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
	"github.com/livekit/sip/pkg/media/h264"

	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/opus"
	"github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/pkg/mixer"
)

type Room struct {
	room     *lksdk.Room
	mix      *mixer.Mixer
	audioOut media.SwitchWriter[media.PCM16Sample]
	videoOut media.SwitchWriter[h264.Sample]
	identity string
}

type lkRoomConfig struct {
	roomName string
	identity string
}

func NewRoom() *Room {
	r := &Room{}
	r.mix = mixer.NewMixer(&r.audioOut, sampleRate)
	return r
}

func (r *Room) Connect(conf *config.Config, roomName, identity, wsUrl, token string) error {
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

func ConnectToRoom(conf *config.Config, roomName string, identity string) (*Room, error) {
	r := NewRoom()
	if err := r.Connect(conf, roomName, identity, "", ""); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Room) AudioOutput() media.Writer[media.PCM16Sample] {
	return r.audioOut.Get()
}

func (r *Room) VideoOutput() media.Writer[h264.Sample] { return r.videoOut.Get() }

func (r *Room) SetAudioOutput(out media.Writer[media.PCM16Sample]) {
	if r == nil {
		return
	}
	r.audioOut.Set(out)
}

func (r *Room) SetVideoOutput(out media.Writer[h264.Sample]) {
	if r == nil {
		return
	}
	r.videoOut.Set(out)
}

func (r *Room) Close() error {
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

func (r *Room) NewParticipant() (media.Writer[media.PCM16Sample], media.Writer[h264.Sample], error) {
	audioTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, r.identity+"-audio", r.identity+"-audio-pion")
	if err != nil {
		return nil, nil, err
	}

	videoTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, r.identity+"-video", r.identity+"-video-pion")
	if err != nil {
		return nil, nil, err
	}

	if _, err = r.room.LocalParticipant.PublishTrack(audioTrack, &lksdk.TrackPublicationOptions{
		Name: r.identity,
	}); err != nil {
		return nil, nil, err
	}

	if _, err = r.room.LocalParticipant.PublishTrack(videoTrack, &lksdk.TrackPublicationOptions{
		Name:        r.identity + "-track",
		VideoWidth:  1280,
		VideoHeight: 720,
	}); err != nil {
		return nil, nil, err
	}

	//ow方法 构造写入opus encode后的字节数据方法，并通过audioTrack写入livekit
	ow := media.FromSampleWriter[opus.Sample](audioTrack, sampleDur)
	//pw方法 构造输入int16 opus encode后输出[]byte数据
	pw, err := opus.Encode(ow, sampleRate, channels)
	if err != nil {
		return nil, nil, err
	}

	vw := h264.BuildSampleWriter[h264.Sample](videoTrack, sampleDur)
	return pw, vw, nil
}

func (r *Room) NewTrack() *Track {
	inp := r.mix.AddInput()
	return &Track{room: r, inp: inp}
}

type Track struct {
	room *Room
	inp  *mixer.Input
}

func (t *Track) Close() error {
	t.room.mix.RemoveInput(t.inp)
	return nil
}

func (t *Track) PlayAudio(ctx context.Context, frames []media.PCM16Sample) {
	_ = media.PlayAudio[media.PCM16Sample](ctx, t, sampleDur, frames)
}

func (t *Track) WriteSample(pcm media.PCM16Sample) error {
	return t.inp.WriteSample(pcm)
}
