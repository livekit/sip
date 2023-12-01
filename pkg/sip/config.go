// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sip

import (
	"encoding/json"
	"io"
	"net/http"
)

func getPublicIP() string {
	req, err := http.Get("http://ip-api.com/json/")
	if err != nil {
		panic(err)
	}
	defer req.Body.Close()

	body, err := io.ReadAll(req.Body)
	if err != nil {
		panic(err)
	}

	ip := struct {
		Query string
	}{}
	if err = json.Unmarshal(body, &ip); err != nil {
		panic(err)
	}

	if ip.Query == "" {
		panic("Query entry was not populated")
	}

	return ip.Query
}
