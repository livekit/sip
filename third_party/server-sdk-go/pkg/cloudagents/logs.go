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
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

type APIError struct {
	Message string            `json:"msg"`
	Meta    map[string]string `json:"meta,omitempty"`
}

// StreamLogs streams the logs for the given agent.
func (c *Client) StreamLogs(ctx context.Context, logType, agentID, agentDeployment string, writer io.Writer, serverRegion string) error {
	logger := c.logger.WithName("StreamLogs")
	if logType == "" {
		logType = "deploy"
	}
	params := url.Values{}
	params.Add("agent_id", agentID)
	params.Add("log_type", logType)
	if agentDeployment != "" {
		params.Add("deployment", agentDeployment)
	}
	fullUrl := fmt.Sprintf("%s/logs?%s", c.getAgentsURL(serverRegion), params.Encode())
	req, err := c.newRequestWithContext(ctx, "GET", fullUrl, nil, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logger.Debugw("failed to get logs", "status", resp.Status)
		var errorResponse APIError
		if err := json.NewDecoder(resp.Body).Decode(&errorResponse); err != nil {
			return fmt.Errorf("failed to parse error response: %w", err)
		}
		return fmt.Errorf("failed to get logs: %s - %v", errorResponse.Message, errorResponse.Meta)
	}
	scanner := bufio.NewScanner(resp.Body)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil {
					return fmt.Errorf("scanner error: %w", err)
				}
				return nil
			}
			line := scanner.Text()
			if _, err := fmt.Fprintln(writer, line); err != nil {
				return fmt.Errorf("failed to write log line: %w", err)
			}
		}
	}
}
