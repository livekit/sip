// Copyright 2026 LiveKit, Inc.
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
	"log/slog"
	"sync"

	"github.com/frostbyte73/core"
	"github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/exp/maps"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/sipgo/sip"
	"github.com/livekit/sipgo/transaction"
	"github.com/livekit/sipgo/transport"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/stats"
)

// CallManager coordinates the lifecycle of calls (signalling, media, teardown,
// etc.)
type CallManager interface {
	// SIP Handlers
	HandleInvite(log *slog.Logger, req *sip.Request, tx sip.ServerTransaction)
	HandleAck(log *slog.Logger, req *sip.Request, tx sip.ServerTransaction)
	HandleBye(log *slog.Logger, req *sip.Request, tx sip.ServerTransaction)
	HandleNotify(log *slog.Logger, req *sip.Request, tx sip.ServerTransaction)
	HandleOptions(log *slog.Logger, req *sip.Request, tx sip.ServerTransaction)
	HandleNoRoute(log *slog.Logger, req *sip.Request, tx sip.ServerTransaction)

	// RPC Handlers
	CreateSIPParticipant(ctx context.Context, req *rpc.InternalCreateSIPParticipantRequest) (*rpc.InternalCreateSIPParticipantResponse, error)
	TransferSIPParticipant(ctx context.Context, req *rpc.InternalTransferSIPParticipantRequest) (*rpc.InternalTransferSIPParticipantResponse, error)
}

// sipTransport is the part of Server that calls use to send SIP messages.
type sipTransport interface {
	transportLayer() *transport.Layer
	transactionLayer() *transaction.Layer
}

type callManager struct {
	log             logger.Logger
	mon             *stats.Monitor
	region          string
	tr              sipTransport
	getStateHandler GetStateHandler
	getRoom         GetRoomFunc
	sipUnhandled    RequestHandler

	imu               sync.Mutex
	inProgressInvites []*inProgressInvite

	closing            core.Fuse
	cmu                sync.RWMutex
	byLocalTag         map[LocalTag]*inboundCall
	provisionalInvites *expirable.LRU[[2]string, LocalTag]
	rejectedInvites    *expirable.LRU[[2]string, rejectedInviteResponse]

	infos struct {
		sync.Mutex
		byLocalTag *expirable.LRU[LocalTag, *inboundCallInfo]
	}

	handler Handler
	conf    *config.Config
	sconf   *ServiceConfig

	cli *Client // optional, for outbound reinvite handling

	res mediaRes
}

var _ CallManager = (*callManager)(nil)

type CallManagerOption func(s *callManager)

func WithGetRoomServer(fn GetRoomFunc) CallManagerOption {
	return func(s *callManager) {
		if fn != nil {
			s.getRoom = fn
		}
	}
}

// WithClient sets the Client that handles outbound calls. In-dialog requests
// for calls the manager doesn't know about are passed to it.
func WithClient(cli *Client) CallManagerOption {
	return func(s *callManager) {
		s.cli = cli
		s.sipUnhandled = cli.OnRequest
	}
}

func newCallManager(tr sipTransport, region string, conf *config.Config, sconf *ServiceConfig, log logger.Logger, mon *stats.Monitor, getStateHandler GetStateHandler, options ...CallManagerOption) *callManager {
	if log == nil {
		log = logger.GetLogger()
	}
	s := &callManager{
		log:                log,
		mon:                mon,
		region:             region,
		tr:                 tr,
		conf:               conf,
		sconf:              sconf,
		getStateHandler:    getStateHandler,
		getRoom:            DefaultGetRoomFunc,
		byLocalTag:         make(map[LocalTag]*inboundCall),
		provisionalInvites: expirable.NewLRU[[2]string, LocalTag](maxCallCache, nil, callCacheTTL),
	}
	// Initialize the rejected-invite replay cache unless explicitly disabled.
	if !conf.DisableRejectedInviteCache {
		s.rejectedInvites = expirable.NewLRU[[2]string, rejectedInviteResponse](maxCallCache, nil, callCacheTTL)
	}
	for _, option := range options {
		option(s)
	}
	s.infos.byLocalTag = expirable.NewLRU[LocalTag, *inboundCallInfo](maxCallCache, nil, callCacheTTL)
	s.initMediaRes()
	return s
}

func (s *callManager) SetHandler(handler Handler) {
	s.handler = handler
}

func (s *callManager) ContactURI(tr Transport) URI {
	return getContactURI(s.conf, s.sconf.SignalingIP, tr)
}

// stop shuts down all active calls.
func (s *callManager) stop() {
	ctx := context.Background()
	s.closing.Break()
	s.cmu.Lock()
	calls := maps.Values(s.byLocalTag)
	s.byLocalTag = make(map[LocalTag]*inboundCall)
	s.cmu.Unlock()
	for _, c := range calls {
		c.Shutdown(ctx)
	}
}

func (s *callManager) CreateSIPParticipant(ctx context.Context, req *rpc.InternalCreateSIPParticipantRequest) (*rpc.InternalCreateSIPParticipantResponse, error) {
	panic("not yet implemented")
}

func (s *callManager) TransferSIPParticipant(ctx context.Context, req *rpc.InternalTransferSIPParticipantRequest) (*rpc.InternalTransferSIPParticipantResponse, error) {
	panic("not yet implemented")
}
