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

package ingest

import (
	"net"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/videobridge/stats"
)

// RTCPHandler manages RTCP communication with the SIP endpoint.
// It sends periodic receiver reports and forwards keyframe requests (PLI/FIR)
// from the LiveKit side to the SIP endpoint.
type RTCPHandler struct {
	log  logger.Logger
	conn *net.UDPConn

	remoteAddr *net.UDPAddr
	localSSRC  uint32
	remoteSSRC atomic.Uint32

	// FIR sequence number (incremented per request)
	firSeq atomic.Uint32

	// Stats for receiver reports
	packetsReceived atomic.Uint64
	packetsLost     atomic.Uint32
	lastSeq         atomic.Uint32
	jitter          atomic.Uint32

	closed atomic.Bool
}

// NewRTCPHandler creates a new RTCP handler.
// rtcpConn is a UDP connection for RTCP (typically RTP port + 1).
func NewRTCPHandler(log logger.Logger, conn *net.UDPConn, localSSRC uint32) *RTCPHandler {
	return &RTCPHandler{
		log:       log,
		conn:      conn,
		localSSRC: localSSRC,
	}
}

// SetRemoteAddr sets the RTCP destination address for the SIP endpoint.
func (h *RTCPHandler) SetRemoteAddr(addr *net.UDPAddr) {
	h.remoteAddr = addr
}

// SetRemoteSSRC updates the remote SSRC (from received RTP packets).
func (h *RTCPHandler) SetRemoteSSRC(ssrc uint32) {
	h.remoteSSRC.Store(ssrc)
}

// UpdateStats updates RTP reception stats for inclusion in receiver reports.
func (h *RTCPHandler) UpdateStats(packetsReceived uint64, lastSeq uint32, packetsLost uint32, jitter uint32) {
	h.packetsReceived.Store(packetsReceived)
	h.lastSeq.Store(lastSeq)
	h.packetsLost.Store(packetsLost)
	h.jitter.Store(jitter)
}

// RequestKeyframe sends a PLI (Picture Loss Indication) to the SIP endpoint,
// requesting it to send a new keyframe. This is called when LiveKit needs
// a keyframe (e.g., new subscriber joins).
func (h *RTCPHandler) RequestKeyframe() error {
	if h.remoteAddr == nil || h.closed.Load() {
		return nil
	}

	remoteSSRC := h.remoteSSRC.Load()
	if remoteSSRC == 0 {
		h.log.Debugw("skipping PLI: remote SSRC not yet known")
		return nil
	}

	pli := &rtcp.PictureLossIndication{
		SenderSSRC: h.localSSRC,
		MediaSSRC:  remoteSSRC,
	}

	data, err := pli.Marshal()
	if err != nil {
		return err
	}

	_, err = h.conn.WriteToUDP(data, h.remoteAddr)
	if err != nil {
		h.log.Warnw("failed to send PLI", err)
		return err
	}

	stats.KeyframeRequests.Inc()
	h.log.Debugw("sent PLI to SIP endpoint", "remoteSSRC", remoteSSRC)
	return nil
}

// RequestKeyframeFIR sends a FIR (Full Intra Request) to the SIP endpoint.
// FIR is an older mechanism than PLI but is more widely supported by SIP devices.
func (h *RTCPHandler) RequestKeyframeFIR() error {
	if h.remoteAddr == nil || h.closed.Load() {
		return nil
	}

	remoteSSRC := h.remoteSSRC.Load()
	if remoteSSRC == 0 {
		return nil
	}

	seq := h.firSeq.Add(1)
	fir := &rtcp.FullIntraRequest{
		SenderSSRC: h.localSSRC,
		MediaSSRC:  remoteSSRC,
		FIR: []rtcp.FIREntry{
			{
				SSRC:           remoteSSRC,
				SequenceNumber: uint8(seq),
			},
		},
	}

	data, err := fir.Marshal()
	if err != nil {
		return err
	}

	_, err = h.conn.WriteToUDP(data, h.remoteAddr)
	if err != nil {
		h.log.Warnw("failed to send FIR", err)
		return err
	}

	stats.KeyframeRequests.Inc()
	h.log.Debugw("sent FIR to SIP endpoint", "remoteSSRC", remoteSSRC, "seq", seq)
	return nil
}

// StartReceiverReports begins sending periodic RTCP receiver reports to the SIP endpoint.
// This is important for the SIP endpoint to monitor quality and adjust its sending behavior.
func (h *RTCPHandler) StartReceiverReports(interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for range ticker.C {
			if h.closed.Load() {
				return
			}
			if err := h.sendReceiverReport(); err != nil {
				h.log.Debugw("failed to send receiver report", "error", err)
			}
		}
	}()
}

func (h *RTCPHandler) sendReceiverReport() error {
	if h.remoteAddr == nil {
		return nil
	}

	remoteSSRC := h.remoteSSRC.Load()
	if remoteSSRC == 0 {
		return nil
	}

	rr := &rtcp.ReceiverReport{
		SSRC: h.localSSRC,
		Reports: []rtcp.ReceptionReport{
			{
				SSRC:               remoteSSRC,
				LastSequenceNumber: h.lastSeq.Load(),
				TotalLost:          h.packetsLost.Load(),
				Jitter:             h.jitter.Load(),
			},
		},
	}

	data, err := rr.Marshal()
	if err != nil {
		return err
	}

	_, err = h.conn.WriteToUDP(data, h.remoteAddr)
	return err
}

// Close stops the RTCP handler.
func (h *RTCPHandler) Close() {
	h.closed.Store(true)
}
