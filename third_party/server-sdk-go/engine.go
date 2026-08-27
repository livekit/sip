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
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
	protoLogger "github.com/livekit/protocol/logger"
	protosignalling "github.com/livekit/protocol/signalling"

	"github.com/livekit/server-sdk-go/v2/e2ee"
	"github.com/livekit/server-sdk-go/v2/signalling"
)

// -------------------------------------------

type engineHandler interface {
	OnLocalTrackUnpublished(response *livekit.TrackUnpublishedResponse)
	OnTrackRemoteMuted(request *livekit.MuteTrackRequest)
	OnDisconnected(protoReason livekit.DisconnectReason)
	OnMediaTrack(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver)
	OnParticipantUpdate([]*livekit.ParticipantInfo)
	OnSpeakersChanged([]*livekit.SpeakerInfo)
	OnDataPacket(identity string, dataPacket DataPacket)
	OnConnectionQuality([]*livekit.ConnectionQualityInfo)
	OnRoomUpdate(room *livekit.Room)
	OnRoomMoved(moved *livekit.RoomMovedResponse)
	OnRestarting()
	OnRestarted(
		room *livekit.Room,
		participant *livekit.ParticipantInfo,
		otherParticipants []*livekit.ParticipantInfo,
	)
	OnResuming()
	OnResumed()
	OnTranscription(*livekit.Transcription)
	OnRoomJoined(
		room *livekit.Room,
		participant *livekit.ParticipantInfo,
		otherParticipants []*livekit.ParticipantInfo,
		serverInfo *livekit.ServerInfo,
		sifTrailer []byte,
	)
	OnRpcRequest(callerIdentity, requestId, method, payload string, responseTimeout time.Duration, version uint32)
	OnRpcAck(requestId string)
	OnRpcResponse(requestId string, payload *string, error *RpcError)
	OnStreamHeader(*livekit.DataStream_Header, string)
	OnStreamChunk(*livekit.DataStream_Chunk)
	OnStreamTrailer(*livekit.DataStream_Trailer)
	OnLocalTrackSubscribed(trackSubscribed *livekit.TrackSubscribed)
	OnSubscribedQualityUpdate(subscribedQualityUpdate *livekit.SubscribedQualityUpdate)
	OnSubscribedAudioCodecUpdate(subscribedAudioCodecUpdate *livekit.SubscribedAudioCodecUpdate)
	OnMediaSectionsRequirement(mediaSectionsRequirement *livekit.MediaSectionsRequirement)
}

// -------------------------------------------

var (
	_ signalling.SignalTransportHandler = (*RTCEngine)(nil)
	_ signalling.SignalProcessor        = (*RTCEngine)(nil)
)

// -------------------------------------------

const (
	reliableDataChannelName = "_reliable"
	lossyDataChannelName    = "_lossy"

	maxReconnectCount        = 10
	initialReconnectInterval = 300 * time.Millisecond
	maxReconnectInterval     = 60 * time.Second

	// Number of recvonly transceivers pre-allocated on the publisher PC in
	// single peer connection mode so auto-subscribed media has m-sections
	// ready in the initial offer.
	initialMediaSectionsAudio = 3
	initialMediaSectionsVideo = 3
)

type RTCEngine struct {
	log protoLogger.Logger

	useSinglePeerConnection  bool
	engineHandler            engineHandler
	cbGetLocalParticipantSID func() string

	pclock                sync.Mutex
	publisher             *PCTransport
	pendingPublisherOffer webrtc.SessionDescription
	subscriber            *PCTransport

	signalling      signalling.Signalling
	signalHandler   signalling.SignalHandler
	signalTransport signalling.SignalTransport

	dclock          sync.RWMutex
	reliableDC      *webrtc.DataChannel
	lossyDC         *webrtc.DataChannel
	reliableDCSub   *webrtc.DataChannel
	lossyDCSub      *webrtc.DataChannel
	reliableMsgLock sync.Mutex
	reliableMsgSeq  uint32

	trackPublishedListenersLock sync.Mutex
	trackPublishedListeners     map[string]chan *livekit.TrackPublishedResponse

	subscriberPrimary bool
	hasPublish        atomic.Bool
	closed            atomic.Bool

	dataCryptor *e2ee.DataCryptor // E2EE data channel encryption (nil = disabled)

	onClose     []func()
	onCloseLock sync.Mutex

	*connectionManager
}

func NewRTCEngine(
	useSinglePeerConnection bool,
	engineHandler engineHandler,
	getLocalParticipantSID func() string,
	regionProvider *regionURLProvider,
) *RTCEngine {
	e := &RTCEngine{
		log:                      logger,
		useSinglePeerConnection:  useSinglePeerConnection,
		engineHandler:            engineHandler,
		cbGetLocalParticipantSID: getLocalParticipantSID,
		trackPublishedListeners:  make(map[string]chan *livekit.TrackPublishedResponse),
		reliableMsgSeq:           1,
		connectionManager:        newConnectionManager(regionProvider),
	}
	e.signalHandler = signalling.NewSignalHandler(signalling.SignalHandlerParams{
		Logger:    e.log,
		Processor: e,
	})
	e.configureSignalling(useSinglePeerConnection)

	return e
}

func (e *RTCEngine) configureSignalling(useSinglePeerConnection bool) {
	e.useSinglePeerConnection = useSinglePeerConnection
	if useSinglePeerConnection {
		e.signalling = signalling.NewSignallingJoinRequest(signalling.SignallingJoinRequestParams{
			Logger: e.log,
		})
	} else {
		e.signalling = signalling.NewSignalling(signalling.SignallingParams{
			Logger: e.log,
		})
	}
	if e.signalTransport != nil {
		e.signalTransport.Close()
	}
	e.signalTransport = signalling.NewSignalTransportWebSocket(signalling.SignalTransportWebSocketParams{
		Logger:                 e.log,
		Version:                Version,
		Protocol:               PROTOCOL,
		Signalling:             e.signalling,
		SignalTransportHandler: e,
		SignalHandler:          e.signalHandler,
	})
}

// SetLogger overrides default logger.
func (e *RTCEngine) SetLogger(l protoLogger.Logger) {
	e.log = l
	e.connectionManager.setLogger(l)
	e.signalling.SetLogger(l)
	e.signalHandler.SetLogger(l)
	e.signalTransport.SetLogger(l)
	if e.publisher != nil {
		e.publisher.SetLogger(l.WithValues("transport", livekit.SignalTarget_PUBLISHER))
	}
	if e.subscriber != nil {
		e.subscriber.SetLogger(l.WithValues("transport", livekit.SignalTarget_SUBSCRIBER))
	}
}

func (e *RTCEngine) join(
	publishWithJoin func() ([]*livekit.AddTrackRequest, error),
	cleanupPublishWithJoinOnError func(),
) error {
	cleanupPublish := func() {
		if cleanupPublishWithJoinOnError != nil {
			cleanupPublishWithJoinOnError()
		}
	}

	plan, err := e.connectionManager.getConnectionPlan()
	if err != nil {
		e.log.Errorw("could not get connection plan to join", err)
		return err
	}

	connParams := e.connectionManager.getConnectParams()
	connectTimeout := e.connectionManager.getConnectTimeout()

	var errorToReport error
	for _, attempt := range plan {
		e.log.Infow("attempting join plan", "attempt", attempt, "connParams", connParams)

		// apply any backoff wait before attempting to connect
		if attempt.backoffWait > 0 {
			select {
			case <-time.After(attempt.backoffWait):
			case <-attempt.ctx.Done():
				return attempt.ctx.Err()
			}
		}

		var (
			publisherOffer   webrtc.SessionDescription
			err              error
			addTrackRequests []*livekit.AddTrackRequest
		)
		if e.signalling.PublishInJoin() {
			e.pclock.Lock()

			// clear & recreate publisher pc to ensure a clean slate for the publisher offer and any pre-added m-sections.
			e.closePeerConnectionsLocked()
			if err = e.createPublisherPCLocked(webrtc.Configuration{}); err != nil {
				e.pclock.Unlock()
				return err
			}

			if err = e.addInitialMediaSectionsLocked(initialMediaSectionsAudio, initialMediaSectionsVideo); err != nil {
				e.pclock.Unlock()
				return err
			}
			e.pclock.Unlock()

			if publishWithJoin != nil {
				if addTrackRequests, err = publishWithJoin(); err != nil {
					cleanupPublish()
					return err
				}
			}

			e.pclock.Lock()
			publisherOffer, err = e.publisher.GetLocalOffer()
			if err != nil {
				e.pclock.Unlock()
				cleanupPublish()
				return err
			}

			e.pendingPublisherOffer = publisherOffer
			e.pclock.Unlock()
		}

		// if context is cancelled, return early,
		// could happen if incoming context had a deadline or is cancelled externally
		if err = attempt.ctx.Err(); err != nil {
			cleanupPublish()
			return err
		}

		// WithTimeout is already bounded by attempt.ctx, so the caller's deadline
		// (if any) is still honored.
		joinCtx, joinCtxCancel := context.WithTimeout(attempt.ctx, connectTimeout)
		err = e.signalTransport.Join(
			joinCtx,
			attempt.region.Url,
			attempt.token,
			connParams,
			addTrackRequests,
			publisherOffer,
		)
		joinCtxCancel()
		if err != nil {
			e.log.Infow("signal transport join failed", "error", err, "attempt", attempt, "connParams", connParams)

			validateCtx, validateCtxCancel := context.WithTimeout(context.Background(), attempt.validateTimeout)
			verr := e.validate(
				validateCtx,
				attempt.region.Url,
				attempt.token,
				&connParams,
				"",
			)
			validateCtxCancel()
			if verr != nil {
				// the endpoint is unreachable/invalid; report the validate error
				e.log.Infow("signal transport validate failed", "error", verr, "attempt", attempt, "connParams", connParams)
				errorToReport = verr
			} else {
				// the endpoint validates but the signal join failed
				errorToReport = ErrCannotConnectSignal
			}
			e.cleanupConnection()
			cleanupPublish()
			continue
		}

		// use left over time after signal transport connection to wait for peer connection to establish
		timeout := time.Duration(0)
		if deadline, ok := joinCtx.Deadline(); ok {
			timeout = max(0, time.Until(deadline))
		}
		if err = e.waitUntilConnected(timeout); err != nil {
			e.cleanupConnection()
			errorToReport = err
			cleanupPublish()
			e.log.Infow("waiting for connection establishment failed", "error", err, "attempt", attempt, "connParams", connParams, "timeout", timeout)
			continue
		}

		e.connectionManager.setConnected(attempt.region)
		return nil
	}

	if errorToReport == nil {
		// defensive: reaching here means no attempt connected, so never report
		// success (guards an empty plan or a future failure branch that forgets
		// to record an error)
		errorToReport = ErrCannotConnectSignal
	}
	return errorToReport
}

func (e *RTCEngine) OnClose(onClose func()) {
	e.onCloseLock.Lock()
	e.onClose = append(e.onClose, onClose)
	e.onCloseLock.Unlock()
}

func (e *RTCEngine) Close() {
	if !e.closed.CompareAndSwap(false, true) {
		return
	}

	e.connectionManager.setClosed()

	e.pclock.Lock()
	e.pendingPublisherOffer = webrtc.SessionDescription{}
	e.pclock.Unlock()

	go func() {
		for e.connectionManager.isRecovering() {
			time.Sleep(50 * time.Millisecond)
		}

		e.onCloseLock.Lock()
		onClose := e.onClose
		e.onClose = []func(){}
		e.onCloseLock.Unlock()

		for _, onCloseHandler := range onClose {
			onCloseHandler()
		}

		if publisher, ok := e.Publisher(); ok {
			_ = publisher.Close()
		}
		if subscriber, ok := e.Subscriber(); ok {
			_ = subscriber.Close()
		}

		e.signalTransport.Close()
	}()
}

func (e *RTCEngine) IsConnected() bool {
	e.pclock.Lock()
	defer e.pclock.Unlock()

	if e.publisher == nil || (!e.useSinglePeerConnection && e.subscriber == nil) {
		return false
	}
	if e.subscriberPrimary {
		return e.subscriber.IsConnected()
	}
	return e.publisher.IsConnected()
}

func (e *RTCEngine) Publisher() (*PCTransport, bool) {
	e.pclock.Lock()
	defer e.pclock.Unlock()
	return e.publisher, e.publisher != nil
}

func (e *RTCEngine) Subscriber() (*PCTransport, bool) {
	e.pclock.Lock()
	defer e.pclock.Unlock()
	return e.subscriber, e.subscriber != nil
}

func (e *RTCEngine) setRTT(rtt uint32) {
	if subscriber, ok := e.Subscriber(); ok {
		subscriber.SetRTT(rtt)
	}
}

func (e *RTCEngine) configure(
	iceServers []*livekit.ICEServer,
	clientConfig *livekit.ClientConfiguration,
	subscriberPrimary *bool,
) error {
	e.log.Debugw("Using ICE servers", "servers", iceServers)
	configuration := e.makeRTCConfiguration(iceServers, clientConfig)

	// reset reliable message sequence
	e.reliableMsgLock.Lock()
	e.reliableMsgSeq = 1
	e.reliableMsgLock.Unlock()

	e.pclock.Lock()
	defer e.pclock.Unlock()

	if subscriberPrimary != nil {
		e.subscriberPrimary = *subscriberPrimary
	}

	if e.publisher != nil {
		setConfiguration(e.publisher, configuration)
	} else {
		if err := e.createPublisherPCLocked(configuration); err != nil {
			return err
		}
	}

	if e.subscriber != nil {
		setConfiguration(e.subscriber, configuration)
	} else {
		if err := e.createSubscriberPCLocked(configuration); err != nil {
			return err
		}
	}

	return nil
}

func (e *RTCEngine) createPublisherPCLocked(configuration webrtc.Configuration) error {
	connParams := e.connectionManager.getConnectParams()
	var err error
	if e.publisher, err = NewPCTransport(PCTransportParams{
		Configuration:              configuration,
		Codecs:                     connParams.Codecs,
		RetransmitBufferSize:       connParams.RetransmitBufferSize,
		Pacer:                      connParams.Pacer,
		Interceptors:               connParams.Interceptors,
		IncludeDefaultInterceptors: connParams.IncludeDefaultInterceptors,
		OnRTTUpdate:                e.setRTT,
		IsSender:                   true,
		DTLSEllipticCurves:         connParams.DTLSEllipticCurves,
		IPv6Only:                   connParams.IPv6Only,
		ICENetworkTypes:            connParams.ICENetworkTypes,
	}); err != nil {
		return err
	}
	e.publisher.SetLogger(e.log.WithValues("transport", livekit.SignalTarget_PUBLISHER))

	e.publisher.pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			// done
			return
		}
		init := candidate.ToJSON()
		e.log.Debugw(
			"local ICE candidate",
			"transport", livekit.SignalTarget_PUBLISHER,
			"candidate", init.Candidate,
		)
		if err := e.signalTransport.SendMessage(
			e.signalling.SignalICECandidate(
				protosignalling.ToProtoTrickle(init, livekit.SignalTarget_PUBLISHER, false),
			),
		); err != nil {
			e.log.Errorw(
				"could not send ICE candidate", err,
				"transport", livekit.SignalTarget_PUBLISHER,
			)
		}
	})

	e.publisher.pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		e.pclock.Lock()
		publisher := e.publisher
		e.pclock.Unlock()
		e.handleICEConnectionStateChange(publisher, livekit.SignalTarget_PUBLISHER, state)
	})

	e.publisher.OnOffer = func(offer webrtc.SessionDescription) {
		e.hasPublish.Store(true)
		if err := e.signalTransport.SendMessage(
			e.signalling.SignalSdpOffer(
				protosignalling.ToProtoSessionDescription(offer, 0, nil),
			),
		); err != nil {
			e.log.Errorw("could not send offer for publisher pc", err)
		}
	}

	if e.useSinglePeerConnection {
		e.publisher.pc.OnTrack(func(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
			e.engineHandler.OnMediaTrack(remote, receiver)
		})
	}

	trueVal := true
	falseVal := false
	maxRetries := uint16(1)
	e.dclock.Lock()
	e.lossyDC, err = e.publisher.pc.CreateDataChannel(lossyDataChannelName, &webrtc.DataChannelInit{
		Ordered:        &falseVal,
		MaxRetransmits: &maxRetries,
	})
	if err != nil {
		e.dclock.Unlock()
		return err
	}
	e.lossyDC.OnMessage(e.handleDataPacket)

	e.reliableDC, err = e.publisher.pc.CreateDataChannel(reliableDataChannelName, &webrtc.DataChannelInit{
		Ordered: &trueVal,
	})
	if err != nil {
		e.dclock.Unlock()
		return err
	}
	e.reliableDC.OnMessage(e.handleDataPacket)
	e.dclock.Unlock()

	return nil
}

func (e *RTCEngine) addInitialMediaSectionsLocked(numAudio, numVideo int) error {
	if e.publisher == nil {
		return nil
	}
	init := webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}
	for i := 0; i < numAudio; i++ {
		if _, err := e.publisher.pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, init); err != nil {
			return err
		}
	}
	for i := 0; i < numVideo; i++ {
		if _, err := e.publisher.pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, init); err != nil {
			return err
		}
	}
	return nil
}

func (e *RTCEngine) createSubscriberPCLocked(configuration webrtc.Configuration) error {
	if e.useSinglePeerConnection {
		return nil
	}

	connParams := e.connectionManager.getConnectParams()
	var err error
	if e.subscriber, err = NewPCTransport(PCTransportParams{
		Configuration:              configuration,
		Codecs:                     connParams.Codecs,
		RetransmitBufferSize:       connParams.RetransmitBufferSize,
		Interceptors:               connParams.Interceptors,
		IncludeDefaultInterceptors: connParams.IncludeDefaultInterceptors,
		DTLSEllipticCurves:         connParams.DTLSEllipticCurves,
		IPv6Only:                   connParams.IPv6Only,
		ICENetworkTypes:            connParams.ICENetworkTypes,
	}); err != nil {
		return err
	}
	e.subscriber.SetLogger(e.log.WithValues("transport", livekit.SignalTarget_SUBSCRIBER))

	e.subscriber.OnRemoteDescriptionSettled(e.createSubscriberPCAnswerAndSend)

	e.subscriber.pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			// done
			return
		}
		init := candidate.ToJSON()
		e.log.Debugw(
			"local ICE candidate",
			"transport", livekit.SignalTarget_SUBSCRIBER,
			"candidate", init.Candidate,
		)
		if err := e.signalTransport.SendMessage(
			e.signalling.SignalICECandidate(
				protosignalling.ToProtoTrickle(init, livekit.SignalTarget_SUBSCRIBER, false),
			),
		); err != nil {
			e.log.Errorw(
				"could not send ICE candidate", err,
				"transport", livekit.SignalTarget_SUBSCRIBER,
			)
		}
	})

	e.subscriber.pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		e.pclock.Lock()
		subscriber := e.subscriber
		e.pclock.Unlock()
		e.handleICEConnectionStateChange(subscriber, livekit.SignalTarget_SUBSCRIBER, state)
	})

	e.subscriber.pc.OnTrack(func(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		e.engineHandler.OnMediaTrack(remote, receiver)
	})

	e.subscriber.pc.OnDataChannel(func(c *webrtc.DataChannel) {
		e.dclock.Lock()
		defer e.dclock.Unlock()
		if c.Label() == reliableDataChannelName {
			e.reliableDCSub = c
		} else if c.Label() == lossyDataChannelName {
			e.lossyDCSub = c
		} else {
			return
		}
		c.OnMessage(e.handleDataPacket)
	})

	return nil
}

func (e *RTCEngine) handleICEConnectionStateChange(
	transport *PCTransport,
	signalTarget livekit.SignalTarget,
	state webrtc.ICEConnectionState,
) {
	if transport == nil {
		return
	}

	switch state {
	case webrtc.ICEConnectionStateConnected:
		var fields []any
		if pair, err := transport.GetSelectedCandidatePair(); err == nil {
			fields = append(fields, "transport", signalTarget, "iceCandidatePair", pair)
		}
		e.log.Debugw("ICE connected", fields...)

	case webrtc.ICEConnectionStateDisconnected:
		e.log.Debugw("ICE disconnected", "transport", signalTarget)

	case webrtc.ICEConnectionStateFailed:
		e.log.Debugw("ICE failed", "transport", signalTarget)
		e.handleDisconnect("ice-failed", false, nil)
	}
}

func (e *RTCEngine) closePeerConnections() {
	e.pclock.Lock()
	defer e.pclock.Unlock()

	e.closePeerConnectionsLocked()
}

func (e *RTCEngine) closePeerConnectionsLocked() {
	if e.publisher != nil {
		e.publisher.Close()
		e.publisher = nil
	}
	e.pendingPublisherOffer = webrtc.SessionDescription{}
	e.hasPublish.Store(false)

	if e.subscriber != nil {
		e.subscriber.Close()
		e.subscriber = nil
	}
}

func (e *RTCEngine) GetDataChannel(kind livekit.DataPacket_Kind) *webrtc.DataChannel {
	e.dclock.RLock()
	defer e.dclock.RUnlock()
	if kind == livekit.DataPacket_RELIABLE {
		return e.reliableDC
	}
	return e.lossyDC
}

func (e *RTCEngine) GetDataChannelSub(kind livekit.DataPacket_Kind) *webrtc.DataChannel {
	e.dclock.RLock()
	defer e.dclock.RUnlock()
	if kind == livekit.DataPacket_RELIABLE {
		return e.reliableDCSub
	}
	return e.lossyDCSub
}

func (e *RTCEngine) waitUntilConnected(timeout time.Duration) error {
	return waitUntilConnected(timeout, func() bool {
		return e.IsConnected()
	})
}

func (e *RTCEngine) ensurePublisherConnected(ensureDataReady bool) error {
	e.pclock.Lock()
	subscriberPrimary := e.subscriberPrimary
	e.pclock.Unlock()
	connectTimeout := e.connectionManager.getConnectTimeout()
	if !subscriberPrimary {
		return e.waitUntilConnected(connectTimeout)
	}

	var negotiated bool
	return waitUntilConnected(connectTimeout, func() bool {
		if publisher, ok := e.Publisher(); ok {
			if publisher.IsConnected() && (!ensureDataReady || e.dataPubChannelReady()) {
				return true
			}
			if !negotiated {
				publisher.Negotiate()
				negotiated = true
			}
		}
		return false
	})
}

func (e *RTCEngine) dataPubChannelReady() bool {
	e.dclock.RLock()
	defer e.dclock.RUnlock()
	return e.reliableDC.ReadyState() == webrtc.DataChannelStateOpen && e.lossyDC.ReadyState() == webrtc.DataChannelStateOpen
}

func (e *RTCEngine) RegisterTrackPublishedListener(cid string, c chan *livekit.TrackPublishedResponse) {
	e.trackPublishedListenersLock.Lock()
	e.trackPublishedListeners[cid] = c
	e.trackPublishedListenersLock.Unlock()
}

func (e *RTCEngine) UnregisterTrackPublishedListener(cid string) {
	e.trackPublishedListenersLock.Lock()
	delete(e.trackPublishedListeners, cid)
	e.trackPublishedListenersLock.Unlock()
}

func (e *RTCEngine) handleDataPacket(msg webrtc.DataChannelMessage) {
	packet, err := e.readDataPacket(msg)
	if err != nil {
		return
	}

	// Decrypt if data channel E2EE is enabled and this is an encrypted packet.
	if ep, ok := packet.Value.(*livekit.DataPacket_EncryptedPacket); ok {
		if e.dataCryptor == nil {
			e.log.Errorw("received encrypted data packet but no data cryptor is configured, dropping packet", nil)
			return
		}
		payload, err := e.dataCryptor.Decrypt(ep.EncryptedPacket)
		if err != nil {
			e.log.Warnw("data decryption failed, dropping packet", err)
			return
		}
		decryptedPayloadToDataPacketValue(packet, payload)
	}

	identity := packet.ParticipantIdentity
	switch msg := packet.Value.(type) {
	case *livekit.DataPacket_User:
		m := msg.User
		//lint:ignore SA1019 backward compatibility
		if ptr := &m.ParticipantIdentity; *ptr == "" {
			*ptr = identity
		}
		//lint:ignore SA1019 backward compatibility
		if ptr := &m.DestinationIdentities; len(*ptr) == 0 {
			*ptr = packet.DestinationIdentities
		}

		if identity == "" {
			//lint:ignore SA1019 backward compatibility
			identity = m.ParticipantIdentity
		}
		e.engineHandler.OnDataPacket(identity, &UserDataPacket{
			Payload: m.Payload,
			Topic:   m.GetTopic(),
		})
	case *livekit.DataPacket_SipDtmf:
		e.engineHandler.OnDataPacket(identity, msg.SipDtmf)
	case *livekit.DataPacket_ChatMessage:
		e.engineHandler.OnDataPacket(identity, msg.ChatMessage)
	case *livekit.DataPacket_Transcription:
		e.engineHandler.OnTranscription(msg.Transcription)
	case *livekit.DataPacket_RpcRequest:
		e.engineHandler.OnRpcRequest(
			packet.ParticipantIdentity,
			msg.RpcRequest.Id,
			msg.RpcRequest.Method,
			msg.RpcRequest.Payload,
			time.Duration(msg.RpcRequest.ResponseTimeoutMs)*time.Millisecond,
			msg.RpcRequest.Version,
		)
	case *livekit.DataPacket_RpcAck:
		e.engineHandler.OnRpcAck(msg.RpcAck.RequestId)
	case *livekit.DataPacket_RpcResponse:
		switch res := msg.RpcResponse.Value.(type) {
		case *livekit.RpcResponse_Payload:
			e.engineHandler.OnRpcResponse(msg.RpcResponse.RequestId, &res.Payload, nil)
		case *livekit.RpcResponse_Error:
			e.engineHandler.OnRpcResponse(msg.RpcResponse.RequestId, nil, fromProto(res.Error))
		}
	case *livekit.DataPacket_StreamHeader:
		e.engineHandler.OnStreamHeader(msg.StreamHeader, identity)
	case *livekit.DataPacket_StreamChunk:
		e.engineHandler.OnStreamChunk(msg.StreamChunk)
	case *livekit.DataPacket_StreamTrailer:
		e.engineHandler.OnStreamTrailer(msg.StreamTrailer)
	}
}

func (e *RTCEngine) readDataPacket(msg webrtc.DataChannelMessage) (*livekit.DataPacket, error) {
	dataPacket := &livekit.DataPacket{}
	if msg.IsString {
		err := protojson.Unmarshal(msg.Data, dataPacket)
		return dataPacket, err
	}
	err := proto.Unmarshal(msg.Data, dataPacket)
	return dataPacket, err
}

func (e *RTCEngine) handleDisconnect(reason string, fullReconnect bool, regionSettings *livekit.RegionSettings) {
	e.log.Debugw(
		"handling disconnect",
		"reason", reason,
		"fullReconnect", fullReconnect,
		"regionSettings", regionSettings,
		"closed", e.closed.Load(),
	)

	if e.closed.Load() {
		e.log.Infow(
			"ignoring disconnect",
			"reason", reason,
			"fullReconnect", fullReconnect,
			"regionSettings", regionSettings,
			"closed", e.closed.Load(),
		)
		return
	}

	// requestRecovery applies the transition and reports whether this call must
	// start the worker; if one is already running it will act on the new state.
	if !e.connectionManager.requestRecovery(fullReconnect, regionSettings) {
		return
	}

	go func() {
		for reconnectCount := 0; reconnectCount < maxReconnectCount && !e.closed.Load(); reconnectCount++ {
			if e.connectionManager.isReconnectingState() {
				if reconnectCount == 0 {
					e.engineHandler.OnRestarting()
				}
				e.log.Infow("restarting connection...", "reconnectCount", reconnectCount)
				err := e.restartConnection()
				if err == nil {
					// a new disconnect may have arrived while settling; if so,
					// keep the worker running
					if e.connectionManager.recoveryWorkerShouldContinue() {
						reconnectCount = 0
						continue
					}
					return
				}
				e.log.Errorw("restart connection failed", err)
			} else {
				if reconnectCount == 0 {
					e.engineHandler.OnResuming()
				}
				e.log.Infow("resuming connection...", "reconnectCount", reconnectCount)
				err := e.resumeConnection()
				if err == nil {
					// a reconnect leave (or another disconnect) may have arrived
					// while resume was settling; if so, keep the worker running
					if e.connectionManager.recoveryWorkerShouldContinue() {
						reconnectCount = 0
						continue
					}
					return
				}

				e.log.Errorw("resume connection failed", err)
				// escalate to a full reconnect so region failover engages
				// instead of retrying resume against the same (dead) region
				e.connectionManager.setReconnecting(nil)
				reconnectCount = 0
				continue
			}

			delay := time.Duration(reconnectCount*reconnectCount) * initialReconnectInterval
			if delay > maxReconnectInterval {
				break
			}
			if reconnectCount < maxReconnectCount-1 {
				e.log.Infow("reconnecting...", "reconnectCount", reconnectCount, "delay", delay)
				time.Sleep(delay)
			}
		}

		// gave up (or closed): release the worker slot so a later disconnect can
		// re-arm recovery, then report the disconnect
		e.connectionManager.stopRecoveryWorker()
		e.engineHandler.OnDisconnected(livekit.DisconnectReason_SIGNAL_CLOSE)
	}()
}

func (e *RTCEngine) resumeConnection() error {
	plan, err := e.connectionManager.getConnectionPlan()
	if err != nil {
		e.log.Errorw("could not get connection plan to resume connection", err)
		return err
	}

	connParams := e.connectionManager.getConnectParams()
	connectTimeout := e.connectionManager.getConnectTimeout()

	var errorToReport error
	for _, attempt := range plan {
		e.log.Infow("attempting resume plan", "attempt", attempt, "connParams", connParams)

		// apply any backoff wait before attempting to connect
		if attempt.backoffWait > 0 {
			select {
			case <-time.After(attempt.backoffWait):
			case <-attempt.ctx.Done():
				return attempt.ctx.Err()
			}
		}

		if err = attempt.ctx.Err(); err != nil {
			return err
		}

		// WithTimeout is already bounded by attempt.ctx, so the caller's deadline
		// (if any) is still honored.
		resumeCtx, resumeCtxCancel := context.WithTimeout(attempt.ctx, connectTimeout)

		err = e.signalTransport.Reconnect(
			resumeCtx,
			attempt.region.Url,
			attempt.token,
			connParams,
			e.cbGetLocalParticipantSID(),
		)
		resumeCtxCancel()
		if err != nil {
			e.log.Infow("signal transport resume failed", "error", err, "attempt", attempt, "connParams", connParams)

			validateCtx, validateCtxCancel := context.WithTimeout(context.Background(), attempt.validateTimeout)
			verr := e.validate(
				validateCtx,
				attempt.region.Url,
				attempt.token,
				&connParams,
				e.cbGetLocalParticipantSID(),
			)
			validateCtxCancel()
			if verr != nil {
				// the endpoint is unreachable/invalid; report the validate error
				e.log.Infow("signal transport validate failed", "error", verr, "attempt", attempt, "connParams", connParams)
				errorToReport = verr
			} else {
				// the endpoint validates but the signal resume failed
				errorToReport = ErrCannotConnectSignal
			}
			continue
		}

		e.signalTransport.Start()

		// send offer if publisher enabled
		e.pclock.Lock()
		sendOffer := !e.subscriberPrimary || e.hasPublish.Load()
		publisher := e.publisher
		e.pclock.Unlock()
		if sendOffer && publisher != nil {
			if err := publisher.createAndSendOffer(&webrtc.OfferOptions{
				ICERestart: true,
			}); err != nil {
				return err
			}
		}

		// use left over time after signal transport connection to wait for peer connection to establish
		timeout := time.Duration(0)
		if deadline, ok := resumeCtx.Deadline(); ok {
			timeout = max(0, time.Until(deadline))
		}
		if err = e.waitUntilConnected(timeout); err != nil {
			e.log.Infow("waiting for connection establishment failed in resume", "error", err, "attempt", attempt, "connParams", connParams, "timeout", timeout)
			return err
		}

		// restore steady state so subsequent resumes start from Connected (and pick
		// up fresh server region settings) and connectedRegion tracks where we are.
		// setResumed is a no-op if a reconnect was requested while resuming, so it
		// won't clobber a pending full reconnect.
		e.connectionManager.setResumed(attempt.region)
		e.engineHandler.OnResumed()
		return nil
	}

	if errorToReport == nil {
		// defensive: reaching here means no attempt resumed, so never report
		// success (guards an empty plan or a future failure branch that forgets
		// to record an error)
		errorToReport = ErrCannotConnectSignal
	}
	return errorToReport
}

func (e *RTCEngine) cleanupConnection() {
	if e.signalTransport.IsStarted() {
		// TODO: special reason for reconnect?
		e.SendLeaveWithReason(livekit.DisconnectReason_UNKNOWN_REASON)
	}
	e.signalTransport.Close()
	e.closePeerConnections()
}

func (e *RTCEngine) restartConnection() error {
	e.cleanupConnection()
	return e.join(nil, nil)
}

func (e *RTCEngine) createSubscriberPCAnswerAndSend() error {
	answer, err := e.subscriber.pc.CreateAnswer(nil)
	if err != nil {
		e.log.Errorw("could not create answer", err)
		return err
	}
	if err := e.subscriber.pc.SetLocalDescription(answer); err != nil {
		e.log.Errorw("could not set subscriber local description", err)
		return err
	}
	e.log.Debugw("sending answer for subscriber", "answer", answer)
	if err := e.signalTransport.SendMessage(
		e.signalling.SignalSdpAnswer(
			protosignalling.ToProtoSessionDescription(answer, 0, nil),
		),
	); err != nil {
		e.log.Errorw("could not send answer for subscriber pc", err)
	}
	return nil
}

func (e *RTCEngine) makeRTCConfiguration(iceServers []*livekit.ICEServer, clientConfig *livekit.ClientConfiguration) webrtc.Configuration {
	connParams := e.connectionManager.getConnectParams()
	if connParams.DisableTURN {
		iceServers = filterTURNServers(iceServers)
	}
	rtcICEServers := protosignalling.FromProtoIceServers(iceServers)
	configuration := webrtc.Configuration{
		ICEServers:         rtcICEServers,
		ICETransportPolicy: connParams.ICETransportPolicy,
	}
	if clientConfig != nil &&
		clientConfig.GetForceRelay() == livekit.ClientConfigSetting_ENABLED {
		configuration.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}
	return configuration
}

func filterTURNServers(iceServers []*livekit.ICEServer) []*livekit.ICEServer {
	var filtered []*livekit.ICEServer
	for _, server := range iceServers {
		var urls []string
		for _, u := range server.Urls {
			lower := strings.ToLower(u)
			if !strings.HasPrefix(lower, "turn:") && !strings.HasPrefix(lower, "turns:") {
				urls = append(urls, u)
			}
		}
		if len(urls) > 0 {
			filtered = append(filtered, &livekit.ICEServer{
				Urls:       urls,
				Username:   server.Username,
				Credential: server.Credential,
			})
		}
	}
	return filtered
}

func (e *RTCEngine) publishDataPacket(pck *livekit.DataPacket, kind livekit.DataPacket_Kind) error {
	err := e.ensurePublisherConnected(true)
	if err != nil {
		e.log.Errorw("could not ensure publisher connected", err)
		return err
	}

	dc := e.GetDataChannel(kind)
	if dc == nil {
		e.log.Errorw("could not get data channel", nil, "kind", kind)
		return errors.New("datachannel not found")
	}

	// Encrypt if data channel E2EE is enabled.
	if e.dataCryptor != nil {
		pck, err = e.dataCryptor.Encrypt(pck)
		if err != nil {
			e.log.Warnw("data encryption failed, dropping packet", err)
			return fmt.Errorf("data encryption: %w", err)
		}
	}

	if kind == livekit.DataPacket_RELIABLE {
		e.reliableMsgLock.Lock()
		defer e.reliableMsgLock.Unlock()

		pck.Sequence = e.reliableMsgSeq
		e.reliableMsgSeq++
	}

	data, err := proto.Marshal(pck)
	if err != nil {
		e.log.Errorw("could not marshal data packet", err)
		return err
	}

	dc.Send(data)
	return nil
}

func (e *RTCEngine) publishDataPacketReliable(pck *livekit.DataPacket) error {
	return e.publishDataPacket(pck, livekit.DataPacket_RELIABLE)
}

//lint:ignore U1000 Ignore unused function
func (e *RTCEngine) publishDataPacketLossy(pck *livekit.DataPacket) error {
	return e.publishDataPacket(pck, livekit.DataPacket_LOSSY)
}

// TODO: adjust RPC methods to return error on publishDataPacket failure
func (e *RTCEngine) publishRpcResponse(destinationIdentity, requestId string, payload []byte, err *RpcError) error {
	resp := &livekit.DataPacket_RpcResponse{
		RpcResponse: &livekit.RpcResponse{
			RequestId: requestId,
		},
	}
	packet := &livekit.DataPacket{
		DestinationIdentities: []string{destinationIdentity},
		Value:                 resp,
	}

	if err != nil {
		resp.RpcResponse.Value = &livekit.RpcResponse_Error{
			Error: err.toProto(),
		}
	} else {
		resp.RpcResponse.Value = &livekit.RpcResponse_Payload{
			Payload: string(payload),
		}
	}

	publishErr := e.publishDataPacketReliable(packet)
	if publishErr != nil {
		e.log.Errorw("could not publish rpc response", publishErr)
	}
	return publishErr
}

func (e *RTCEngine) publishRpcAck(destinationIdentity, requestId string) error {
	packet := &livekit.DataPacket{
		DestinationIdentities: []string{destinationIdentity},
		Value: &livekit.DataPacket_RpcAck{
			RpcAck: &livekit.RpcAck{
				RequestId: requestId,
			},
		},
	}

	publishErr := e.publishDataPacketReliable(packet)
	if publishErr != nil {
		e.log.Errorw("could not publish rpc ack", publishErr)
	}
	return publishErr
}

func (e *RTCEngine) publishRpcRequest(destinationIdentity, requestId, method, payload string, responseTimeout time.Duration) error {
	packet := &livekit.DataPacket{
		DestinationIdentities: []string{destinationIdentity},
		Value: &livekit.DataPacket_RpcRequest{
			RpcRequest: &livekit.RpcRequest{
				Id:                requestId,
				Method:            method,
				Payload:           payload,
				ResponseTimeoutMs: uint32(responseTimeout.Milliseconds()),
				Version:           1,
			},
		},
	}

	publishErr := e.publishDataPacketReliable(packet)
	if publishErr != nil {
		e.log.Errorw("could not publish rpc request", publishErr)
	}
	return publishErr
}

func (e *RTCEngine) publishStreamHeader(header *livekit.DataStream_Header, destinationIdentities []string) error {
	packet := &livekit.DataPacket{
		DestinationIdentities: destinationIdentities,
		Value: &livekit.DataPacket_StreamHeader{
			StreamHeader: header,
		},
	}

	publishErr := e.publishDataPacketReliable(packet)
	if publishErr != nil {
		e.log.Errorw("could not publish stream header", publishErr)
	}
	return publishErr
}

func (e *RTCEngine) publishStreamChunk(chunk *livekit.DataStream_Chunk, destinationIdentities []string) error {
	packet := &livekit.DataPacket{
		DestinationIdentities: destinationIdentities,
		Value: &livekit.DataPacket_StreamChunk{
			StreamChunk: chunk,
		},
	}

	publishErr := e.publishDataPacketReliable(packet)
	if publishErr != nil {
		e.log.Errorw("could not publish stream chunk", publishErr)
	}
	return publishErr
}

func (e *RTCEngine) publishStreamTrailer(streamId string, destinationIdentities []string) error {
	packet := &livekit.DataPacket{
		DestinationIdentities: destinationIdentities,
		Value: &livekit.DataPacket_StreamTrailer{
			StreamTrailer: &livekit.DataStream_Trailer{
				StreamId: streamId,
			},
		},
	}

	publishErr := e.publishDataPacketReliable(packet)
	if publishErr != nil {
		e.log.Errorw("could not publish stream trailer", publishErr)
	}
	return publishErr
}

func (e *RTCEngine) isBufferStatusLow(kind livekit.DataPacket_Kind) bool {
	dc := e.GetDataChannel(kind)
	if dc != nil {
		return dc.BufferedAmount() <= dc.BufferedAmountLowThreshold()
	}
	return false
}

func (e *RTCEngine) waitForBufferStatusLow(kind livekit.DataPacket_Kind) {
	for !e.isBufferStatusLow(kind) {
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *RTCEngine) SendAddTrack(addTrack *livekit.AddTrackRequest) error {
	return e.signalTransport.SendMessage(e.signalling.SignalAddTrack(addTrack))
}

func (e *RTCEngine) SendSubscriptionPermission(subscriptionPermission *livekit.SubscriptionPermission) error {
	return e.signalTransport.SendMessage(e.signalling.SignalSubscriptionPermission(subscriptionPermission))
}

func (e *RTCEngine) SendUpdateTrackSettings(settings *livekit.UpdateTrackSettings) error {
	return e.signalTransport.SendMessage(e.signalling.SignalUpdateTrackSettings(settings))
}

func (e *RTCEngine) SendUpdateParticipantMetadata(metadata *livekit.UpdateParticipantMetadata) error {
	return e.signalTransport.SendMessage(e.signalling.SignalUpdateParticipantMetadata(metadata))
}

func (e *RTCEngine) SendMuteTrack(sid string, muted bool) error {
	return e.signalTransport.SendMessage(
		e.signalling.SignalMuteTrack(
			&livekit.MuteTrackRequest{
				Sid:   sid,
				Muted: muted,
			},
		),
	)
}

func (e *RTCEngine) SendUpdateSubscription(updateSubscription *livekit.UpdateSubscription) error {
	return e.signalTransport.SendMessage(e.signalling.SignalUpdateSubscription(updateSubscription))
}

func (e *RTCEngine) SendSyncState(syncState *livekit.SyncState) error {
	return e.signalTransport.SendMessage(e.signalling.SignalSyncState(syncState))
}

func (e *RTCEngine) SendLeaveWithReason(reason livekit.DisconnectReason) error {
	return e.signalTransport.SendMessage(
		e.signalling.SignalLeaveRequest(
			&livekit.LeaveRequest{
				Reason: reason,
			},
		),
	)
}

func (e *RTCEngine) Simulate(scenario SimulateScenario) {
	switch scenario {
	case SimulateSignalReconnect:
		e.signalTransport.Close()

	case SimulateForceTCP:
		// pion does not support active tcp candidate, skip

	case SimulateForceTLS:
		e.signalTransport.SendMessage(
			e.signalling.SignalSimulateScenario(
				&livekit.SimulateScenario{
					Scenario: &livekit.SimulateScenario_SwitchCandidateProtocol{
						SwitchCandidateProtocol: livekit.CandidateProtocol_TLS,
					},
				},
			),
		)
		e.OnLeave(&livekit.LeaveRequest{
			Action: livekit.LeaveRequest_RECONNECT,
			Reason: livekit.DisconnectReason_CLIENT_INITIATED,
		})

	case SimulateSpeakerUpdate:
		e.signalTransport.SendMessage(
			e.signalling.SignalSimulateScenario(
				&livekit.SimulateScenario{
					Scenario: &livekit.SimulateScenario_SpeakerUpdate{
						SpeakerUpdate: SimulateSpeakerUpdateInterval,
					},
				},
			),
		)

	case SimulateMigration:
		e.signalTransport.SendMessage(
			e.signalling.SignalSimulateScenario(
				&livekit.SimulateScenario{
					Scenario: &livekit.SimulateScenario_Migration{
						Migration: true,
					},
				},
			),
		)

	case SimulateServerLeave:
		e.signalTransport.SendMessage(
			e.signalling.SignalSimulateScenario(
				&livekit.SimulateScenario{
					Scenario: &livekit.SimulateScenario_ServerLeave{
						ServerLeave: true,
					},
				},
			),
		)

	case SimulateNodeFailure:
		e.signalTransport.SendMessage(
			e.signalling.SignalSimulateScenario(
				&livekit.SimulateScenario{
					Scenario: &livekit.SimulateScenario_NodeFailure{
						NodeFailure: true,
					},
				},
			),
		)

	case SimulateLeaveRequestFullReconnect:
		e.signalTransport.SendMessage(
			e.signalling.SignalSimulateScenario(
				&livekit.SimulateScenario{
					Scenario: &livekit.SimulateScenario_LeaveRequestFullReconnect{
						LeaveRequestFullReconnect: true,
					},
				},
			),
		)
	}
}

func (e *RTCEngine) validate(
	ctx context.Context,
	urlPrefix string,
	token string,
	connectParams *signalling.ConnectParams,
	participantSID string,
) error {
	req, err := e.signalling.HTTPRequestForValidate(
		ctx,
		Version,
		PROTOCOL,
		urlPrefix,
		token,
		connectParams,
		participantSID,
	)
	if err != nil {
		return err
	}

	hresp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.log.Errorw("error getting validation", err, "httpResponse", hresp)
		return err
	}
	defer hresp.Body.Close()

	if hresp.StatusCode == http.StatusOK {
		// no specific errors to return if validate succeeds
		e.log.Infow("validate succeeded")
		return nil
	}

	var errString string
	switch hresp.StatusCode {
	case http.StatusUnauthorized:
		errString = "unauthorized: "
	case http.StatusNotFound:
		errString = "not found"
	case http.StatusServiceUnavailable:
		errString = "unavailable: "
	}
	if hresp.StatusCode != http.StatusNotFound {
		body, err := io.ReadAll(hresp.Body)
		if err == nil {
			errString += e.signalling.DecodeErrorResponse(body)
		}
		e.log.Errorw("validation error", errors.New(errString), "httpResponse", hresp)
	} else {
		e.log.Errorw("validation error", errors.New(errString))
	}
	return errors.New(errString)
}

// signalling.SignalTransportHandler implementation
func (e *RTCEngine) OnTransportClose() {
	e.handleDisconnect("transport-close", false, nil)
}

// signalling.SignalProcessor implementation
func (e *RTCEngine) OnJoinResponse(res *livekit.JoinResponse) error {
	isRestarting := e.connectionManager.isReconnectingState()

	err := e.configure(res.IceServers, res.ClientConfiguration, proto.Bool(res.SubscriberPrimary))
	if err != nil {
		e.log.Warnw("could not configure", err)
		return err
	}

	e.engineHandler.OnRoomJoined(
		res.Room,
		res.Participant,
		res.OtherParticipants,
		res.ServerInfo,
		res.SifTrailer,
	)

	e.signalTransport.Start()

	if e.signalling.PublishInJoin() {
		if publisher, ok := e.Publisher(); ok {
			e.pclock.Lock()
			pendingPublisherOffer := e.pendingPublisherOffer
			e.pendingPublisherOffer = webrtc.SessionDescription{}
			e.pclock.Unlock()

			if pendingPublisherOffer.SDP != "" {
				publisher.SetLocalOffer(pendingPublisherOffer)
			}
		}
	} else {
		// send offer
		if !res.SubscriberPrimary || res.FastPublish {
			if publisher, ok := e.Publisher(); ok {
				publisher.Negotiate()
			} else {
				e.log.Warnw("no publisher peer connection", ErrNoPeerConnection)
			}
		}
	}

	if isRestarting {
		e.engineHandler.OnRestarted(res.Room, res.Participant, res.OtherParticipants)
	}
	return nil
}

func (e *RTCEngine) OnReconnectResponse(res *livekit.ReconnectResponse) error {
	configuration := e.makeRTCConfiguration(res.IceServers, res.ClientConfiguration)

	e.pclock.Lock()
	defer e.pclock.Unlock()

	if e.publisher != nil {
		if err := e.publisher.SetConfiguration(configuration); err != nil {
			e.log.Errorw("could not set rtc configuration for publisher", err)
			return err
		}
	}

	if e.subscriber != nil {
		if err := e.subscriber.SetConfiguration(configuration); err != nil {
			e.log.Errorw("could not set rtc configuration for subscriber", err)
			return err
		}
	}

	return nil
}

func (e *RTCEngine) OnAnswer(sd webrtc.SessionDescription, answerId uint32, _midToTrackID map[string]string) {
	if e.closed.Load() {
		e.log.Debugw("ignoring SDP answer after closed")
		return
	}

	if err := e.publisher.SetRemoteDescription(sd); err != nil {
		e.log.Errorw("could not set remote description", err)
	} else {
		e.log.Debugw("successfully set publisher answer")
	}
}

func (e *RTCEngine) OnOffer(sd webrtc.SessionDescription, offerId uint32, _midToTrackID map[string]string) {
	if e.closed.Load() {
		e.log.Debugw("ignoring SDP offer after closed")
		return
	}

	e.log.Debugw("received offer for subscriber", "offer", sd, "offerId", offerId)
	if err := e.subscriber.SetRemoteDescription(sd); err != nil {
		e.log.Errorw("could not set remote description", err)
		return
	}
}

func (e *RTCEngine) OnTrickle(init webrtc.ICECandidateInit, target livekit.SignalTarget) {
	if e.closed.Load() {
		e.log.Debugw("ignoring trickle after closed")
		return
	}

	var err error
	e.log.Debugw("remote ICE candidate",
		"target", target,
		"candidate", init.Candidate,
	)
	switch target {
	case livekit.SignalTarget_PUBLISHER:
		err = e.publisher.AddICECandidate(init)
	case livekit.SignalTarget_SUBSCRIBER:
		err = e.subscriber.AddICECandidate(init)
	}
	if err != nil {
		e.log.Errorw("could not add ICE candidate", err)
	}
}

func (e *RTCEngine) OnParticipantUpdate(info []*livekit.ParticipantInfo) {
	e.engineHandler.OnParticipantUpdate(info)
}

func (e *RTCEngine) OnLocalTrackPublished(res *livekit.TrackPublishedResponse) {
	e.trackPublishedListenersLock.Lock()
	listener, ok := e.trackPublishedListeners[res.Cid]
	e.trackPublishedListenersLock.Unlock()

	if ok {
		listener <- res
	}
}

func (e *RTCEngine) OnLocalTrackUnpublished(res *livekit.TrackUnpublishedResponse) {
	e.engineHandler.OnLocalTrackUnpublished(res)
}

func (e *RTCEngine) OnSpeakersChanged(si []*livekit.SpeakerInfo) {
	e.engineHandler.OnSpeakersChanged(si)
}

func (e *RTCEngine) OnConnectionQuality(cqi []*livekit.ConnectionQualityInfo) {
	e.engineHandler.OnConnectionQuality(cqi)
}

func (e *RTCEngine) OnRoomUpdate(room *livekit.Room) {
	e.engineHandler.OnRoomUpdate(room)
}

func (e *RTCEngine) OnRoomMoved(moved *livekit.RoomMovedResponse) {
	e.engineHandler.OnRoomMoved(moved)
}

func (e *RTCEngine) OnTrackRemoteMuted(request *livekit.MuteTrackRequest) {
	e.engineHandler.OnTrackRemoteMuted(request)
}

func (e *RTCEngine) OnTokenRefresh(refreshToken string) {
	e.connectionManager.setToken(refreshToken)
}

func (e *RTCEngine) OnLeave(leave *livekit.LeaveRequest) {
	if leave.GetAction() == livekit.LeaveRequest_DISCONNECT {
		e.log.Debugw("received leave request", "leave", protoLogger.Proto(leave))
	} else {
		e.log.Infow("received leave request", "leave", protoLogger.Proto(leave))
	}

	switch leave.GetAction() {
	case livekit.LeaveRequest_DISCONNECT:
		e.Close()
		reason := leave.GetReason()
		e.log.Infow("server initiated leave", "reason", reason)
		e.engineHandler.OnDisconnected(reason)

	case livekit.LeaveRequest_RECONNECT:
		e.handleDisconnect("leave-reconnect", true, leave.GetRegions())

	case livekit.LeaveRequest_RESUME:
		e.handleDisconnect("leave-resume", false, leave.GetRegions())

	default:
	}
}

func (e *RTCEngine) OnLocalTrackSubscribed(trackSubscribed *livekit.TrackSubscribed) {
	e.engineHandler.OnLocalTrackSubscribed(trackSubscribed)
}

func (e *RTCEngine) OnSubscribedQualityUpdate(subscribedQualityUpdate *livekit.SubscribedQualityUpdate) {
	e.engineHandler.OnSubscribedQualityUpdate(subscribedQualityUpdate)
}

func (e *RTCEngine) OnSubscribedAudioCodecUpdate(subscribedAudioCodecUpdate *livekit.SubscribedAudioCodecUpdate) {
	e.engineHandler.OnSubscribedAudioCodecUpdate(subscribedAudioCodecUpdate)
}

func (e *RTCEngine) OnMediaSectionsRequirement(mediaSectionsRequirement *livekit.MediaSectionsRequirement) {
	e.engineHandler.OnMediaSectionsRequirement(mediaSectionsRequirement)
}

// ------------------------------------

func setConfiguration(pcTransport *PCTransport, configuration webrtc.Configuration) {
	if pcTransport != nil {
		pcTransport.SetConfiguration(configuration)
	}
}

func waitUntilConnected(d time.Duration, test func() bool) error {
	if test() {
		return nil
	}

	timeout := time.NewTimer(d)
	defer timeout.Stop()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout.C:
			return ErrConnectionTimeout

		case <-ticker.C:
			if test() {
				return nil
			}
		}
	}
}
