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
	"errors"
	"io"
	"slices"
	"sync/atomic"

	"github.com/frostbyte73/core"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/sip"

	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/dtmf"
	"github.com/livekit/sip/pkg/media/opus"
	"github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/pkg/mixer"
)

type ParticipantInfo struct {
	ID       string
	RoomName string
	Identity string
	Name     string
}

type Room struct {
	log     logger.Logger
	room    *lksdk.Room
	mix     *mixer.Mixer
	out     *media.SwitchWriter[media.PCM16Sample]
	outDtmf atomic.Pointer[rtp.Stream]
	p       ParticipantInfo
	ready   atomic.Bool
	stopped core.Fuse
}

type ParticipantConfig struct {
	Identity   string
	Name       string
	Metadata   string
	Attributes map[string]string
}

type RoomConfig struct {
	WsUrl       string
	Token       string
	RoomName    string
	Participant ParticipantConfig
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

func (r *Room) Room() *lksdk.Room {
	if r == nil {
		return nil
	}
	return r.room
}

func (r *Room) Connect(conf *config.Config, rconf RoomConfig) error {
	if rconf.WsUrl == "" {
		rconf.WsUrl = conf.WsUrl
	}
	partConf := rconf.Participant
	r.p = ParticipantInfo{
		RoomName: rconf.RoomName,
		Identity: partConf.Identity,
		Name:     partConf.Name,
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
				if !r.ready.Load() {
					r.log.Warnw("ignoring track, room not ready", nil, "trackID", pub.SID())
					return
				}

				go func() {
					mTrack := r.NewTrack()
					defer mTrack.Close()

					odec, err := opus.Decode(mTrack, channels)
					if err != nil {
						r.log.Errorw("cannot create opus decoder", err, "trackID", pub.SID())
						return
					}
					defer odec.Close()

					var h rtp.Handler = rtp.NewMediaStreamIn[opus.Sample](odec)
					h = rtp.HandleJitter(int(track.Codec().ClockRate), h)
					err = rtp.HandleLoop(track, h)
					if err != nil && errors.Unwrap(err) != io.EOF {
						logger.Infow("room track rtp handler returned with failure", "error", err)
					}
				}()
			},
			OnDataPacket: func(data lksdk.DataPacket, params lksdk.DataReceiveParams) {
				switch data := data.(type) {
				case *livekit.SipDTMF:
					// TODO: Only generate audio DTMF if the message was a broadcast from another SIP participant.
					//       DTMF audio tone will be automatically mixed in any case.
					r.sendDTMF(data, conf.AudioDTMF)
				}
			},
		},
		OnDisconnected: func() {
			r.stopped.Break()
		},
	}

	if rconf.Token == "" {
		// TODO: Remove this code path, always sign tokens on LiveKit server.
		//       For now, match Cloud behavior and do not send extra attrs in the token.
		tokenAttrs := make(map[string]string, len(partConf.Attributes))
		for _, k := range []string{
			livekit.AttrSIPCallID,
			livekit.AttrSIPTrunkID,
			livekit.AttrSIPDispatchRuleID,
			livekit.AttrSIPTrunkNumber,
			livekit.AttrSIPPhoneNumber,
		} {
			if v, ok := partConf.Attributes[k]; ok {
				tokenAttrs[k] = v
			}
		}
		var err error
		rconf.Token, err = sip.BuildSIPToken(
			conf.ApiKey, conf.ApiSecret, rconf.RoomName,
			partConf.Identity, partConf.Name, partConf.Metadata,
			tokenAttrs,
		)
		if err != nil {
			return err
		}
	}
	room := lksdk.NewRoom(roomCallback)
	room.SetLogger(r.log)
	err := room.JoinWithToken(rconf.WsUrl, rconf.Token, lksdk.WithAutoSubscribe(false))
	if err != nil {
		return err
	}
	r.room = room
	r.p.ID = r.room.LocalParticipant.SID()
	r.p.Identity = r.room.LocalParticipant.Identity()
	room.LocalParticipant.SetAttributes(partConf.Attributes)
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

// SwapOutput sets room audio output and returns the old one.
// Caller is responsible for closing the old writer.
func (r *Room) SwapOutput(out media.PCM16Writer) media.PCM16Writer {
	if r == nil {
		return nil
	}
	if out == nil {
		return r.out.Swap(nil)
	}
	return r.out.Swap(media.ResampleWriter(out, r.mix.SampleRate()))
}

func (r *Room) CloseOutput() error {
	w := r.SwapOutput(nil)
	if w == nil {
		return nil
	}
	return w.Close()
}

func (r *Room) SetDTMFOutput(out *rtp.Stream) {
	if r == nil {
		return
	}
	if out == nil {
		r.outDtmf.Store(nil)
		return
	}
	r.outDtmf.Store(out)
}

func (r *Room) sendDTMF(msg *livekit.SipDTMF, audio bool) {
	outAudio := r.Output()
	if !audio {
		outAudio = nil
	}
	outDTMF := r.outDtmf.Load()
	if outAudio == nil && outDTMF == nil {
		r.log.Infow("ignoring dtmf", "digit", msg.Digit)
		return
	}
	if outAudio != nil {
		// We should still mix other audio too. Need a separate track for DTMF tones.
		t := r.NewTrack()
		defer t.Close()
		outAudio = t
	}
	// TODO: Separate goroutine?
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.log.Infow("forwarding dtmf to sip", "digit", msg.Digit)
	_ = dtmf.Write(ctx, outAudio, outDTMF, msg.Digit)
}

func (r *Room) Close() error {
	if r == nil {
		return nil
	}
	r.ready.Store(false)
	err := r.CloseOutput()
	r.SetDTMFOutput(nil)
	if r.room != nil {
		r.room.Disconnect()
		r.room = nil
	}
	if r.mix != nil {
		r.mix.Stop()
		r.mix = nil
	}
	return err
}

func (r *Room) Participant() ParticipantInfo {
	if r == nil {
		return ParticipantInfo{}
	}
	return r.p
}

func (r *Room) NewParticipantTrack(sampleRate int) (media.WriteCloser[media.PCM16Sample], error) {
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
	return &Track{inp: r.mix.NewInput()}
}

type Track struct {
	inp *mixer.Input
}

func (t *Track) Close() error {
	return t.inp.Close()
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
	return t.inp.SampleRate()
}

func (t *Track) WriteSample(pcm media.PCM16Sample) error {
	return t.inp.WriteSample(pcm)
}
