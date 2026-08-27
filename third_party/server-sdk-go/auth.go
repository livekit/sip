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

	"github.com/google/uuid"
	"github.com/twitchtv/twirp"

	"github.com/livekit/protocol/auth"

	"github.com/livekit/server-sdk-go/v2/signalling"
)

// requestIDHeader carries a per-request idempotency key. SDK auto-retries (see failoverTransport) will
// keep the same key across attempts, so the server can identify and deduplicate repeated requests.
const requestIDHeader = "X-Livekit-Request-Id"

type authBase struct {
	apiKey    string
	apiSecret string
	// token, when set, is sent verbatim and per-call grant signing is skipped.
	token string
}

type authOption interface {
	Apply(t *auth.AccessToken)
}

type withVideoGrant auth.VideoGrant

func (g withVideoGrant) Apply(t *auth.AccessToken) {
	t.SetVideoGrant((*auth.VideoGrant)(&g))
}

type withSIPGrant auth.SIPGrant

func (g withSIPGrant) Apply(t *auth.AccessToken) {
	t.SetSIPGrant((*auth.SIPGrant)(&g))
}

type withAgentGrant auth.AgentGrant

func (g withAgentGrant) Apply(t *auth.AccessToken) {
	t.SetAgentGrant((*auth.AgentGrant)(&g))
}

// prepareContext builds the context for an outgoing API request: it signs an
// access token for the given grants and attaches it as a request header, then
// detaches a long-enough deadline so failover can reset it per attempt (see
// withFailoverTimeout).
func (b authBase) prepareContext(ctx context.Context, opt authOption, options ...authOption) (context.Context, error) {
	token := b.token
	if token == "" {
		at := auth.NewAccessToken(b.apiKey, b.apiSecret)
		opt.Apply(at)
		for _, opt := range options {
			opt.Apply(at)
		}
		var err error
		if token, err = at.ToJWT(); err != nil {
			return nil, err
		}
	}

	h := signalling.NewHTTPHeaderWithToken(token)
	ctxH, _ := twirp.HTTPRequestHeaders(ctx)
	if ctxH != nil {
		ctxH = ctxH.Clone()
	} else {
		ctxH = make(http.Header)
	}

	// merge new header with the ones already present in the context
	for k, vv := range h {
		for _, v := range vv {
			ctxH.Add(k, v)
		}
	}

	// Attach a stable idempotency key once per logical call, unless the caller
	// already supplied one. failoverTransport replays the same headers on each
	// retry, so every attempt carries this id and the server can dedup on it.
	if ctxH.Get(requestIDHeader) == "" {
		ctxH.Set(requestIDHeader, uuid.NewString())
	}

	// Detach a long-enough deadline so it isn't enforced across failover retries
	// (twirp re-checks the context after each request); the transport re-applies
	// the budget per attempt. A no-op for short or deadline-free requests.
	ctx = withFailoverTimeout(ctx)

	return twirp.WithHTTPRequestHeaders(ctx, ctxH)
}
