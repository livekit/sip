// Copyright 2026 LiveKit, Inc.
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
	"errors"
	"os"
)

// LiveKitAPI is a single entry point to every LiveKit server API. Construct it
// with NewLiveKitAPI and reach each service through its accessor, e.g.
// api.Room().CreateRoom(ctx, req).
type LiveKitAPI struct {
	room          *RoomServiceClient
	egress        *EgressClient
	ingress       *IngressClient
	sip           *SIPClient
	agentDispatch *AgentDispatchClient
	connector     *ConnectorClient
}

type liveKitAPIOptions struct {
	url       string
	apiKey    string
	apiSecret string
	token     string
}

// LiveKitAPIOption configures NewLiveKitAPI.
type LiveKitAPIOption func(*liveKitAPIOptions)

// WithURL sets the LiveKit server URL. If omitted, it falls back to the
// LIVEKIT_URL environment variable.
func WithURL(url string) LiveKitAPIOption {
	return func(o *liveKitAPIOptions) { o.url = url }
}

// WithAPIKey authenticates by signing a token per request from the key and secret.
func WithAPIKey(apiKey, apiSecret string) LiveKitAPIOption {
	return func(o *liveKitAPIOptions) {
		o.apiKey = apiKey
		o.apiSecret = apiSecret
	}
}

// WithToken authenticates with a pre-signed token, sent verbatim on every
// request. The token must already carry the grants for the calls it's used with;
// unlike WithAPIKey it needs no secret, so the SDK can run client-side.
func WithToken(token string) LiveKitAPIOption {
	return func(o *liveKitAPIOptions) { o.token = token }
}

// NewLiveKitAPI creates a client for all LiveKit server APIs. The URL and
// credentials fall back to the LIVEKIT_URL, LIVEKIT_TOKEN, LIVEKIT_API_KEY, and
// LIVEKIT_API_SECRET environment variables, so with those set it can be called
// with no arguments. Otherwise provide the URL (WithURL) and either an API key
// and secret (WithAPIKey) or a pre-signed token (WithToken).
func NewLiveKitAPI(opts ...LiveKitAPIOption) (*LiveKitAPI, error) {
	o := &liveKitAPIOptions{}
	for _, opt := range opts {
		opt(o)
	}
	url := o.url
	if url == "" {
		url = os.Getenv("LIVEKIT_URL")
	}
	if url == "" {
		return nil, errors.New("url is required (use WithURL or set LIVEKIT_URL)")
	}
	// Only fall back to environment credentials when none were provided
	// explicitly, so an ambient LIVEKIT_TOKEN can't silently override an
	// explicit WithAPIKey (or vice versa).
	if o.token == "" && o.apiKey == "" && o.apiSecret == "" {
		o.token = os.Getenv("LIVEKIT_TOKEN")
	}
	if o.token == "" && o.apiKey == "" && o.apiSecret == "" {
		o.apiKey = os.Getenv("LIVEKIT_API_KEY")
		o.apiSecret = os.Getenv("LIVEKIT_API_SECRET")
	}
	if o.token == "" && (o.apiKey == "" || o.apiSecret == "") {
		return nil, errors.New("either a token or an API key and secret are required")
	}

	ab := authBase{apiKey: o.apiKey, apiSecret: o.apiSecret, token: o.token}
	// Share one HTTP client (and its connection pool) across every service
	// rather than letting each open its own.
	httpClient := newAPIHTTPClient()
	return &LiveKitAPI{
		room:          newRoomServiceClient(url, ab, httpClient),
		egress:        newEgressClient(url, ab, httpClient),
		ingress:       newIngressClient(url, ab, httpClient),
		sip:           newSIPClient(url, ab, httpClient),
		agentDispatch: newAgentDispatchServiceClient(url, ab, httpClient),
		connector:     newConnectorClient(url, ab, httpClient),
	}, nil
}

func (a *LiveKitAPI) Room() *RoomServiceClient            { return a.room }
func (a *LiveKitAPI) Egress() *EgressClient               { return a.egress }
func (a *LiveKitAPI) Ingress() *IngressClient             { return a.ingress }
func (a *LiveKitAPI) SIP() *SIPClient                     { return a.sip }
func (a *LiveKitAPI) AgentDispatch() *AgentDispatchClient { return a.agentDispatch }
func (a *LiveKitAPI) Connector() *ConnectorClient         { return a.connector }
