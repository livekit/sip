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

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
)

type StateUpdater interface {
	UpdateSIPCallState(ctx context.Context, req *rpc.UpdateSIPCallStateRequest, opts ...psrpc.RequestOption) (*emptypb.Empty, error)
}

func NewCallState(cli StateUpdater, initial *livekit.SIPCallInfo) *CallState {
	if initial == nil {
		initial = &livekit.SIPCallInfo{}
	}
	s := &CallState{
		cli:   cli,
		info:  initial,
		dirty: true,
	}
	return s
}

type CallState struct {
	mu    sync.Mutex
	cli   StateUpdater
	info  *livekit.SIPCallInfo
	dirty bool
}

func (s *CallState) DeferUpdate(update func(info *livekit.SIPCallInfo)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirty = true
	update(s.info)
}

func (s *CallState) Update(ctx context.Context, update func(info *livekit.SIPCallInfo)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirty = true
	update(s.info)
	s.flush(ctx)
}

func (s *CallState) flush(ctx context.Context) {
	if s.cli == nil {
		s.dirty = false
		return
	}
	_, err := s.cli.UpdateSIPCallState(context.WithoutCancel(ctx), &rpc.UpdateSIPCallStateRequest{
		CallInfo: s.info,
	})
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
