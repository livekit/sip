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
	"strconv"
	"strings"

	"github.com/emiago/sipgo/sip"
	"github.com/livekit/psrpc"
	"github.com/pkg/errors"
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

func sendAndACK(c Signaling, req *sip.Request) {
	tx, err := c.Transaction(req)
	if err != nil {
		return
	}
	defer tx.Terminate()
	r, err := sipResponse(tx, nil)
	if err != nil {
		return
	}
	if r.StatusCode == 200 {
		_ = c.WriteRequest(sip.NewAckRequest(req, r, nil))
	}
}

func NewReferRequest(inviteRequest *sip.Request, inviteResponse *sip.Response, referToUrl string) *sip.Request {
	req := sip.NewRequest(sip.REFER, inviteRequest.Recipient)

	req.SipVersion = inviteRequest.SipVersion
	sip.CopyHeaders("Via", inviteRequest, req)
	// if inviteResponse.IsSuccess() {
	// update branch, 2xx ACK is separate Tx
	viaHop, _ := req.Via()
	viaHop.Params.Add("branch", sip.GenerateBranch())
	// }

	if len(inviteRequest.GetHeaders("Route")) > 0 {
		sip.CopyHeaders("Route", inviteRequest, req)
	} else {
		hdrs := inviteResponse.GetHeaders("Record-Route")
		for i := len(hdrs) - 1; i >= 0; i-- {
			rrh, ok := hdrs[i].(*sip.RecordRouteHeader)
			if !ok {
				continue
			}

			h := rrh.Clone()
			req.AppendHeader(h)
		}
	}

	maxForwardsHeader := sip.MaxForwardsHeader(70)
	req.AppendHeader(&maxForwardsHeader)

	if h, _ := inviteRequest.From(); h != nil {
		sip.CopyHeaders("From", inviteRequest, req)
	}

	if h, _ := inviteResponse.To(); h != nil {
		sip.CopyHeaders("To", inviteResponse, req)
	}

	if h, _ := inviteRequest.CallID(); h != nil {
		sip.CopyHeaders("Call-ID", inviteRequest, req)
	}

	if h, _ := inviteRequest.CSeq(); h != nil {
		sip.CopyHeaders("CSeq", inviteRequest, req)
		// Increase the invite request cseq so that we use the proper cseq in the bye request eventually
		h.SeqNo = h.SeqNo + 1
	}

	cseq, _ := req.CSeq()
	cseq.SeqNo = cseq.SeqNo + 1
	cseq.MethodName = sip.REFER

	// Set Refer-To header
	referTo := sip.NewHeader("Refer-To", referToUrl)
	req.AppendHeader(referTo)
	req.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, CANCEL, BYE, NOTIFY, REFER, MESSAGE, OPTIONS, INFO, SUBSCRIBE"))

	req.SetTransport(inviteRequest.Transport())
	req.SetSource(inviteRequest.Source())
	req.SetDestination(inviteRequest.Destination())

	return req
}

func sendRefer(c Signaling, req *sip.Request, stop <-chan struct{}) (*sip.Response, error) {
	tx, err := c.Transaction(req)
	if err != nil {
		return nil, err
	}
	defer tx.Terminate()

	resp, err := sipResponse(tx, stop)
	if err != nil {
		return nil, err
	}

	switch resp.StatusCode {
	case 200, 202:
		return resp, nil
	case 403:
		return resp, psrpc.NewErrorf(psrpc.PermissionDenied, "SIP REFER was denied")
	default:
		return resp, psrpc.NewErrorf(psrpc.Internal, "SIP REFER failed with code", resp.StatusCode)
	}
}

func parseNotifyBody(body string) (int, error) {
	v := strings.Split(body, " ")

	if len(v) < 2 {
		return 0, psrpc.NewErrorf(psrpc.InvalidArgument, "invalid notify body: not enough tokens")
	}

	if strings.ToUpper(v[0]) != "SIP/2.0" {
		return 0, psrpc.NewErrorf(psrpc.InvalidArgument, "invalid notify body: wrong prefix or SIP version")
	}

	c, err := strconv.Atoi(v[1])
	if err != nil {
		return 0, errors.Wrap(err, "invalid notify body: unable to parse status code")
	}

	return c, nil
}

func handleNotify(req *sip.Request) (method sip.RequestMethod, cseq uint32, status int, err error) {
	event := req.GetHeader("Event")
	var cseq64 uint64

	switch {
	case strings.HasPrefix(strings.ToLower(event.Value()), "refer"):
		// REFER Notify
		method = sip.REFER

		// Get the CSeq of the request this relates to if given
		v := strings.Split(event.Value(), ";")
		if len(v) >= 2 {
			cseq64, _ = strconv.ParseUint(v[1], 10, 32)
		}

		status, err = parseNotifyBody(string(req.Body()))
		if err != nil {
			return "", 0, 0, err
		}

		return method, uint32(cseq64), status, nil
	}
	return "", 0, 0, psrpc.NewErrorf(psrpc.Unimplemented, "unknown event")
}
