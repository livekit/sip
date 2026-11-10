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
	"errors"
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
	"github.com/livekit/sip/pkg/sealer"
	"github.com/livekit/sip/pkg/stats"
)

// CallManager coordinates the lifecycle of calls (signalling, media, teardown,
// etc.)
type CallManager interface {
	// Lifecycle
	Start(tr SIPTransport, cli *Client, sconf *ServiceConfig)
	SetHandler(handler Handler)
	ActiveCalls() ActiveCalls
	GetCallForwardingInfo(callID string, headers []string) (*CallForwardingInfo, error)
	Stop()

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

// SIPTransport can send SIP messages.
type SIPTransport interface {
	transportLayer() *transport.Layer
	transactionLayer() *transaction.Layer
}

type callManager struct {
	log             logger.Logger
	mon             *stats.Monitor
	region          string
	tr              SIPTransport
	getStateHandler GetStateHandler
	getRoom         GetRoomFunc
	sealer          sealer.Sealer

	closing            core.Fuse
	cmu                sync.RWMutex
	byLocalTag         map[LocalTag]*inboundCall
	activeCalls        map[LocalTag]*outboundCall
	provisionalInvites *expirable.LRU[[2]string, LocalTag]
	rejectedInvites    *expirable.LRU[[2]string, rejectedInviteResponse]

	infos struct {
		sync.Mutex
		byLocalTag *expirable.LRU[LocalTag, *inboundCallInfo]
	}

	handler Handler
	conf    *config.Config
	sconf   *ServiceConfig

	cli *Client

	res mediaRes

	mu               sync.Mutex // protects pendingTransfers
	pendingTransfers map[LocalTag]*PendingTransfer
}

var _ CallManager = (*callManager)(nil)

type CallManagerOption func(s *callManager)

func WithGetRoom(fn GetRoomFunc) CallManagerOption {
	return func(cm *callManager) {
		if fn != nil {
			cm.getRoom = fn
		}
	}
}

// NewCallManager returns the default CallManager.
func NewCallManager(region string, conf *config.Config, log logger.Logger, mon *stats.Monitor, sealer sealer.Sealer, getStateHandler GetStateHandler, options ...CallManagerOption) (CallManager, error) {
	if sealer == nil {
		return nil, errors.New("no sealer provided")
	}
	return newCallManager(nil, nil, region, conf, nil, log, mon, sealer, getStateHandler, options...), nil
}

func newCallManager(tr SIPTransport, cli *Client, region string, conf *config.Config, sconf *ServiceConfig, log logger.Logger, mon *stats.Monitor, sealer sealer.Sealer, getStateHandler GetStateHandler, options ...CallManagerOption) *callManager {
	if log == nil {
		log = logger.GetLogger()
	}
	s := &callManager{
		log:                log,
		mon:                mon,
		region:             region,
		tr:                 tr,
		cli:                cli,
		conf:               conf,
		sconf:              sconf,
		getStateHandler:    getStateHandler,
		sealer:             sealer,
		getRoom:            DefaultGetRoomFunc,
		byLocalTag:         make(map[LocalTag]*inboundCall),
		activeCalls:        make(map[LocalTag]*outboundCall),
		pendingTransfers:   make(map[LocalTag]*PendingTransfer),
		provisionalInvites: expirable.NewLRU[[2]string, LocalTag](maxCallCache, nil, callCacheTTL),
	}
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

func (cm *callManager) Start(tr SIPTransport, cli *Client, sconf *ServiceConfig) {
	cm.tr = tr
	cm.cli = cli
	cm.sconf = sconf
}

func (cm *callManager) SetHandler(handler Handler) {
	cm.handler = handler
}

func (cm *callManager) ContactURI(tr Transport) URI {
	return getContactURI(cm.conf, cm.sconf.SignalingIP, tr)
}

func (cm *callManager) ActiveCalls() ActiveCalls {
	cm.cmu.RLock()
	outboundSamples, outboundTotal := sampleMap(5, cm.activeCalls, func(v *outboundCall) string {
		if v == nil || v.cc == nil {
			return "<nil>"
		}
		return string(v.cc.id)
	})

	inboundSamples, inboundTotal := sampleMap(5, cm.byLocalTag, func(v *inboundCall) string {
		if v == nil || v.cc == nil {
			return "<nil>"
		}
		return string(v.cc.id)
	})
	cm.cmu.RUnlock()

	return ActiveCalls{
		Inbound:   inboundTotal,
		Outbound:  outboundTotal,
		SampleIDs: append(outboundSamples, inboundSamples...),
	}
}

// Stop shuts down all active calls.
func (cm *callManager) Stop() {
	ctx := context.Background()
	cm.closing.Break()
	cm.cmu.Lock()
	outCalls := maps.Values(cm.activeCalls)
	cm.activeCalls = make(map[LocalTag]*outboundCall)
	calls := maps.Values(cm.byLocalTag)
	cm.byLocalTag = make(map[LocalTag]*inboundCall)
	cm.cmu.Unlock()
	for _, c := range outCalls {
		c.Close(ctx)
	}
	for _, c := range calls {
		c.Shutdown(ctx)
	}
}
