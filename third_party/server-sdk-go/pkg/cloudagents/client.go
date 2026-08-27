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
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	lkproto "github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/twitchtv/twirp"
)

// Client is a wrapper around the lksdk.AgentClient that provides a simpler interface for creating and deploying agents.
type Client struct {
	*lksdk.AgentClient
	projectURL    string
	apiKey        string
	apiSecret     string
	agentsURL     string
	httpClient    *http.Client
	headers       map[string]string
	logger        logger.Logger
	jsonLogStream bool
}

// New returns a new Client with the given project URL, API key, and API secret.
func New(opts ...ClientOption) (*Client, error) {
	client := &Client{
		logger: logger.GetLogger(),
	}
	for _, opt := range opts {
		opt(client)
	}
	if client.projectURL == "" {
		return nil, fmt.Errorf("project credentials are required")
	}
	if client.httpClient == nil {
		client.httpClient = &http.Client{}
	}
	agentClient, err := lksdk.NewAgentClient(client.projectURL, client.apiKey, client.apiSecret,
		lksdk.WithHTTPClient(client.httpClient),
		lksdk.WithTwirpClientOptions(
			twirp.WithClientHooks(&twirp.ClientHooks{
				RequestPrepared: func(ctx context.Context, req *http.Request) (context.Context, error) {
					if err := client.setLivekitHeaders(req, nil); err != nil {
						return ctx, err
					}
					return ctx, nil
				},
			})))
	if err != nil {
		return nil, err
	}
	client.AgentClient = agentClient
	client.agentsURL = client.getAgentsURL("")
	return client, nil
}

// CreateAgent creates a new agent by building from source.
func (c *Client) CreateAgent(
	ctx context.Context,
	source fs.FS,
	secrets []*lkproto.AgentSecret,
	regions []string,
	excludeFiles []string,
	buildLogStreamWriter io.Writer,
) (*lkproto.CreateAgentResponse, error) {
	resp, err := c.AgentClient.CreateAgent(ctx, &lkproto.CreateAgentRequest{
		Secrets: secrets,
		Regions: regions,
	})
	if err != nil {
		return nil, err
	}
	if err := c.uploadAndBuild(ctx,
		resp.AgentId,
		resp.PresignedUrl,
		resp.PresignedPostRequest,
		source,
		nil, // no attributes on create
		"",  // production (create always targets production)
		excludeFiles,
		buildLogStreamWriter,
	); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *Client) CreateAgentV2(
	ctx context.Context,
	secrets []*lkproto.AgentSecret,
	regions []string,
) (*lkproto.CreateAgentV2Response, error) {
	resp, err := c.AgentClient.CreateAgentV2(ctx, &lkproto.CreateAgentV2Request{
		Secrets: secrets,
		Regions: regions,
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// DeployAgent deploys new agent by building from source.
func (c *Client) DeployAgent(
	ctx context.Context,
	agentID string,
	source fs.FS,
	secrets []*lkproto.AgentSecret,
	attributes map[string]string,
	excludeFiles []string,
	buildLogStreamWriter io.Writer,
) error {
	resp, err := c.AgentClient.DeployAgent(ctx, &lkproto.DeployAgentRequest{
		AgentId:    agentID,
		Secrets:    secrets,
		Attributes: attributes,
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("failed to deploy agent: %s", resp.Message)
	}
	return c.uploadAndBuild(ctx, agentID, resp.PresignedUrl, resp.PresignedPostRequest, source, attributes, "", excludeFiles, buildLogStreamWriter)
}

func (c *Client) DeployAgentV2(
	ctx context.Context,
	agentID string,
	source fs.FS,
	secrets []*lkproto.AgentSecret,
	attributes map[string]string,
	agentDeployment string,
	excludeFiles []string,
	buildLogStreamWriter io.Writer,
) error {
	resp, err := c.AgentClient.DeployAgentV2(ctx, &lkproto.DeployAgentV2Request{
		AgentId:    agentID,
		Secrets:    secrets,
		Deployment: agentDeployment,
		Attributes: attributes,
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("failed to deploy agent: %s", resp.Message)
	}
	return c.uploadAndBuild(ctx, agentID, "", resp.PresignedReq, source, attributes, agentDeployment, excludeFiles, buildLogStreamWriter)
}

func (c *Client) PromoteAgent(
	ctx context.Context,
	agentID string,
	srcDeployment string,
	dstDeployment string,
) error {
	resp, err := c.AgentClient.PromoteAgent(ctx, &lkproto.PromoteAgentRequest{
		AgentId:       agentID,
		SrcDeployment: srcDeployment,
		DstDeployment: dstDeployment,
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("failed to promote agent: %s", resp.Message)
	}
	return nil
}

// RegisterAgent creates an agent record without uploading source or triggering a build.
// Use this when you intend to push a prebuilt image immediately after via GetPushTarget.
func (c *Client) RegisterAgent(ctx context.Context, secrets []*lkproto.AgentSecret, regions []string) (string, error) {
	resp, err := c.AgentClient.CreateAgent(ctx, &lkproto.CreateAgentRequest{
		Secrets: secrets,
		Regions: regions,
	})
	if err != nil {
		return "", err
	}
	return resp.AgentId, nil
}

// CreatePrivateLink creates a new private link for cloud agents.
func (c *Client) CreatePrivateLink(ctx context.Context, req *lkproto.CreatePrivateLinkRequest) (*lkproto.CreatePrivateLinkResponse, error) {
	return c.AgentClient.CreatePrivateLink(ctx, req)
}

// DestroyPrivateLink deletes a private link by ID.
func (c *Client) DestroyPrivateLink(ctx context.Context, req *lkproto.DestroyPrivateLinkRequest) (*lkproto.DestroyPrivateLinkResponse, error) {
	return c.AgentClient.DestroyPrivateLink(ctx, req)
}

// ListPrivateLinks lists private links for the project.
func (c *Client) ListPrivateLinks(ctx context.Context, req *lkproto.ListPrivateLinksRequest) (*lkproto.ListPrivateLinksResponse, error) {
	return c.AgentClient.ListPrivateLinks(ctx, req)
}

// uploadAndBuild uploads the source and triggers remote build
func (c *Client) uploadAndBuild(
	ctx context.Context,
	agentID string,
	presignedUrl string,
	presignedPostRequest *lkproto.PresignedPostRequest,
	source fs.FS,
	attributes map[string]string,
	agentDeployment string,
	excludeFiles []string,
	buildLogStreamWriter io.Writer,
) error {
	if err := uploadSource(
		source,
		presignedUrl,
		presignedPostRequest,
		excludeFiles,
	); err != nil {
		return err
	}
	if err := c.build(ctx, agentID, attributes, agentDeployment, buildLogStreamWriter); err != nil {
		return err
	}
	return nil
}

// getAgentsURL derives the cloud-agents service URL from the project URL.
// It replaces the project subdomain with "agents" (and optionally a region prefix)
// so that build/log requests are routed to the cloud-agents service.
//
// Examples:
//
//	getAgentsURL("")            -> https://agents.livekit.cloud
//	getAgentsURL("osbxash1a")  -> https://osbxash1a.agents.livekit.cloud
//
// When serverRegion is set, the request is pinned to a specific cloud-agents
// cluster rather than relying on GeoDNS resolution.
func (c *Client) getAgentsURL(serverRegion string) string {
	agentsURL := c.projectURL
	if os.Getenv("LK_AGENTS_URL") != "" {
		agentsURL = os.Getenv("LK_AGENTS_URL")
	}
	if strings.HasPrefix(agentsURL, "ws") {
		agentsURL = strings.Replace(agentsURL, "ws", "http", 1)
	}

	// skip rewrite for local development
	if isLocalURL(agentsURL) {
		return agentsURL
	}

	pattern := `^https://[a-zA-Z0-9\-]+\.`
	re := regexp.MustCompile(pattern)
	if serverRegion != "" {
		serverRegion = fmt.Sprintf("%s.", serverRegion)
	}
	return re.ReplaceAllString(agentsURL, fmt.Sprintf("https://%sagents.", serverRegion))
}

func isLocalURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1"
}
