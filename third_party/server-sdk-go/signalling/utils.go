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

package signalling

import (
	"net/http"
	"strings"
)

func ToHttpURL(url string) string {
	if strings.HasPrefix(url, "ws") {
		return strings.Replace(url, "ws", "http", 1)
	}
	return url
}

func ToWebsocketURL(url string) string {
	if strings.HasPrefix(url, "http") {
		return strings.Replace(url, "http", "ws", 1)
	}
	return url
}

func NewHTTPHeaderWithToken(token string) http.Header {
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+token)
	return header
}
