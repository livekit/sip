// Copyright 2025 LiveKit, Inc.
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
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/guid"
	"github.com/livekit/psrpc"
	"github.com/pkg/errors"
)

type StateUpdater interface {
	UpdateSIPCallState(ctx context.Context, req *rpc.UpdateSIPCallStateRequest, opts ...psrpc.RequestOption) (*emptypb.Empty, error)
}

func NewCallState(cli StateUpdater, initial *livekit.SIPCallInfo) *CallState {
	if initial == nil {
		initial = &livekit.SIPCallInfo{}
	}
	s := &CallState{
		cli:           cli,
		callInfo:      initial,
		transferInfos: make(map[string]*livekit.SIPTransferInfo),
		dirty:         true,
	}
	return s
}

type CallState struct {
	mu            sync.Mutex
	cli           StateUpdater
	callInfo      *livekit.SIPCallInfo
	transferInfos map[string]*livekit.SIPTransferInfo
	dirty         bool
}

func (s *CallState) DeferUpdate(update func(info *livekit.SIPCallInfo)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirty = true
	update(s.callInfo)
}

func (s *CallState) Update(ctx context.Context, update func(info *livekit.SIPCallInfo)) {
	ctx, span := Tracer.Start(ctx, "sip.CallState.Update")
	defer span.End()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirty = true
	update(s.callInfo)
	s.flush(ctx)
}

func (s *CallState) StartTransfer(ctx context.Context, transferTo string) string {
	ti := &livekit.SIPTransferInfo{
		TransferId:            guid.New(utils.SIPTransferPrefix),
		CallId:                s.callInfo.CallId,
		TransferTo:            transferTo,
		TransferInitiatedAtNs: time.Now().UnixNano(),
		TransferStatus:        livekit.SIPTransferStatus_STS_TRANSFER_ONGOING,
	}

	req := &rpc.UpdateSIPCallStateRequest{
		TransferInfo: ti,
	}

	s.cli.UpdateSIPCallState(ctx, req)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.transferInfos[ti.TransferId] = ti

	return ti.TransferId
}

func (s *CallState) EndTransfer(ctx context.Context, transferID string, inErr error) {
	s.mu.Lock()
	ti := s.transferInfos[transferID]
	delete(s.transferInfos, transferID)
	s.mu.Unlock()

	if ti == nil {
		return
	}

	ti.TransferCompletedAtNs = time.Now().UnixNano()
	if inErr != nil {
		ti.Error = inErr.Error()
		ti.TransferStatus = livekit.SIPTransferStatus_STS_TRANSFER_FAILED
	} else {
		ti.TransferStatus = livekit.SIPTransferStatus_STS_TRANSFER_SUCCESSFUL
	}

	var sipStatus *livekit.SIPStatus
	if errors.As(inErr, &sipStatus) {
		ti.TransferStatusCode = sipStatus
	}

	req := &rpc.UpdateSIPCallStateRequest{
		TransferInfo: ti,
	}

	s.cli.UpdateSIPCallState(ctx, req)

	return
}

func (s *CallState) flush(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	if s.cli == nil {
		s.dirty = false
		return
	}
	_, err := s.cli.UpdateSIPCallState(ctx, &rpc.UpdateSIPCallStateRequest{
		CallInfo: s.callInfo,
	})
	if err == nil {
		s.dirty = false
	}
}

func (s *CallState) Flush(ctx context.Context) {
	ctx, span := Tracer.Start(ctx, "sip.CallState.Flush")
	defer span.End()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return
	}
	s.flush(ctx)
}

func (s *CallState) ForceFlush(ctx context.Context) {
	ctx, span := Tracer.Start(ctx, "sip.CallState.ForceFlush")
	defer span.End()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flush(ctx)
}
