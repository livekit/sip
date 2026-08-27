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

package lksdk

import (
	"context"

	"github.com/twitchtv/twirp"

	"github.com/livekit/protocol/livekit"
)

type AgentSimulationClient struct {
	simulationClient livekit.AgentSimulation
	authBase
}

func NewAgentSimulationClient(url string, apiKey string, apiSecret string, opts ...twirp.ClientOption) *AgentSimulationClient {
	client := livekit.NewAgentSimulationProtobufClient(url, newAPIHTTPClient(), opts...)
	return &AgentSimulationClient{
		simulationClient: client,
		authBase:         authBase{apiKey: apiKey, apiSecret: apiSecret},
	}
}

func (c *AgentSimulationClient) CreateSimulationRun(ctx context.Context, req *livekit.SimulationRun_Create_Request) (*livekit.SimulationRun_Create_Response, error) {
	ctx, err := c.prepareContext(ctx, withAgentGrant{SimulationAdmin: true})
	if err != nil {
		return nil, err
	}
	return c.simulationClient.CreateSimulationRun(ctx, req)
}

func (c *AgentSimulationClient) ConfirmSimulationSourceUpload(ctx context.Context, req *livekit.SimulationRun_ConfirmSourceUpload_Request) (*livekit.SimulationRun_ConfirmSourceUpload_Response, error) {
	ctx, err := c.prepareContext(ctx, withAgentGrant{SimulationAdmin: true})
	if err != nil {
		return nil, err
	}
	return c.simulationClient.ConfirmSimulationSourceUpload(ctx, req)
}

func (c *AgentSimulationClient) GetSimulationRun(ctx context.Context, req *livekit.SimulationRun_Get_Request) (*livekit.SimulationRun_Get_Response, error) {
	ctx, err := c.prepareContext(ctx, withAgentGrant{SimulationAdmin: true})
	if err != nil {
		return nil, err
	}
	return c.simulationClient.GetSimulationRun(ctx, req)
}

func (c *AgentSimulationClient) ListSimulationRuns(ctx context.Context, req *livekit.SimulationRun_List_Request) (*livekit.SimulationRun_List_Response, error) {
	ctx, err := c.prepareContext(ctx, withAgentGrant{SimulationAdmin: true})
	if err != nil {
		return nil, err
	}
	return c.simulationClient.ListSimulationRuns(ctx, req)
}

func (c *AgentSimulationClient) CancelSimulationRun(ctx context.Context, req *livekit.SimulationRun_Cancel_Request) (*livekit.SimulationRun_Cancel_Response, error) {
	ctx, err := c.prepareContext(ctx, withAgentGrant{SimulationAdmin: true})
	if err != nil {
		return nil, err
	}
	return c.simulationClient.CancelSimulationRun(ctx, req)
}

func (c *AgentSimulationClient) CreateScenarioFromSession(ctx context.Context, req *livekit.Scenario_CreateFromSession_Request) (*livekit.Scenario_CreateFromSession_Response, error) {
	ctx, err := c.prepareContext(ctx, withAgentGrant{SimulationAdmin: true})
	if err != nil {
		return nil, err
	}
	return c.simulationClient.CreateScenarioFromSession(ctx, req)
}
