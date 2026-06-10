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
	"errors"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/guid"
	"github.com/livekit/psrpc"
)

// StateUpdater is the upstream RPC surface CallState's default StateHandler
// forwards to.
type StateUpdater interface {
	UpdateSIPCallState(ctx context.Context, req *rpc.UpdateSIPCallStateRequest, opts ...psrpc.RequestOption) (*emptypb.Empty, error)
	RecordCallContext(ctx context.Context, req *rpc.RecordCallContextRequest, opts ...psrpc.RequestOption) (*emptypb.Empty, error)
}

// StateHandler receives outgoing CallState changes. CallState invokes the
// handler whenever the proto needs to be sent upstream (HandleUpdate) or a
// transfer transitions (HandleTransfer). Implementations forward to an RPC
// sink, drive in-process observability, or both.
//
// Methods are called while CallState holds its internal mutex. Implementations
// must not call back into the same CallState. The supplied protos remain owned
// by CallState; implementations that retain them past the call must clone.
//
// Resend / retry semantics belong to the implementation — CallState clears
// dirty after every call regardless of upstream outcome.
type StateHandler interface {
	HandleUpdate(info *livekit.SIPCallInfo)
	HandleTransfer(ti *livekit.SIPTransferInfo)
	HandleCallContextRecorded(info *livekit.SIPCallInfo)
}

// NewRPCStateHandler returns a StateHandler that forwards updates to a
// StateUpdater. nil cli yields a no-op handler — useful for tests that don't
// care about the upstream sink.
func NewRPCStateHandler(cli StateUpdater) StateHandler {
	return &rpcStateHandler{cli: cli}
}

type rpcStateHandler struct{ cli StateUpdater }

func (h *rpcStateHandler) HandleUpdate(info *livekit.SIPCallInfo) {
	if h.cli == nil {
		return
	}
	_, _ = h.cli.UpdateSIPCallState(context.Background(), &rpc.UpdateSIPCallStateRequest{CallInfo: info})
}

func (h *rpcStateHandler) HandleTransfer(ti *livekit.SIPTransferInfo) {
	if h.cli == nil {
		return
	}
	_, _ = h.cli.UpdateSIPCallState(context.Background(), &rpc.UpdateSIPCallStateRequest{TransferInfo: ti})
}

func (h *rpcStateHandler) HandleCallContextRecorded(info *livekit.SIPCallInfo) {
	if h.cli == nil {
		return
	}
	_, _ = h.cli.RecordCallContext(context.Background(), &rpc.RecordCallContextRequest{CallInfo: info})
}

func NewCallState(handler StateHandler, initial *livekit.SIPCallInfo) *CallState {
	if initial == nil {
		initial = &livekit.SIPCallInfo{}
	} else {
		initial = proto.CloneOf(initial)
	}
	return &CallState{
		handler:       handler,
		callInfo:      initial,
		transferInfos: make(map[string]*livekit.SIPTransferInfo),
		dirty:         true,
	}
}

type CallState struct {
	mu            sync.Mutex
	handler       StateHandler
	callInfo      *livekit.SIPCallInfo
	transferInfos map[string]*livekit.SIPTransferInfo
	dirty         bool
}

func (s *CallState) Info() *livekit.SIPCallInfo {
	if s == nil {
		return nil
	}
	return s.callInfo
}

func (s *CallState) CloneInfo() *livekit.SIPCallInfo {
	if s == nil {
		return nil
	}
	return proto.CloneOf(s.callInfo)
}

func (s *CallState) DeferUpdate(update func(info *livekit.SIPCallInfo)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirty = true
	update(s.callInfo)
}

func (s *CallState) Update(update func(info *livekit.SIPCallInfo)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirty = true
	update(s.callInfo)
	s.flushLocked()
}

func (s *CallState) StartTransfer(transferTo string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ti := &livekit.SIPTransferInfo{
		TransferId:            guid.New(utils.SIPTransferPrefix),
		CallId:                s.callInfo.CallId,
		TransferTo:            transferTo,
		TransferInitiatedAtNs: time.Now().UnixNano(),
		TransferStatus:        livekit.SIPTransferStatus_STS_TRANSFER_ONGOING,
	}
	s.handler.HandleTransfer(ti)
	s.transferInfos[ti.TransferId] = ti
	return ti.TransferId
}

func (s *CallState) EndTransfer(transferID string, inErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ti := s.transferInfos[transferID]
	delete(s.transferInfos, transferID)
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
	s.handler.HandleTransfer(ti)
}

// RecordCallContext appends late-arriving context to the canonical callInfo
// (e.g. PCAP links published after the call has ended) and signals the
// handler that the post-call context has been recorded. Does not touch the
// dirty bit: this is a terminal post-call signal, not a regular flush.
func (s *CallState) RecordCallContext(appendInfo func(info *livekit.SIPCallInfo)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if appendInfo != nil {
		appendInfo(s.callInfo)
	}
	s.handler.HandleCallContextRecorded(s.callInfo)
}

func (s *CallState) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return
	}
	s.flushLocked()
}

func (s *CallState) ForceFlush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
}

// flushLocked sends the current callInfo through the handler. Caller must
// hold s.mu. Retry/resend semantics live in the handler; CallState clears
// dirty unconditionally.
func (s *CallState) flushLocked() {
	s.handler.HandleUpdate(s.callInfo)
	s.dirty = false
}
