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

type IngressClient struct {
	ingressClient livekit.Ingress
	authBase
}

func NewIngressClient(url string, apiKey string, secretKey string, opts ...twirp.ClientOption) *IngressClient {
	return newIngressClient(url, authBase{apiKey: apiKey, apiSecret: secretKey}, newAPIHTTPClient(), opts...)
}

func newIngressClient(url string, auth authBase, httpClient *http.Client, opts ...twirp.ClientOption) *IngressClient {
	opts = append(opts, xtwirp.DefaultClientOptions()...)
	url = signalling.ToHttpURL(url)
	client := livekit.NewIngressProtobufClient(url, httpClient, opts...)
	return &IngressClient{
		ingressClient: client,
		authBase:      auth,
	}
}

func (c *IngressClient) CreateIngress(ctx context.Context, in *livekit.CreateIngressRequest) (*livekit.IngressInfo, error) {
	if in == nil {
		return nil, ErrInvalidParameter
	}

	ctx, err := c.prepareContext(ctx, withVideoGrant{IngressAdmin: true})
	if err != nil {
		return nil, err
	}
	return c.ingressClient.CreateIngress(ctx, in)
}

func (c *IngressClient) UpdateIngress(ctx context.Context, in *livekit.UpdateIngressRequest) (*livekit.IngressInfo, error) {
	if in == nil {
		return nil, ErrInvalidParameter
	}

	ctx, err := c.prepareContext(ctx, withVideoGrant{IngressAdmin: true})
	if err != nil {
		return nil, err
	}
	return c.ingressClient.UpdateIngress(ctx, in)
}

func (c *IngressClient) ListIngress(ctx context.Context, in *livekit.ListIngressRequest) (*livekit.ListIngressResponse, error) {
	if in == nil {
		return nil, ErrInvalidParameter
	}

	ctx, err := c.prepareContext(ctx, withVideoGrant{IngressAdmin: true})
	if err != nil {
		return nil, err
	}
	return c.ingressClient.ListIngress(ctx, in)
}

func (c *IngressClient) DeleteIngress(ctx context.Context, in *livekit.DeleteIngressRequest) (*livekit.IngressInfo, error) {
	if in == nil {
		return nil, ErrInvalidParameter
	}

	ctx, err := c.prepareContext(ctx, withVideoGrant{IngressAdmin: true})
	if err != nil {
		return nil, err
	}
	return c.ingressClient.DeleteIngress(ctx, in)
}
