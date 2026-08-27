// Copyright 2024 LiveKit, Inc.
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
	"time"

	"github.com/twitchtv/twirp"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/xtwirp"
	"github.com/livekit/server-sdk-go/v2/signalling"
)

type PhoneNumberClient struct {
	phoneNumberClient livekit.PhoneNumberService
	authBase
}

// NewPhoneNumberClient creates a LiveKit Phone Number client.
func NewPhoneNumberClient(url string, apiKey string, secretKey string, opts ...twirp.ClientOption) *PhoneNumberClient {
	opts = append(opts, xtwirp.DefaultClientOptions()...)
	return &PhoneNumberClient{
		phoneNumberClient: livekit.NewPhoneNumberServiceProtobufClient(signalling.ToHttpURL(url), newAPIHTTPClient(), opts...),
		authBase: authBase{
			apiKey:    apiKey,
			apiSecret: secretKey,
		},
	}
}

// SearchPhoneNumbers searches for available phone numbers in inventory.
func (p *PhoneNumberClient) SearchPhoneNumbers(ctx context.Context, in *livekit.SearchPhoneNumbersRequest) (*livekit.SearchPhoneNumbersResponse, error) {
	if in == nil {
		return nil, ErrInvalidParameter
	}

	ctx, err := p.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}

	// Add timeout to prevent connection issues
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return p.phoneNumberClient.SearchPhoneNumbers(ctx, in)
}

// PurchasePhoneNumber purchases a phone number from inventory.
func (p *PhoneNumberClient) PurchasePhoneNumber(ctx context.Context, in *livekit.PurchasePhoneNumberRequest) (*livekit.PurchasePhoneNumberResponse, error) {
	if in == nil {
		return nil, ErrInvalidParameter
	}

	ctx, err := p.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}
	return p.phoneNumberClient.PurchasePhoneNumber(ctx, in)
}

// ListPhoneNumbers lists phone numbers for a project.
func (p *PhoneNumberClient) ListPhoneNumbers(ctx context.Context, in *livekit.ListPhoneNumbersRequest) (*livekit.ListPhoneNumbersResponse, error) {
	if in == nil {
		return nil, ErrInvalidParameter
	}

	ctx, err := p.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}
	return p.phoneNumberClient.ListPhoneNumbers(ctx, in)
}

// GetPhoneNumber gets a phone number from a project.
func (p *PhoneNumberClient) GetPhoneNumber(ctx context.Context, in *livekit.GetPhoneNumberRequest) (*livekit.GetPhoneNumberResponse, error) {
	if in == nil {
		return nil, ErrInvalidParameter
	}

	ctx, err := p.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}
	return p.phoneNumberClient.GetPhoneNumber(ctx, in)
}

// UpdatePhoneNumber updates a phone number in a project.
func (p *PhoneNumberClient) UpdatePhoneNumber(ctx context.Context, in *livekit.UpdatePhoneNumberRequest) (*livekit.UpdatePhoneNumberResponse, error) {
	if in == nil {
		return nil, ErrInvalidParameter
	}

	ctx, err := p.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}
	return p.phoneNumberClient.UpdatePhoneNumber(ctx, in)
}

// ReleasePhoneNumbers releases phone numbers.
func (p *PhoneNumberClient) ReleasePhoneNumbers(ctx context.Context, in *livekit.ReleasePhoneNumbersRequest) (*livekit.ReleasePhoneNumbersResponse, error) {
	if in == nil {
		return nil, ErrInvalidParameter
	}

	ctx, err := p.prepareContext(ctx, withSIPGrant{Admin: true})
	if err != nil {
		return nil, err
	}
	return p.phoneNumberClient.ReleasePhoneNumbers(ctx, in)
}
