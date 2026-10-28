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

	"github.com/livekit/protocol/rpc"
	"github.com/livekit/sipgo/sip"
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
