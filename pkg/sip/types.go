// Copyright 2024 LiveKit, Inc.
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
	"errors"

	"github.com/emiago/sipgo/sip"
)

type LocalTag string
type RemoteTag string

func getFromTag(r *sip.Request) (RemoteTag, error) {
	from, ok := r.From()
	if !ok {
		return "", errors.New("no From on Request")
	}
	return getTagFrom(from.Params)
}

func getToTag(r *sip.Response) (RemoteTag, error) {
	to, ok := r.To()
	if !ok {
		return "", errors.New("no To on Response")
	}
	return getTagFrom(to.Params)
}

func getTagFrom(params sip.HeaderParams) (RemoteTag, error) {
	tag, ok := params["tag"]
	if !ok {
		return "", errors.New("no tag on the address")
	}
	return RemoteTag(tag), nil
}
