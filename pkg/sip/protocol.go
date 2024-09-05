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
	"fmt"

	"github.com/emiago/sipgo/sip"
)

type ErrorStatus struct {
	StatusCode int
	Message    string
}

func (e *ErrorStatus) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("sip status: %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("sip status: %d", e.StatusCode)
}

type Signaling interface {
	From() sip.Uri
	To() sip.Uri
	ID() LocalTag
	Tag() RemoteTag
	RemoteHeaders() Headers

	WriteRequest(req *sip.Request) error
	Transaction(req *sip.Request) (sip.ClientTransaction, error)

	Drop()
}

func sendBye(c Signaling, bye *sip.Request) {
	tx, err := c.Transaction(bye)
	if err != nil {
		return
	}
	defer tx.Terminate()
	r, err := sipResponse(tx)
	if err != nil {
		return
	}
	if r.StatusCode == 200 {
		_ = c.WriteRequest(sip.NewAckRequest(bye, r, nil))
	}
}
