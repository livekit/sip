// Copyright 2025 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cloudagents

import (
	"net/http"

	"github.com/livekit/protocol/logger"
)

// ClientOption provides a way to configure the Client.
type ClientOption func(*Client)

// WithLogger sets the logger for the Client.
func WithLogger(logger logger.Logger) ClientOption {
	return func(c *Client) {
		c.logger = logger
	}
}

// WithProject sets the livekit project credentials for the Client.
func WithProject(projectURL, apiKey, apiSecret string) ClientOption {
	return func(c *Client) {
		c.projectURL = projectURL
		c.apiKey = apiKey
		c.apiSecret = apiSecret
	}
}

// WithHTTPClient sets the http client for the Client.
func WithHTTPClient(httpClient *http.Client) ClientOption {
	return func(c *Client) {
		c.httpClient = httpClient
	}
}

func WithHeaders(headers map[string]string) ClientOption {
	return func(c *Client) {
		c.headers = headers
	}
}

func WithJsonLogStream() ClientOption {
	return func(c *Client) {
		c.jsonLogStream = true
	}
}
