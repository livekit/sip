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
	"github.com/livekit/protocol/logger"
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

	log := logger.GetLogger().WithValues(
		"callID", s.callInfo.CallId,
		"callStatus", s.callInfo.CallStatus.String(),
		"transferID", ti.TransferId,
		"transferTo", transferTo,
		"transferStatus", ti.TransferStatus.String(),
	)
	if s.callInfo.StartedAtNs != 0 {
		log = log.WithValues("startedAtNs", s.callInfo.StartedAtNs)
	}
	if s.callInfo.EndedAtNs != 0 {
		log = log.WithValues("endedAtNs", s.callInfo.EndedAtNs)
	}
	log.Infow("calling UpdateSIPCallState for transfer start")

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
	callStatus := s.callInfo.CallStatus
	startedAtNs := s.callInfo.StartedAtNs
	endedAtNs := s.callInfo.EndedAtNs
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

	log := logger.GetLogger().WithValues(
		"callID", ti.CallId,
		"callStatus", callStatus.String(),
		"transferID", ti.TransferId,
		"transferStatus", ti.TransferStatus.String(),
		"hasError", inErr != nil,
	)
	if startedAtNs != 0 {
		log = log.WithValues("startedAtNs", startedAtNs)
	}
	if endedAtNs != 0 {
		log = log.WithValues("endedAtNs", endedAtNs)
	}
	if inErr != nil {
		log = log.WithValues("error", inErr.Error())
	}
	log.Infow("calling UpdateSIPCallState for transfer end")

	s.cli.UpdateSIPCallState(ctx, req)
}

func (s *CallState) flush(ctx context.Context) {
	if s.cli == nil {
		s.dirty = false
		return
	}
	req := &rpc.UpdateSIPCallStateRequest{
		CallInfo: s.callInfo,
	}
	log := logger.GetLogger().WithValues(
		"callID", s.callInfo.CallId,
		"callStatus", s.callInfo.CallStatus.String(),
	)
	if s.callInfo.RoomId != "" {
		log = log.WithValues("roomID", s.callInfo.RoomId)
	}
	if s.callInfo.StartedAtNs != 0 {
		log = log.WithValues("startedAtNs", s.callInfo.StartedAtNs)
	}
	if s.callInfo.EndedAtNs != 0 {
		log = log.WithValues("endedAtNs", s.callInfo.EndedAtNs)
	}
	log.Infow("calling UpdateSIPCallState for call state update")

	_, err := s.cli.UpdateSIPCallState(context.WithoutCancel(ctx), req)
	if err == nil {
		s.dirty = false
	}
}

func (s *CallState) Flush(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return
	}
	s.flush(ctx)
}

func (s *CallState) ForceFlush(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flush(ctx)
}
