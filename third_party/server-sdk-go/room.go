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
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	protoLogger "github.com/livekit/protocol/logger"
	protosignalling "github.com/livekit/protocol/signalling"

	"github.com/livekit/server-sdk-go/v2/e2ee"
	"github.com/livekit/server-sdk-go/v2/signalling"

	"github.com/livekit/mediatransportutil/pkg/pacer"
	"github.com/livekit/protocol/auth"
	protoCodecs "github.com/livekit/protocol/codecs"
	"github.com/livekit/protocol/livekit"

	dtlsElliptic "github.com/pion/dtls/v3/pkg/crypto/elliptic"
)

var (
	_ engineHandler = (*Room)(nil)
)

// -----------------------------------------------

type SimulateScenario int

const (
	SimulateSignalReconnect SimulateScenario = iota
	SimulateForceTCP
	SimulateForceTLS
	SimulateSpeakerUpdate
	SimulateMigration
	SimulateServerLeave
	SimulateNodeFailure
	// SimulateLeaveRequestFullReconnect asks the server to send a reconnect
	// LeaveRequest that drops the current region, exercising region failover.
	SimulateLeaveRequestFullReconnect
)

type ConnectionState string

const (
	ConnectionStateConnected    ConnectionState = "connected"
	ConnectionStateReconnecting ConnectionState = "reconnecting"
	ConnectionStateDisconnected ConnectionState = "disconnected"
)

// -----------------------------------------------

const (
	SimulateSpeakerUpdateInterval = 5
)

type (
	TrackPubCallback func(track Track, pub TrackPublication, participant *RemoteParticipant)
	PubCallback      func(pub TrackPublication, participant *RemoteParticipant)
)

type ParticipantKind int

const (
	ParticipantStandard  = ParticipantKind(livekit.ParticipantInfo_STANDARD)
	ParticipantIngress   = ParticipantKind(livekit.ParticipantInfo_INGRESS)
	ParticipantEgress    = ParticipantKind(livekit.ParticipantInfo_EGRESS)
	ParticipantSIP       = ParticipantKind(livekit.ParticipantInfo_SIP)
	ParticipantAgent     = ParticipantKind(livekit.ParticipantInfo_AGENT)
	ParticipantConnector = ParticipantKind(livekit.ParticipantInfo_CONNECTOR)
)

type ConnectInfo struct {
	APIKey                string
	APISecret             string
	RoomName              string
	ParticipantName       string
	ParticipantIdentity   string
	ParticipantKind       ParticipantKind
	ParticipantMetadata   string
	ParticipantAttributes map[string]string
}

type connPubTrack struct {
	track           webrtc.TrackLocal
	simulcastTracks []*LocalTrack

	opts    *TrackPublicationOptions
	pubOpts []LocalTrackPublishOption
}

type connParams struct {
	*signalling.ConnectParams

	pubTracks []*connPubTrack
}

type ConnectOption func(*connParams)

func WithConnectTimeout(timeout time.Duration) ConnectOption {
	return func(p *connParams) {
		p.ConnectTimeout = timeout
	}
}

// WithAutoSubscribe sets whether the participant should automatically subscribe to tracks.
// Default is true.
func WithAutoSubscribe(val bool) ConnectOption {
	return func(p *connParams) {
		p.AutoSubscribe = val
	}
}

// WithRetransmitBufferSize sets the retransmit buffer size to respond to NACK requests.
// Must be one of: 1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768.
func WithRetransmitBufferSize(val uint16) ConnectOption {
	return func(p *connParams) {
		p.RetransmitBufferSize = val
	}
}

// WithPacer enables the use of a pacer on this connection
// A pacer helps to smooth out video packet rate to avoid overwhelming downstream. Learn more at: https://chromium.googlesource.com/external/webrtc/+/master/modules/pacing/g3doc/index.md
func WithPacer(pacer pacer.Factory) ConnectOption {
	return func(p *connParams) {
		p.Pacer = pacer
	}
}

// WithInterceptors sets custom RTP interceptors for the connection.
func WithInterceptors(interceptors []interceptor.Factory) ConnectOption {
	return func(p *connParams) {
		p.Interceptors = interceptors
	}
}

// WithIncludeDefaultInterceptors sets whether to register default interceptors
// along with custom interceptors.
func WithIncludeDefaultInterceptors(include bool) ConnectOption {
	return func(p *connParams) {
		p.IncludeDefaultInterceptors = include
	}
}

// WithICETransportPolicy sets the ICE transport policy (UDP, Relay, etc.).
func WithICETransportPolicy(iceTransportPolicy webrtc.ICETransportPolicy) ConnectOption {
	return func(p *connParams) {
		p.ICETransportPolicy = iceTransportPolicy
	}
}

// WithDisableTURN removes TURN/TURNS URLs from the ICE server configuration
// provided by the SFU. Use this when the client is co-located with the SFU
// and does not need relay candidates.
func WithDisableTURN() ConnectOption {
	return func(p *connParams) {
		p.DisableTURN = true
	}
}

// WithIPv6Only restricts ICE negotiation to IPv6. Only IPv6 local candidates
// are gathered and sent to the server, and any IPv4 candidates advertised by
// the server are rejected before being added to the ICE agent. Note that IPv4
// TURN relay candidates can still appear; combine with WithDisableTURN for a
// strictly IPv6 path.
func WithIPv6Only() ConnectOption {
	return func(p *connParams) {
		p.IPv6Only = true
	}
}

// WithICENetworkTypes overrides the ICE candidate network types that are
// gathered locally and accepted from the remote. Pion defaults to UDP4/UDP6
// only (TCP is not enabled). Pass NetworkTypeTCP4 to use an SFU rtc.tcp_port
// candidate (ICE-TCP fallback or force-TCP).
func WithICENetworkTypes(types ...webrtc.NetworkType) ConnectOption {
	return func(p *connParams) {
		p.ICENetworkTypes = types
	}
}

// WithDisableRegionDiscovery disables automatic region discovery for LiveKit Cloud.
func WithDisableRegionDiscovery() ConnectOption {
	return func(p *connParams) {
		p.DisableRegionDiscovery = true
	}
}

// WithMetadata sets custom metadata for the participant.
func WithMetadata(metadata string) ConnectOption {
	return func(p *connParams) {
		p.Metadata = metadata
	}
}

// WithExtraAttributes sets additional key-value attributes for the participant.
// Empty string values will be ignored.
func WithExtraAttributes(attrs map[string]string) ConnectOption {
	return func(p *connParams) {
		if len(attrs) != 0 && p.Attributes == nil {
			p.Attributes = make(map[string]string, len(attrs))
		}
		for k, v := range attrs {
			if v == "" {
				continue
			}
			p.Attributes[k] = v
		}
	}
}

func WithCodecs(codecs []livekit.Codec) ConnectOption {
	return func(p *connParams) {
		pCodecs := make([]webrtc.RTPCodecParameters, 0, len(codecs))
		for i := range codecs {
			pCodecs = append(pCodecs, protoCodecs.ToWebrtcCodecParameters(&codecs[i]))
		}
		p.Codecs = pCodecs
	}
}

// WithDTLSEllipticCurves configures the DTLS elliptic curves used for key exchange.
// Use this on FIPS 140-enabled systems to specify NIST-approved curves (e.g. P-256, P-384)
// instead of the default X25519.
func WithDTLSEllipticCurves(curves ...dtlsElliptic.Curve) ConnectOption {
	return func(p *connParams) {
		p.DTLSEllipticCurves = curves
	}
}

func WithLogger(l protoLogger.Logger) ConnectOption {
	return func(p *connParams) {
		p.Logger = l
	}
}

// WithDataEncryption enables end-to-end encryption for data channel messages.
// When set, outgoing data packets are encrypted and incoming EncryptedPacket
// messages are decrypted automatically using the provided KeyProvider.
func WithDataEncryption(opts *EncryptionOptions) ConnectOption {
	return func(p *connParams) {
		if opts != nil {
			p.DataEncryptionKeyProvider = opts.KeyProvider
		}
	}
}

// WithSinglePeerConnection enables single peer connection mode. In this mode
// the publisher PC handles both publishing and receiving media.
func WithSinglePeerConnection() ConnectOption {
	return func(p *connParams) {
		p.UseSinglePeerConnection = true
	}
}

// WithTrack queues a track to be published as part of the join
// request. Requires WithSinglePeerConnection(). It can be called multiple times to
// publish multiple tracks.
//
// The track's LocalTrackPublication is available via the OnLocalTrackPublished
// callback or LocalParticipant.TrackPublications() as soon as Join returns.
func WithTrack(track webrtc.TrackLocal, opts *TrackPublicationOptions, pubOpts ...LocalTrackPublishOption) ConnectOption {
	return func(p *connParams) {
		p.pubTracks = append(p.pubTracks, &connPubTrack{
			track:   track,
			opts:    opts,
			pubOpts: pubOpts,
		})
	}
}

// WithSimulcastTrack queues a simulcast video track (multiple
// LocalTracks representing different quality layers, low to high) to be
// published as part of the join request. Requires WithSinglePeerConnection().
//
// The track's LocalTrackPublication is available via the OnLocalTrackPublished
// callback or LocalParticipant.TrackPublications() as soon as Join returns.
func WithSimulcastTrack(tracks []*LocalTrack, opts *TrackPublicationOptions, pubOpts ...LocalTrackPublishOption) ConnectOption {
	return func(p *connParams) {
		p.pubTracks = append(p.pubTracks, &connPubTrack{
			simulcastTracks: tracks,
			opts:            opts,
			pubOpts:         pubOpts,
		})
	}
}

type PLIWriter func(webrtc.SSRC)

type trackPublicationWithJoin struct {
	pub *LocalTrackPublication
	// cid keys the trackPublished listener. For a single-layer publication
	// it's the track's CID; for a simulcast publication it's the main
	// (highest-quality) layer's CID, matching AddTrackRequest.Cid.
	cid       string
	opts      *TrackPublicationOptions
	pubChan   chan *livekit.TrackPublishedResponse
	simulcast bool
}

type Room struct {
	log                     protoLogger.Logger
	useSinglePeerConnection bool
	engine                  *RTCEngine
	sid                     string
	name                    string
	LocalParticipant        *LocalParticipant
	callback                *RoomCallback
	sidReady                chan struct{}
	disconnectReason        livekit.DisconnectReason

	remoteParticipants map[livekit.ParticipantIdentity]*RemoteParticipant
	sidToIdentity      map[livekit.ParticipantID]livekit.ParticipantIdentity
	sidDefers          map[livekit.ParticipantID]map[livekit.TrackID]func(p *RemoteParticipant)
	metadata           string
	activeRecording    bool
	activeSpeakers     []Participant
	serverInfo         *livekit.ServerInfo
	regionURLProvider  *regionURLProvider

	sifTrailer []byte

	byteStreamHandlers *sync.Map
	byteStreamReaders  *sync.Map
	textStreamHandlers *sync.Map
	textStreamReaders  *sync.Map
	rpcHandlers        *sync.Map

	lock sync.RWMutex
}

// NewRoom can be used to update callbacks before calling Join
func NewRoom(callback *RoomCallback) *Room {
	r := &Room{
		log:                logger,
		remoteParticipants: make(map[livekit.ParticipantIdentity]*RemoteParticipant),
		sidToIdentity:      make(map[livekit.ParticipantID]livekit.ParticipantIdentity),
		sidDefers:          make(map[livekit.ParticipantID]map[livekit.TrackID]func(*RemoteParticipant)),
		callback:           NewRoomCallback(),
		sidReady:           make(chan struct{}),
		regionURLProvider:  newRegionURLProvider(),
		byteStreamHandlers: &sync.Map{},
		byteStreamReaders:  &sync.Map{},
		textStreamHandlers: &sync.Map{},
		textStreamReaders:  &sync.Map{},
		rpcHandlers:        &sync.Map{},
	}
	r.callback.Merge(callback)

	r.engine = NewRTCEngine(r.useSinglePeerConnection, r, r.getLocalParticipantSID, r.regionURLProvider)
	r.LocalParticipant = newLocalParticipant(r.engine, r.callback, r.serverInfo, r.log)
	return r
}

// ConnectToRoom creates and joins the room
func ConnectToRoom(url string, info ConnectInfo, callback *RoomCallback, opts ...ConnectOption) (*Room, error) {
	room := NewRoom(callback)
	err := room.Join(url, info, opts...)
	if err != nil {
		return nil, err
	}
	return room, nil
}

// ConnectToRoomWithToken creates and joins the room
func ConnectToRoomWithToken(url, token string, callback *RoomCallback, opts ...ConnectOption) (*Room, error) {
	room := NewRoom(callback)
	err := room.JoinWithToken(url, token, opts...)
	if err != nil {
		return nil, err
	}
	return room, nil
}

// SetLogger overrides default logger.
func (r *Room) SetLogger(l protoLogger.Logger) {
	r.log = l
	r.engine.SetLogger(l)
	r.LocalParticipant.SetLogger(l)
	r.lock.RLock()
	for _, rp := range r.remoteParticipants {
		rp.SetLogger(l)
	}
	r.lock.RUnlock()
}

func (r *Room) Name() string {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.name
}

// SID returns the unique session ID of the room.
// This will block until session ID is available, which could take up to 2s after joining the room.
func (r *Room) SID() string {
	<-r.sidReady
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.sid
}

// PrepareConnection - with LiveKit Cloud, determine the best edge data center for the current client to connect to
func (r *Room) PrepareConnection(url, token string) error {
	cloudHostname, _ := parseCloudURL(url)
	if cloudHostname == "" {
		return nil
	}

	return r.regionURLProvider.RefreshRegionSettings(cloudHostname, token)
}

// Join - joins the room with default permissions
func (r *Room) Join(url string, info ConnectInfo, opts ...ConnectOption) error {
	return r.JoinWithContext(context.Background(), url, info, opts...)
}

// JoinWithContext - like Join, but accepts a context for cancellation/deadline.
func (r *Room) JoinWithContext(ctx context.Context, url string, info ConnectInfo, opts ...ConnectOption) error {
	// generate token
	at := auth.NewAccessToken(info.APIKey, info.APISecret)
	grant := &auth.VideoGrant{
		RoomJoin: true,
		Room:     info.RoomName,
	}
	at.SetVideoGrant(grant).
		SetIdentity(info.ParticipantIdentity).
		SetMetadata(info.ParticipantMetadata).
		SetAttributes(info.ParticipantAttributes).
		SetName(info.ParticipantName).
		SetKind(livekit.ParticipantInfo_Kind(info.ParticipantKind))

	token, err := at.ToJWT()
	if err != nil {
		return err
	}

	return r.JoinWithContextAndToken(ctx, url, token, opts...)
}

// JoinWithToken - customize participant options by generating your own token
func (r *Room) JoinWithToken(url, token string, opts ...ConnectOption) error {
	return r.JoinWithContextAndToken(context.Background(), url, token, opts...)
}

// JoinWithContextAndToken - like JoinWithToken, but accepts a context for cancellation/deadline.
func (r *Room) JoinWithContextAndToken(ctx context.Context, url, token string, opts ...ConnectOption) error {
	params := &connParams{
		ConnectParams: &signalling.ConnectParams{
			AutoSubscribe:           true,
			ConnectTimeout:          5 * time.Second,
			UseSinglePeerConnection: r.useSinglePeerConnection,
		},
	}

	for _, opt := range opts {
		opt(params)
	}

	r.engine.setIncomingRequestParams(ctx, url, token, params.ConnectParams, params.DisableRegionDiscovery)

	if params.Logger != nil {
		r.SetLogger(params.Logger)
	}

	// Enable data channel E2EE if a key provider was supplied.
	if params.DataEncryptionKeyProvider != nil {
		r.engine.dataCryptor = e2ee.NewDataCryptor(params.DataEncryptionKeyProvider)
	}

	if params.UseSinglePeerConnection {
		r.useSinglePeerConnection = params.UseSinglePeerConnection
	}
	r.engine.configureSignalling(r.useSinglePeerConnection)

	if len(params.pubTracks) > 0 && !r.useSinglePeerConnection {
		return ErrPublishRequiresSinglePC
	}

	var trackPubs []*trackPublicationWithJoin
	pubFunc := func() ([]*livekit.AddTrackRequest, error) {
		pubs, req, err := r.publishTracksWithJoin(params.pubTracks)
		trackPubs = pubs
		return req, err
	}
	cleanPubReg := func() {
		for _, tp := range trackPubs {
			r.engine.UnregisterTrackPublishedListener(tp.cid)
			// clean simulcast track transceiver as it has been set during publishing
			for _, lt := range tp.pub.simulcastTracks {
				lt.SetTransceiver(nil)
			}
		}
		trackPubs = nil
	}

	if err := r.engine.join(pubFunc, cleanPubReg); err != nil {
		return err
	}

	if err := r.completePublishWithJoin(ctx, trackPubs); err != nil {
		return err
	}

	return nil
}

// Disconnect leaves the room, indicating the client initiated the disconnect.
func (r *Room) Disconnect() {
	r.DisconnectWithReason(livekit.DisconnectReason_CLIENT_INITIATED)
}

// DisconnectWithReason leaves the room with a specific disconnect reason.
func (r *Room) DisconnectWithReason(reason livekit.DisconnectReason) {
	_ = r.engine.SendLeaveWithReason(reason)
	r.cleanup()
}

// ConnectionState returns the current connection state of the room.
func (r *Room) ConnectionState() ConnectionState {
	r.lock.RLock()
	defer r.lock.RUnlock()

	switch r.engine.currentState() {
	case connectionManagerStateInitial, connectionManagerStateClosed:
		return ConnectionStateDisconnected
	case connectionManagerStateResuming, connectionManagerStateReconnecting:
		return ConnectionStateReconnecting
	case connectionManagerStateConnected:
		return ConnectionStateConnected
	}

	return ConnectionStateDisconnected
}

func (r *Room) deferParticipantUpdate(sid livekit.ParticipantID, trackID livekit.TrackID, fnc func(p *RemoteParticipant)) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.sidDefers[sid] == nil {
		r.sidDefers[sid] = make(map[livekit.TrackID]func(p *RemoteParticipant))
	}
	r.sidDefers[sid][trackID] = fnc
}

func (r *Room) runParticipantDefers(sid livekit.ParticipantID, p *RemoteParticipant) {
	r.lock.Lock()
	fncs := r.sidDefers[sid]
	delete(r.sidDefers, sid)
	r.lock.Unlock()

	if len(fncs) != 0 {
		r.log.Infow(
			"running deferred updates for participant",
			"participant", p.Identity(),
			"participantID", sid,
			"numUpdates", len(fncs),
		)
		for _, fnc := range fncs {
			fnc(p)
		}
	}
}

func (r *Room) clearParticipantDefers(sid livekit.ParticipantID, pi *livekit.ParticipantInfo) {
	r.lock.Lock()
	defer r.lock.Unlock()

	for trackID := range r.sidDefers[sid] {
		found := false
		for _, ti := range pi.Tracks {
			if livekit.TrackID(ti.GetSid()) == trackID {
				found = true
				break
			}
		}
		if !found {
			r.log.Infow(
				"deleting deferred update for participant",
				"participant", pi.Identity,
				"participantID", sid,
				"trackID", trackID,
			)
			delete(r.sidDefers[sid], trackID)
			if len(r.sidDefers[sid]) == 0 {
				delete(r.sidDefers, sid)
			}
		}
	}
}

// GetParticipantByIdentity returns a remote participant by their identity.
// Returns nil if not found.
// Note: this represents the current view from the local participant's perspective
func (r *Room) GetParticipantByIdentity(identity string) *RemoteParticipant {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.remoteParticipants[livekit.ParticipantIdentity(identity)]
}

// GetParticipantBySID returns a remote participant by their session ID.
// Returns nil if not found.
// Note: this represents the current view from the local participant's perspective
func (r *Room) GetParticipantBySID(sid string) *RemoteParticipant {
	r.lock.RLock()
	defer r.lock.RUnlock()

	if identity, ok := r.sidToIdentity[livekit.ParticipantID(sid)]; ok {
		return r.remoteParticipants[identity]
	}
	return nil
}

// GetRemoteParticipants returns all remote participants in the room as seen by the local participant
// Note: this does not represent the exact state from the server's view. To get all participants that
// exists on the server, use [RoomServiceClient.ListParticipants] instead.
func (r *Room) GetRemoteParticipants() []*RemoteParticipant {
	r.lock.RLock()
	defer r.lock.RUnlock()

	var participants []*RemoteParticipant
	for _, rp := range r.remoteParticipants {
		participants = append(participants, rp)
	}
	return participants
}

// ActiveSpeakers returns a list of currently active speakers.
// Speakers are ordered by audio level (loudest first).
func (r *Room) ActiveSpeakers() []Participant {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.activeSpeakers
}

func (r *Room) Metadata() string {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.metadata
}

// IsRecording returns true if the room is currently being recorded.
func (r *Room) IsRecording() bool {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.activeRecording
}

// ServerInfo returns information about the LiveKit server.
func (r *Room) ServerInfo() *livekit.ServerInfo {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return proto.Clone(r.serverInfo).(*livekit.ServerInfo)
}

// SifTrailer returns the SIF (Server Injected Frames) trailer data used by E2EE
func (r *Room) SifTrailer() []byte {
	r.lock.RLock()
	defer r.lock.RUnlock()
	trailer := make([]byte, len(r.sifTrailer))
	copy(trailer, r.sifTrailer)
	return trailer
}

func (r *Room) addRemoteParticipant(pi *livekit.ParticipantInfo, updateExisting bool) *RemoteParticipant {
	r.lock.Lock()
	defer r.lock.Unlock()
	rp, ok := r.remoteParticipants[livekit.ParticipantIdentity(pi.Identity)]
	if ok {
		if updateExisting {
			rp.updateInfo(pi)
			r.sidToIdentity[livekit.ParticipantID(pi.Sid)] = livekit.ParticipantIdentity(pi.Identity)
		}

		return rp
	}

	rp = newRemoteParticipant(pi, r.callback, r.engine, func(ssrc webrtc.SSRC) {
		pli := []rtcp.Packet{
			&rtcp.PictureLossIndication{SenderSSRC: uint32(ssrc), MediaSSRC: uint32(ssrc)},
		}
		if subscriber, ok := r.engine.Subscriber(); ok {
			_ = subscriber.pc.WriteRTCP(pli)
		}
	}, r.log.WithValues("participant", pi.Identity))
	r.remoteParticipants[livekit.ParticipantIdentity(pi.Identity)] = rp
	r.sidToIdentity[livekit.ParticipantID(pi.Sid)] = livekit.ParticipantIdentity(pi.Identity)
	return rp
}

func (r *Room) sendSyncState() {
	var previousOffer *webrtc.SessionDescription
	var previousAnswer *webrtc.SessionDescription
	if r.useSinglePeerConnection {
		publisher, ok := r.engine.Publisher()
		if ok {
			previousOffer = publisher.pc.RemoteDescription()
			previousAnswer = publisher.pc.LocalDescription()
		}
	} else {
		subscriber, ok := r.engine.Subscriber()
		if ok {
			previousOffer = subscriber.pc.RemoteDescription()
			previousAnswer = subscriber.pc.LocalDescription()
		}
	}
	if previousOffer == nil || previousAnswer == nil {
		return
	}

	var trackSids []string
	var trackSidsDisabled []string
	sendUnsub := r.engine.getConnectParams().AutoSubscribe
	for _, rp := range r.GetRemoteParticipants() {
		for _, t := range rp.TrackPublications() {
			if t.IsSubscribed() != sendUnsub {
				trackSids = append(trackSids, t.SID())
			}

			if rpub, ok := t.(*RemoteTrackPublication); ok {
				if !rpub.IsEnabled() {
					trackSidsDisabled = append(trackSidsDisabled, t.SID())
				}
			}
		}
	}

	var publishedTracks []*livekit.TrackPublishedResponse
	for _, t := range r.LocalParticipant.TrackPublications() {
		if t.Track() != nil {
			publishedTracks = append(publishedTracks, &livekit.TrackPublishedResponse{
				Cid:   t.Track().ID(),
				Track: t.TrackInfo(),
			})
		}
	}

	var dataChannels []*livekit.DataChannelInfo
	getDCinfo := func(dc *webrtc.DataChannel, target livekit.SignalTarget) {
		if dc != nil && dc.ID() != nil {
			dataChannels = append(dataChannels, &livekit.DataChannelInfo{
				Label:  dc.Label(),
				Id:     uint32(*dc.ID()),
				Target: target,
			})
		}
	}

	getDCinfo(r.engine.GetDataChannel(livekit.DataPacket_RELIABLE), livekit.SignalTarget_PUBLISHER)
	getDCinfo(r.engine.GetDataChannel(livekit.DataPacket_LOSSY), livekit.SignalTarget_PUBLISHER)
	getDCinfo(r.engine.GetDataChannelSub(livekit.DataPacket_RELIABLE), livekit.SignalTarget_SUBSCRIBER)
	getDCinfo(r.engine.GetDataChannelSub(livekit.DataPacket_LOSSY), livekit.SignalTarget_SUBSCRIBER)

	r.engine.SendSyncState(&livekit.SyncState{
		Offer:  protosignalling.ToProtoSessionDescription(*previousOffer, 0, nil),
		Answer: protosignalling.ToProtoSessionDescription(*previousAnswer, 0, nil),
		Subscription: &livekit.UpdateSubscription{
			TrackSids: trackSids,
			Subscribe: !sendUnsub,
		},
		PublishTracks:     publishedTracks,
		DataChannels:      dataChannels,
		TrackSidsDisabled: trackSidsDisabled,
		// MIGRATION-TODO DatachannelReceiveStates
	})
}

func (r *Room) cleanup() {
	r.engine.Close()
	r.LocalParticipant.closeTracks()
	r.setSid("", true)

	r.byteStreamHandlers.Clear()
	r.byteStreamReaders.Range(func(key, value any) bool {
		if reader, ok := value.(*ByteStreamReader); ok {
			reader.close()
		}
		return true
	})
	r.byteStreamReaders.Clear()

	r.textStreamHandlers.Clear()
	r.textStreamReaders.Range(func(key, value any) bool {
		if reader, ok := value.(*TextStreamReader); ok {
			reader.close()
		}
		return true
	})
	r.textStreamReaders.Clear()

	r.rpcHandlers.Clear()
	r.LocalParticipant.cleanup()
}

func (r *Room) setSid(sid string, allowEmpty bool) {
	r.lock.Lock()
	if sid != "" || allowEmpty {
		select {
		case <-r.sidReady:
		// already closed
		default:
			r.sid = sid
			close(r.sidReady)
		}
	}
	r.lock.Unlock()
}

// Simulate triggers various test scenarios for debugging and testing purposes.
// This is primarily used for development and testing.
func (r *Room) Simulate(scenario SimulateScenario) {
	r.engine.Simulate(scenario)
}

func (r *Room) getLocalParticipantSID() string {
	if r.LocalParticipant != nil {
		return r.LocalParticipant.SID()
	}

	return ""
}

func (r *Room) publishTracksWithJoin(pubTracks []*connPubTrack) ([]*trackPublicationWithJoin, []*livekit.AddTrackRequest, error) {
	addTrackRequests := make([]*livekit.AddTrackRequest, 0, len(pubTracks))
	trackPubs := make([]*trackPublicationWithJoin, 0, len(pubTracks))

	rollback := func() {
		for _, tp := range trackPubs {
			r.engine.UnregisterTrackPublishedListener(tp.cid)
			for _, lt := range tp.pub.simulcastTracks {
				lt.SetTransceiver(nil)
			}
		}
	}

	for _, pubTrack := range pubTracks {
		var (
			publication *LocalTrackPublication
			req         *livekit.AddTrackRequest
			simulcast   bool
			err         error
		)
		switch {
		case len(pubTrack.simulcastTracks) > 0:
			publication, req, _, err = r.LocalParticipant.prepareSimulcastTrackPublication(pubTrack.simulcastTracks, pubTrack.opts, pubTrack.pubOpts...)
			simulcast = true
		case pubTrack.track != nil:
			publication, req, err = r.LocalParticipant.prepareTrackPublication(pubTrack.track, pubTrack.opts, pubTrack.pubOpts...)
		default:
			err = ErrInvalidParameter
		}
		if err != nil {
			r.log.Errorw("failed to prepare publish track with join", err)
			// Roll back listeners and transceivers we already registered so the
			// publisher PC doesn't ship orphan m-sections in the join offer.
			rollback()
			return nil, nil, err
		}
		pubChan := make(chan *livekit.TrackPublishedResponse, 1)
		r.engine.RegisterTrackPublishedListener(req.Cid, pubChan)
		trackPubs = append(trackPubs, &trackPublicationWithJoin{
			pub:       publication,
			cid:       req.Cid,
			opts:      pubTrack.opts,
			pubChan:   pubChan,
			simulcast: simulcast,
		})
		addTrackRequests = append(addTrackRequests, req)
	}

	return trackPubs, addTrackRequests, nil
}

func (r *Room) completePublishWithJoin(ctx context.Context, trackPubs []*trackPublicationWithJoin) error {
	if len(trackPubs) == 0 {
		return nil
	}

	g, gctx := errgroup.WithContext(ctx)
	waitCtx, cancel := context.WithTimeout(gctx, trackPublishTimeout)
	defer cancel()
	for _, tp := range trackPubs {
		publication, cid, opts, pubChan, simulcast := tp.pub, tp.cid, tp.opts, tp.pubChan, tp.simulcast
		g.Go(func() error {
			defer r.engine.UnregisterTrackPublishedListener(cid)

			var pubRes *livekit.TrackPublishedResponse
			select {
			case pubRes = <-pubChan:
			case <-waitCtx.Done():
				return waitCtx.Err()
			}

			publication.updateInfo(pubRes.Track)
			r.LocalParticipant.addPublication(publication)
			r.LocalParticipant.notifyTrackPublished(publication)
			label := "published track"
			if simulcast {
				label = "published simulcast track"
			}
			r.engine.log.Infow(label, "name", opts.Name, "source", opts.Source.String(), "trackID", pubRes.Track.Sid)
			return nil
		})
	}

	return g.Wait()
}

// RegisterRpcCtxMethod establishes the participant as a receiver for calls of the specified RPC method.
// Will overwrite any existing callback for the same method.
//
//   - @param method - The name of the indicated RPC method
//   - @param handler - Will be invoked when an RPC request for this method is received
//   - @returns A promise that resolves when the method is successfully registered
//
// Example:
//
//	room.LocalParticipant?.registerRpcMethod(
//		"greet",
//		func (data: RpcInvocationData) => {
//			fmt.Println("Received greeting from ", data.callerIdentity, "with payload ", data.payload)
//			return "Hello, " + data.callerIdentity + "!";
//		}
//	);
//
// The handler should return either a string or an error.
// If unable to respond within `responseTimeout`, the request will result in an error on the caller's side.
//
// You may throw errors of type `RpcError` with a string `message` in the handler,
// and they will be received on the caller's side with the message intact.
// Other errors thrown in your handler will not be transmitted as-is, and will instead arrive to the caller as `1500` ("Application Error").
func (r *Room) RegisterRpcCtxMethod(method string, handler RpcHandlerCtxFunc) error {
	if _, loaded := r.rpcHandlers.LoadOrStore(method, handler); loaded {
		return fmt.Errorf("rpc handler already registered for method: %s, unregisterRpcMethod before trying to register again", method)
	}
	return nil
}

func (r *Room) RegisterRpcMethod(method string, handler RpcHandlerFunc) error {
	return r.RegisterRpcCtxMethod(method, func(ctx context.Context, data []byte) ([]byte, error) {
		req := RpcInvocationData{Payload: string(data)}
		if deadline, ok := ctx.Deadline(); ok && !deadline.IsZero() {
			req.ResponseTimeout = time.Until(deadline)
		}
		if meta := RPCMetadataFromContext(ctx); meta != nil {
			req.RequestID = meta.RequestID
			req.CallerIdentity = meta.CallerIdentity
			req.ResponseTimeout = meta.ResponseTimeout
		}
		resp, err := handler(req)
		return []byte(resp), err
	})
}

// UnregisterRpcMethod unregisters a previously registered RPC method.
func (r *Room) UnregisterRpcMethod(method string) {
	r.rpcHandlers.Delete(method)
}

// RegisterTextStreamHandler registers a handler for incoming text streams on a specific topic.
// The handler will be called when a text stream is received for the given topic.
// It returns an error if a handler is already registered for this topic.
func (r *Room) RegisterTextStreamHandler(topic string, handler TextStreamHandler) error {
	if _, loaded := r.textStreamHandlers.LoadOrStore(topic, handler); loaded {
		return fmt.Errorf("text stream handler already registered for topic: %s", topic)
	}
	return nil
}

// UnregisterTextStreamHandler removes a previously registered text stream handler.
func (r *Room) UnregisterTextStreamHandler(topic string) {
	r.textStreamHandlers.Delete(topic)
}

// RegisterByteStreamHandler registers a handler for incoming byte streams on a specific topic.
// The handler will be called when a byte stream is received for the given topic.
// It returns an error if a handler is already registered for this topic.
func (r *Room) RegisterByteStreamHandler(topic string, handler ByteStreamHandler) error {
	if _, loaded := r.byteStreamHandlers.LoadOrStore(topic, handler); loaded {
		return fmt.Errorf("byte stream handler already registered for topic: %s", topic)
	}
	return nil
}

// UnregisterByteStreamHandler removes a previously registered byte stream handler.
func (r *Room) UnregisterByteStreamHandler(topic string) {
	r.byteStreamHandlers.Delete(topic)
}

// engineHandler implementation
func (r *Room) OnMediaTrack(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	// ensure we have the participant
	participantID, streamID := unpackStreamID(track.StreamID())
	trackID := track.ID()

	if strings.HasPrefix(streamID, "TR_") {
		// backwards compatibility
		trackID = streamID
	}
	update := func(p *RemoteParticipant) {
		p.addSubscribedMediaTrack(track, trackID, receiver)
	}

	rp := r.GetParticipantBySID(participantID)
	if rp == nil {
		r.log.Infow(
			"could not find participant, deferring track update",
			"participantID", participantID,
			"trackID", trackID,
			"streamID", streamID,
		)
		r.deferParticipantUpdate(livekit.ParticipantID(participantID), livekit.TrackID(trackID), update)
		return
	}
	update(rp)
	r.runParticipantDefers(livekit.ParticipantID(participantID), rp)
}

func (r *Room) OnRoomJoined(
	room *livekit.Room,
	participant *livekit.ParticipantInfo,
	otherParticipants []*livekit.ParticipantInfo,
	serverInfo *livekit.ServerInfo,
	sifTrailer []byte,
) {
	r.lock.Lock()
	r.name = room.Name
	r.metadata = room.Metadata
	r.activeRecording = room.ActiveRecording
	r.serverInfo = serverInfo
	r.sifTrailer = make([]byte, len(sifTrailer))
	copy(r.sifTrailer, sifTrailer)
	r.lock.Unlock()

	r.setSid(room.Sid, false)

	r.LocalParticipant.updateInfo(participant)
	r.LocalParticipant.updateSubscriptionPermission()

	for _, pi := range otherParticipants {
		r.addRemoteParticipant(pi, true)
		r.clearParticipantDefers(livekit.ParticipantID(pi.Sid), pi)
		// no need to run participant defers here, since we are connected for the first time
	}
}

func (r *Room) OnDisconnected(reason livekit.DisconnectReason) {
	r.lock.Lock()
	r.disconnectReason = reason
	r.lock.Unlock()

	r.callback.OnDisconnected()
	r.callback.OnDisconnectedWithReason(GetDisconnectionReason(reason))

	r.cleanup()
}

// DisconnectReason returns the raw protocol disconnect reason reported by the
// server. Returns livekit.DisconnectReason_UNKNOWN_REASON if the room has not
// been disconnected or no reason was reported. Use this when the coarse
// DisconnectionReason enum surfaced by OnDisconnectedWithReason collapses
// values you need to distinguish (e.g. ROOM_DELETED vs ROOM_CLOSED).
func (r *Room) DisconnectReason() livekit.DisconnectReason {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.disconnectReason
}

func (r *Room) OnRestarting() {
	r.callback.OnReconnecting()

	for _, rp := range r.GetRemoteParticipants() {
		r.OnParticipantDisconnect(rp, livekit.DisconnectReason_UNKNOWN_REASON)
	}
}

func (r *Room) OnRestarted(
	room *livekit.Room,
	participant *livekit.ParticipantInfo,
	otherParticipants []*livekit.ParticipantInfo,
) {
	r.OnRoomUpdate(room)

	r.LocalParticipant.updateInfo(participant)
	r.LocalParticipant.updateSubscriptionPermission()

	r.OnParticipantUpdate(otherParticipants)

	r.LocalParticipant.republishTracks()

	r.callback.OnReconnected()
}

func (r *Room) OnResuming() {
	r.callback.OnReconnecting()
}

func (r *Room) OnResumed() {
	r.callback.OnReconnected()
	r.sendSyncState()
	r.LocalParticipant.updateSubscriptionPermission()
}

func (r *Room) OnDataPacket(identity string, dataPacket DataPacket) {
	if identity == r.LocalParticipant.Identity() {
		// if sent by itself, do not handle data
		return
	}
	p := r.GetParticipantByIdentity(identity)
	params := DataReceiveParams{
		SenderIdentity: identity,
		Sender:         p,
	}
	switch msg := dataPacket.(type) {
	case *UserDataPacket: // compatibility
		params.Topic = msg.Topic
		if p != nil {
			p.Callback.OnDataReceived(msg.Payload, params)
		}
		r.callback.OnDataReceived(msg.Payload, params)
	}
	if p != nil {
		p.Callback.OnDataPacket(dataPacket, params)
	}
	r.callback.OnDataPacket(dataPacket, params)
}

func (r *Room) OnParticipantUpdate(participants []*livekit.ParticipantInfo) {
	for _, pi := range participants {
		if pi.Sid == r.LocalParticipant.SID() || pi.Identity == r.LocalParticipant.Identity() {
			r.LocalParticipant.updateInfo(pi)
			continue
		}

		rp := r.GetParticipantByIdentity(pi.Identity)
		isNew := rp == nil

		if pi.State == livekit.ParticipantInfo_DISCONNECTED {
			r.OnParticipantDisconnect(rp, pi.GetDisconnectReason())
		} else if isNew {
			rp = r.addRemoteParticipant(pi, true)
			r.clearParticipantDefers(livekit.ParticipantID(pi.Sid), pi)
			r.runParticipantDefers(livekit.ParticipantID(pi.Sid), rp)
			go r.callback.OnParticipantConnected(rp)
		} else {
			oldSid := livekit.ParticipantID(rp.SID())
			rp.updateInfo(pi)
			newSid := livekit.ParticipantID(rp.SID())
			if oldSid != newSid {
				r.log.Infow(
					"participant sid update",
					"participant", rp.Identity(),
					"sid-old", oldSid,
					"sid-new", newSid,
				)
				r.lock.Lock()
				delete(r.sidDefers, oldSid)
				delete(r.sidToIdentity, oldSid)
				r.sidToIdentity[newSid] = livekit.ParticipantIdentity(rp.Identity())
				r.lock.Unlock()
			}
			r.clearParticipantDefers(livekit.ParticipantID(pi.Sid), pi)
			r.runParticipantDefers(newSid, rp)
		}
	}
}

func (r *Room) OnParticipantDisconnect(rp *RemoteParticipant, reason livekit.DisconnectReason) {
	if rp == nil {
		return
	}

	r.lock.Lock()
	delete(r.remoteParticipants, livekit.ParticipantIdentity(rp.Identity()))
	delete(r.sidToIdentity, livekit.ParticipantID(rp.SID()))
	delete(r.sidDefers, livekit.ParticipantID(rp.SID()))
	r.lock.Unlock()

	rp.unpublishAllTracks()
	r.LocalParticipant.handleParticipantDisconnected(rp.Identity())

	rp.info.DisconnectReason = reason
	go r.callback.OnParticipantDisconnected(rp)
}

func (r *Room) OnSpeakersChanged(speakerUpdates []*livekit.SpeakerInfo) {
	speakerMap := make(map[string]Participant)
	for _, p := range r.ActiveSpeakers() {
		speakerMap[p.SID()] = p
	}
	for _, info := range speakerUpdates {
		var participant Participant
		if info.Sid == r.LocalParticipant.SID() {
			participant = r.LocalParticipant
		} else {
			rp := r.GetParticipantBySID(info.Sid)
			if rp == nil {
				continue
			}
			participant = rp
		}

		participant.setAudioLevel(info.Level)
		participant.setIsSpeaking(info.Active)

		if info.Active {
			speakerMap[info.Sid] = participant
		} else {
			delete(speakerMap, info.Sid)
		}
	}

	activeSpeakers := slices.SortedFunc(maps.Values(speakerMap), func(p1, p2 Participant) int {
		return cmp.Compare(p2.AudioLevel(), p1.AudioLevel())
	})
	r.lock.Lock()
	r.activeSpeakers = activeSpeakers
	r.lock.Unlock()
	go r.callback.OnActiveSpeakersChanged(activeSpeakers)
}

func (r *Room) OnConnectionQuality(updates []*livekit.ConnectionQualityInfo) {
	for _, update := range updates {
		if update.ParticipantSid == r.LocalParticipant.SID() {
			r.LocalParticipant.setConnectionQualityInfo(update)
		} else {
			p := r.GetParticipantBySID(update.ParticipantSid)
			if p != nil {
				p.setConnectionQualityInfo(update)
			} else {
				r.log.Debugw("could not find participant", "sid", update.ParticipantSid,
					"localParticipant", r.LocalParticipant.SID())
			}
		}
	}
}

func (r *Room) OnRoomUpdate(room *livekit.Room) {
	metadataChanged := false
	recordingChanged := false
	r.lock.Lock()
	if r.metadata != room.Metadata {
		metadataChanged = true
		r.metadata = room.Metadata
	}
	if r.activeRecording != room.ActiveRecording {
		recordingChanged = true
		r.activeRecording = room.ActiveRecording
	}
	r.lock.Unlock()
	r.setSid(room.Sid, false)
	if metadataChanged {
		go r.callback.OnRoomMetadataChanged(room.Metadata)
	}
	if recordingChanged {
		go r.callback.OnRecordingStatusChanged(room.ActiveRecording)
	}
}

func (r *Room) OnRoomMoved(moved *livekit.RoomMovedResponse) {
	r.log.Infow("room moved", "newRoom", moved.Room.Name)
	r.OnRoomUpdate(moved.Room)

	// setSid is one-shot; update name/sid directly so the getters report the destination room.
	r.lock.Lock()
	r.name = moved.Room.Name
	r.sid = moved.Room.Sid
	r.lock.Unlock()

	for _, rp := range r.GetRemoteParticipants() {
		r.OnParticipantDisconnect(rp, livekit.DisconnectReason_MIGRATION)
	}

	go r.callback.OnRoomMoved(moved.Room.Name, moved.Token)
	go r.callback.OnRoomMovedWithSID(moved.Room.Name, moved.Room.Sid, moved.Token)

	infos := make([]*livekit.ParticipantInfo, 0, len(moved.OtherParticipants)+1)
	infos = append(infos, moved.Participant)
	infos = append(infos, moved.OtherParticipants...)
	r.OnParticipantUpdate(infos)
}

func (r *Room) OnTrackRemoteMuted(msg *livekit.MuteTrackRequest) {
	for _, pub := range r.LocalParticipant.TrackPublications() {
		if pub.SID() == msg.Sid {
			localPub := pub.(*LocalTrackPublication)
			// TODO: pause sending data because it'll be dropped by SFU
			localPub.setMuted(msg.Muted, true)
		}
	}
}

func (r *Room) OnLocalTrackUnpublished(msg *livekit.TrackUnpublishedResponse) {
	err := r.LocalParticipant.UnpublishTrack(msg.TrackSid)
	if err != nil {
		r.log.Errorw("could not unpublish track", err, "trackID", msg.TrackSid)
	}
}

func (r *Room) OnTranscription(transcription *livekit.Transcription) {
	var (
		p           Participant
		publication TrackPublication
	)

	if transcription.TranscribedParticipantIdentity == r.LocalParticipant.Identity() {
		p = r.LocalParticipant
		publication = r.LocalParticipant.getPublication(transcription.TrackId)
	} else {
		rp := r.GetParticipantByIdentity(transcription.TranscribedParticipantIdentity)
		if rp == nil {
			r.log.Debugw("received transcription for unknown participant", "participant", transcription.TranscribedParticipantIdentity)
			return
		}
		publication = rp.getPublication(transcription.TrackId)
		p = rp
	}
	transcriptionSegments := ExtractTranscriptionSegments(transcription)

	r.callback.OnTranscriptionReceived(transcriptionSegments, p, publication)
}

func (r *Room) OnLocalTrackSubscribed(trackSubscribed *livekit.TrackSubscribed) {
	trackPublication := r.LocalParticipant.getLocalPublication(trackSubscribed.TrackSid)
	if trackPublication == nil {
		r.log.Debugw("received track subscribed for unknown track", "trackID", trackSubscribed.TrackSid)
		return
	}
	r.callback.OnLocalTrackSubscribed(trackPublication, r.LocalParticipant)
}

func (r *Room) OnSubscribedQualityUpdate(subscribedQualityUpdate *livekit.SubscribedQualityUpdate) {
	r.LocalParticipant.handleSubscribedQualityUpdate(subscribedQualityUpdate)
}

func (r *Room) OnSubscribedAudioCodecUpdate(subscribedAudioCodecUpdate *livekit.SubscribedAudioCodecUpdate) {
	r.LocalParticipant.handleSubscribedAudioCodecUpdate(subscribedAudioCodecUpdate)
}

func (r *Room) OnMediaSectionsRequirement(mediaSectionsRequirement *livekit.MediaSectionsRequirement) {
	addTransceivers := func(transport *PCTransport, kind webrtc.RTPCodecType, count uint32) {
		for i := uint32(0); i < count; i++ {
			if _, err := transport.PeerConnection().AddTransceiverFromKind(
				kind,
				webrtc.RTPTransceiverInit{
					Direction: webrtc.RTPTransceiverDirectionRecvonly,
				},
			); err != nil {
				r.log.Warnw(
					"could not add transceiver", err,
					"room", r.name,
					"roomID", r.sid,
					"participant", r.LocalParticipant.Identity(),
					"participantID", r.LocalParticipant.SID(),
					"kind", kind,
				)
			} else {
				r.log.Debugw(
					"added transceiver of kind",
					"room", r.name,
					"roomID", r.sid,
					"participant", r.LocalParticipant.Identity(),
					"participantID", r.LocalParticipant.SID(),
					"kind", kind,
				)
			}
		}
	}

	publisher, ok := r.engine.Publisher()
	if !ok {
		r.log.Warnw("no publisher peer connection", ErrNoPeerConnection)
		return
	}

	addTransceivers(publisher, webrtc.RTPCodecTypeAudio, mediaSectionsRequirement.NumAudios)
	addTransceivers(publisher, webrtc.RTPCodecTypeVideo, mediaSectionsRequirement.NumVideos)
	publisher.Negotiate()
}

func (r *Room) OnStreamHeader(streamHeader *livekit.DataStream_Header, participantIdentity string) {
	switch header := streamHeader.ContentHeader.(type) {
	case *livekit.DataStream_Header_TextHeader:
		streamHandlerCallback, ok := r.textStreamHandlers.Load(streamHeader.Topic)
		if !ok {
			r.log.Debugw("ignoring incoming text stream due to no handler for topic", "topic", streamHeader.Topic)
			return
		}

		info := TextStreamInfo{
			baseStreamInfo: &baseStreamInfo{
				Id:         streamHeader.StreamId,
				MimeType:   streamHeader.MimeType,
				Size:       streamHeader.TotalLength,
				Topic:      streamHeader.Topic,
				Timestamp:  streamHeader.Timestamp,
				Attributes: streamHeader.Attributes,
			},
		}

		textStreamReader := NewTextStreamReader(info, streamHeader.TotalLength)
		r.textStreamReaders.Store(streamHeader.StreamId, textStreamReader)
		go streamHandlerCallback.(TextStreamHandler)(textStreamReader, participantIdentity)
	case *livekit.DataStream_Header_ByteHeader:
		streamHandlerCallback, ok := r.byteStreamHandlers.Load(streamHeader.Topic)
		if !ok {
			r.log.Debugw("ignoring incoming byte stream due to no handler for topic", "topic", streamHeader.Topic)
			return
		}

		info := ByteStreamInfo{
			baseStreamInfo: &baseStreamInfo{
				Id:         streamHeader.StreamId,
				MimeType:   streamHeader.MimeType,
				Size:       streamHeader.TotalLength,
				Topic:      streamHeader.Topic,
				Timestamp:  streamHeader.Timestamp,
				Attributes: streamHeader.Attributes,
			},
			Name: &header.ByteHeader.Name,
		}

		byteStreamReader := NewByteStreamReader(info, streamHeader.TotalLength)
		r.byteStreamReaders.Store(streamHeader.StreamId, byteStreamReader)
		go streamHandlerCallback.(ByteStreamHandler)(byteStreamReader, participantIdentity)
	}
}

func (r *Room) OnStreamChunk(streamChunk *livekit.DataStream_Chunk) {
	streamId := streamChunk.StreamId

	byteStreamReader, ok := r.byteStreamReaders.Load(streamId)
	if ok {
		if len(streamChunk.Content) > 0 {
			byteStreamReader.(*ByteStreamReader).enqueue(streamChunk)
		}
	}

	textStreamReader, ok := r.textStreamReaders.Load(streamId)
	if ok {
		if len(streamChunk.Content) > 0 {
			textStreamReader.(*TextStreamReader).enqueue(streamChunk)
		}
	}
}

func (r *Room) OnStreamTrailer(streamTrailer *livekit.DataStream_Trailer) {
	streamId := streamTrailer.StreamId

	byteStreamReader, ok := r.byteStreamReaders.Load(streamId)
	if ok {
		reader := byteStreamReader.(*ByteStreamReader)
		for k, v := range streamTrailer.Attributes {
			reader.Info.Attributes[k] = v
		}
		reader.close()
		r.byteStreamReaders.Delete(streamId)
	}

	textStreamReader, ok := r.textStreamReaders.Load(streamId)
	if ok {
		reader := textStreamReader.(*TextStreamReader)
		for k, v := range streamTrailer.Attributes {
			reader.Info.Attributes[k] = v
		}
		reader.close()
		r.textStreamReaders.Delete(streamId)
	}
}

func (r *Room) OnRpcRequest(callerIdentity, requestId, method, payload string, responseTimeout time.Duration, version uint32) {
	r.engine.publishRpcAck(callerIdentity, requestId)
	returnError := func(e *RpcError) {
		r.engine.publishRpcResponse(callerIdentity, requestId, nil, e)
	}

	if version != 1 {
		returnError(rpcErrorFromBuiltInCodes(RpcUnsupportedVersion, nil))
		return
	}

	handler, ok := r.rpcHandlers.Load(method)
	if !ok {
		returnError(rpcErrorFromBuiltInCodes(RpcUnsupportedMethod, nil))
		return
	}
	ctx := context.Background()
	ctx = withRPCMetadata(ctx, &RpcInvocationMetadata{
		RequestID:       requestId,
		CallerIdentity:  callerIdentity,
		ResponseTimeout: responseTimeout,
	})
	var cancel func()
	if responseTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, responseTimeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	response, err := handler.(RpcHandlerCtxFunc)(ctx, []byte(payload))

	if err != nil {
		if e, ok := errors.AsType[*RpcError](err); ok {
			returnError(e)
			return
		}
		r.log.Warnw("unexpected error returned by RPC handler for method, using application error instead", err, "method", method)
		returnError(rpcErrorFromBuiltInCodes(RpcApplicationError, nil))
		return
	}

	if len(response) > MaxDataBytes {
		returnError(rpcErrorFromBuiltInCodes(RpcResponsePayloadTooLarge, nil))
		return
	}
	r.engine.publishRpcResponse(callerIdentity, requestId, response, nil)
}

func (r *Room) OnRpcAck(requestId string) {
	r.LocalParticipant.HandleIncomingRpcAck(requestId)
}

func (r *Room) OnRpcResponse(requestId string, payload *string, error *RpcError) {
	r.LocalParticipant.HandleIncomingRpcResponse(requestId, payload, error)
}

// ---------------------------------------------------------

func unpackStreamID(packed string) (participantId string, trackId string) {
	parts := strings.Split(packed, "|")
	if len(parts) > 1 {
		return parts[0], packed[len(parts[0])+1:]
	}
	return packed, ""
}
