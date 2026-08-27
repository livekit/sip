// Copyright 2025 LiveKit, Inc.
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

package cloudagents

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/livekit/protocol/auth"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

// newRequestWithContext creates a new HTTP request with the auth and version headers.
func (c *Client) newRequestWithContext(ctx context.Context, method, url string, body io.Reader, attrs map[string]string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if err := c.setAuthToken(req); err != nil {
		return nil, err
	}
	if err := c.setLivekitHeaders(req, attrs); err != nil {
		return nil, err
	}
	return req, nil
}

// setAuthToken sets the auth token in the request header.
func (c *Client) setAuthToken(req *http.Request) error {
	at := auth.NewAccessToken(c.apiKey, c.apiSecret)
	at.SetAgentGrant(&auth.AgentGrant{Admin: true})
	token, err := at.ToJWT()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	return nil
}

// setLivekitHeaders set the LiveKit headers in the request header.
func (c *Client) setLivekitHeaders(req *http.Request, attrs map[string]string) error {
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-LIVEKIT-CLI-VERSION", lksdk.Version)
	if len(attrs) > 0 {
		encoded, err := json.Marshal(attrs)
		if err != nil {
			return fmt.Errorf("failed to encode attributes: %w", err)
		}
		req.Header.Set(attributesHeader, base64.StdEncoding.EncodeToString(encoded))
	}
	return nil
}
