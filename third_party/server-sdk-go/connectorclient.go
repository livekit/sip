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

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/xtwirp"
	"github.com/livekit/server-sdk-go/v2/signalling"
	"github.com/twitchtv/twirp"
)

type ConnectorClient struct {
	connector livekit.Connector
	authBase
}

func NewConnectorClient(url string, apiKey string, secretKey string, opts ...twirp.ClientOption) *ConnectorClient {
	return newConnectorClient(url, authBase{apiKey: apiKey, apiSecret: secretKey}, newAPIHTTPClient(), opts...)
}

func newConnectorClient(url string, auth authBase, httpClient *http.Client, opts ...twirp.ClientOption) *ConnectorClient {
	opts = append(opts, xtwirp.DefaultClientOptions()...)
	url = signalling.ToHttpURL(url)
	client := livekit.NewConnectorProtobufClient(url, httpClient, opts...)
	return &ConnectorClient{
		connector: client,
		authBase:  auth,
	}
}

func (c *ConnectorClient) ConnectTwilioCall(ctx context.Context, req *livekit.ConnectTwilioCallRequest) (*livekit.ConnectTwilioCallResponse, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomCreate: true, Room: req.RoomName})
	if err != nil {
		return nil, err
	}
	return c.connector.ConnectTwilioCall(ctx, req)
}

func (c *ConnectorClient) DialWhatsAppCall(ctx context.Context, req *livekit.DialWhatsAppCallRequest) (*livekit.DialWhatsAppCallResponse, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomCreate: true, Room: req.RoomName})
	if err != nil {
		return nil, err
	}
	return c.connector.DialWhatsAppCall(ctx, req)
}

func (c *ConnectorClient) AcceptWhatsAppCall(ctx context.Context, req *livekit.AcceptWhatsAppCallRequest) (*livekit.AcceptWhatsAppCallResponse, error) {
	// When waiting for the inbound party to join, the request can block, so default
	// the deadline to the standard ring window when the caller set none.
	if _, ok := ctx.Deadline(); !ok && req.WaitUntilAnswered {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultRingingTimeout)
		defer cancel()
	}

	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomCreate: true, Room: req.RoomName})
	if err != nil {
		return nil, err
	}
	return c.connector.AcceptWhatsAppCall(ctx, req)
}

func (c *ConnectorClient) ConnectWhatsAppCall(ctx context.Context, req *livekit.ConnectWhatsAppCallRequest) (*livekit.ConnectWhatsAppCallResponse, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomCreate: true})
	if err != nil {
		return nil, err
	}
	return c.connector.ConnectWhatsAppCall(ctx, req)
}

func (c *ConnectorClient) DisconnectWhatsAppCall(ctx context.Context, req *livekit.DisconnectWhatsAppCallRequest) (*livekit.DisconnectWhatsAppCallResponse, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomCreate: true})
	if err != nil {
		return nil, err
	}
	return c.connector.DisconnectWhatsAppCall(ctx, req)
}
