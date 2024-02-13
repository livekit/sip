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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"time"

	"github.com/at-wat/ebml-go"
	"github.com/at-wat/ebml-go/webm"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v2"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/media/ulaw"
)

type ClientConfig struct {
	IP       string
	Number   string
	AuthUser string
	AuthPass string
	Log      *slog.Logger
}

func NewClient(conf ClientConfig) (*Client, error) {
	if conf.Log == nil {
		conf.Log = slog.Default()
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
	cli := &Client{conf: conf, log: conf.Log}
	cli.rtp = &rtp.Packet{
		Header: rtp.Header{
			Version: 2,
			SSRC:    5000,
		},
	}

	var err error
	cli.media, err = net.ListenUDP("udp", &net.UDPAddr{
		Port: 0,
		IP:   net.ParseIP("0.0.0.0"),
	})
	if err != nil {
		cli.Close()
		return nil, err
	}
	cli.mediaAddr = cli.media.LocalAddr().(*net.UDPAddr)
	conf.Log.Debug("media address", "addr", cli.mediaAddr)

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(conf.Number),
	)
	if err != nil {
		cli.Close()
		return nil, err
	}

	cli.sip, err = sipgo.NewClient(ua, sipgo.WithClientHostname(conf.IP))
	if err != nil {
		cli.Close()
		return nil, err
	}

	return cli, nil
}

type Client struct {
	conf       ClientConfig
	log        *slog.Logger
	media      *net.UDPConn
	mediaAddr  *net.UDPAddr
	mediaDst   *net.UDPAddr
	sip        *sipgo.Client
	inviteReq  *sip.Request
	inviteResp *sip.Response
	rtp        *rtp.Packet
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
	if c.sip != nil {
		c.sip.Close()
	}
	if c.media != nil {
		c.media.Close()
	}
}

func (c *Client) record(w io.WriteCloser) error {
	buf := make([]byte, 1500)

	ws, err := webm.NewSimpleBlockWriter(w,
		[]webm.TrackEntry{
			{
				Name:            "Audio",
				TrackNumber:     1,
				TrackUID:        12345,
				CodecID:         "A_PCM/INT/LIT",
				TrackType:       2,
				DefaultDuration: 20000000,
				Audio: &webm.Audio{
					SamplingFrequency: 8000.0,
					Channels:          1,
				},
			},
		})
	if err != nil {
		panic(err)
	}

	var audioTimestamp int64
	for {
		n, _, err := c.media.ReadFromUDP(buf)
		if err != nil {
			return err
		}

		var p rtp.Packet
		if err := p.Unmarshal(buf[:n]); err != nil {
			c.conf.Log.Warn("cannot parse rtp packet", "err", err)
			continue
		}

		audioTimestamp += 20
		decoded := ulaw.DecodeUlaw(p.Payload)

		out := make([]byte, 2*len(decoded))
		for _, sample := range decoded {
			out = append(out, byte(sample&0xff), byte(sample>>8))
		}

		if _, err := ws[0].Write(true, audioTimestamp, out); err != nil {
			return err
		}
	}
}

func (c *Client) Record(w io.WriteCloser) {
	go func() {
		if err := c.record(w); err != nil {
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
		req.Recipient.Port = 5060
	}

	if recordRouteHeader, ok := resp.RecordRoute(); ok {
		req.AppendHeader(&sip.RouteHeader{Address: recordRouteHeader.Address})
	}

	if err = c.sip.WriteRequest(sip.NewAckRequest(req, resp, nil)); err != nil {
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
	c.log.Debug("connected!")
	c.inviteReq = req
	c.inviteResp = resp
	c.mediaDst = dstAddr
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

	tx, err := c.sip.TransactionRequest(req)
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

	tx, err := c.sip.TransactionRequest(req)
	if err != nil {
		return
	}
	select {
	case <-tx.Done():
	case <-tx.Responses():
	}
}

func (c *Client) rtpNext() {
	c.rtp.Header.Timestamp += 160
	c.rtp.Header.SequenceNumber += 1
	// reset
	c.rtp.PayloadType = 0
	c.rtp.Payload = nil
	c.rtp.Marker = false
}

var dtmfCharToEvent = map[rune]byte{
	'0': 0, '1': 1, '2': 2, '3': 3, '4': 4,
	'5': 5, '6': 6, '7': 7, '8': 8, '9': 9,
	'*': 10, '#': 11,
	'a': 12, 'b': 13, 'c': 14, 'd': 15,
}

func (c *Client) SendDTMF(dtmf string) error {
	c.log.Debug("sending dtmf", "str", dtmf)
	data := make([]byte, 4)
	for _, r := range dtmf {
		// TODO: this is just enough for us to think it's DTMF; make a proper one later
		data[0] = dtmfCharToEvent[r]
		c.rtp.PayloadType = 101
		c.rtp.Marker = true
		c.rtp.Payload = data

		raw, err := c.rtp.Marshal()
		if err != nil {
			return err
		}
		c.rtpNext()

		if _, err = c.media.WriteTo(raw, c.mediaDst); err != nil {
			return err
		}
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
					Port:    sdp.RangedPort{Value: c.mediaAddr.Port},
					Protos:  []string{"RTP", "AVP"},
					Formats: []string{"0"},
				},
				Attributes: []sdp.Attribute{
					{Key: "rtpmap", Value: "0 PCMU/8000"},
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

	var audioFrames [][]byte
	for _, cluster := range ret.Segment.Cluster {
		for _, block := range cluster.SimpleBlock {
			audioFrames = append(audioFrames, block.Data...)
		}
	}

	i := 0
	for range time.NewTicker(20 * time.Millisecond).C {
		if i >= len(audioFrames) {
			break
		}

		c.rtp.PayloadType = 0
		c.rtp.Marker = false
		c.rtp.Payload = audioFrames[i]

		raw, err := c.rtp.Marshal()
		if err != nil {
			return err
		}

		if _, err = c.media.WriteTo(raw, c.mediaDst); err != nil {
			return err
		}

		c.rtpNext()
		i++
	}
	return nil
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
