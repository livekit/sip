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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// attributesHeader carries base64-encoded JSON deploy attributes on prebuilt-image
// (BYOC) pushes. cloud-agents reads it on the manifest PUT and persists the attributes
// on the AgentVersion. Must match the header name expected by the cloud-agents proxy.
const attributesHeader = "X-LIVEKIT-AGENT-VERSION-ATTRIBUTES"

// PushTarget describes where and how the CLI should push a prebuilt image.
type PushTarget struct {
	// ProxyHost is the OCI registry host exposed by cloud-agents (e.g. "agents.livekit.io").
	ProxyHost string `json:"proxy_host"`
	// Name is the OCI repository name to use in /v2/{name}/... paths.
	Name string `json:"name"`
	// Tag is the version tag cloud-agents generated; use this as the image tag.
	Tag string `json:"tag"`
}

// GetPushTarget asks cloud-agents for the OCI proxy location for the given agent.
// The caller should then push the image to ProxyHost/Name:Tag using a transport
// returned by NewRegistryTransport.
func (c *Client) GetPushTarget(ctx context.Context, agentID string) (*PushTarget, error) {
	params := url.Values{}
	params.Add("agent_id", agentID)
	fullURL := fmt.Sprintf("%s/push-target?%s", c.agentsURL, params.Encode())

	req, err := c.newRequestWithContext(ctx, http.MethodGet, fullURL, nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to call push-target: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("push-target returned %d: %s", resp.StatusCode, body)
	}
	var target PushTarget
	if err := json.NewDecoder(resp.Body).Decode(&target); err != nil {
		return nil, fmt.Errorf("failed to decode push-target response: %w", err)
	}
	return &target, nil
}

// NewRegistryTransport returns an http.RoundTripper that injects the LiveKit JWT on every
// request. Pass this to crane via crane.WithTransport when pushing to the cloud-agents
// OCI proxy so the proxy's auth middleware accepts the requests. Any attributes are
// forwarded to cloud-agents so they can be recorded on the resulting AgentVersion.
func (c *Client) NewRegistryTransport(attributes map[string]string) http.RoundTripper {
	return &lkRegistryTransport{base: http.DefaultTransport, client: c, attributes: attributes}
}

// lkRegistryTransport injects LK auth headers on every HTTP request, allowing crane
// to push through the cloud-agents OCI proxy without doing OCI token negotiation.
type lkRegistryTransport struct {
	base       http.RoundTripper
	client     *Client
	attributes map[string]string
}

func (t *lkRegistryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if err := t.client.setAuthToken(req); err != nil {
		return nil, err
	}
	if err := t.client.setLivekitHeaders(req, t.attributes); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}
