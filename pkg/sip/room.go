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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frostbyte73/core"
	"github.com/pion/webrtc/v4"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/g711"
	"github.com/livekit/media-sdk/jitter"
	"github.com/livekit/media-sdk/mixer"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/sip"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/media/opus"
)

// errRoomClosed is returned when the room handle is gone, which happens once the
// call has been torn down.
var errRoomClosed = errors.New("room is closed")

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

	TrackSubscribes uint64 `json:"track_subscribes"`
	Resumes         uint64 `json:"resumes"`
	Reconnects      uint64 `json:"reconnects"`
	// Recovering reports whether the signal connection was down when the
	// snapshot was taken. PublishedFrames and PublishTX are unreliable while set.
	Recovering bool `json:"recovering"`

	LatencyOutRecv LatencyStatsSnapshot `json:"latency_out_recv"`

	Closed bool `json:"closed"`
}

type RoomStats struct {
	PublishedFrames  atomic.Uint64
	PublishedSamples atomic.Uint64
	PublishTX        atomic.Uint64

	rtpStats    rtpCountingStats
	dataPackets atomic.Uint64

	// TrackSubscribes counts subscribe requests issued for remote tracks.
	// Attempts, not confirmations.
	TrackSubscribes atomic.Uint64

	// Resumes and Reconnects count the two ways the signal connection recovers
	// during a call, and are mutually exclusive. A resume keeps the peer
	// connections and subscriptions; a reconnect rebuilds them. Neither is
	// counted until the recovery succeeds.
	Resumes    atomic.Uint64
	Reconnects atomic.Uint64

	// Recovering is set while the signal connection is down. PublishedFrames
	// and PublishTX are counted before the track write, so they keep reporting a
	// healthy rate even though the audio is being dropped. Read them only when
	// this is false.
	Recovering atomic.Bool

	JitterBufferPacketsLost    atomic.Uint64
	JitterBufferPacketsDropped atomic.Uint64

	LatencyOutRecv LatencyStats // measures track recv → opus decode → mixer input.

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

		TrackSubscribes: s.TrackSubscribes.Load(),
		Resumes:         s.Resumes.Load(),
		Reconnects:      s.Reconnects.Load(),
		Recovering:      s.Recovering.Load(),

		PublishedFrames:  s.PublishedFrames.Load(),
		PublishedSamples: s.PublishedSamples.Load(),
		PublishTX:        math.Float64frombits(s.PublishTX.Load()),
		LatencyOutRecv:   s.LatencyOutRecv.Load(),
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
	Connect(ctx context.Context, conf *config.Config, rconf RoomConfig) error
	Closed() <-chan struct{}
	ClosedReason() livekit.DisconnectReason
	Subscribed() <-chan struct{}
	Room() *lksdk.Room
	Subscribe()
	Close() error
	CloseWithReason(reason livekit.DisconnectReason) error
	Participant() ParticipantInfo
	NewParticipantTrack(sampleRate int) (msdk.WriteCloser[msdk.PCM16Sample], error)
	NewTrack() *mixer.Input
	lksdk.RoomRPCInterface

	// WriteOutboundAudioTo tells the room where to send audio to.
	// Returns the previously-set writer (if one exists).
	WriteOutboundAudioTo(w msdk.PCM16Writer) msdk.PCM16Writer

	// WriteOutboundDTMFTo tells the room where to send DTMF to.
	// Returns the previously-set writer (if one exists).
	WriteOutboundDTMFTo(w msdk.WriteCloser[*livekit.SipDTMF]) msdk.WriteCloser[*livekit.SipDTMF]

	// GetInboundAudioWriter returns a writer that, when written to, writes
	// audio to the room.
	GetInboundAudioWriter() (msdk.PCM16Writer, error)
	// GetInboundDTMFWriter returns a writer that, when written to, writes DTMF
	// to the room.
	GetInboundDTMFWriter() msdk.WriteCloser[*livekit.SipDTMF]
}

type GetRoomFunc func(log logger.Logger, st *RoomStats) RoomInterface

func DefaultGetRoomFunc(log logger.Logger, st *RoomStats) RoomInterface {
	return NewRoom(log, st)
}

type Room struct {
	log     logger.Logger
	roomLog logger.Logger // deferred logger
	// room is cleared on close while SDK callback goroutines still read it.
	room atomic.Pointer[lksdk.Room]
	mix  *mixer.Mixer

	outboundAudio *msdk.WriteCloserSwitch[msdk.PCM16Sample]
	outboundDTMF  *msdk.WriteCloserSwitch[*livekit.SipDTMF]
	inboundDTMF   inboundDTMFWriter

	// p is replaced on every reconnect, since the server issues a new
	// participant SID, and read concurrently by Participant().
	p          atomic.Pointer[ParticipantInfo]
	reconnect  atomic.Pointer[reconnectState]
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
	r := &Room{
		log:   log,
		stats: st,

		outboundAudio: msdk.NewWriteCloserSwitch[msdk.PCM16Sample](RoomSampleRate),
		outboundDTMF:  msdk.NewWriteCloserSwitch[*livekit.SipDTMF](0),
	}
	r.inboundDTMF = inboundDTMFWriter{r}

	var err error
	r.mix, err = mixer.NewMixer(r.outboundAudio, rtp.DefFrameDur, 1, mixer.WithStats(&st.Mixer), mixer.WithOutputChannel())
	if err != nil {
		panic(err)
	}

	roomLog, resolve := log.WithDeferredValues()
	r.roomLog = roomLog

	go func() {
		select {
		case <-r.ready.Watch():
			if room := r.room.Load(); room != nil {
				resolve.Resolve("room", room.Name(), "roomID", room.SID())
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

// ClosedReason returns the raw protocol disconnect reason once Closed() has
// fired. Returns livekit.DisconnectReason_UNKNOWN_REASON if the room hasn't
// disconnected or no reason was reported.
func (r *Room) ClosedReason() livekit.DisconnectReason {
	if r == nil {
		return livekit.DisconnectReason_UNKNOWN_REASON
	}
	room := r.room.Load()
	if room == nil {
		return livekit.DisconnectReason_UNKNOWN_REASON
	}
	return room.DisconnectReason()
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
	return r.room.Load()
}

func (r *Room) participantJoin(rp *lksdk.RemoteParticipant) {
	log := r.roomLog.WithValues("participant", rp.Identity(), "participantID", rp.SID())
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
	log := r.roomLog.WithValues("participant", rp.Identity(), "participantID", rp.SID())
	log.Debugw("participant left")
}

func (r *Room) subscribeTo(pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	log := r.roomLog.WithValues("participant", rp.Identity(), "participantID", rp.SID(), "trackID", pub.SID(), "trackName", pub.Name())
	if pub.Kind() != lksdk.TrackKindAudio {
		log.Debugw("skipping non-audio track")
		return
	}
	log.Debugw("subscribing to a track")
	r.stats.TrackSubscribes.Add(1)
	if err := pub.SetSubscribed(true); err != nil {
		log.Errorw("cannot subscribe to the track", err)
		return
	}
	r.subscribed.Break()
}

func (r *Room) Connect(ctx context.Context, conf *config.Config, rconf RoomConfig) error {
	if rconf.WsUrl == "" {
		rconf.WsUrl = conf.WsUrl
	}
	partConf := rconf.Participant
	r.p.Store(&ParticipantInfo{
		RoomName: rconf.RoomName,
		Identity: partConf.Identity,
		Name:     partConf.Name,
	})
	roomCallback := r.newRoomCallback(conf, rconf)

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
	room.SetLogger(newRoomOverrideLogger(r.log))
	err := room.JoinWithContextAndToken(ctx, rconf.WsUrl, rconf.Token,
		lksdk.WithAutoSubscribe(false),
		lksdk.WithExtraAttributes(partConf.Attributes),
	)
	if err != nil {
		return err
	}
	r.room.Store(room)
	r.setParticipantFromRoom()
	p := r.Participant()
	r.log = r.log.WithValues("room", room.Name(), "roomID", room.SID(), "participant", p.Identity, "participantID", p.ID)
	r.log.Infow("SIP participant joined room")
	room.LocalParticipant.SetAttributes(partConf.Attributes)
	r.ready.Break()
	r.subscribe.Store(false) // already false, but keep for visibility

	// Not subscribing to any tracks just yet!
	return nil
}

// setParticipantFromRoom refreshes the cached participant identifiers from the
// SDK room. Runs on recovery too, since a reconnect gets a new SID.
//
// Does not rebuild r.log, which is read without synchronisation elsewhere in
// this file. The reconnect handler logs the SID change instead.
func (r *Room) setParticipantFromRoom() {
	room := r.room.Load()
	if room == nil {
		return
	}
	p := ParticipantInfo{}
	if cur := r.p.Load(); cur != nil {
		p = *cur
	}
	p.ID = room.LocalParticipant.SID()
	p.Identity = room.LocalParticipant.Identity()
	r.p.Store(&p)
}

// newRoomCallback builds the LiveKit room callback for this SIP participant.
// Separate from Connect so tests can build it without joining a room.
func (r *Room) newRoomCallback(conf *config.Config, rconf RoomConfig) *lksdk.RoomCallback {
	return &lksdk.RoomCallback{
		OnParticipantConnected: func(rp *lksdk.RemoteParticipant) {
			log := r.roomLog.WithValues("participant", rp.Identity(), "participantID", rp.SID())
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
				log := r.roomLog.WithValues("participant", rp.Identity(), "participantID", rp.SID(), "trackID", pub.SID(), "trackName", pub.Name())
				if !r.subscribe.Load() {
					log.Debugw("skipping track publish event - subscribed flag not set")
					return // will subscribe later
				}
				r.subscribeTo(pub, rp)
			},
			OnTrackSubscribed: func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				go func() {
					subscribedAt := time.Now().UnixMilli()
					log := r.roomLog.WithValues("participant", rp.Identity(), "participantID", rp.SID(), "trackID", track.ID(), "trackName", pub.Name(), "subscribedAt", subscribedAt)
					if !r.ready.IsBroken() {
						log.Warnw("ignoring track, room not ready", nil)
						return
					}
					defer func() { log.Infow("track closed", "closedAt", time.Now().UnixMilli()) }()

					mTrack := r.NewTrack()
					if mTrack == nil {
						return // closed
					}
					defer mTrack.Close()

					var out msdk.PCM16Writer = mTrack
					// Outbound latency: measure track recv → opus decode → mixer input.
					var outRecvLatencyEntry atomic.Int64
					out = newLatencyPCMExit(out, &outRecvLatencyEntry, &r.stats.LatencyOutRecv)
					if rconf.LogSignalChanges {
						var err error
						out, err = NewSignalLogger(log, track.ID(), out)
						if err != nil {
							log.Errorw("cannot create signal logger", err)
							return
						}
					}

					codec := track.Codec()
					codecName := strings.TrimPrefix(codec.MimeType, "audio/")
					var rh rtp.Handler
					switch strings.ToLower(codecName) {
					case "opus":
						cw, err := opus.Decode(out, channels, log)
						if err != nil {
							log.Errorw("cannot create opus decoder", err)
							return
						}
						defer cw.Close()

						rh = rtp.NewMediaStreamIn(cw)
					case "pcmu":
						cw := g711.DecodeULaw(out)
						rh = rtp.NewMediaStreamIn(cw)
					case "pcma":
						cw := g711.DecodeALaw(out)
						rh = rtp.NewMediaStreamIn(cw)
					default:
						log.Warnw("unsupported sip room codec", nil, "codec", codec.MimeType)
						return
					}
					h := rtp.NewNopCloser(rh)
					if conf.EnableJitterBuffer {
						h = rtp.HandleJitter(h, jitter.WithPacketLossHandler(func(packetsLost, packetsDropped uint64) {
							r.stats.JitterBufferPacketsLost.Store(packetsLost)
							r.stats.JitterBufferPacketsDropped.Store(packetsDropped)
						}))
					}

					h = newRTPStreamStats(h, &r.stats.rtpStats)
					h = newLatencyRTPEntry(h, &outRecvLatencyEntry)
					err := rtp.HandleLoop(track, h)
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
				r.roomLog.Infow("track unsubscribed", "participant", rp.Identity(), "participantID", rp.SID(), "trackID", track.ID(), "trackName", pub.Name())
			},
		},
		OnReconnecting: func() {
			r.onReconnecting()
		},
		OnReconnected: func() {
			r.onReconnected()
		},
		OnDisconnected: func() {
			r.stopped.Break()
		},
		OnDisconnectedWithReason: func(reason lksdk.DisconnectionReason) {
			// OnDisconnected fires first and owns the teardown. This only
			// records the reason, which CloseWithReason may clear later.
			r.roomLog.Infow("disconnected from room", "reason", reason)
		},
	}
}

// reconnectState is captured when the signal connection drops so onReconnected
// can tell a resume from a reconnect and report the gap.
type reconnectState struct {
	startedAt time.Time
	sid       string
}

func recoveryKind(resumed bool) string {
	if resumed {
		return "resume"
	}
	return "reconnect"
}

// onReconnecting runs when the SDK loses the signal connection and starts
// recovering. Audio published until onReconnected may be dropped: a reconnect
// detaches our track from its peer connection while the connection is rebuilt,
// and writes to a detached track are discarded without an error.
func (r *Room) onReconnecting() {
	var sid string
	if room := r.room.Load(); room != nil {
		sid = room.LocalParticipant.SID()
	}
	r.reconnect.Store(&reconnectState{startedAt: time.Now(), sid: sid})
	r.stats.Recovering.Store(true)
	r.roomLog.Infow("lost connection to room, recovering", "participantID", sid)
}

// onReconnected runs when the SDK recovers the signal connection, either by
// resuming the old session or reconnecting from scratch.
//
// A resume keeps the peer connections, and the SDK replays subscription state
// itself, so leave it alone. A reconnect builds a new subscriber peer
// connection with no subscriptions and the SDK restores only what we publish,
// so re-issue the subscriptions here.
func (r *Room) onReconnected() {
	prev := r.reconnect.Swap(nil)
	r.stats.Recovering.Store(false)

	room := r.room.Load()
	if room == nil {
		return
	}

	// The SID is stable across a resume and changes on a reconnect. Treat an
	// unknown previous SID as a reconnect: re-subscribing is idempotent, while
	// missing one leaves the call with no inbound room audio.
	sid := room.LocalParticipant.SID()
	resumed := prev != nil && prev.sid == sid

	var gap time.Duration
	if prev != nil {
		gap = time.Since(prev.startedAt)
	}
	if resumed {
		r.stats.Resumes.Add(1)
	} else {
		r.stats.Reconnects.Add(1)
	}

	r.setParticipantFromRoom()
	r.roomLog.Infow("recovered connection to room",
		"kind", recoveryKind(resumed),
		"gap", gap,
		"participantID", sid,
		"previousParticipantID", func() string {
			if prev == nil {
				return ""
			}
			return prev.sid
		}(),
	)

	if resumed {
		return
	}
	if !r.subscribe.Load() {
		// Call is not answered yet, so subscribing here would pull room audio
		// into a leg that has not been accepted.
		return
	}
	// The SDK calls this from the reconnect itself, inside the join's timeout,
	// and subscribing does a blocking websocket write per track. Pass the room we
	// already read so a concurrent close cannot make this a nil dereference.
	go r.resubscribeAfterReconnect(room)
}

func (r *Room) resubscribeAfterReconnect(room *lksdk.Room) {
	if r.closed.IsBroken() || r.stopped.IsBroken() {
		return
	}
	r.roomLog.Infow("re-subscribing to remote tracks after reconnect")
	r.subscribeAll(room)
}

func (r *Room) RegisterRpcCtxMethod(method string, handler lksdk.RpcHandlerCtxFunc) error {
	room := r.room.Load()
	if room == nil {
		return errRoomClosed
	}
	return room.RegisterRpcCtxMethod(method, handler)
}

func (r *Room) Subscribe() {
	room := r.room.Load()
	if room == nil {
		return
	}
	r.subscribe.Store(true)
	r.subscribeAll(room)
}

// subscribeAll subscribes to every remote audio track in the room. Safe to
// repeat, since a duplicate subscribe is a no-op server side.
func (r *Room) subscribeAll(room *lksdk.Room) {
	list := room.GetRemoteParticipants()
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

func (r *Room) sendDTMF(ctx context.Context, msg *livekit.SipDTMF) {
	// TODO: Separate goroutine?
	r.log.Debugw("forwarding dtmf to sip", "digit", msg.Digit)
	r.outboundDTMF.WriteSample(msg)
}

func (r *Room) Close() error {
	return r.CloseWithReason(livekit.DisconnectReason_UNKNOWN_REASON)
}

func (r *Room) CloseWithReason(reason livekit.DisconnectReason) error {
	if r == nil {
		return nil
	}
	var errs []error
	r.closed.Once(func() {
		defer r.stats.Closed.Store(true)

		r.subscribe.Store(false)
		errs = append(errs, r.outboundAudio.Close())
		errs = append(errs, r.outboundDTMF.Close())
		if room := r.room.Swap(nil); room != nil {
			room.DisconnectWithReason(reason)
		}
		if r.mix != nil {
			r.mix.Stop()
		}
	})
	return errors.Join(errs...)
}

func (r *Room) Participant() ParticipantInfo {
	if r == nil {
		return ParticipantInfo{}
	}
	if p := r.p.Load(); p != nil {
		return *p
	}
	return ParticipantInfo{}
}

// NewParticipantTrack publishes a local Opus audio track into the LiveKit room.
// TODO(alexfish): Remove this from the public interface.
func (r *Room) NewParticipantTrack(sampleRate int) (msdk.WriteCloser[msdk.PCM16Sample], error) {
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion")
	if err != nil {
		return nil, err
	}
	room := r.room.Load()
	if room == nil {
		return nil, errRoomClosed
	}
	p := room.LocalParticipant
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
	room := r.room.Load()
	if room == nil {
		return nil
	}
	return room.LocalParticipant.PublishDataPacket(data, opts...)
}

func (r *Room) NewTrack() *mixer.Input {
	if r == nil {
		return nil
	}
	return r.mix.NewInput()
}

func (r *Room) WriteOutboundAudioTo(w msdk.PCM16Writer) msdk.PCM16Writer {
	return r.outboundAudio.Swap(w)
}

func (r *Room) WriteOutboundDTMFTo(w msdk.WriteCloser[*livekit.SipDTMF]) msdk.WriteCloser[*livekit.SipDTMF] {
	return r.outboundDTMF.Swap(w)
}

func (r *Room) GetInboundAudioWriter() (msdk.PCM16Writer, error) {
	return r.NewParticipantTrack(RoomSampleRate)
}

func (r *Room) GetInboundDTMFWriter() msdk.WriteCloser[*livekit.SipDTMF] {
	return &r.inboundDTMF
}

type inboundDTMFWriter struct {
	r *Room
}

func (w *inboundDTMFWriter) String() string {
	return "inboundDTMFWriter"
}

func (w *inboundDTMFWriter) SampleRate() int {
	return dtmf.SampleRate
}

func (w *inboundDTMFWriter) Close() error {
	return nil
}

func (w *inboundDTMFWriter) WriteSample(sample *livekit.SipDTMF) error {
	if sample == nil {
		return nil
	}
	return w.r.SendData(sample, lksdk.WithDataPublishReliable(true))
}

// roomOverrideLogger converts errors to warnings and ignore debug
type roomOverrideLogger struct {
	logger.Logger
}

func newRoomOverrideLogger(l logger.Logger) *roomOverrideLogger {
	if l == nil {
		l = logger.GetLogger()
	}

	return &roomOverrideLogger{
		Logger: l.WithCallDepth(1),
	}
}

func (l *roomOverrideLogger) Debugw(msg string, keysAndValues ...interface{}) {
	// ignore
}

func (l *roomOverrideLogger) Infow(msg string, keysAndValues ...interface{}) {
	l.Logger.Infow(msg, keysAndValues...)
}

func (l *roomOverrideLogger) Warnw(msg string, err error, keysAndValues ...interface{}) {
	l.Logger.Warnw(msg, err, keysAndValues...)
}

func (l *roomOverrideLogger) Errorw(msg string, err error, keysAndValues ...interface{}) {
	l.Logger.Warnw(msg, err, keysAndValues...)
}
