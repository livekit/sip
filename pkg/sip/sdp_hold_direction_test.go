// Copyright 2026 LiveKit, Inc.
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

package sip

import (
	"strings"
	"testing"
)

// A carrier's HOLD re-INVITE, as captured on a live trunk (Plivo/genband SBC).
const holdOffer = "v=0\r\n" +
	"o=genband 1642851412 1894065156 IN IP4 203.0.113.20\r\n" +
	"s=-\r\n" +
	"c=IN IP4 203.0.113.20\r\n" +
	"t=0 0\r\n" +
	"m=audio 29076 RTP/AVP 0 8 101\r\n" +
	"c=IN IP4 203.0.113.20\r\n" +
	"a=rtpmap:0 PCMU/8000\r\n" +
	"a=sendonly\r\n" +
	"a=ptime:20\r\n"

// Our cached, already-negotiated local SDP — note a=sendrecv.
const cachedLocal = "v=0\r\n" +
	"o=- 9157931411869268290 9157931411869268294 IN IP4 203.0.113.10\r\n" +
	"s=LiveKit\r\n" +
	"c=IN IP4 203.0.113.10\r\n" +
	"t=0 0\r\n" +
	"m=audio 17830 RTP/AVP 0 101\r\n" +
	"a=rtpmap:0 PCMU/8000\r\n" +
	"a=rtpmap:101 telephone-event/8000\r\n" +
	"a=fmtp:101 0-16\r\n" +
	"a=ptime:20\r\n" +
	"a=sendrecv\r\n"

// TestHoldOfferIsAnsweredRecvonly is THE regression test.
//
// Before this fix, both re-INVITE paths replayed cachedLocal verbatim, so a sendonly (hold)
// offer was answered a=sendrecv. RFC 3264 §6.1 requires recvonly, and carriers BYE the dialog
// ~60ms after the invalid answer — which on a bridged/transferred call ends BOTH legs, so a
// supervisor pressing HOLD hung up the customer.
func TestHoldOfferIsAnsweredRecvonly(t *testing.T) {
	dir := answerDirectionFor([]byte(holdOffer))
	if dir != "recvonly" {
		t.Fatalf("a=sendonly offer must be answered recvonly (RFC 3264 §6.1), got %q", dir)
	}

	answer := string(withSDPDirection([]byte(cachedLocal), dir))
	if !strings.Contains(answer, "a=recvonly") {
		t.Fatalf("answer must carry a=recvonly:\n%s", answer)
	}
	if strings.Contains(answer, "a=sendrecv") {
		t.Fatalf("answer must NOT still say sendrecv — this is the bug that makes carriers BYE:\n%s", answer)
	}
	// Everything else about the negotiated body must survive untouched.
	for _, keep := range []string{
		"m=audio 17830 RTP/AVP 0 101",
		"a=rtpmap:0 PCMU/8000",
		"a=rtpmap:101 telephone-event/8000",
		"a=fmtp:101 0-16",
		"a=ptime:20",
		"c=IN IP4 203.0.113.10",
	} {
		if !strings.Contains(answer, keep) {
			t.Errorf("rewrite must not disturb %q:\n%s", keep, answer)
		}
	}
	if !strings.Contains(answer, "\r\n") {
		t.Error("CRLF line endings must be preserved (RFC 4566; strict SBCs care)")
	}
}

func TestDirectionMapping(t *testing.T) {
	cases := map[string]string{
		"a=sendonly": "recvonly",
		"a=recvonly": "sendonly",
		"a=inactive": "inactive",
		"a=sendrecv": "", // no rewrite needed
	}
	for offer, want := range cases {
		body := "v=0\r\nm=audio 1 RTP/AVP 0\r\n" + offer + "\r\n"
		if got := answerDirectionFor([]byte(body)); got != want {
			t.Errorf("offer %s -> got %q, want %q", offer, got, want)
		}
	}
}

// A re-INVITE with NO direction attribute (codec renegotiation, port change, session-timer
// refresh) must be answered exactly as before — sendrecv is implied by RFC 4566 §6, and this
// is the overwhelmingly common case. Regressing it would break every non-hold re-INVITE.
func TestNoDirectionInOfferLeavesSDPUntouched(t *testing.T) {
	offer := "v=0\r\nm=audio 29076 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"
	if dir := answerDirectionFor([]byte(offer)); dir != "" {
		t.Fatalf("absent direction must yield no rewrite, got %q", dir)
	}
	if got := string(withSDPDirection([]byte(cachedLocal), "")); got != cachedLocal {
		t.Fatal("empty direction must return the cached SDP byte-identical")
	}
}

// Unhold: the carrier re-offers sendrecv, and we must go back to sending audio.
func TestUnholdRestoresSendrecv(t *testing.T) {
	held := withSDPDirection([]byte(cachedLocal), "recvonly")
	if !strings.Contains(string(held), "a=recvonly") {
		t.Fatal("setup: expected held SDP to be recvonly")
	}
	// carrier unholds -> offer has a=sendrecv -> answerDirectionFor returns "" (no rewrite),
	// so the caller passes the cached (sendrecv) SDP through unchanged.
	if dir := answerDirectionFor([]byte(strings.Replace(holdOffer, "a=sendonly", "a=sendrecv", 1))); dir != "" {
		t.Fatalf("sendrecv offer needs no rewrite, got %q", dir)
	}
}

func TestDirectionLineAppendedWhenAbsentFromLocal(t *testing.T) {
	local := "v=0\r\nm=audio 17830 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"
	out := string(withSDPDirection([]byte(local), "recvonly"))
	if !strings.Contains(out, "a=recvonly") {
		t.Fatalf("direction must be appended when local has none:\n%s", out)
	}
	if strings.Count(out, "a=recvonly") != 1 {
		t.Fatalf("exactly one direction line expected:\n%s", out)
	}
}

func TestDuplicateDirectionLinesCollapseToOne(t *testing.T) {
	local := "v=0\r\na=sendrecv\r\nm=audio 1 RTP/AVP 0\r\na=sendrecv\r\n"
	out := string(withSDPDirection([]byte(local), "recvonly"))
	if strings.Count(out, "a=recvonly") != 1 {
		t.Fatalf("duplicate direction lines must collapse to one:\n%s", out)
	}
	if strings.Contains(out, "a=sendrecv") {
		t.Fatalf("no stale sendrecv may remain:\n%s", out)
	}
}
