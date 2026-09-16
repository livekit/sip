// Copyright 2026 LiveKit, Inc.
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
	"bytes"
	"strings"
)

// answerDirectionFor returns the answer direction for a hold/unhold offer.
// Only sendonly→recvonly is handled today; media_port hold support matches that.
// sendrecv restores bidirectional direction if a prior answer left recvonly cached.
// Empty means leave the cached local SDP direction alone.
// Callers must reject unsupportedHoldOffer directions before answering.
func answerDirectionFor(offer []byte) string {
	switch offerDirection(offer) {
	case "sendonly":
		return "recvonly"
	case "sendrecv":
		return "sendrecv"
	default:
		return ""
	}
}

// unsupportedHoldOffer is true for recvonly/inactive offers.
// media_port only implements sendonly hold; answering sendrecv would be invalid.
func unsupportedHoldOffer(offer []byte) bool {
	switch offerDirection(offer) {
	case "recvonly", "inactive":
		return true
	default:
		return false
	}
}

func offerDirection(sdp []byte) string {
	var found string
	for _, line := range sdpLines(sdp) {
		switch line {
		case "a=sendonly", "a=recvonly", "a=sendrecv", "a=inactive":
			found = strings.TrimPrefix(line, "a=")
		}
	}
	return found
}

// withSDPDirection sets the media direction attribute on a cached local SDP.
// dir empty returns local unchanged.
func withSDPDirection(local []byte, dir string) []byte {
	if dir == "" || len(local) == 0 {
		return local
	}
	nl := "\n"
	if bytes.Contains(local, []byte("\r\n")) {
		nl = "\r\n"
	}
	attr := "a=" + dir
	var out []byte
	replaced := false
	for _, line := range sdpLines(local) {
		switch line {
		case "a=sendonly", "a=recvonly", "a=sendrecv", "a=inactive":
			if replaced {
				continue
			}
			out = append(out, []byte(attr)...)
			out = append(out, []byte(nl)...)
			replaced = true
		default:
			out = append(out, []byte(line)...)
			out = append(out, []byte(nl)...)
		}
	}
	if !replaced {
		out = append(out, []byte(attr)...)
		out = append(out, []byte(nl)...)
	}
	return out
}

func sdpLines(sdp []byte) []string {
	s := string(sdp)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	raw := strings.Split(s, "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		if line == "" && len(lines) == len(raw)-1 {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
