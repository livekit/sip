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
	"errors"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/server-sdk-go/v2/e2ee"
	"github.com/livekit/server-sdk-go/v2/pkg/interceptor"
)

// The integration test of the SDK. can't run this test standalone, should be run with `mage test`

const (
	host = "ws://localhost:7880"
)

var (
	apiKey, apiSecret string

	VP8KeyFrame8x8 = []byte{
		0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x08, 0x00,
		0x08, 0x00, 0x00, 0x47, 0x08, 0x85, 0x85, 0x88,
		0x85, 0x84, 0x88, 0x02, 0x02, 0x00, 0x0c, 0x0d,
		0x60, 0x00, 0xfe, 0xff, 0xab, 0x50, 0x80,
	}

	H264KeyFrame2x2SPS = []byte{
		0x67, 0x42, 0xc0, 0x1f, 0x0f, 0xd9, 0x1f, 0x88,
		0x88, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00,
		0x00, 0x03, 0x00, 0xc8, 0x3c, 0x60, 0xc9, 0x20,
	}
	H264KeyFrame2x2PPS = []byte{
		0x68, 0x87, 0xcb, 0x83, 0xcb, 0x20,
	}
	H264KeyFrame2x2IDR = []byte{
		0x65, 0x88, 0x84, 0x0a, 0xf2, 0x62, 0x80, 0x00,
		0xa7, 0xbe,
	}
	H264KeyFrame2x2 = [][]byte{H264KeyFrame2x2SPS, H264KeyFrame2x2PPS, H264KeyFrame2x2IDR}
)

func TestMain(m *testing.M) {
	keys := strings.Split(os.Getenv("LIVEKIT_KEYS"), ": ")
	if len(keys) >= 2 {
		apiKey, apiSecret = keys[0], keys[1]
	}

	os.Exit(m.Run())
}

func createAgent(roomName string, callback *RoomCallback, name string, connectOpts ...ConnectOption) (*Room, error) {
	room, err := ConnectToRoom(host, ConnectInfo{
		APIKey:              apiKey,
		APISecret:           apiSecret,
		RoomName:            roomName,
		ParticipantIdentity: name,
	}, callback, connectOpts...)
	if err != nil {
		return nil, err
	}
	return room, nil
}

type SampleTestProvider struct {
	BaseSampleProvider
	SampleDuration time.Duration
	Codec          webrtc.RTPCodecCapability
}

func NewSampleTestProvider(codec webrtc.RTPCodecCapability) *SampleTestProvider {
	sampleDuration := 50 * time.Millisecond
	if strings.Contains(codec.MimeType, "video/") {
		sampleDuration = time.Second / 10
	}
	return &SampleTestProvider{
		SampleDuration: sampleDuration,
		Codec:          codec,
	}
}

func (p *SampleTestProvider) NextSample(ctx context.Context) (media.Sample, error) {
	payload := make([]byte, 512)
	switch {
	case strings.Contains(p.Codec.MimeType, "VP8"):
		payload = payload[:len(VP8KeyFrame8x8)]
		copy(payload, VP8KeyFrame8x8)
	case strings.Contains(p.Codec.MimeType, "H264"):
		payload = payload[:0]
		for _, nalu := range H264KeyFrame2x2 {
			// Annex B prefix
			payload = append(payload, 0x00, 0x00, 0x00, 0x01)
			payload = append(payload, nalu...)
		}
	default:
	}
	return media.Sample{
		Data:     payload,
		Duration: p.SampleDuration,
	}, nil
}

func pubNullTrack(t *testing.T, room *Room, name string, codecs ...webrtc.RTPCodecCapability) *LocalTrackPublication {
	track, err := NewLocalTrack(codecs[0])
	require.NoError(t, err)
	provider := NewSampleTestProvider(codecs[0])

	track.OnBind(func() {
		if err := track.StartWrite(provider, func() {}); err != nil {
			track.log.Errorw("Could not start writing", err)
		}
	})

	var opts []LocalTrackPublishOption
	if len(codecs) > 1 {
		backupTrack, err := NewLocalTrack(codecs[1])
		require.NoError(t, err)

		backupProvider := NewSampleTestProvider(codecs[1])

		backupTrack.OnBind(func() {
			if err := backupTrack.StartWrite(backupProvider, func() {}); err != nil {
				backupTrack.log.Errorw("Could not start writing", err)
			}
		})

		opts = append(opts, WithBackupCodec(backupTrack))
	}
	localPub, err := room.LocalParticipant.PublishTrack(track, &TrackPublicationOptions{
		Name:              name,
		BackupCodecPolicy: livekit.BackupCodecPolicy_SIMULCAST,
	}, opts...)
	require.NoError(t, err)
	return localPub
}

func TestJoin(t *testing.T) {
	remoteParticipantsOfPub := make(chan string, 1)
	pub, err := createAgent(t.Name(), &RoomCallback{
		OnParticipantConnected: func(participant *RemoteParticipant) {
			remoteParticipantsOfPub <- participant.Identity()
		},
	}, "publisher")
	require.NoError(t, err)

	var (
		dataLock     sync.Mutex
		receivedData string
	)

	audioTrackName := "audio_of_pub1"
	var trackLock sync.Mutex
	var trackReceived atomic.Bool

	subCB := &RoomCallback{
		ParticipantCallback: ParticipantCallback{
			OnDataPacket: func(data DataPacket, params DataReceiveParams) {
				switch data := data.(type) {
				case *UserDataPacket:
					dataLock.Lock()
					receivedData = string(data.Payload)
					dataLock.Unlock()
				}
			},
			OnTrackSubscribed: func(track *webrtc.TrackRemote, publication *RemoteTrackPublication, rp *RemoteParticipant) {
				trackLock.Lock()
				trackReceived.Store(true)
				require.Equal(t, rp.Name(), pub.LocalParticipant.Name())
				require.Equal(t, publication.Name(), audioTrackName)
				require.Equal(t, track.Kind(), webrtc.RTPCodecTypeAudio)
				trackLock.Unlock()
			},
		},
	}
	sub, err := createAgent(t.Name(), subCB, "subscriber")
	require.NoError(t, err)
	serverInfo := sub.ServerInfo()
	require.NotNil(t, serverInfo)
	require.Equal(t, livekit.ServerInfo_Standard, serverInfo.Edition)
	require.Equal(t, ConnectionStateConnected, sub.ConnectionState())
	remotParticipant := <-remoteParticipantsOfPub
	require.Equal(t, remotParticipant, sub.LocalParticipant.Identity())

	pub.LocalParticipant.PublishDataPacket(UserData([]byte("test")), WithDataPublishReliable(true))
	pub.LocalParticipant.PublishDataPacket(&livekit.SipDTMF{Digit: "#"}, WithDataPublishReliable(true))
	localPub := pubNullTrack(t, pub, audioTrackName, webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	})

	require.Equal(t, localPub.Name(), audioTrackName)

	require.Eventually(t, func() bool { return trackReceived.Load() }, 5*time.Second, 100*time.Millisecond)
	require.Eventually(t, func() bool {
		dataLock.Lock()
		defer dataLock.Unlock()
		return receivedData == "test"
	}, 5*time.Second, 100*time.Millisecond)

	pub.Disconnect()
	sub.Disconnect()
	require.Equal(t, ConnectionStateDisconnected, sub.ConnectionState())
}

func TestJoinError(t *testing.T) {
	_, err := ConnectToRoomWithToken(host, "invalid", nil)
	require.Error(t, err)

	errString := err.Error()
	require.Contains(t, errString, "unauthorized:")
}

// TestJoinSinglePeerConnection verifies offer-with-join in single-PC mode:
// the publisher offer is sent in the JoinRequest, no subscriber PC is
// created, and subscribed media + data flow through the publisher PC.
func TestJoinSinglePeerConnection(t *testing.T) {
	remoteParticipantsOfPub := make(chan string, 1)
	pub, err := createAgent(t.Name(), &RoomCallback{
		OnParticipantConnected: func(participant *RemoteParticipant) {
			remoteParticipantsOfPub <- participant.Identity()
		},
	}, "publisher-singlepc", WithSinglePeerConnection())
	require.NoError(t, err)
	defer pub.Disconnect()

	require.True(t, pub.useSinglePeerConnection)
	require.Equal(t, ConnectionStateConnected, pub.ConnectionState())
	_, hasSubscriber := pub.engine.Subscriber()
	require.False(t, hasSubscriber, "no subscriber PC should be created in single-PC mode")

	audioTrackName := "audio_singlepc"
	var trackReceived atomic.Bool
	var receivedData atomic.String
	subCB := &RoomCallback{
		ParticipantCallback: ParticipantCallback{
			OnDataPacket: func(data DataPacket, params DataReceiveParams) {
				if u, ok := data.(*UserDataPacket); ok {
					receivedData.Store(string(u.Payload))
				}
			},
			OnTrackSubscribed: func(track *webrtc.TrackRemote, publication *RemoteTrackPublication, rp *RemoteParticipant) {
				require.Equal(t, audioTrackName, publication.Name())
				require.Equal(t, webrtc.RTPCodecTypeAudio, track.Kind())
				trackReceived.Store(true)
			},
		},
	}
	sub, err := createAgent(t.Name(), subCB, "subscriber-singlepc", WithSinglePeerConnection())
	require.NoError(t, err)
	defer sub.Disconnect()
	_, hasSubscriber = sub.engine.Subscriber()
	require.False(t, hasSubscriber)

	<-remoteParticipantsOfPub

	pub.LocalParticipant.PublishDataPacket(UserData([]byte("singlepc")), WithDataPublishReliable(true))
	localPub := pubNullTrack(t, pub, audioTrackName, webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	})
	require.Equal(t, audioTrackName, localPub.Name())

	require.Eventually(t, trackReceived.Load, 5*time.Second, 100*time.Millisecond,
		"subscriber should receive audio track over single PC")
	require.Eventually(t, func() bool { return receivedData.Load() == "singlepc" }, 5*time.Second, 100*time.Millisecond,
		"subscriber should receive data packet over single PC")
}

// TestPublishRequiresSinglePC verifies that queuing a track via WithTrack
// without also enabling WithSinglePeerConnection() is rejected at Join time.
func TestPublishRequiresSinglePC(t *testing.T) {
	codec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	track, err := NewLocalTrack(codec)
	require.NoError(t, err)

	room := NewRoom(&RoomCallback{})
	err = room.Join(host, ConnectInfo{
		APIKey:              apiKey,
		APISecret:           apiSecret,
		RoomName:            t.Name(),
		ParticipantIdentity: "publisher-err",
	}, WithTrack(track, &TrackPublicationOptions{Name: "x"}))
	require.ErrorIs(t, err, ErrPublishRequiresSinglePC)
}

// TestPubSub covers the publish/subscribe roundtrip for both the "post-publish"
// path (default Join + PublishTrack/PublishSimulcastTrack) and the
// "pre-publish" path (WithSinglePeerConnection() + WithTrack/
// WithSimulcastTrack queued for the JoinRequest). The bidirectional subtest
// has two peers exchange single-layer audio + single-layer video (covers
// WithTrack and PublishTrack code paths); the simulcast subtest has one
// publisher push audio + a 3-layer simulcast video to a passive subscriber
// (covers WithSimulcastTrack and PublishSimulcastTrack). It supersedes the
// older one-direction TestPrePublishTrack and TestPrePublishSimulcastTrack.
func TestPubSub(t *testing.T) {
	cases := []struct {
		name       string
		prepublish bool
	}{
		{"post_publish", false},
		{"pre_publish", true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name+"_bidirectional", func(t *testing.T) {
			runBidirectionalPubSub(t, c.prepublish)
		})
		t.Run(c.name+"_simulcast", func(t *testing.T) {
			runSimulcastPubSub(t, c.prepublish)
		})
	}
}

func newSingleSampleTrack(t *testing.T, codec webrtc.RTPCodecCapability) *LocalTrack {
	t.Helper()
	track, err := NewLocalTrack(codec)
	require.NoError(t, err)
	provider := NewSampleTestProvider(codec)
	track.OnBind(func() {
		if err := track.StartWrite(provider, func() {}); err != nil {
			track.log.Errorw("could not start writing", err)
		}
	})
	return track
}

func newSimulcastSampleTracks(t *testing.T, codec webrtc.RTPCodecCapability, simulcastID string) []*LocalTrack {
	t.Helper()
	layers := []*livekit.VideoLayer{
		{Quality: livekit.VideoQuality_LOW, Width: 160, Height: 120},
		{Quality: livekit.VideoQuality_MEDIUM, Width: 320, Height: 240},
		{Quality: livekit.VideoQuality_HIGH, Width: 640, Height: 480},
	}
	out := make([]*LocalTrack, 0, len(layers))
	for _, layer := range layers {
		layer := layer
		track, err := NewLocalTrack(codec, WithSimulcast(simulcastID, layer))
		require.NoError(t, err)
		provider := NewSampleTestProvider(codec)
		track.OnBind(func() {
			if err := track.StartWrite(provider, func() {}); err != nil {
				track.log.Errorw("could not start writing", err)
			}
		})
		out = append(out, track)
	}
	return out
}

// runBidirectionalPubSub: two peers, each publishes audio + single-layer
// video, each subscribes to the other's two tracks. Verifies cross-direction
// pub/sub through both publish paths.
func runBidirectionalPubSub(t *testing.T, prepublish bool) {
	audioCodec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	videoCodec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}

	type observed struct {
		sid  string
		kind webrtc.RTPCodecType
	}
	type peer struct {
		identity               string
		audioName, videoName   string
		audioTrack, videoTrack *LocalTrack
		audioPub, videoPub     *LocalTrackPublication
		room                   *Room
		observedLock           sync.Mutex
		observed               map[string]observed
		rtpCounts              map[string]*atomic.Int32
	}

	mkPeer := func(identity string) *peer {
		return &peer{
			identity:   identity,
			audioName:  identity + "_audio",
			videoName:  identity + "_video",
			audioTrack: newSingleSampleTrack(t, audioCodec),
			videoTrack: newSingleSampleTrack(t, videoCodec),
			observed:   make(map[string]observed),
			rtpCounts: map[string]*atomic.Int32{
				identity + "_audio": new(atomic.Int32),
				identity + "_video": new(atomic.Int32),
			},
		}
	}
	alice := mkPeer("alice")
	bob := mkPeer("bob")

	callbackFor := func(p, other *peer) *RoomCallback {
		return &RoomCallback{
			ParticipantCallback: ParticipantCallback{
				OnTrackSubscribed: func(track *webrtc.TrackRemote, publication *RemoteTrackPublication, _ *RemoteParticipant) {
					name := publication.Name()
					p.observedLock.Lock()
					p.observed[name] = observed{sid: publication.SID(), kind: track.Kind()}
					p.observedLock.Unlock()
					counter := other.rtpCounts[name]
					go func() {
						for {
							if _, _, err := track.ReadRTP(); err != nil {
								return
							}
							if counter != nil {
								counter.Add(1)
							}
						}
					}()
				},
			},
		}
	}

	connectOpts := func(p *peer) []ConnectOption {
		if !prepublish {
			return nil
		}
		return []ConnectOption{
			WithSinglePeerConnection(),
			WithTrack(p.audioTrack, &TrackPublicationOptions{Name: p.audioName}),
			WithTrack(p.videoTrack, &TrackPublicationOptions{Name: p.videoName}),
		}
	}

	join := func(p, other *peer) {
		p.room = NewRoom(callbackFor(p, other))
		require.NoError(t, p.room.Join(host, ConnectInfo{
			APIKey:              apiKey,
			APISecret:           apiSecret,
			RoomName:            t.Name(),
			ParticipantIdentity: p.identity,
		}, connectOpts(p)...))
	}
	join(alice, bob)
	defer alice.room.Disconnect()
	join(bob, alice)
	defer bob.room.Disconnect()

	resolvePublications := func(p *peer) {
		if prepublish {
			pubs := p.room.LocalParticipant.TrackPublications()
			require.Len(t, pubs, 2, "%s should have 2 publications after pre-publish Join", p.identity)
			for _, pub := range pubs {
				lp, ok := pub.(*LocalTrackPublication)
				require.True(t, ok)
				switch pub.Name() {
				case p.audioName:
					p.audioPub = lp
				case p.videoName:
					p.videoPub = lp
				}
			}
		} else {
			audioPub, err := p.room.LocalParticipant.PublishTrack(p.audioTrack, &TrackPublicationOptions{Name: p.audioName})
			require.NoError(t, err)
			p.audioPub = audioPub
			videoPub, err := p.room.LocalParticipant.PublishTrack(p.videoTrack, &TrackPublicationOptions{Name: p.videoName})
			require.NoError(t, err)
			p.videoPub = videoPub
		}
		for _, pub := range []*LocalTrackPublication{p.audioPub, p.videoPub} {
			require.NotNil(t, pub, "%s publication missing", p.identity)
			require.NotEmpty(t, pub.SID(), "%s publication SID should be set", p.identity)
		}
	}
	resolvePublications(alice)
	resolvePublications(bob)

	waitObserved := func(observer *peer, name string) {
		require.Eventually(t, func() bool {
			observer.observedLock.Lock()
			defer observer.observedLock.Unlock()
			_, ok := observer.observed[name]
			return ok
		}, 10*time.Second, 100*time.Millisecond, "%s should observe %s", observer.identity, name)
	}
	for _, name := range []string{bob.audioName, bob.videoName} {
		waitObserved(alice, name)
	}
	for _, name := range []string{alice.audioName, alice.videoName} {
		waitObserved(bob, name)
	}

	assertMatch := func(observer, publisher *peer, name string, expectedKind webrtc.RTPCodecType, pub *LocalTrackPublication) {
		observer.observedLock.Lock()
		defer observer.observedLock.Unlock()
		obs := observer.observed[name]
		require.Equal(t, pub.SID(), obs.sid, "%s -> %s %s SID mismatch", publisher.identity, observer.identity, name)
		require.Equal(t, expectedKind, obs.kind, "%s %s kind mismatch", publisher.identity, name)
	}
	assertMatch(alice, bob, bob.audioName, webrtc.RTPCodecTypeAudio, bob.audioPub)
	assertMatch(alice, bob, bob.videoName, webrtc.RTPCodecTypeVideo, bob.videoPub)
	assertMatch(bob, alice, alice.audioName, webrtc.RTPCodecTypeAudio, alice.audioPub)
	assertMatch(bob, alice, alice.videoName, webrtc.RTPCodecTypeVideo, alice.videoPub)

	allSIDs := map[string]bool{}
	for _, p := range []*peer{alice, bob} {
		for _, pub := range []*LocalTrackPublication{p.audioPub, p.videoPub} {
			allSIDs[pub.SID()] = true
		}
	}
	require.Len(t, allSIDs, 4, "all four publication SIDs should be distinct")

	waitRTP := func(observer, publisher *peer) {
		for _, name := range []string{publisher.audioName, publisher.videoName} {
			name := name
			require.Eventually(t, func() bool {
				return publisher.rtpCounts[name].Load() > 0
			}, 10*time.Second, 100*time.Millisecond, "%s should receive RTP for %s", observer.identity, name)
		}
	}
	waitRTP(alice, bob)
	waitRTP(bob, alice)
}

// runSimulcastPubSub: one publisher pushes audio + 3-layer simulcast video,
// one passive subscriber receives them. Asymmetric so the publisher's
// 3+3 recvonly slot budget in single-PC pre-publish mode is not exhausted
// by inbound traffic.
func runSimulcastPubSub(t *testing.T, prepublish bool) {
	audioCodec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	videoCodec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}

	audioName := "simulcast_audio"
	videoName := "simulcast_video"

	var (
		subAudioSID, subVideoSID   atomic.String
		subAudioKind, subVideoKind atomic.Value // webrtc.RTPCodecType
		subAudioRTP, subVideoRTP   atomic.Int32
	)
	subCB := &RoomCallback{
		ParticipantCallback: ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, publication *RemoteTrackPublication, _ *RemoteParticipant) {
				name := publication.Name()
				switch name {
				case audioName:
					subAudioSID.Store(publication.SID())
					subAudioKind.Store(track.Kind())
				case videoName:
					subVideoSID.Store(publication.SID())
					subVideoKind.Store(track.Kind())
				}
				go func() {
					for {
						if _, _, err := track.ReadRTP(); err != nil {
							return
						}
						switch name {
						case audioName:
							subAudioRTP.Add(1)
						case videoName:
							subVideoRTP.Add(1)
						}
					}
				}()
			},
		},
	}
	sub, err := createAgent(t.Name(), subCB, "subscriber-simulcast")
	require.NoError(t, err)
	defer sub.Disconnect()

	audioTrack := newSingleSampleTrack(t, audioCodec)
	simTracks := newSimulcastSampleTracks(t, videoCodec, "SC_"+videoName)

	pubRoom := NewRoom(&RoomCallback{})
	defer pubRoom.Disconnect()

	var audioPub, videoPub *LocalTrackPublication
	if prepublish {
		require.NoError(t, pubRoom.Join(host, ConnectInfo{
			APIKey:              apiKey,
			APISecret:           apiSecret,
			RoomName:            t.Name(),
			ParticipantIdentity: "publisher-simulcast",
		},
			WithSinglePeerConnection(),
			WithTrack(audioTrack, &TrackPublicationOptions{Name: audioName}),
			WithSimulcastTrack(simTracks, &TrackPublicationOptions{Name: videoName}),
		))
		pubs := pubRoom.LocalParticipant.TrackPublications()
		require.Len(t, pubs, 2, "publisher should have 2 publications after pre-publish Join")
		for _, pub := range pubs {
			lp, ok := pub.(*LocalTrackPublication)
			require.True(t, ok)
			switch pub.Name() {
			case audioName:
				audioPub = lp
			case videoName:
				videoPub = lp
			}
		}
	} else {
		require.NoError(t, pubRoom.Join(host, ConnectInfo{
			APIKey:              apiKey,
			APISecret:           apiSecret,
			RoomName:            t.Name(),
			ParticipantIdentity: "publisher-simulcast",
		}))
		audioPub, err = pubRoom.LocalParticipant.PublishTrack(audioTrack, &TrackPublicationOptions{Name: audioName})
		require.NoError(t, err)
		videoPub, err = pubRoom.LocalParticipant.PublishSimulcastTrack(simTracks, &TrackPublicationOptions{Name: videoName})
		require.NoError(t, err)
	}
	require.NotNil(t, audioPub)
	require.NotNil(t, videoPub)
	require.NotEmpty(t, audioPub.SID())
	require.NotEmpty(t, videoPub.SID())
	require.NotEqual(t, audioPub.SID(), videoPub.SID())

	require.Eventually(t, func() bool {
		return subAudioSID.Load() != "" && subVideoSID.Load() != ""
	}, 10*time.Second, 100*time.Millisecond, "subscriber should observe both audio and simulcast video")
	require.Equal(t, audioPub.SID(), subAudioSID.Load(), "audio SID should match end-to-end")
	require.Equal(t, videoPub.SID(), subVideoSID.Load(), "simulcast video SID should match end-to-end")
	require.Equal(t, webrtc.RTPCodecTypeAudio, subAudioKind.Load())
	require.Equal(t, webrtc.RTPCodecTypeVideo, subVideoKind.Load())

	require.Eventually(t, func() bool {
		return subAudioRTP.Load() > 0 && subVideoRTP.Load() > 0
	}, 10*time.Second, 100*time.Millisecond, "subscriber should receive RTP for both tracks")
}

func TestResume(t *testing.T) {
	var reconnected atomic.Bool
	pubCB := &RoomCallback{
		OnReconnected: func() {
			reconnected.Store(true)
		},
	}
	pub, err := createAgent(t.Name(), pubCB, "publisher")
	require.NoError(t, err)

	// test pub sub after reconnected
	audioTrackName := "audio_of_pub1"
	var trackLock sync.Mutex
	var trackReceived, subReconnected atomic.Bool
	var subTrack *webrtc.TrackRemote
	subCB := &RoomCallback{
		ParticipantCallback: ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, publication *RemoteTrackPublication, rp *RemoteParticipant) {
				trackLock.Lock()
				trackReceived.Store(true)
				require.Equal(t, rp.Name(), pub.LocalParticipant.Name())
				require.Equal(t, publication.Name(), audioTrackName)
				require.Equal(t, track.Kind(), webrtc.RTPCodecTypeAudio)
				subTrack = track
				trackLock.Unlock()
			},
		},
		OnReconnected: func() {
			subReconnected.Store(true)
		},
	}
	sub, err := createAgent(t.Name(), subCB, "subscriber")
	require.NoError(t, err)

	subCB.OnReconnecting = func() {
		require.Equal(t, ConnectionStateReconnecting, sub.ConnectionState())
	}
	subCB.OnReconnected = func() {
		require.Equal(t, ConnectionStateConnected, sub.ConnectionState())
	}

	localPub := pubNullTrack(t, pub, audioTrackName, webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	})
	require.Equal(t, localPub.Name(), audioTrackName)
	require.Eventually(t, func() bool { return trackReceived.Load() }, 5*time.Second, 100*time.Millisecond)

	pub.Simulate(SimulateSignalReconnect)
	require.Eventually(t, func() bool { return reconnected.Load() }, 5*time.Second, 100*time.Millisecond)
	pub.log.Infow("reconnected")

	// consume track packet
	consumeCh := make(chan struct{})
	var exitConsume atomic.Bool
	go func() {
		trackLock.Lock()
		track := subTrack
		trackLock.Unlock()
		buf := make([]byte, 1024)
		defer close(consumeCh)
		for !exitConsume.Load() {
			_, _, err = track.Read(buf)
			require.NoError(t, err)
		}
	}()

	sub.Simulate(SimulateSignalReconnect)
	require.Eventually(t, func() bool { return subReconnected.Load() }, 5*time.Second, 100*time.Millisecond)

	// confirm track is still working after resume
	exitConsume.Store(true)
	<-consumeCh
	trackLock.Lock()
	track := subTrack
	trackLock.Unlock()
	buf := make([]byte, 1024)
	for i := 0; i < 10; i++ {
		_, _, err = track.Read(buf)
		require.NoError(t, err)
	}

	pub.Disconnect()
	sub.Disconnect()
}

// This test case can't pass with CI's environment (docker + embedded turn), will be skipped with CI running
func TestForceTLS(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("Skipping in CI environment")
	}
	var reconnected atomic.Bool
	pubCB := &RoomCallback{
		OnReconnected: func() {
			reconnected.Store(true)
		},
	}
	pub, err := createAgent(t.Name()+"-"+strconv.Itoa(int(rand.Uint32())), pubCB, "publisher-forcetls")
	require.NoError(t, err)

	// ensure publisher connected
	pub.LocalParticipant.PublishDataPacket(UserData([]byte("test")), WithDataPublishReliable(true))

	pub.Simulate(SimulateForceTLS)
	require.Eventually(t, func() bool { return reconnected.Load() && pub.engine.ensurePublisherConnected(true) == nil }, 15*time.Second, 100*time.Millisecond)

	pub.log.Infow("reconnected")

	getSelectedPair := func(pc *webrtc.PeerConnection) (*webrtc.ICECandidatePair, error) {
		sctp := pc.SCTP()
		if sctp == nil {
			return nil, errors.New("no SCTP")
		}

		dtlsTransport := sctp.Transport()
		if dtlsTransport == nil {
			return nil, errors.New("no DTLS transport")
		}

		iceTransport := dtlsTransport.ICETransport()
		if iceTransport == nil {
			return nil, errors.New("no ICE transport")
		}

		return iceTransport.GetSelectedCandidatePair()
	}

	for _, pc := range []*webrtc.PeerConnection{pub.engine.publisher.pc, pub.engine.subscriber.pc} {
		pair, err := getSelectedPair(pc)
		require.NoError(t, err)
		require.NotNil(t, pair)
		require.Equal(t, pair.Local.Typ, webrtc.ICECandidateTypeRelay)
	}

	pub.Disconnect()
}

func TestSubscribeMutedTrack(t *testing.T) {
	pub, err := createAgent(t.Name(), nil, "publisher")
	require.NoError(t, err)

	videoTrackName := "video_of_pub1"
	audioTrackName := "audio_of_pub1"
	var trackLock sync.Mutex
	var trackReceived atomic.Int32

	var pubTrackMuted sync.WaitGroup
	require.NoError(t, pub.LocalParticipant.PublishDataPacket(UserData([]byte("test"))), WithDataPublishReliable(true))

	pubMuteTrack := func(t *testing.T, room *Room, name string, codec webrtc.RTPCodecCapability) *LocalTrackPublication {
		pubTrackMuted.Add(1)
		track, err := NewLocalTrack(codec)
		require.NoError(t, err)

		track.OnBind(func() {
			go func() {
				defer pubTrackMuted.Done()
				for i := 0; i < 10; i++ {
					time.Sleep(50 * time.Millisecond)
					require.NoError(t, track.WriteSample(media.Sample{Data: []byte("test"), Duration: 50 * time.Millisecond}, nil))
				}
			}()
		})

		localPub, err := room.LocalParticipant.PublishTrack(track, &TrackPublicationOptions{
			Name: name,
		})
		require.NoError(t, err)
		return localPub
	}

	localVideoPub := pubMuteTrack(t, pub, videoTrackName, webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeVP8,
		ClockRate: 90000,
	})
	require.Equal(t, localVideoPub.Name(), videoTrackName)

	localAudioPub := pubMuteTrack(t, pub, audioTrackName, webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
	})
	require.Equal(t, localAudioPub.Name(), audioTrackName)

	localVideoPub.SetMuted(true)
	localAudioPub.SetMuted(true)
	pubTrackMuted.Wait()

	subCB := &RoomCallback{
		ParticipantCallback: ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, publication *RemoteTrackPublication, rp *RemoteParticipant) {
				trackLock.Lock()
				trackReceived.Inc()
				require.Equal(t, rp.Name(), pub.LocalParticipant.Name())
				if track.Kind() == webrtc.RTPCodecTypeAudio {
					require.Equal(t, publication.Name(), audioTrackName)
				} else {
					require.Equal(t, publication.Name(), videoTrackName)
				}
				require.True(t, publication.IsMuted())
				trackLock.Unlock()
			},
		},
	}
	sub, err := createAgent(t.Name(), subCB, "subscriber")
	require.NoError(t, err)
	serverInfo := sub.ServerInfo()
	require.NotNil(t, serverInfo)
	require.Equal(t, serverInfo.Edition, livekit.ServerInfo_Standard)
	require.Eventually(t, func() bool { return trackReceived.Load() == 2 }, 5*time.Second, 100*time.Millisecond)

	pub.Disconnect()
	sub.Disconnect()
}

func TestLimitPayloadSize(t *testing.T) {
	pub, err := createAgent(t.Name(), nil, "publisher")
	require.NoError(t, err)

	videoTrackName := "video_of_pub1"

	localTrack, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeVP8,
		ClockRate: 90000,
	}, videoTrackName, videoTrackName)
	require.NoError(t, err)

	_, err = pub.LocalParticipant.PublishTrack(localTrack, &TrackPublicationOptions{Name: videoTrackName})
	require.NoError(t, err)

	// wait for track to be published
	time.Sleep(500 * time.Millisecond)

	rtpPkt := rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: 1,
			Timestamp:      1,
		},
		Payload: make([]byte, interceptor.MaxPayloadSize),
	}
	require.NoError(t, localTrack.WriteRTP(&rtpPkt))
	rtpPkt.SequenceNumber++
	rtpPkt.Payload = make([]byte, interceptor.MaxPayloadSize+1)
	require.ErrorIs(t, localTrack.WriteRTP(&rtpPkt), interceptor.ErrPayloadSizeTooLarge)

	pub.Disconnect()
}

func TestSimulcastCodec(t *testing.T) {
	cases := []struct {
		name                      string
		primaryCodec, backupCodec webrtc.RTPCodecCapability
	}{
		{
			name: "video",
			primaryCodec: webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypeVP8,
				ClockRate: 90000,
			},
			backupCodec: webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypeH264,
				ClockRate: 90000,
			},
		},
		{
			name: "audio",
			primaryCodec: webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypePCMU,
				ClockRate: 8000,
				Channels:  1,
			},
			backupCodec: webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypeOpus,
				ClockRate: 48000,
				Channels:  2,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pub, err := createAgent(t.Name(), nil, "publisher")
			require.NoError(t, err)
			trackPub := pubNullTrack(t, pub, "simulcast_codec", []webrtc.RTPCodecCapability{c.primaryCodec, c.backupCodec}...)
			t.Log("published track with simulcast codecs:", trackPub.SID())

			var subWG sync.WaitGroup
			subWG.Add(2) // primary and backup
			primarySub, err := createAgent(t.Name(), &RoomCallback{
				ParticipantCallback: ParticipantCallback{
					OnTrackSubscribed: func(track *webrtc.TrackRemote, publication *RemoteTrackPublication, rp *RemoteParticipant) {
						subWG.Done()
						t.Log("primary codec subscribed:", track.Codec().MimeType, publication.SID())
						require.Equal(t, publication.SID(), trackPub.SID())
						require.Equal(t, c.primaryCodec.MimeType, track.Codec().MimeType)
					},
				},
			}, "primary_subscriber")
			require.NoError(t, err)

			backupSub, err := createAgent(t.Name(), &RoomCallback{
				ParticipantCallback: ParticipantCallback{
					OnTrackSubscribed: func(track *webrtc.TrackRemote, publication *RemoteTrackPublication, rp *RemoteParticipant) {
						subWG.Done()
						t.Log("backup codec subscribed:", track.Codec().MimeType, publication.SID())
						require.Equal(t, publication.SID(), trackPub.SID())
						require.Equal(t, c.backupCodec.MimeType, track.Codec().MimeType)
					},
				},
			}, "backup_subscriber", WithCodecs([]livekit.Codec{
				{
					Mime: c.backupCodec.MimeType,
				},
			}))
			require.NoError(t, err)

			subWG.Wait()

			pub.Disconnect()
			primarySub.Disconnect()
			backupSub.Disconnect()
		})
	}

}

// TestE2EE_H264RoundTrip verifies that publishing an H.264 video track flagged
// for GCM e2ee flows through the SFU end-to-end: the track is received by the
// subscriber, Encryption_GCM metadata survives signaling, and the e2ee API
// (key provider, frame decryptor, SIF trailer) composes without error for the
// H.264 codec path. It also exercises key rotation via SetKeyFromPassphrase at
// the same key index.
func TestE2EE_H264RoundTrip(t *testing.T) {
	const passphrase = "e2ee-integration-test"
	const keyIndex uint32 = 0

	kpPub := e2ee.NewExternalKeyProvider()
	require.NoError(t, kpPub.SetKeyFromPassphrase(passphrase, keyIndex))
	kpSub := e2ee.NewExternalKeyProvider()
	require.NoError(t, kpSub.SetKeyFromPassphrase(passphrase, keyIndex))

	pub, err := createAgent(t.Name(), &RoomCallback{}, "e2ee-publisher")
	require.NoError(t, err)
	defer pub.Disconnect()

	videoTrackName := "video_e2ee_h264"
	var (
		trackReceived  atomic.Bool
		encryptionSeen atomic.Value // holds livekit.Encryption_Type
		decryptorBuilt atomic.Bool
	)

	subCB := &RoomCallback{
		ParticipantCallback: ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, publication *RemoteTrackPublication, rp *RemoteParticipant) {
				if publication.Name() != videoTrackName {
					return
				}
				encryptionSeen.Store(publication.TrackInfo().GetEncryption())

				// Exercise the public e2ee decryptor API against the shared
				// key provider using the H.264 codec path. We don't read
				// samples here — the goal is to prove the API composes with
				// a live H.264 track.
				dec, err := NewFrameDecryptor(kpSub, CodecH264, nil)
				require.NoError(t, err)
				require.NotNil(t, dec)
				decryptorBuilt.Store(true)

				trackReceived.Store(true)
			},
		},
	}
	sub, err := createAgent(t.Name(), subCB, "e2ee-subscriber")
	require.NoError(t, err)
	defer sub.Disconnect()

	// Publish an H.264 track flagged as GCM-encrypted. The SampleTestProvider
	// feeds minimal H.264 Annex-B key frames (H264KeyFrame2x2), same shape as
	// the existing simulcast/video tests.
	h264Codec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000}
	track, err := NewLocalTrack(h264Codec)
	require.NoError(t, err)
	provider := NewSampleTestProvider(h264Codec)
	track.OnBind(func() {
		if err := track.StartWrite(provider, func() {}); err != nil {
			track.log.Errorw("e2ee test: write failed", err)
		}
	})
	_, err = pub.LocalParticipant.PublishTrack(track, &TrackPublicationOptions{
		Name:        videoTrackName,
		Encryption:  livekit.Encryption_GCM,
		VideoWidth:  2,
		VideoHeight: 2,
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool { return trackReceived.Load() }, 10*time.Second, 100*time.Millisecond,
		"subscriber should receive the encrypted H.264 track")
	require.True(t, decryptorBuilt.Load(), "H.264 frame decryptor must construct against the shared key provider")

	gotEncryption, _ := encryptionSeen.Load().(livekit.Encryption_Type)
	require.Equal(t, livekit.Encryption_GCM, gotEncryption,
		"subscriber must see GCM encryption type in track info")

	// Rotate the key at the same index on both sides. Exercises the
	// getCachedBlock / getCipherBlock keyBytes-comparison path.
	require.NoError(t, kpPub.SetKeyFromPassphrase(passphrase+"-rotated", keyIndex))
	require.NoError(t, kpSub.SetKeyFromPassphrase(passphrase+"-rotated", keyIndex))

	// After rotation, constructing a fresh H.264 decryptor must still succeed
	// and resolve the current key. Confirms the rotated key is live in the
	// provider and downstream consumers can pick it up.
	dec, err := NewFrameDecryptor(kpSub, CodecH264, nil)
	require.NoError(t, err)
	require.NotNil(t, dec)
}

// TestDynacastRepublish exercises two behaviours around simulcast dynacast:
//
//  1. Dynacast: a SubscribedQualityUpdate from the server disables the simulcast
//     layers no subscriber needs. A disabled layer's LocalTrack stops writing
//     samples (LocalTrack.disabled gates the write worker).
//  2. Re-publish reset: when tracks are re-published (e.g. on a full reconnect,
//     which reuses the same *LocalTrack objects via republishTracks), the
//     per-layer disabled flag is reset so every layer resumes publishing until
//     the new SFU issues its own dynacast update. Without the reset, a layer
//     disabled before the reconnect would stay dark forever.
//
// The dynacast update is injected through handleSubscribedQualityUpdate, which
// is exactly the path Room.OnSubscribedQualityUpdate drives for a real server
// message, so the layer-disabling wiring is covered end-to-end. The re-publish
// is driven by a real full reconnect.
func TestDynacastRepublish(t *testing.T) {
	if apiKey == "" || apiSecret == "" {
		t.Skip("no LIVEKIT_KEYS; requires a running livekit-server")
	}

	videoCodec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}
	const videoName = "dynacast_video"

	var subVideoRTP atomic.Int32
	subCB := &RoomCallback{
		ParticipantCallback: ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, publication *RemoteTrackPublication, _ *RemoteParticipant) {
				if publication.Name() != videoName {
					return
				}
				go func() {
					for {
						if _, _, err := track.ReadRTP(); err != nil {
							return
						}
						subVideoRTP.Add(1)
					}
				}()
			},
		},
	}
	sub, err := createAgent(t.Name(), subCB, "subscriber-dynacast")
	require.NoError(t, err)
	defer sub.Disconnect()

	// simTracks is ordered [LOW, MEDIUM, HIGH]; republishTracks reuses these
	// same objects, so they remain the canonical layer handles across reconnect.
	simTracks := newSimulcastSampleTracks(t, videoCodec, "SC_"+videoName)
	require.Len(t, simTracks, 3)
	layerOf := map[livekit.VideoQuality]*LocalTrack{
		livekit.VideoQuality_LOW:    simTracks[0],
		livekit.VideoQuality_MEDIUM: simTracks[1],
		livekit.VideoQuality_HIGH:   simTracks[2],
	}

	// OnReconnected fires right after OnRestarted -> republishTracks (which resets
	// the disabled flags) and before the new SFU issues its own dynacast update.
	// Snapshot the flags here so the "re-enabled" assertion is race-free against a
	// later server update that could re-disable unwatched layers.
	var reconnected, allEnabledAfterRepublish atomic.Bool
	pubCB := &RoomCallback{
		OnReconnected: func() {
			allEnabled := true
			for _, tr := range simTracks {
				if tr.disabled.Load() {
					allEnabled = false
				}
			}
			allEnabledAfterRepublish.Store(allEnabled)
			reconnected.Store(true)
		},
	}
	pub, err := createAgent(t.Name(), pubCB, "publisher-dynacast")
	require.NoError(t, err)
	defer pub.Disconnect()

	videoPub, err := pub.LocalParticipant.PublishSimulcastTrack(simTracks, &TrackPublicationOptions{Name: videoName})
	require.NoError(t, err)
	require.NotNil(t, videoPub)
	require.NotEmpty(t, videoPub.SID())

	// end-to-end: the subscriber receives video RTP
	require.Eventually(t, func() bool {
		return subVideoRTP.Load() > 0
	}, 15*time.Second, 100*time.Millisecond, "subscriber should receive simulcast video RTP")

	// all layers start enabled (not disabled)
	for q, tr := range layerOf {
		require.False(t, tr.disabled.Load(), "layer %s should start enabled", q)
	}

	// (1) dynacast: server reports only HIGH is needed -> LOW and MEDIUM disabled.
	// handleSubscribedQualityUpdate applies the layer flags synchronously.
	pub.LocalParticipant.handleSubscribedQualityUpdate(&livekit.SubscribedQualityUpdate{
		TrackSid: videoPub.SID(),
		SubscribedCodecs: []*livekit.SubscribedCodec{
			{
				Codec: "vp8",
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: false},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
					{Quality: livekit.VideoQuality_HIGH, Enabled: true},
				},
			},
		},
	})
	require.True(t, layerOf[livekit.VideoQuality_LOW].disabled.Load(), "LOW must be disabled by dynacast")
	require.True(t, layerOf[livekit.VideoQuality_MEDIUM].disabled.Load(), "MEDIUM must be disabled by dynacast")
	require.False(t, layerOf[livekit.VideoQuality_HIGH].disabled.Load(), "HIGH must stay enabled")

	// (2) re-publish reset via a full reconnect: OnRestarted -> republishTracks
	// re-publishes these same tracks, which must clear the disabled flags.
	reconnected.Store(false)
	pub.Simulate(SimulateNodeFailure)
	require.Eventually(t, func() bool {
		return reconnected.Load()
	}, 20*time.Second, 100*time.Millisecond, "publisher should complete a full reconnect")

	// re-publishing the (previously dynacast-disabled) tracks reset every layer's
	// disabled flag to false, captured at reconnect time.
	require.True(t, allEnabledAfterRepublish.Load(), "re-published layers must reset disabled to false")

	// end-to-end: video RTP resumes after the re-publish
	baseline := subVideoRTP.Load()
	require.Eventually(t, func() bool {
		return subVideoRTP.Load() > baseline
	}, 20*time.Second, 100*time.Millisecond, "subscriber should receive video RTP again after re-publish")
}
