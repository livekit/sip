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
	"log"

	"github.com/go-logr/stdr"

	protoLogger "github.com/livekit/protocol/logger"
)

var logger protoLogger.Logger = protoLogger.LogRLogger(stdr.New(log.Default()))

// SetLogger overrides default logger. To use a [logr](https://github.com/go-logr/logr) compatible logger,
// pass in SetLogger(logger.LogRLogger(logRLogger))
func SetLogger(l protoLogger.Logger) {
	logger = l
}
