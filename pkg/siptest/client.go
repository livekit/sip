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
	"time"

	"github.com/at-wat/ebml-go"
	"github.com/at-wat/ebml-go/webm"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"github.com/pion/sdp/v2"

	"github.com/livekit/sip/pkg/audiotest"
	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/dtmf"
	"github.com/livekit/sip/pkg/media/rtp"
	lksdp "github.com/livekit/sip/pkg/media/sdp"
	"github.com/livekit/sip/pkg/media/ulaw"
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
		conf.Codec = ulaw.SDPName
	}
	codec := lksdp.CodecByName(conf.Codec).(rtp.AudioCodec)
	cli := &Client{
		conf:       conf,
		ack:        make(chan struct{}, 1),
		log:        conf.Log,
		audioCodec: codec,
		audioType:  codec.Info().RTPDefType,
	}
	if !codec.Info().RTPIsStatic {
		cli.audioType = 102
	}
	cli.mediaConn = rtp.NewConn(conf.OnMediaTimeout)
	cli.media = rtp.NewSeqWriter(cli.mediaConn)
	cli.mediaAudio = cli.media.NewStream(cli.audioType)
	cli.mediaDTMF = cli.media.NewStream(101)

	err := cli.mediaConn.Listen(0, 0, "0.0.0.0")
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

	return cli, nil
}

type Client struct {
	conf       ClientConfig
	log        *slog.Logger
	ack        chan struct{}
	audioCodec rtp.AudioCodec
	audioType  byte
	mediaConn  *rtp.Conn
	media      *rtp.SeqWriter
	mediaAudio *rtp.Stream
	mediaDTMF  *rtp.Stream
	sipClient  *sipgo.Client
	sipServer  *sipgo.Server
	inviteReq  *sip.Request
	inviteResp *sip.Response
}

func (c *Client) LocalIP() string {
	return c.conf.IP
}

func (c *Client) Close() {
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
	if c.media != nil {
		c.mediaConn.Close()
	}
}

func (c *Client) record(w io.WriteCloser) error {
	ws := webmm.NewPCM16Writer(w, rtp.DefSampleRate, rtp.DefFrameDur)
	h := c.audioCodec.DecodeRTP(ws, c.audioType)
	for {
		p, _, err := c.mediaConn.ReadRTP()
		if err != nil {
			c.log.Error("cannot read rtp packet", "err", err)
			return err
		}
		if err = h.HandleRTP(p); err != nil {
			return err
		}
	}
}

func (c *Client) Record(w io.WriteCloser) {
	go func() {
		if err := c.record(w); err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}

			panic(err)
		}
	}()
}

func (c *Client) Dial(ip string, uri string, number string) error {
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
		req, resp, err = c.attemptInvite(ip, uri, number, offer, authHeaderVal)
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
	c.mediaConn.SetDestAddr(dstAddr)
	c.log.Debug("client connected", "media-dst", dstAddr)
	return nil
}

func (c *Client) attemptInvite(ip, uri, number string, offer []byte, authHeader string) (*sip.Request, *sip.Response, error) {
	req := sip.NewRequest(sip.INVITE, &sip.Uri{User: number, Host: uri})
	req.SetDestination(ip)
	req.SetBody(offer)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.AppendHeader(sip.NewHeader("Contact", fmt.Sprintf("<sip:livekit@%s:5060>", c.conf.IP)))
	req.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, CANCEL, BYE, NOTIFY, REFER, MESSAGE, OPTIONS, INFO, SUBSCRIBE"))

	if authHeader != "" {
		req.AppendHeader(sip.NewHeader("Proxy-Authorization", authHeader))
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
	w := c.audioCodec.EncodeRTP(c.mediaAudio)
	return dtmf.Write(context.Background(), w, c.mediaDTMF, digits)
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
	w := c.audioCodec.EncodeRTP(c.mediaAudio)
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

const (
	signalAmp    = math.MaxInt16 / 4
	signalAmpMin = signalAmp - signalAmp/4 // TODO: why it's so low?
	signalAmpMax = signalAmp + signalAmp/10
)

// SendSignal generate an audio signal with a given value. It repeats the signal n times, each frame containing one signal.
// If n <= 0, it will send the signal until the context is cancelled.
func (c *Client) SendSignal(ctx context.Context, n int, val int) error {
	signal := make(media.PCM16Sample, rtp.DefPacketDur)
	audiotest.GenSignal(signal, []audiotest.Wave{{Ind: val, Amp: signalAmp}})
	wr := c.audioCodec.EncodeRTP(c.mediaAudio)
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
	var ws media.PCM16WriteCloser
	if w != nil {
		ws = webmm.NewPCM16Writer(w, rtp.DefSampleRate, rtp.DefFrameDur)
		defer ws.Close()
	}
	decoded := make(media.PCM16Sample, rtp.DefPacketDur)
	dec := c.audioCodec.DecodeRTP(&decoded, c.audioType)
	lastLog := time.Now()
	for {
		p, _, err := c.mediaConn.ReadRTP()
		if err != nil {
			c.log.Error("cannot read rtp packet", "err", err)
			return err
		}
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
		if err = dec.HandleRTP(p); err != nil {
			return err
		}
		if ws != nil {
			if err = ws.WriteSample(decoded); err != nil {
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
}

func getResponse(tx sip.ClientTransaction) (*sip.Response, error) {
	select {
	case <-tx.Done():
		return nil, errors.New("transaction failed to complete")
	case res := <-tx.Responses():
		if res.StatusCode == 100 || res.StatusCode == 180 || res.StatusCode == 183 {
			return getResponse(tx)
		}
		return res, nil
	}
}

func parseSDPAnswer(in []byte) (string, int, error) {
	offer := sdp.SessionDescription{}
	if err := offer.Unmarshal(in); err != nil {
		return "", 0, err
	}

	return offer.ConnectionInformation.Address.Address, offer.MediaDescriptions[0].MediaName.Port.Value, nil
}
