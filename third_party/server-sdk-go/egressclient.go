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

type EgressClient struct {
	egressClient livekit.Egress
	authBase
}

func NewEgressClient(url string, apiKey string, secretKey string, opts ...twirp.ClientOption) *EgressClient {
	return newEgressClient(url, authBase{apiKey: apiKey, apiSecret: secretKey}, newAPIHTTPClient(), opts...)
}

func newEgressClient(url string, auth authBase, httpClient *http.Client, opts ...twirp.ClientOption) *EgressClient {
	opts = append(opts, xtwirp.DefaultClientOptions()...)
	url = signalling.ToHttpURL(url)
	client := livekit.NewEgressProtobufClient(url, httpClient, opts...)
	return &EgressClient{
		egressClient: client,
		authBase:     auth,
	}
}

func (c *EgressClient) StartEgress(ctx context.Context, req *livekit.StartEgressRequest) (*livekit.EgressInfo, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomRecord: true})
	if err != nil {
		return nil, err
	}
	return c.egressClient.StartEgress(ctx, req)
}

func (c *EgressClient) StartRoomCompositeEgress(ctx context.Context, req *livekit.RoomCompositeEgressRequest) (*livekit.EgressInfo, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomRecord: true})
	if err != nil {
		return nil, err
	}
	return c.egressClient.StartRoomCompositeEgress(ctx, req)
}

func (c *EgressClient) StartParticipantEgress(ctx context.Context, req *livekit.ParticipantEgressRequest) (*livekit.EgressInfo, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomRecord: true})
	if err != nil {
		return nil, err
	}
	return c.egressClient.StartParticipantEgress(ctx, req)
}

func (c *EgressClient) StartTrackCompositeEgress(ctx context.Context, req *livekit.TrackCompositeEgressRequest) (*livekit.EgressInfo, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomRecord: true})
	if err != nil {
		return nil, err
	}
	return c.egressClient.StartTrackCompositeEgress(ctx, req)
}

func (c *EgressClient) StartTrackEgress(ctx context.Context, req *livekit.TrackEgressRequest) (*livekit.EgressInfo, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomRecord: true})
	if err != nil {
		return nil, err
	}
	return c.egressClient.StartTrackEgress(ctx, req)
}

func (c *EgressClient) StartWebEgress(ctx context.Context, req *livekit.WebEgressRequest) (*livekit.EgressInfo, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomRecord: true})
	if err != nil {
		return nil, err
	}
	return c.egressClient.StartWebEgress(ctx, req)
}

func (c *EgressClient) UpdateLayout(ctx context.Context, req *livekit.UpdateLayoutRequest) (*livekit.EgressInfo, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomRecord: true})
	if err != nil {
		return nil, err
	}
	return c.egressClient.UpdateLayout(ctx, req)
}

func (c *EgressClient) UpdateStream(ctx context.Context, req *livekit.UpdateStreamRequest) (*livekit.EgressInfo, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomRecord: true})
	if err != nil {
		return nil, err
	}
	return c.egressClient.UpdateStream(ctx, req)
}

func (c *EgressClient) ListEgress(ctx context.Context, req *livekit.ListEgressRequest) (*livekit.ListEgressResponse, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomRecord: true})
	if err != nil {
		return nil, err
	}
	return c.egressClient.ListEgress(ctx, req)
}

func (c *EgressClient) StopEgress(ctx context.Context, req *livekit.StopEgressRequest) (*livekit.EgressInfo, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomRecord: true})
	if err != nil {
		return nil, err
	}
	return c.egressClient.StopEgress(ctx, req)
}
