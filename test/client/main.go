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

package main

import (
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
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

var (
	sipServer    = flag.String("sip-server", "", "SIP server to connect to")
	to           = flag.String("to", "+15550100000", "number to dial")
	from         = flag.String("from", "+15550100001", "client number")
	username     = flag.String("username", "", "username for INVITE")
	password     = flag.String("password", "", "password for INVITE")
	sipUri       = flag.String("sip-uri", "example.pstn.twilio.com", "SIP server URI")
	filePathPlay = flag.String("play", "audio.mkv", "play audio")
	filePathSave = flag.String("save", "save.mkv", "save incoming audio to file")
	sendDTMF     = flag.String("dtmf", "", "send DTMF sequence")

	localIP = ""
)

func startMediaListener() *net.UDPConn {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{
		Port: 0,
		IP:   net.ParseIP("0.0.0.0"),
	})
	if err != nil {
		panic(err)
	}

	go func() {
		var (
			p              rtp.Packet
			audioTimestamp int64
		)
		buf := make([]byte, 1500)

		w, err := os.OpenFile(*filePathSave, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			panic(err)
		}

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

		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}

			p = rtp.Packet{}
			if err := p.Unmarshal(buf[:n]); err != nil {
				continue
			}

			audioTimestamp += 20
			decoded := ulaw.DecodeUlaw(p.Payload)
			out := []byte{}
			for _, sample := range decoded {
				out = append(out, byte(sample&0xff))
				out = append(out, byte(sample>>8))
			}

			if _, err := ws[0].Write(true, audioTimestamp, out); err != nil {
				panic(err)
			}
		}
	}()

	return conn
}

func getResponse(tx sip.ClientTransaction) *sip.Response {
	select {
	case <-tx.Done():
		panic("transaction failed to complete")
	case res := <-tx.Responses():
		if res.StatusCode == 100 || res.StatusCode == 180 || res.StatusCode == 183 {
			return getResponse(tx)
		}

		return res
	}
}

func createOffer(port int) ([]byte, error) {
	sessionId := rand.Uint64()

	offer := sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      sessionId,
			SessionVersion: sessionId,
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: localIP,
		},
		SessionName: "LiveKit",
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: localIP},
		},
		TimeDescriptions: []sdp.TimeDescription{
			sdp.TimeDescription{
				Timing: sdp.Timing{
					StartTime: 0,
					StopTime:  0,
				},
			},
		},
		MediaDescriptions: []*sdp.MediaDescription{
			&sdp.MediaDescription{
				MediaName: sdp.MediaName{
					Media:   "audio",
					Port:    sdp.RangedPort{Value: port},
					Protos:  []string{"RTP", "AVP"},
					Formats: []string{"0"},
				},
				Attributes: []sdp.Attribute{
					sdp.Attribute{Key: "rtpmap", Value: "0 PCMU/8000"},
				},
			},
		},
	}

	return offer.Marshal()
}

func parseAnswer(in []byte) (string, int) {
	offer := sdp.SessionDescription{}
	if err := offer.Unmarshal(in); err != nil {
		panic(err)
	}

	return offer.ConnectionInformation.Address.Address, offer.MediaDescriptions[0].MediaName.Port.Value
}

var rtpPkt = &rtp.Packet{
	Header: rtp.Header{
		Version: 2,
		SSRC:    5000,
	},
}

func rtpNext() {
	rtpPkt.Header.Timestamp += 160
	rtpPkt.Header.SequenceNumber += 1
}

func sendAudioPackets(conn *net.UDPConn, dstAddr *net.UDPAddr, path string) {
	slog.Info("playing audio", "file", path)

	r, err := os.Open(path)
	if err != nil {
		panic(err)
	}
	defer r.Close()

	var ret struct {
		Header  webm.EBMLHeader `ebml:"EBML"`
		Segment webm.Segment    `ebml:"Segment"`
	}
	if err := ebml.Unmarshal(r, &ret); err != nil {
		panic(err)
	}

	audioFrames := [][]byte{}
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

		rtpPkt.PayloadType = 0
		rtpPkt.Marker = false
		rtpPkt.Payload = audioFrames[i]

		raw, err := rtpPkt.Marshal()
		if err != nil {
			panic(err)
		}

		if _, err = conn.WriteTo(raw, dstAddr); err != nil {
			return
		}

		rtpNext()
		i++
	}
}

var dtmfCharToEvent = map[rune]byte{
	'0': 0, '1': 1, '2': 2, '3': 3, '4': 4,
	'5': 5, '6': 6, '7': 7, '8': 8, '9': 9,
	'*': 10, '#': 11,
	'a': 12, 'b': 13, 'c': 14, 'd': 15,
}

func sendDTMFPackets(conn *net.UDPConn, dstAddr *net.UDPAddr, dtmf string) {
	slog.Info("sending dtmf", "str", dtmf)
	data := make([]byte, 4)
	for _, r := range dtmf {
		// TODO: this is just enough for us to think it's DTMF; make a proper one later
		data[0] = dtmfCharToEvent[r]
		rtpPkt.PayloadType = 101
		rtpPkt.Marker = true
		rtpPkt.Payload = data

		raw, err := rtpPkt.Marshal()
		if err != nil {
			panic(err)
		}

		if _, err = conn.WriteTo(raw, dstAddr); err != nil {
			return
		}
		rtpNext()
	}
}

func attemptInvite(sipClient *sipgo.Client, offer []byte, authorizationHeaderValue string) (*sip.Request, *sip.Response) {
	inviteRecipent := &sip.Uri{User: *to, Host: *sipUri}
	inviteRequest := sip.NewRequest(sip.INVITE, inviteRecipent)
	inviteRequest.SetDestination(*sipServer)
	inviteRequest.SetBody(offer)
	inviteRequest.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	inviteRequest.AppendHeader(sip.NewHeader("Contact", fmt.Sprintf("<sip:livekit@%s:5060>", localIP)))
	inviteRequest.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, CANCEL, BYE, NOTIFY, REFER, MESSAGE, OPTIONS, INFO, SUBSCRIBE"))

	if authorizationHeaderValue != "" {
		inviteRequest.AppendHeader(sip.NewHeader("Proxy-Authorization", authorizationHeaderValue))
	}

	tx, err := sipClient.TransactionRequest(inviteRequest)
	if err != nil {
		panic(err)
	}
	defer tx.Terminate()

	return inviteRequest, getResponse(tx)
}

func main() {
	flag.Parse()

	var err error
	localIP, err = config.GetLocalIP()
	if err != nil {
		panic(err)
	}
	slog.Info("local address", "addr", localIP)

	if *sipServer == "" {
		*sipServer = localIP + ":5060"
	}
	slog.Info("server address", "addr", *sipServer)

	mediaConn := startMediaListener()
	maddr := mediaConn.LocalAddr().(*net.UDPAddr)
	slog.Info("media address", "addr", maddr)
	offer, err := createOffer(maddr.Port)
	if err != nil {
		panic(err)
	}

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(*from),
	)
	if err != nil {
		panic(err)
	}

	sipClient, err := sipgo.NewClient(ua, sipgo.WithClientHostname(localIP))
	if err != nil {
		panic(err)
	}

	var (
		authorizationHeaderValue = ""
		inviteResponse           *sip.Response
		inviteRequest            *sip.Request
	)

	for {
		slog.Info("sending invite")
		inviteRequest, inviteResponse = attemptInvite(sipClient, offer, authorizationHeaderValue)

		if inviteResponse.StatusCode == 407 {
			slog.Info("auth requested")
			if *username == "" || *password == "" {
				panic("Server responded with 407, but no username or password was provided")
			}

			headerVal := inviteResponse.GetHeader("Proxy-Authenticate")
			challenge, err := digest.ParseChallenge(headerVal.Value())
			if err != nil {
				panic(err)
			}

			toHeader, ok := inviteResponse.To()
			if !ok {
				panic("No To Header on Request")
			}

			cred, _ := digest.Digest(challenge, digest.Options{
				Method:   inviteRequest.Method.String(),
				URI:      toHeader.Address.String(),
				Username: *username,
				Password: *password,
			})

			authorizationHeaderValue = cred.String()
			// Compute digest and try again
			continue
		} else if inviteResponse.StatusCode != 200 {
			panic(fmt.Sprintf("Unexpected StatusCode from INVITE response %d", inviteResponse.StatusCode))
		}

		break
	}

	if contactHeader, ok := inviteResponse.Contact(); ok {
		inviteRequest.Recipient = &contactHeader.Address
		inviteRequest.Recipient.Port = 5060
	}

	if recordRouteHeader, ok := inviteResponse.RecordRoute(); ok {
		inviteRequest.AppendHeader(&sip.RouteHeader{Address: recordRouteHeader.Address})
	}

	if err = sipClient.WriteRequest(sip.NewAckRequest(inviteRequest, inviteResponse, nil)); err != nil {
		panic(err)
	}
	slog.Info("connected")

	sendBye := func() {
		req := sip.NewByeRequest(inviteRequest, inviteResponse, nil)
		inviteRequest.AppendHeader(sip.NewHeader("User-Agent", "LiveKit"))

		tx, err := sipClient.TransactionRequest(req)
		if err != nil {
			panic(err)
		}

		getResponse(tx)
		mediaConn.Close()
	}

	byeSent := atomic.Bool{}
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)
	go func() {
		<-signalChan
		sendBye()
		byeSent.Store(true)
	}()
	ip, port := parseAnswer(inviteResponse.Body())
	dstAddr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", ip, port))
	if err != nil {
		panic(err)
	}

	if dtmf := *sendDTMF; dtmf != "" {
		sendDTMFPackets(mediaConn, dstAddr, dtmf)
		time.Sleep(time.Second)
	}

	sendAudioPackets(mediaConn, dstAddr, *filePathPlay)
	if !byeSent.Load() {
		sendBye()
	}
}
