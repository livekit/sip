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
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var _ SignalTransport = (*signalTransportWebSocket)(nil)

type SignalTransportWebSocketParams struct {
	Logger                 logger.Logger
	Version                string
	Protocol               int
	Signalling             Signalling
	SignalTransportHandler SignalTransportHandler
	SignalHandler          SignalHandler
}

type signalTransportWebSocket struct {
	signalTransportUnimplemented

	params SignalTransportWebSocketParams

	conn            atomic.Pointer[websocket.Conn]
	lock            sync.Mutex
	isStarted       atomic.Bool
	pendingResponse proto.Message // only for old servers which do not send ReconnectResponse
	readerClosedCh  chan struct{}
}

func NewSignalTransportWebSocket(params SignalTransportWebSocketParams) SignalTransport {
	return &signalTransportWebSocket{
		params: params,
	}
}

func (s *signalTransportWebSocket) SetLogger(l logger.Logger) {
	s.params.Logger = l
}

func (s *signalTransportWebSocket) Start() {
	if s.isStarted.Swap(true) {
		return
	}
	s.readerClosedCh = make(chan struct{})
	go s.readWorker(s.readerClosedCh)
}

func (s *signalTransportWebSocket) IsStarted() bool {
	return s.isStarted.Load()
}

func (s *signalTransportWebSocket) Close() {
	isStarted := s.IsStarted()
	readerClosedCh := s.readerClosedCh
	conn := s.websocketConn()
	if conn != nil {
		_ = conn.Close()
	}
	if isStarted && readerClosedCh != nil {
		<-readerClosedCh
	}
}

func (s *signalTransportWebSocket) Join(
	ctx context.Context,
	url string,
	token string,
	connectParams ConnectParams,
	addTrackRequests []*livekit.AddTrackRequest,
	publisherOffer webrtc.SessionDescription,
) error {
	msg, err := s.connect(ctx, url, token, connectParams, addTrackRequests, publisherOffer, "")
	if err != nil {
		return err
	}

	return s.params.SignalHandler.HandleMessage(msg)
}

// Reconnect starts a new WebSocket connection to the server, passing in reconnect=1
// when successful, it'll return a ReconnectResponse;
// older versions of the server will not send back a ReconnectResponse
func (s *signalTransportWebSocket) Reconnect(
	ctx context.Context,
	url string,
	token string,
	connectParams ConnectParams,
	participantSID string,
) error {
	connectParams.Reconnect = true
	msg, err := s.connect(
		ctx,
		url,
		token,
		connectParams,
		nil,
		webrtc.SessionDescription{},
		participantSID,
	)
	if err != nil {
		return err
	}

	res, ok := msg.(*livekit.SignalResponse)
	if !ok {
		s.params.Logger.Warnw(
			"unknown message type", nil,
			"messageType", fmt.Sprintf("%T", msg),
		)
		return ErrInvalidMessageType
	}
	s.params.Logger.Debugw("reconnect received response", "response", res.String())

	s.lock.Lock()
	s.pendingResponse = nil
	if res != nil {
		switch payload := res.Message.(type) {
		case *livekit.SignalResponse_Reconnect:
			s.lock.Unlock()
			return s.params.SignalHandler.HandleMessage(msg)

		case *livekit.SignalResponse_Leave:
			s.lock.Unlock()
			s.params.SignalHandler.HandleMessage(msg)
			return fmt.Errorf("reconnect received leave, reason: %s", payload.Leave.GetReason())
		}

		s.pendingResponse = res
	}
	s.lock.Unlock()

	return nil
}

func (s *signalTransportWebSocket) SendMessage(msg proto.Message) error {
	conn := s.websocketConn()
	if conn == nil {
		return errors.New("signal transport is not connected")
	}

	payload, err := proto.Marshal(msg)
	if err != nil {
		return err
	}

	s.lock.Lock()
	defer s.lock.Unlock()
	return conn.WriteMessage(websocket.BinaryMessage, payload)
}

func (s *signalTransportWebSocket) connect(
	ctx context.Context,
	urlPrefix string,
	token string,
	connectParams ConnectParams,
	addTrackRequests []*livekit.AddTrackRequest,
	publisherOffer webrtc.SessionDescription,
	participantSID string,
) (proto.Message, error) {
	if urlPrefix == "" {
		return nil, ErrURLNotProvided
	}
	urlPrefix = ToWebsocketURL(urlPrefix)

	queryParams, err := s.params.Signalling.ConnectQueryParams(
		s.params.Version,
		s.params.Protocol,
		&connectParams,
		addTrackRequests,
		publisherOffer,
		participantSID,
	)
	if err != nil {
		return nil, err
	}

	u, err := url.Parse(urlPrefix + s.params.Signalling.Path() + fmt.Sprintf("?%s", queryParams))
	if err != nil {
		return nil, err
	}

	header := NewHTTPHeaderWithToken(token)
	path := u.String()

	startedAt := time.Now()
	conn, hresp, err := websocket.DefaultDialer.DialContext(ctx, path, header)
	if err != nil {
		fields := []interface{}{
			"duration", time.Since(startedAt),
		}
		if hresp != nil {
			body, _ := io.ReadAll(hresp.Body)
			fields = append(fields, "status", hresp.StatusCode, "response", string(body))
		}
		s.params.Logger.Errorw("error establishing signal connection", err, fields...)

		if strings.HasSuffix(err.Error(), ":53: server misbehaving") {
			// DNS issue, abort
			return nil, ErrCannotDialSignal
		}

		return nil, err
	}

	s.Close() // close previous conn, if any
	s.conn.Store(conn)

	// server should send join as soon as connected
	res, err := s.readResponse()
	if err != nil {
		return nil, err
	}
	s.params.Logger.Debugw("first message received", "elapsed", time.Since(startedAt))

	return res, nil
}

func (s *signalTransportWebSocket) websocketConn() *websocket.Conn {
	return s.conn.Load()
}

func (s *signalTransportWebSocket) readWorker(readerClosedCh chan struct{}) {
	defer func() {
		s.isStarted.Store(false)
		s.conn.Store(nil)
		close(readerClosedCh)

		s.params.SignalTransportHandler.OnTransportClose()
	}()

	var pendingResponse proto.Message
	s.lock.Lock()
	pendingResponse = s.pendingResponse
	s.pendingResponse = nil
	s.lock.Unlock()
	if pendingResponse != nil {
		s.params.SignalHandler.HandleMessage(pendingResponse)
	}

	for {
		res, err := s.readResponse()
		if err != nil {
			if !isIgnoredWebsocketError(err) {
				s.params.Logger.Infow("error while reading from signal client", "error", err)
			}
			return
		}
		s.params.SignalHandler.HandleMessage(res)
	}
}

func (s *signalTransportWebSocket) readResponse() (proto.Message, error) {
	conn := s.websocketConn()
	if conn == nil {
		return nil, errors.New("cannot read response without signal transport")
	}

	// handle special messages and pass on the rest
	messageType, payload, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}

	msg := &livekit.SignalResponse{}
	switch messageType {
	case websocket.BinaryMessage:
		// protobuf encoded
		err := proto.Unmarshal(payload, msg)
		return msg, err

	case websocket.TextMessage:
		// json encoded
		err := protojson.Unmarshal(payload, msg)
		return msg, err

	default:
		return nil, nil
	}
}

// ----------------------------------

func isIgnoredWebsocketError(err error) bool {
	if err == nil ||
		err == io.EOF ||
		strings.Contains(err.Error(), "use of closed network connection") ||
		strings.Contains(err.Error(), "connection reset by peer") {
		return true
	}

	return websocket.IsCloseError(
		err,
		websocket.CloseAbnormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseNormalClosure,
		websocket.CloseNoStatusReceived,
	)
}
