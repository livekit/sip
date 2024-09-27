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

package siptest

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"net"
	"os"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/at-wat/ebml-go"
	"github.com/at-wat/ebml-go/webm"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/frostbyte73/core"
	"github.com/icholy/digest"
	"github.com/pion/sdp/v3"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/sip/pkg/audiotest"
	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/dtmf"
	"github.com/livekit/sip/pkg/media/g711"
	"github.com/livekit/sip/pkg/media/rtp"
	lksdp "github.com/livekit/sip/pkg/media/sdp"
	webmm "github.com/livekit/sip/pkg/media/webm"
)

type ClientConfig struct {
	IP             string
	Number         string
	AuthUser       string
	AuthPass       string
	Log            *slog.Logger
	OnBye          func()
	OnMediaTimeout func()
	OnDTMF         func(ev dtmf.Event)
	OnRefer        func(req *sip.Request)
	Codec          string
}

func NewClient(id string, conf ClientConfig) (*Client, error) {
	if conf.Log == nil {
		conf.Log = slog.Default()
	}
	if conf.OnMediaTimeout == nil {
		conf.OnMediaTimeout = func() {
			panic("media-timeout")
		}
	}
	if id != "" {
		conf.Log = conf.Log.With("id", id)
	}
	if conf.IP == "" {
		localIP, err := config.GetLocalIP()
		if err != nil {
			return nil, err
		}
		conf.IP = localIP
		conf.Log.Debug("setting local address", "ip", localIP)
	}
	if conf.Number == "" {
		conf.Number = "1000"
	}
	if conf.Codec == "" {
		conf.Codec = g711.ULawSDPName
	}
	codec := lksdp.CodecByName(conf.Codec).(rtp.AudioCodec)
	cli := &Client{
		id:         id,
		conf:       conf,
		ack:        make(chan struct{}, 1),
		log:        conf.Log,
		audioCodec: codec,
		audioType:  codec.Info().RTPDefType,
	}
	if !codec.Info().RTPIsStatic {
		cli.audioType = 102
	}
	cli.mediaConn = rtp.NewConn(&rtp.ConnConfig{TimeoutCallback: conf.OnMediaTimeout})
	cli.mediaConn.EnableTimeout(false) // enabled later
	cli.media = rtp.NewSeqWriter(cli.mediaConn)
	cli.mediaAudio = cli.media.NewStream(cli.audioType, codec.Info().RTPClockRate)
	cli.mediaDTMF = cli.media.NewStream(101, dtmf.SampleRate)
	cli.audioOut = cli.audioCodec.EncodeRTP(cli.mediaAudio)

	cli.setupRTPReceiver()

	err := cli.mediaConn.ListenAndServe(0, 0, "0.0.0.0")
	if err != nil {
		cli.Close()
		return nil, err
	}
	conf.Log.Info("media address", "addr", cli.mediaConn.LocalAddr())

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(conf.Number),
	)
	if err != nil {
		cli.Close()
		return nil, err
	}

	cli.sipClient, err = sipgo.NewClient(ua, sipgo.WithClientHostname(conf.IP))
	if err != nil {
		cli.Close()
		return nil, err
	}

	cli.sipServer, err = sipgo.NewServer(ua)
	if err != nil {
		cli.Close()
		return nil, err
	}

	cli.sipServer.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
		tx.Terminate()
		if conf.OnBye != nil {
			conf.OnBye()
		}
	})
	cli.sipServer.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		select {
		case cli.ack <- struct{}{}:
		default:
		}
	})
	cli.sipServer.OnRefer(func(req *sip.Request, tx sip.ServerTransaction) {
		if conf.OnRefer != nil {
			conf.OnRefer(req)
		}

		err = tx.Respond(sip.NewResponseFromRequest(req, 202, "Accepted", nil))
		logger.Errorw("REFER Respond", err)
		tx.Terminate()
	})

	return cli, nil
}

type Client struct {
	id            string
	conf          ClientConfig
	log           *slog.Logger
	ack           chan struct{}
	audioCodec    rtp.AudioCodec
	audioType     byte
	mediaConn     *rtp.Conn
	mux           *rtp.Mux
	media         *rtp.SeqWriter
	mediaAudio    *rtp.Stream
	mediaDTMF     *rtp.Stream
	audioOut      media.PCM16Writer
	sipClient     *sipgo.Client
	sipServer     *sipgo.Server
	inviteReq     *sip.Request
	inviteResp    *sip.Response
	recordHandler atomic.Pointer[rtp.Handler]
	lastCSeq      atomic.Uint32
	closed        core.Fuse
}

func (c *Client) LocalIP() string {
	return c.conf.IP
}

func (c *Client) RemoteHeaders() []sip.Header {
	if c.inviteResp == nil {
		return nil
	}
	return c.inviteResp.Headers()
}

func (c *Client) Close() {
	c.closed.Once(func() {
		if c.mediaConn != nil {
			c.mediaConn.Close()
		}
		if c.inviteResp != nil {
			c.sendBye()
			c.inviteReq = nil
			c.inviteResp = nil
		}
		if c.sipClient != nil {
			c.sipClient.Close()
		}
		if c.sipServer != nil {
			c.sipServer.Close()
		}
	})
}

func (c *Client) setupRTPReceiver() {
	var lastTs atomic.Uint32

	c.mux = rtp.NewMux(rtp.HandlerFunc(func(pck *rtp.Packet) error {
		lastTs.Store(pck.Timestamp)

		h := c.recordHandler.Load()
		if h != nil {
			return (*h).HandleRTP(pck)
		}
		return nil
	}))
	c.mux.Register(101, rtp.HandlerFunc(func(pck *rtp.Packet) error {
		ts := lastTs.Load()
		var diff int64
		if ts > 0 {
			diff = int64(pck.Timestamp) - int64(ts)
		}

		if diff > int64(c.audioCodec.Info().RTPClockRate) || diff < -int64(c.audioCodec.Info().RTPClockRate) {
			c.log.Info("reveived out of sync DTMF message", "dtmfTs", pck.Timestamp, "lastTs", ts)
			return nil
		}

		if c.conf.OnDTMF == nil {
			return nil
		}
		if ev, ok := dtmf.DecodeRTP(pck); ok {
			c.conf.OnDTMF(ev)
		}
		return nil
	}))

	c.mediaConn.OnRTP(c.mux)
}

func (c *Client) Record(w io.WriteCloser) {
	ws := webmm.NewPCM16Writer(w, c.audioCodec.Info().SampleRate, rtp.DefFrameDur)
	h := c.audioCodec.DecodeRTP(ws, c.audioType)
	c.recordHandler.Store(&h)
}

func (c *Client) Dial(ip string, uri string, number string, headers map[string]string) error {
	c.log.Debug("dialing SIP server", "ip", ip, "uri", uri, "number", number)
	offer, err := c.createOffer()
	if err != nil {
		return err
	}

	var (
		authHeaderVal = ""
		req           *sip.Request
		resp          *sip.Response
	)

	for {
		req, resp, err = c.attemptInvite(ip, uri, number, offer, authHeaderVal, headers)
		if err != nil {
			return err
		}

		if resp.StatusCode == 407 {
			c.log.Debug("auth requested")
			if c.conf.AuthUser == "" || c.conf.AuthPass == "" {
				return fmt.Errorf("server responded with 407, but no username or password was provided")
			}

			headerVal := resp.GetHeader("Proxy-Authenticate")
			challenge, err := digest.ParseChallenge(headerVal.Value())
			if err != nil {
				return err
			}

			toHeader, ok := resp.To()
			if !ok {
				return errors.New("no To header on Request")
			}

			cred, _ := digest.Digest(challenge, digest.Options{
				Method:   req.Method.String(),
				URI:      toHeader.Address.String(),
				Username: c.conf.AuthUser,
				Password: c.conf.AuthPass,
			})

			authHeaderVal = cred.String()
			// Compute digest and try again
			continue
		} else if resp.StatusCode != 200 {
			return fmt.Errorf("unexpected status from INVITE response %d", resp.StatusCode)
		}

		break
	}

	if contactHeader, ok := resp.Contact(); ok {
		req.Recipient = &contactHeader.Address
		if req.Recipient.Port == 0 {
			req.Recipient.Port = 5060
		}
	}

	if recordRouteHeader, ok := resp.RecordRoute(); ok {
		req.AppendHeader(&sip.RouteHeader{Address: recordRouteHeader.Address})
	}

	c.mediaConn.EnableTimeout(true)
	if err = c.sipClient.WriteRequest(sip.NewAckRequest(req, resp, nil)); err != nil {
		return err
	}
	ip, port, err := parseSDPAnswer(resp.Body())
	if err != nil {
		return err
	}
	dstAddr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", ip, port))
	if err != nil {
		return err
	}
	c.inviteReq = req
	c.inviteResp = resp

	if h, ok := req.CSeq(); ok {
		c.lastCSeq.Store(h.SeqNo)
	}

	c.mediaConn.SetDestAddr(dstAddr)
	c.log.Debug("client connected", "media-dst", dstAddr)
	return nil
}

func (c *Client) attemptInvite(ip, uri, number string, offer []byte, authHeader string, headers map[string]string) (*sip.Request, *sip.Response, error) {
	req := sip.NewRequest(sip.INVITE, &sip.Uri{User: number, Host: uri})
	req.SetDestination(ip)
	req.SetBody(offer)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.AppendHeader(sip.NewHeader("Contact", fmt.Sprintf("<sip:livekit@%s:5060>", c.conf.IP)))
	req.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, CANCEL, BYE, NOTIFY, REFER, MESSAGE, OPTIONS, INFO, SUBSCRIBE"))
	if c.id != "" {
		req.AppendHeader(sip.NewHeader("X-Lk-Test-Id", c.id))
	}

	if authHeader != "" {
		req.AppendHeader(sip.NewHeader("Proxy-Authorization", authHeader))
	}
	for k, v := range headers {
		req.AppendHeader(sip.NewHeader(k, v))
	}

	tx, err := c.sipClient.TransactionRequest(req)
	if err != nil {
		panic(err)
	}
	defer tx.Terminate()

	resp, err := getResponse(tx)

	return req, resp, err
}

func (c *Client) sendBye() {
	c.log.Debug("sending bye")
	req := sip.NewByeRequest(c.inviteReq, c.inviteResp, nil)
	req.AppendHeader(sip.NewHeader("User-Agent", "LiveKit"))

	cseq := c.lastCSeq.Add(1)
	cseqH, _ := req.CSeq()
	cseqH.SeqNo = cseq

	tx, err := c.sipClient.TransactionRequest(req)
	if err != nil {
		return
	}
	defer tx.Terminate()
	select {
	case <-c.ack:
	case <-tx.Done():
	case r := <-tx.Responses():
		if r.StatusCode == 200 {
			_ = c.sipClient.WriteRequest(sip.NewAckRequest(req, r, nil))
		}
	}
}

func (c *Client) SendDTMF(digits string) error {
	c.log.Debug("sending dtmf", "str", digits)
	return dtmf.Write(context.Background(), c.audioOut, c.mediaDTMF, c.mediaAudio.GetCurrentTimestamp(), digits)
}

func (c *Client) SendNotify(eventReq *sip.Request, notifyStatus string) error {
	var recipient *sip.Uri

	if contact, ok := eventReq.Contact(); ok {
		recipient = &contact.Address
	} else if from, ok := eventReq.From(); ok {
		recipient = &from.Address
	} else {
		errors.New("missing destination address")
	}

	req := sip.NewRequest(sip.NOTIFY, recipient)

	req.SipVersion = eventReq.SipVersion
	sip.CopyHeaders("Via", eventReq, req)

	if len(eventReq.GetHeaders("Route")) > 0 {
		sip.CopyHeaders("Route", eventReq, req)
	} else {
		hdrs := c.inviteResp.GetHeaders("Record-Route")
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

	if to, ok := eventReq.To(); ok {
		req.AppendHeader((*sip.FromHeader)(to))
	} else {
		return errors.New("missing To header in REFER request")
	}

	if from, ok := eventReq.From(); ok {
		req.AppendHeader((*sip.ToHeader)(from))
	} else {
		return errors.New("missing From header in REFER request")
	}

	if callId, ok := eventReq.CallID(); ok {
		req.AppendHeader(callId)
	}

	ct := sip.ContentTypeHeader("message/sipfrag")
	req.AppendHeader(&ct)

	cseq := c.lastCSeq.Add(1)
	cseqH := &sip.CSeqHeader{
		SeqNo:      cseq,
		MethodName: sip.NOTIFY,
	}
	req.AppendHeader(cseqH)

	req.SetTransport(eventReq.Transport())
	req.SetSource(eventReq.Destination())
	req.SetDestination(eventReq.Source())

	if eventCSeq, ok := eventReq.CSeq(); ok {
		req.AppendHeader(sip.NewHeader("Event", fmt.Sprintf("refer;id=%d", eventCSeq.SeqNo)))
	} else {
		return errors.New("missing CSeq header in REFER request")
	}

	req.SetBody([]byte(notifyStatus))

	tx, err := c.sipClient.TransactionRequest(req)
	if err != nil {
		return err
	}
	defer tx.Terminate()

	resp, err := getResponse(tx)
	if err != nil {
		return err
	}

	if resp.StatusCode != sip.StatusOK {
		return fmt.Errorf("NOTIFY failed with status %d", resp.StatusCode)
	}

	return nil
}

func (c *Client) createOffer() ([]byte, error) {
	sessionId := rand.Uint64()

	offer := sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      sessionId,
			SessionVersion: sessionId,
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: c.conf.IP,
		},
		SessionName: "LiveKit",
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: c.conf.IP},
		},
		TimeDescriptions: []sdp.TimeDescription{
			{
				Timing: sdp.Timing{
					StartTime: 0,
					StopTime:  0,
				},
			},
		},
		MediaDescriptions: []*sdp.MediaDescription{
			{
				MediaName: sdp.MediaName{
					Media:   "audio",
					Port:    sdp.RangedPort{Value: c.mediaConn.LocalAddr().Port},
					Protos:  []string{"RTP", "AVP"},
					Formats: []string{strconv.Itoa(int(c.audioType)) + " 101"},
				},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: fmt.Sprintf("%d %s", c.audioType, c.audioCodec.Info().SDPName)},
					{Key: "rtpmap", Value: "101 " + dtmf.SDPName},
				},
			},
		},
	}

	return offer.Marshal()
}

func (c *Client) SendAudio(path string) error {
	f, err := os.Open(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	var ret struct {
		Header  webm.EBMLHeader `ebml:"EBML"`
		Segment webm.Segment    `ebml:"Segment"`
	}
	if err := ebml.Unmarshal(f, &ret); err != nil {
		return err
	}

	var audioFrames []media.PCM16Sample
	for _, cluster := range ret.Segment.Cluster {
		for _, block := range cluster.SimpleBlock {
			for _, frame := range block.Data {
				data := make(media.PCM16Sample, len(frame)/2)
				for i := 0; i < len(frame); i += 2 {
					data[i/2] = int16(binary.LittleEndian.Uint16(frame[i:]))
				}
				audioFrames = append(audioFrames, data)
			}
		}
	}

	i := 0
	w := c.audioOut
	for range time.NewTicker(rtp.DefFrameDur).C {
		if i >= len(audioFrames) {
			break
		}
		if err = w.WriteSample(audioFrames[i]); err != nil {
			return err
		}
		i++
	}
	return nil
}

func (c *Client) SendSilence(ctx context.Context) error {
	const framesPerSec = int(time.Second / rtp.DefFrameDur)
	buf := make(media.PCM16Sample, c.audioCodec.Info().SampleRate/framesPerSec)
	wr := c.audioOut

	ticker := time.NewTicker(rtp.DefFrameDur)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		if err := wr.WriteSample(buf); err != nil {
			return err
		}
	}
}

const (
	signalAmp    = math.MaxInt16 / 4
	signalAmpMin = signalAmp - signalAmp/4 // TODO: why it's so low?
	signalAmpMax = signalAmp + signalAmp/10
)

// SendSignal generate an audio signal with a given value. It repeats the signal n times, each frame containing one signal.
// If n <= 0, it will send the signal until the context is cancelled.
func (c *Client) SendSignal(ctx context.Context, n int, val int) error {
	const framesPerSec = int(time.Second / rtp.DefFrameDur)
	signal := make(media.PCM16Sample, c.audioCodec.Info().SampleRate/framesPerSec)
	audiotest.GenSignal(signal, []audiotest.Wave{{Ind: val, Amp: signalAmp}})
	wr := c.audioOut
	c.log.Info("sending signal", "len", len(signal), "n", n, "sig", val)

	ticker := time.NewTicker(rtp.DefFrameDur)
	defer ticker.Stop()
	i := 0
	for {
		if n > 0 && i >= n {
			break
		}
		select {
		case <-ctx.Done():
			if n <= 0 {
				c.log.Debug("stopping signal", "n", i, "sig", val)
				return nil
			}
			return ctx.Err()
		case <-ticker.C:
		}

		if err := wr.WriteSample(signal); err != nil {
			return err
		}
		i++
	}
	return nil
}

// WaitSignals waits for an audio frame to contain all signals.
func (c *Client) WaitSignals(ctx context.Context, vals []int, w io.WriteCloser) error {
	sampleRate := c.audioCodec.Info().SampleRate
	var ws media.PCM16Writer
	if w != nil {
		ws = webmm.NewPCM16Writer(w, sampleRate, rtp.DefFrameDur)
		defer ws.Close()
	}
	const framesPerSec = int(time.Second / rtp.DefFrameDur)
	decoded := make(media.PCM16Sample, sampleRate/framesPerSec)
	dec := c.audioCodec.DecodeRTP(media.NewPCM16BufferWriter(&decoded, sampleRate), c.audioType)
	lastLog := time.Now()

	pkts := make(chan *rtp.Packet, 1)
	done := make(chan struct{})

	h := rtp.Handler(rtp.HandlerFunc(func(pkt *rtp.Packet) error {
		// Make sure er do not send on a closed channel
		select {
		case <-done:
			return ctx.Err()
		default:
		}

		select {
		case <-ctx.Done():
			close(pkts)
			close(done)
			return ctx.Err()
		case pkts <- pkt:
		}

		return nil
	}))
	c.recordHandler.Store(&h)

	for p := range pkts {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if p.PayloadType != c.audioType {
			c.log.Debug("skipping payload", "type", p.PayloadType)
			continue
		}
		decoded = decoded[:0]
		if err := dec.HandleRTP(p); err != nil {
			return err
		}
		if ws != nil {
			if err := ws.WriteSample(decoded); err != nil {
				return err
			}
		}
		if !slices.ContainsFunc(decoded, func(v int16) bool { return v != 0 }) {
			continue // Ignore silence.
		}
		out := audiotest.FindSignal(decoded)
		if len(out) >= len(vals) {
			// Only consider first N strongest signals.
			out = out[:len(vals)]
			// Sort them again by index, so it's easier to compare.
			slices.SortFunc(out, func(a, b audiotest.Wave) int {
				return a.Ind - b.Ind
			})
			ok := true
			for i := range vals {
				// All signals must match the frequency and have around the same amplitude.
				if out[i].Ind != vals[i] || out[i].Amp < signalAmpMin || out[i].Amp > signalAmpMax {
					ok = false
					break
				}
			}
			if ok {
				c.log.Debug("signal found", "sig", vals)
				return nil
			}
		}
		// Remove most other components from the logs.
		if len(out) > len(vals)*2 {
			out = out[:len(vals)*2]
		}
		if time.Since(lastLog) > time.Second {
			lastLog = time.Now()
			c.log.Debug("skipping signal", "len", len(decoded), "signals", out)
		}
	}

	return nil
}

func getResponse(tx sip.ClientTransaction) (*sip.Response, error) {
	cnt := 0
	for {
		select {
		case <-tx.Done():
			return nil, fmt.Errorf("transaction failed to complete (%d intermediate responses)", cnt)
		case res := <-tx.Responses():
			switch res.StatusCode {
			default:
				return res, nil
			case 100, 180, 183:
				// continue
				cnt++
			}
		}
	}
}

func parseSDPAnswer(in []byte) (string, int, error) {
	offer := sdp.SessionDescription{}
	if err := offer.Unmarshal(in); err != nil {
		return "", 0, err
	}

	return offer.ConnectionInformation.Address.Address, offer.MediaDescriptions[0].MediaName.Port.Value, nil
}
