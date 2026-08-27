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
	"net/http"
	"time"

	"github.com/twitchtv/twirp"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/xtwirp"
	"github.com/livekit/server-sdk-go/v2/signalling"
)

// dialDeadline returns how long a phone-dialing call (SIP or WhatsApp) may run:
// the ring window plus a margin, so the request doesn't abort before the call can
// be answered. The request's ringing_timeout is used when set; otherwise it falls
// back to defaultRingingTimeout (the server default), which also gets the margin.
func dialDeadline(ringingTimeout *durationpb.Duration) time.Duration {
	ring := defaultRingingTimeout
	if ringingTimeout != nil {
		ring = ringingTimeout.AsDuration()
	}
	return ring + ringingTimeoutMargin
}

// dialContext returns a context whose deadline is long enough to outlast ringing
// for a call that waits for an answer. It uses the longer of the caller's own
// deadline and the dial budget (dialDeadline); when the caller's deadline is
// shorter or absent it detaches that deadline — still forwarding explicit
// cancellation — so a too-short deadline can't abort the request before the call
// is answered. Call before prepareContext so failover sees the budget.
func dialContext(ctx context.Context, ringingTimeout *durationpb.Duration) (context.Context, context.CancelFunc) {
	budget := dialDeadline(ringingTimeout)
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) >= budget {
		return ctx, func() {} // the caller's deadline already outlasts ringing
	}
	return context.WithTimeout(detachDeadline(ctx), budget)
}

// pinRingingTimeout returns ringingTimeout when set, else the default ring window
// (defaultRingingTimeout). Callers set it back on the request so the ring window
// sent to the server — which the dial deadline is derived from — is explicit and
// doesn't depend on the server's default (which could change).
func pinRingingTimeout(ringingTimeout *durationpb.Duration) *durationpb.Duration {
	if ringingTimeout == nil {
		return durationpb.New(defaultRingingTimeout)
	}
	return ringingTimeout
}

//lint:file-ignore SA1019 We still support some deprecated functions for backward compatibility

type SIPClient struct {
	sipClient livekit.SIP
	authBase
}

// NewSIPClient creates a LiveKit SIP client.
func NewSIPClient(url string, apiKey string, secretKey string, opts ...twirp.ClientOption) *SIPClient {
	return newSIPClient(url, authBase{apiKey: apiKey, apiSecret: secretKey}, newAPIHTTPClient(), opts...)
}

func newSIPClient(url string, auth authBase, httpClient *http.Client, opts ...twirp.ClientOption) *SIPClient {
	opts = append(opts, xtwirp.DefaultClientOptions()...)
	return &SIPClient{
		sipClient: livekit.NewSIPProtobufClient(signalling.ToHttpURL(url), httpClient, opts...),
		authBase:  auth,
	}
}

// CreateSIPInboundTrunk creates a new SIP Trunk for accepting inbound calls to LiveKit.
func (s *SIPClient) CreateSIPInboundTrunk(ctx context.Context, in *livekit.CreateSIPInboundTrunkRequest) (*livekit.SIPInboundTrunkInfo, error) {
	if in == nil || in.Trunk == nil || in.Trunk.SipTrunkId != "" {
		return nil, ErrInvalidParameter
	}

	ctx, err := s.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}
	return s.sipClient.CreateSIPInboundTrunk(ctx, in)
}

// CreateSIPOutboundTrunk creates a new SIP Trunk for making outbound calls from LiveKit.
func (s *SIPClient) CreateSIPOutboundTrunk(ctx context.Context, in *livekit.CreateSIPOutboundTrunkRequest) (*livekit.SIPOutboundTrunkInfo, error) {
	if in == nil || in.Trunk == nil || in.Trunk.SipTrunkId != "" {
		return nil, ErrInvalidParameter
	}

	ctx, err := s.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}
	return s.sipClient.CreateSIPOutboundTrunk(ctx, in)
}

// UpdateSIPInboundTrunk updates an existing SIP Inbound Trunk.
func (s *SIPClient) UpdateSIPInboundTrunk(ctx context.Context, in *livekit.UpdateSIPInboundTrunkRequest) (*livekit.SIPInboundTrunkInfo, error) {
	if in == nil || in.Action == nil || in.SipTrunkId == "" {
		return nil, ErrInvalidParameter
	}

	ctx, err := s.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}
	return s.sipClient.UpdateSIPInboundTrunk(ctx, in)
}

// UpdateSIPOutboundTrunk updates an existing SIP Outbound Trunk.
func (s *SIPClient) UpdateSIPOutboundTrunk(ctx context.Context, in *livekit.UpdateSIPOutboundTrunkRequest) (*livekit.SIPOutboundTrunkInfo, error) {
	if in == nil || in.Action == nil || in.SipTrunkId == "" {
		return nil, ErrInvalidParameter
	}

	ctx, err := s.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}
	return s.sipClient.UpdateSIPOutboundTrunk(ctx, in)
}

// GetSIPInboundTrunksByIDs gets SIP Inbound Trunks by ID.
// Returned slice is in the same order as the IDs. Missing IDs will have nil in the corresponding position.
func (s *SIPClient) GetSIPInboundTrunksByIDs(ctx context.Context, ids []string) ([]*livekit.SIPInboundTrunkInfo, error) {
	if len(ids) == 0 {
		return nil, ErrInvalidParameter
	}

	ctx, err := s.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}
	req := &livekit.ListSIPInboundTrunkRequest{
		TrunkIds: ids,
	}
	resp, err := s.ListSIPInboundTrunk(ctx, req)
	if err != nil {
		return nil, err
	}
	// Client-side filtering, in case SDK is newer than the server.
	return req.FilterSlice(resp.Items), nil
}

// GetSIPOutboundTrunksByIDs gets SIP Outbound Trunks by ID.
// Returned slice is in the same order as the IDs. Missing IDs will have nil in the corresponding position.
func (s *SIPClient) GetSIPOutboundTrunksByIDs(ctx context.Context, ids []string) ([]*livekit.SIPOutboundTrunkInfo, error) {
	if len(ids) == 0 {
		return nil, ErrInvalidParameter
	}

	ctx, err := s.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}
	req := &livekit.ListSIPOutboundTrunkRequest{
		TrunkIds: ids,
	}
	resp, err := s.ListSIPOutboundTrunk(ctx, req)
	if err != nil {
		return nil, err
	}
	// Client-side filtering, in case SDK is newer than the server.
	return req.FilterSlice(resp.Items), nil
}

// ListSIPTrunk lists SIP Trunks.
//
// Deprecated: Use ListSIPInboundTrunk or ListSIPOutboundTrunk
func (s *SIPClient) ListSIPTrunk(ctx context.Context, in *livekit.ListSIPTrunkRequest) (*livekit.ListSIPTrunkResponse, error) {
	if in == nil {
		return nil, ErrInvalidParameter
	}

	ctx, err := s.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}
	return s.sipClient.ListSIPTrunk(ctx, in)
}

// ListSIPInboundTrunk lists SIP Trunks accepting inbound calls.
func (s *SIPClient) ListSIPInboundTrunk(ctx context.Context, in *livekit.ListSIPInboundTrunkRequest) (*livekit.ListSIPInboundTrunkResponse, error) {
	if in == nil {
		return nil, ErrInvalidParameter
	}

	ctx, err := s.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}
	return s.sipClient.ListSIPInboundTrunk(ctx, in)
}

// ListSIPOutboundTrunk lists SIP Trunks for making outbound calls.
func (s *SIPClient) ListSIPOutboundTrunk(ctx context.Context, in *livekit.ListSIPOutboundTrunkRequest) (*livekit.ListSIPOutboundTrunkResponse, error) {
	if in == nil {
		return nil, ErrInvalidParameter
	}

	ctx, err := s.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}
	return s.sipClient.ListSIPOutboundTrunk(ctx, in)
}

// DeleteSIPTrunk deletes SIP Trunk given an ID.
func (s *SIPClient) DeleteSIPTrunk(ctx context.Context, in *livekit.DeleteSIPTrunkRequest) (*livekit.SIPTrunkInfo, error) {
	if in == nil {
		return nil, ErrInvalidParameter
	}

	ctx, err := s.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}
	return s.sipClient.DeleteSIPTrunk(ctx, in)
}

// CreateSIPDispatchRule creates SIP Dispatch Rules.
func (s *SIPClient) CreateSIPDispatchRule(ctx context.Context, in *livekit.CreateSIPDispatchRuleRequest) (*livekit.SIPDispatchRuleInfo, error) {
	if in == nil {
		return nil, ErrInvalidParameter
	}

	ctx, err := s.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}
	return s.sipClient.CreateSIPDispatchRule(ctx, in)
}

// UpdateSIPDispatchRule updates an existing SIP Dispatch Rule.
func (s *SIPClient) UpdateSIPDispatchRule(ctx context.Context, in *livekit.UpdateSIPDispatchRuleRequest) (*livekit.SIPDispatchRuleInfo, error) {
	if in == nil || in.Action == nil || in.SipDispatchRuleId == "" {
		return nil, ErrInvalidParameter
	}

	ctx, err := s.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}
	return s.sipClient.UpdateSIPDispatchRule(ctx, in)
}

// GetSIPDispatchRulesByIDs gets SIP Dispatch Rules by ID.
// Returned slice is in the same order as the IDs. Missing IDs will have nil in the corresponding position.
func (s *SIPClient) GetSIPDispatchRulesByIDs(ctx context.Context, ids []string) ([]*livekit.SIPDispatchRuleInfo, error) {
	if len(ids) == 0 {
		return nil, ErrInvalidParameter
	}

	ctx, err := s.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}
	req := &livekit.ListSIPDispatchRuleRequest{
		DispatchRuleIds: ids,
	}
	resp, err := s.ListSIPDispatchRule(ctx, req)
	if err != nil {
		return nil, err
	}
	// Client-side filtering, in case SDK is newer than the server.
	return req.FilterSlice(resp.Items), nil
}

// ListSIPDispatchRule lists SIP Dispatch Rules.
func (s *SIPClient) ListSIPDispatchRule(ctx context.Context, in *livekit.ListSIPDispatchRuleRequest) (*livekit.ListSIPDispatchRuleResponse, error) {
	if in == nil {
		return nil, ErrInvalidParameter
	}

	ctx, err := s.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}
	return s.sipClient.ListSIPDispatchRule(ctx, in)
}

// DeleteSIPDispatchRule deletes SIP Dispatch Rule given an ID.
func (s *SIPClient) DeleteSIPDispatchRule(ctx context.Context, in *livekit.DeleteSIPDispatchRuleRequest) (*livekit.SIPDispatchRuleInfo, error) {
	if in == nil {
		return nil, ErrInvalidParameter
	}

	ctx, err := s.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}
	return s.sipClient.DeleteSIPDispatchRule(ctx, in)
}

// CreateSIPParticipant creates SIP Participant by making an outbound call.
func (s *SIPClient) CreateSIPParticipant(ctx context.Context, in *livekit.CreateSIPParticipantRequest) (*livekit.SIPParticipantInfo, error) {
	if in == nil {
		return nil, ErrInvalidParameter
	}

	// Dialing a phone and waiting for an answer takes longer than a normal
	// request and must outlast ringing, so give it a long-enough deadline even if
	// the caller's is shorter (or absent). Pin the ring window explicitly so the
	// deadline doesn't depend on the server's default (which could change).
	if in.WaitUntilAnswered {
		in.RingingTimeout = pinRingingTimeout(in.RingingTimeout)
		var cancel context.CancelFunc
		ctx, cancel = dialContext(ctx, in.RingingTimeout)
		defer cancel()
	}

	ctx, err := s.prepareContext(ctx, withSIPGrant{Call: true})
	if err != nil {
		return nil, err
	}
	return s.sipClient.CreateSIPParticipant(ctx, in)
}

// TransferSIPParticipant transfer an existing SIP participant to an outside SIP endpoint.
func (s *SIPClient) TransferSIPParticipant(ctx context.Context, in *livekit.TransferSIPParticipantRequest) (*livekit.TransferSIPParticipantResponse, error) {
	if in == nil {
		return nil, ErrInvalidParameter
	}

	// Transferring a call dials a phone and must outlast ringing, so give it a
	// long-enough deadline even if the caller's is shorter (or absent). Pin the
	// ring window explicitly so the deadline doesn't depend on the server default.
	in.RingingTimeout = pinRingingTimeout(in.RingingTimeout)
	var cancel context.CancelFunc
	ctx, cancel = dialContext(ctx, in.RingingTimeout)
	defer cancel()

	ctx, err := s.prepareContext(ctx, withSIPGrant{Call: true}, withVideoGrant{RoomAdmin: true, Room: in.RoomName})
	if err != nil {
		return nil, err
	}
	return s.sipClient.TransferSIPParticipant(ctx, in)
}

// SIPStatusFrom unwraps an error and returns associated SIP call status, if any.
func SIPStatusFrom(err error) *livekit.SIPStatus {
	return livekit.SIPStatusFrom(err)
}
