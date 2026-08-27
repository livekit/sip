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

package signalling

import (
	"context"
	"net/http"
	"time"

	"github.com/livekit/mediatransportutil/pkg/pacer"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	e2eetypes "github.com/livekit/server-sdk-go/v2/e2ee/types"
	dtlsElliptic "github.com/pion/dtls/v3/pkg/crypto/elliptic"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
	"go.uber.org/zap/zapcore"
	"google.golang.org/protobuf/proto"
)

type Signalling interface {
	SetLogger(l logger.Logger)

	Path() string
	ValidatePath() string

	PublishInJoin() bool

	ConnectQueryParams(
		version string,
		protocol int,
		connectParams *ConnectParams,
		addTrackRequests []*livekit.AddTrackRequest,
		publisherOffer webrtc.SessionDescription,
		participantSID string,
	) (string, error)
	HTTPRequestForValidate(
		ctx context.Context,
		version string,
		protocol int,
		urlPrefix string,
		token string,
		connectParams *ConnectParams,
		participantSID string,
	) (*http.Request, error)
	DecodeErrorResponse(errorDetails []byte) string

	SignalLeaveRequest(leave *livekit.LeaveRequest) proto.Message
	SignalICECandidate(trickle *livekit.TrickleRequest) proto.Message
	SignalSdpOffer(offer *livekit.SessionDescription) proto.Message
	SignalSdpAnswer(answer *livekit.SessionDescription) proto.Message
	SignalSimulateScenario(simulate *livekit.SimulateScenario) proto.Message
	SignalMuteTrack(mute *livekit.MuteTrackRequest) proto.Message
	SignalUpdateSubscription(updateSubscription *livekit.UpdateSubscription) proto.Message
	SignalSyncState(syncState *livekit.SyncState) proto.Message
	SignalAddTrack(addTrack *livekit.AddTrackRequest) proto.Message
	SignalSubscriptionPermission(subscriptionPermission *livekit.SubscriptionPermission) proto.Message
	SignalUpdateTrackSettings(settings *livekit.UpdateTrackSettings) proto.Message
	SignalUpdateParticipantMetadata(metadata *livekit.UpdateParticipantMetadata) proto.Message
}

type ConnectParams struct {
	AutoSubscribe          bool
	Reconnect              bool
	DisableRegionDiscovery bool
	// timeout for each connection attempt, default is 5 seconds
	ConnectTimeout time.Duration

	RetransmitBufferSize uint16

	Metadata string // See WithMetadata

	Attributes map[string]string // See WithExtraAttributes

	Pacer pacer.Factory

	Interceptors []interceptor.Factory

	IncludeDefaultInterceptors bool

	ICETransportPolicy webrtc.ICETransportPolicy

	// DisableTURN removes TURN/TURNS URLs from the ICE server list provided by the SFU.
	// Use this when the client is co-located with the SFU and does not need relay candidates.
	DisableTURN bool

	// IPv6Only restricts ICE to IPv6. Only IPv6 local candidates are gathered
	// (and sent to the server), and any IPv4 remote candidates from the server
	// are rejected before being added to the ICE agent.
	IPv6Only bool

	// ICENetworkTypes overrides pion's default ICE networks (UDP4/UDP6 only).
	// Include webrtc.NetworkTypeTCP4 to accept SFU ICE-TCP (rtc.tcp_port) candidates.
	ICENetworkTypes []webrtc.NetworkType

	DTLSEllipticCurves []dtlsElliptic.Curve // FIPS 140: override default DTLS curves

	// DataEncryptionKeyProvider enables data channel E2EE when set.
	DataEncryptionKeyProvider e2eetypes.KeyProvider

	// UseSinglePeerConnection enables single peer connection mode in which the
	// publisher PC is reused for both publishing and receiving media.
	UseSinglePeerConnection bool

	// internal use
	Codecs []webrtc.RTPCodecParameters

	Logger logger.Logger
}

func (c ConnectParams) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddBool("AutoSubscribe", c.AutoSubscribe)
	e.AddBool("Reconnect", c.Reconnect)
	e.AddBool("DisableRegionDiscovery", c.DisableRegionDiscovery)
	e.AddDuration("ConnectTimeout", c.ConnectTimeout)
	e.AddUint16("RetransmitBufferSize", c.RetransmitBufferSize)
	e.AddBool("DisableTURN", c.DisableTURN)
	e.AddBool("UseSinglePeerConnection", c.UseSinglePeerConnection)
	return nil
}

type SignalTransport interface {
	SetLogger(l logger.Logger)

	Start()
	IsStarted() bool
	Close()
	Join(
		ctx context.Context,
		url string,
		token string,
		connectParams ConnectParams,
		addTrackRequests []*livekit.AddTrackRequest,
		publisherOffer webrtc.SessionDescription,
	) error
	Reconnect(
		ctx context.Context,
		url string,
		token string,
		connectParams ConnectParams,
		participantSID string,
	) error
	SendMessage(msg proto.Message) error
}

type SignalTransportHandler interface {
	OnTransportClose()
}

type SignalHandler interface {
	SetLogger(l logger.Logger)

	HandleMessage(msg proto.Message) error
}

type SignalProcessor interface {
	OnJoinResponse(joinResponse *livekit.JoinResponse) error
	OnReconnectResponse(reconnectResponse *livekit.ReconnectResponse) error
	OnAnswer(sd webrtc.SessionDescription, answerId uint32, midToTrackID map[string]string)
	OnOffer(sd webrtc.SessionDescription, offerId uint32, midToTrackID map[string]string)
	OnTrickle(init webrtc.ICECandidateInit, target livekit.SignalTarget)
	OnParticipantUpdate([]*livekit.ParticipantInfo)
	OnLocalTrackPublished(response *livekit.TrackPublishedResponse)
	OnLocalTrackUnpublished(response *livekit.TrackUnpublishedResponse)
	OnSpeakersChanged([]*livekit.SpeakerInfo)
	OnConnectionQuality([]*livekit.ConnectionQualityInfo)
	OnRoomUpdate(room *livekit.Room)
	OnRoomMoved(moved *livekit.RoomMovedResponse)
	OnTrackRemoteMuted(request *livekit.MuteTrackRequest)
	OnTokenRefresh(refreshToken string)
	OnLeave(*livekit.LeaveRequest)
	OnLocalTrackSubscribed(trackSubscribed *livekit.TrackSubscribed)
	OnSubscribedQualityUpdate(subscribedQualityUpdate *livekit.SubscribedQualityUpdate)
	OnSubscribedAudioCodecUpdate(subscribedAudioCodecUpdate *livekit.SubscribedAudioCodecUpdate)
	OnMediaSectionsRequirement(mediaSectionsRequirement *livekit.MediaSectionsRequirement)
}
