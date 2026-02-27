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
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frostbyte73/core"
	"github.com/pion/webrtc/v4"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/jitter"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/logger/medialogutils"
	"github.com/livekit/protocol/sip"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/livekit/media-sdk/mixer"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/media/opus"
)

type RoomStatsSnapshot struct {
	// Stats quantifying total incoming traffic from all tracks
	InputPackets   uint64 `json:"input_packets"`
	InputBytes     uint64 `json:"input_bytes"`
	Resets         uint64 `json:"resets"`
	Gaps           uint64 `json:"gaps"`
	GapsSum        uint64 `json:"gaps_sum"`
	Late           uint64 `json:"late"`
	LateSum        uint64 `json:"late_sum"`
	DelayedPackets uint64 `json:"delayed_packets"`
	DelayedSum     uint64 `json:"delayed_sum"`
	RapidPackets   uint64 `json:"rapid_packets"`
	DataPackets    uint64 `json:"data_packets"`

	// Stats quantifying total outgoing traffic
	PublishedFrames  uint64  `json:"published_frames"`
	PublishedSamples uint64  `json:"published_samples"`
	PublishTX        float64 `json:"publish_tx"`

	JitterBufferPacketsLost    uint64 `json:"jitter_buffer_packets_lost"`
	JitterBufferPacketsDropped uint64 `json:"jitter_buffer_packets_dropped"`

	Closed bool `json:"closed"`
}

type RoomStats struct {
	PublishedFrames  atomic.Uint64
	PublishedSamples atomic.Uint64
	PublishTX        atomic.Uint64

	rtpStats    rtpCountingStats
	dataPackets atomic.Uint64

	JitterBufferPacketsLost    atomic.Uint64
	JitterBufferPacketsDropped atomic.Uint64

	Mixer mixer.Stats

	Closed atomic.Bool

	mu   sync.Mutex
	last struct {
		Time             time.Time
		PublishedSamples uint64
	}
}

func (s *RoomStats) Load() RoomStatsSnapshot {
	return RoomStatsSnapshot{
		InputPackets:               s.rtpStats.packets.Load(),
		InputBytes:                 s.rtpStats.bytes.Load(),
		Resets:                     s.rtpStats.resets.Load(),
		Gaps:                       s.rtpStats.gaps.Load(),
		GapsSum:                    s.rtpStats.gapsSum.Load(),
		Late:                       s.rtpStats.late.Load(),
		LateSum:                    s.rtpStats.lateSum.Load(),
		DelayedPackets:             s.rtpStats.delayedPackets.Load(),
		DelayedSum:                 s.rtpStats.delayedSum.Load(),
		RapidPackets:               s.rtpStats.rapidPackets.Load(),
		DataPackets:                s.dataPackets.Load(),
		JitterBufferPacketsLost:    s.JitterBufferPacketsLost.Load(),
		JitterBufferPacketsDropped: s.JitterBufferPacketsDropped.Load(),

		PublishedFrames:  s.PublishedFrames.Load(),
		PublishedSamples: s.PublishedSamples.Load(),
		PublishTX:        math.Float64frombits(s.PublishTX.Load()),
		Closed:           s.Closed.Load(),
	}
}

func (s *RoomStats) Update() {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := time.Now()
	dt := t.Sub(s.last.Time).Seconds()

	curPublishedSamples := s.PublishedSamples.Load()

	if dt > 0 {
		txSamples := curPublishedSamples - s.last.PublishedSamples

		txRate := float64(txSamples) / dt

		s.PublishTX.Store(math.Float64bits(txRate))
	}

	s.last.Time = t
	s.last.PublishedSamples = curPublishedSamples
}

type ParticipantInfo struct {
	ID       string
	RoomName string
	Identity string
	Name     string
}

// RoomInterface defines the interface for room operations
type RoomInterface interface {
	Connect(conf *config.Config, rconf RoomConfig) error
	Closed() <-chan struct{}
	Subscribed() <-chan struct{}
	Room() *lksdk.Room
	Subscribe()
	Output() msdk.Writer[msdk.PCM16Sample]
	SwapOutput(out msdk.PCM16Writer) msdk.PCM16Writer
	CloseOutput() error
	SetDTMFOutput(w dtmf.Writer)
	Close() error
	CloseWithReason(reason livekit.DisconnectReason) error
	Participant() ParticipantInfo
	NewParticipantTrack(sampleRate int) (msdk.WriteCloser[msdk.PCM16Sample], error)
	SendData(data lksdk.DataPacket, opts ...lksdk.DataPublishOption) error
	NewTrack() *mixer.Input
}

type GetRoomFunc func(log logger.Logger, st *RoomStats) RoomInterface

func DefaultGetRoomFunc(log logger.Logger, st *RoomStats) RoomInterface {
	return NewRoom(log, st)
}

type Room struct {
	log        logger.Logger
	roomLog    logger.Logger // deferred logger
	room       *lksdk.Room
	mix        *mixer.Mixer
	out        *msdk.SwitchWriter
	outDtmf    atomic.Pointer[dtmf.Writer]
	p          ParticipantInfo
	ready      core.Fuse
	subscribe  atomic.Bool
	subscribed core.Fuse
	stopped    core.Fuse
	closed     core.Fuse
	stats      *RoomStats
}

type ParticipantConfig struct {
	Identity   string
	Name       string
	Metadata   string
	Attributes map[string]string
}

type RoomConfig struct {
	WsUrl            string
	Token            string
	RoomName         string
	Participant      ParticipantConfig
	RoomPreset       string
	RoomConfig       *livekit.RoomConfiguration
	JitterBuf        bool
	LogSignalChanges bool
}

func NewRoom(log logger.Logger, st *RoomStats) *Room {
	if st == nil {
		st = &RoomStats{}
	}
	r := &Room{log: log, stats: st, out: msdk.NewSwitchWriter(RoomSampleRate)}

	var err error
	r.mix, err = mixer.NewMixer(r.out, rtp.DefFrameDur, 1, mixer.WithStats(&st.Mixer), mixer.WithOutputChannel())
	if err != nil {
		panic(err)
	}

	roomLog, resolve := log.WithDeferredValues()
	r.roomLog = roomLog

	go func() {
		select {
		case <-r.ready.Watch():
			if r.room != nil {
				resolve.Resolve("room", r.room.Name(), "roomID", r.room.SID())
			} else {
				resolve.Resolve()
			}
		case <-r.stopped.Watch():
			resolve.Resolve()
		case <-r.closed.Watch():
			resolve.Resolve()
		}
	}()

	return r
}

func (r *Room) Closed() <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.stopped.Watch()
}

func (r *Room) Subscribed() <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.subscribed.Watch()
}

func (r *Room) Room() *lksdk.Room {
	if r == nil {
		return nil
	}
	return r.room
}

func (r *Room) participantJoin(rp *lksdk.RemoteParticipant) {
	log := r.roomLog.WithValues("participant", rp.Identity(), "pID", rp.SID())
	log.Debugw("participant joined")
	switch rp.Kind() {
	case lksdk.ParticipantSIP:
		// Avoid a deadlock where two SIP participant join a room and won't publish their track.
		// Each waits for the other's track to subscribe before publishing its own track.
		// So we just assume SIP participants will eventually start speaking.
		r.subscribed.Break()
		log.Infow("unblocking subscription - second sip participant is in the room")
	}
}

func (r *Room) participantLeft(rp *lksdk.RemoteParticipant) {
	log := r.roomLog.WithValues("participant", rp.Identity(), "pID", rp.SID())
	log.Debugw("participant left")
}

func (r *Room) subscribeTo(pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	log := r.roomLog.WithValues("participant", rp.Identity(), "pID", rp.SID(), "trackID", pub.SID(), "trackName", pub.Name())
	if pub.Kind() != lksdk.TrackKindAudio {
		log.Debugw("skipping non-audio track")
		return
	}
	log.Debugw("subscribing to a track")
	if err := pub.SetSubscribed(true); err != nil {
		log.Errorw("cannot subscribe to the track", err)
		return
	}
	r.subscribed.Break()
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
	roomCallback := &lksdk.RoomCallback{
		OnParticipantConnected: func(rp *lksdk.RemoteParticipant) {
			log := r.roomLog.WithValues("participant", rp.Identity(), "pID", rp.SID())
			if !r.subscribe.Load() {
				log.Debugw("skipping participant join event - subscribed flag not set")
				return // will subscribe later
			}
			r.participantJoin(rp)
		},
		OnParticipantDisconnected: func(rp *lksdk.RemoteParticipant) {
			r.participantLeft(rp)
		},
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackPublished: func(pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				log := r.roomLog.WithValues("participant", rp.Identity(), "pID", rp.SID(), "trackID", pub.SID(), "trackName", pub.Name())
				if !r.subscribe.Load() {
					log.Debugw("skipping track publish event - subscribed flag not set")
					return // will subscribe later
				}
				r.subscribeTo(pub, rp)
			},
			OnTrackSubscribed: func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				log := r.roomLog.WithValues("participant", rp.Identity(), "pID", rp.SID(), "trackID", track.ID(), "trackName", pub.Name())
				if !r.ready.IsBroken() {
					log.Warnw("ignoring track, room not ready", nil)
					return
				}
				log.Infow("mixing track")

				go func() {
					mTrack := r.NewTrack()
					if mTrack == nil {
						return // closed
					}
					defer log.Infow("track closed")
					defer mTrack.Close()

					var out msdk.PCM16Writer = mTrack
					if rconf.LogSignalChanges {
						var err error
						out, err = NewSignalLogger(log, track.ID(), out)
						if err != nil {
							log.Errorw("cannot create signal logger", err)
							return
						}
					}

					codec, err := opus.Decode(out, channels, log)
					if err != nil {
						log.Errorw("cannot create opus decoder", err)
						return
					}
					defer codec.Close()

					var h rtp.HandlerCloser = rtp.NewNopCloser(rtp.NewMediaStreamIn[opus.Sample](codec))
					if conf.EnableJitterBuffer {
						h = rtp.HandleJitter(h, jitter.WithPacketLossHandler(func(packetsLost, packetsDropped uint64) {
							r.stats.JitterBufferPacketsLost.Store(packetsLost)
							r.stats.JitterBufferPacketsDropped.Store(packetsDropped)
						}))
					}

					h = newRTPStreamStats(h, &r.stats.rtpStats)
					err = rtp.HandleLoop(track, h)
					if err != nil && !errors.Is(err, io.EOF) {
						log.Infow("room track rtp handler returned with failure", "error", err)
					}
				}()
			},
			OnDataPacket: func(data lksdk.DataPacket, params lksdk.DataReceiveParams) {
				switch data := data.(type) {
				case *livekit.SipDTMF:
					r.stats.dataPackets.Add(1)
					// TODO: Only generate audio DTMF if the message was a broadcast from another SIP participant.
					//       DTMF audio tone will be automatically mixed in any case.
					r.sendDTMF(context.Background(), data)
				}
			},
			OnTrackUnsubscribed: func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				r.roomLog.Infow("track unsubscribed", "participant", rp.Identity(), "pID", rp.SID(), "trackID", track.ID(), "trackName", pub.Name())
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
		rconf.Token, err = sip.BuildSIPToken(sip.SIPTokenParams{
			APIKey:                conf.ApiKey,
			APISecret:             conf.ApiSecret,
			RoomName:              rconf.RoomName,
			ParticipantIdentity:   partConf.Identity,
			ParticipantName:       partConf.Name,
			ParticipantMetadata:   partConf.Metadata,
			ParticipantAttributes: tokenAttrs,
			RoomPreset:            rconf.RoomPreset,
			RoomConfig:            rconf.RoomConfig,
		})
		if err != nil {
			return err
		}
	}
	room := lksdk.NewRoom(roomCallback)
	room.SetLogger(medialogutils.NewOverrideLogger(r.log))
	err := room.JoinWithToken(rconf.WsUrl, rconf.Token,
		lksdk.WithAutoSubscribe(false),
		lksdk.WithExtraAttributes(partConf.Attributes),
	)
	if err != nil {
		return err
	}
	r.room = room
	r.p.ID = r.room.LocalParticipant.SID()
	r.p.Identity = r.room.LocalParticipant.Identity()
	r.log = r.log.WithValues("room", r.room.Name(), "roomID", r.room.SID(), "participant", r.p.Identity, "pID", r.p.ID)
	r.log.Infow("SIP participant joined room")
	room.LocalParticipant.SetAttributes(partConf.Attributes)
	r.ready.Break()
	r.subscribe.Store(false) // already false, but keep for visibility

	// Not subscribing to any tracks just yet!
	return nil
}

func (r *Room) Subscribe() {
	if r.room == nil {
		return
	}
	r.subscribe.Store(true)
	list := r.room.GetRemoteParticipants()
	r.log.Debugw("subscribing to existing room participants", "participants", len(list))
	for _, rp := range list {
		r.participantJoin(rp)
		for _, pub := range rp.TrackPublications() {
			if remotePub, ok := pub.(*lksdk.RemoteTrackPublication); ok {
				r.subscribeTo(remotePub, rp)
			}
		}
	}
}

func (r *Room) Output() msdk.Writer[msdk.PCM16Sample] {
	return r.out.Get()
}

// SwapOutput sets room audio output and returns the old one.
// Caller is responsible for closing the old writer.
func (r *Room) SwapOutput(out msdk.PCM16Writer) msdk.PCM16Writer {
	if r == nil {
		return nil
	}
	if out == nil {
		return r.out.Swap(nil)
	}
	return r.out.Swap(msdk.ResampleWriter(out, r.mix.SampleRate()))
}

func (r *Room) CloseOutput() error {
	w := r.SwapOutput(nil)
	if w == nil {
		return nil
	}
	return w.Close()
}

func (r *Room) SetDTMFOutput(w dtmf.Writer) {
	if r == nil {
		return
	}
	if w == nil {
		r.outDtmf.Store(nil)
		return
	}
	r.outDtmf.Store(&w)
}

func (r *Room) sendDTMF(ctx context.Context, msg *livekit.SipDTMF) {
	outDTMF := r.outDtmf.Load()
	if outDTMF == nil {
		r.log.Infow("ignoring dtmf", "digit", msg.Digit)
		return
	}
	// TODO: Separate goroutine?
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.log.Infow("forwarding dtmf to sip", "digit", msg.Digit)
	_ = (*outDTMF).WriteDTMF(ctx, msg.Digit)
}

func (r *Room) Close() error {
	return r.CloseWithReason(livekit.DisconnectReason_UNKNOWN_REASON)
}

func (r *Room) CloseWithReason(reason livekit.DisconnectReason) error {
	if r == nil {
		return nil
	}
	var err error
	r.closed.Once(func() {
		defer r.stats.Closed.Store(true)

		r.subscribe.Store(false)
		err = r.CloseOutput()
		r.SetDTMFOutput(nil)
		if r.room != nil {
			r.room.DisconnectWithReason(reason)
			r.room = nil
		}
		if r.mix != nil {
			r.mix.Stop()
		}
	})
	return err
}

func (r *Room) Participant() ParticipantInfo {
	if r == nil {
		return ParticipantInfo{}
	}
	return r.p
}

func (r *Room) NewParticipantTrack(sampleRate int) (msdk.WriteCloser[msdk.PCM16Sample], error) {
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
	ow := msdk.FromSampleWriter[opus.Sample](track, sampleRate, rtp.DefFrameDur)
	pw, err := opus.Encode(ow, channels, r.log)
	if err != nil {
		return nil, err
	}
	return newMediaWriterCount(pw, &r.stats.PublishedFrames, &r.stats.PublishedSamples), nil
}

func (r *Room) SendData(data lksdk.DataPacket, opts ...lksdk.DataPublishOption) error {
	if r == nil || !r.ready.IsBroken() || r.closed.IsBroken() {
		return nil
	}
	return r.room.LocalParticipant.PublishDataPacket(data, opts...)
}

func (r *Room) NewTrack() *mixer.Input {
	if r == nil {
		return nil
	}
	return r.mix.NewInput()
}
