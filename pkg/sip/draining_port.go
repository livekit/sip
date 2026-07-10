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
	"errors"
	"net"
	"os"
	"time"

	"github.com/livekit/protocol/logger"
)

func DrainPort(log logger.Logger, conn *net.UDPConn, idleTimeout, maxDuration time.Duration) {
	defer conn.Close()
	if idleTimeout <= 0 && maxDuration <= 0 {
		return
	}
	log = log.WithValues("local", conn.LocalAddr(), "idleTimeout", idleTimeout, "maxDuration", maxDuration)
	if idleTimeout <= 0 {
		idleTimeout = maxDuration + (1 * time.Second)
	}

	buf := make([]byte, 1500)
	var packets uint64
	var maxDeadline time.Time
	var lastPacket time.Time
	if maxDuration > 0 {
		maxDeadline = time.Now().Add(maxDuration)
	}
	start := time.Now()
	log.Debugw("draining port started")
	for {
		currentDeadline := time.Now().Add(idleTimeout)
		if !maxDeadline.IsZero() && maxDeadline.Before(currentDeadline) {
			currentDeadline = maxDeadline
		}
		_ = conn.SetReadDeadline(currentDeadline)
		_, _, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			now := time.Now()
			logFunc := log.Debugw
			reason := "error"
			if errors.Is(err, os.ErrDeadlineExceeded) {
				if !maxDeadline.IsZero() && now.After(maxDeadline) {
					reason = "maxDuration"
					logFunc = log.Infow
				} else {
					reason = "idleTimeout"
				}
			}
			since := time.Duration(0)
			if !lastPacket.IsZero() {
				since = now.Sub(lastPacket)
			}
			logFunc("draining port released", "error", err, "reason", reason, "packets",
				packets, "drainDuration", now.Sub(start), "sinceLastPacket", since)
			return
		}
		packets++
		lastPacket = time.Now()
	}
}
