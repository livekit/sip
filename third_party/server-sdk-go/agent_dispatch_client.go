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

	"github.com/twitchtv/twirp"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/xtwirp"
	"github.com/livekit/server-sdk-go/v2/signalling"
)

type AgentDispatchClient struct {
	agentDispatchService livekit.AgentDispatchService
	authBase
}

func NewAgentDispatchServiceClient(url string, apiKey string, secretKey string, opts ...twirp.ClientOption) *AgentDispatchClient {
	return newAgentDispatchServiceClient(url, authBase{apiKey: apiKey, apiSecret: secretKey}, newAPIHTTPClient(), opts...)
}

func newAgentDispatchServiceClient(url string, auth authBase, httpClient *http.Client, opts ...twirp.ClientOption) *AgentDispatchClient {
	opts = append(opts, xtwirp.DefaultClientOptions()...)
	url = signalling.ToHttpURL(url)
	client := livekit.NewAgentDispatchServiceProtobufClient(url, httpClient, opts...)

	return &AgentDispatchClient{
		agentDispatchService: client,
		authBase:             auth,
	}
}

func (c *AgentDispatchClient) CreateDispatch(ctx context.Context, req *livekit.CreateAgentDispatchRequest) (*livekit.AgentDispatch, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomAdmin: true, Room: req.Room})
	if err != nil {
		return nil, err
	}

	return c.agentDispatchService.CreateDispatch(ctx, req)
}

func (c *AgentDispatchClient) DeleteDispatch(ctx context.Context, req *livekit.DeleteAgentDispatchRequest) (*livekit.AgentDispatch, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomAdmin: true, Room: req.Room})
	if err != nil {
		return nil, err
	}

	return c.agentDispatchService.DeleteDispatch(ctx, req)
}

func (c *AgentDispatchClient) ListDispatch(ctx context.Context, req *livekit.ListAgentDispatchRequest) (*livekit.ListAgentDispatchResponse, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomAdmin: true, Room: req.Room})
	if err != nil {
		return nil, err
	}

	return c.agentDispatchService.ListDispatch(ctx, req)
}

// GetDispatch returns the agent dispatch with the given ID in the room, or nil
// if no matching dispatch exists.
func (c *AgentDispatchClient) GetDispatch(ctx context.Context, dispatchID string, room string) (*livekit.AgentDispatch, error) {
	res, err := c.ListDispatch(ctx, &livekit.ListAgentDispatchRequest{DispatchId: dispatchID, Room: room})
	if err != nil {
		return nil, err
	}
	if len(res.AgentDispatches) == 0 {
		return nil, nil
	}
	return res.AgentDispatches[0], nil
}
