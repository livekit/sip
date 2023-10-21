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

package service

import (
	"context"

	"github.com/livekit/sip/pkg/config"
)

type Handler struct {
	conf *config.Config
}

func (h *Handler) HandleSIP(ctx context.Context, info any, wsUrl, token string, extraParams any) {
	select {}
}

func (h *Handler) Kill() {
}

func NewHandler(conf *config.Config) *Handler {
	return &Handler{
		conf: conf,
	}
}
