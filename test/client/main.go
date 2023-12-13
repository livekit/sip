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
	"encoding/binary"
	"flag"
	"fmt"
	"io"
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
	"github.com/pion/sdp/v2"

	"github.com/livekit/sip/pkg/media"
	lkrtp "github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/pkg/media/ulaw"
	lksip "github.com/livekit/sip/pkg/sip"
)

var (
	sipServer    = flag.String("sip-server", "", "")
	to           = flag.String("to", "+15550100000", "")
	from         = flag.String("from", "+15550100001", "")
	username     = flag.String("username", "", "")
	password     = flag.String("password", "", "")
	sipUri       = flag.String("sip-uri", "example.pstn.twilio.com", "")
	filePathPlay = flag.String("play", "audio.mkv", "")
	filePathSave = flag.String("save", "save.mkv", "")
)

func NewMKVWriter(w io.WriteCloser) (*MKVWriter, error) {
	ws, err := webm.NewSimpleBlockWriter(w,
		[]webm.TrackEntry{{
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
		}},
	)
	if err != nil {
		return nil, err
	}
	return &MKVWriter{w: w, ws: ws[0]}, nil
}

type MKVWriter struct {
	w              io.WriteCloser
	ws             webm.BlockWriteCloser
	audioTimestamp int64
	buf            []byte
}

func (w *MKVWriter) WriteSample(in media.PCM16Sample) error {
	w.audioTimestamp += 20
	if sz := 2 * len(in); cap(w.buf) < sz {
		w.buf = make([]byte, sz)
	} else {
		w.buf = w.buf[:sz]
	}
	for i, v := range in {
		binary.LittleEndian.PutUint16(w.buf[2*i:], uint16(v))
	}
	_, err := w.ws.Write(true, w.audioTimestamp, w.buf)
	return err
}

func (w *MKVWriter) Close() error {
	var last error
	if err := w.ws.Close(); err != nil {
		last = err
	}
	if err := w.w.Close(); err != nil {
		last = err
	}
	return last
}

func saveToFile(conn *lksip.MediaConn, path string) func() error {
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	wr, err := NewMKVWriter(f)
	if err != nil {
		f.Close()
		panic(err)
	}

	law := ulaw.Decode(wr)
	conn.OnRTP(lkrtp.NewMediaStreamIn(law))
	return wr.Close
}

func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		panic(err)
	}
	for _, address := range addrs {
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}

	panic("No IP Found")
}

func getResponse(tx sip.ClientTransaction) *sip.Response {
	for {
		select {
		case <-tx.Done():
			panic("transaction failed to complete")
		case res := <-tx.Responses():
			switch res.StatusCode {
			default:
				return res
			case 100, 180, 183:
				slog.With("status", res.StatusCode).Info(res.Reason)
			}
		}
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
			UnicastAddress: getLocalIP(),
		},
		SessionName: "LiveKit",
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: getLocalIP()},
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
					Port:    sdp.RangedPort{Value: port},
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

func parseAnswer(in []byte) (string, int) {
	offer := sdp.SessionDescription{}
	if err := offer.Unmarshal(in); err != nil {
		panic(err)
	}

	return offer.ConnectionInformation.Address.Address, offer.MediaDescriptions[0].MediaName.Port.Value
}

func sendAudioPackets(conn *lksip.MediaConn) {
	r, err := os.Open(*filePathPlay)
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

	var audioFrames [][]byte
	for _, cluster := range ret.Segment.Cluster {
		for _, block := range cluster.SimpleBlock {
			audioFrames = append(audioFrames, block.Data...)
		}
	}

	i := 0
	s := lkrtp.NewMediaStreamOut[ulaw.Sample](conn, 160)

	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for range tick.C {
		if i >= len(audioFrames) {
			break
		}
		if err = s.WriteSample(audioFrames[i]); err != nil {
			return
		}
		i++
	}
}

func attemptInvite(sipClient *sipgo.Client, offer []byte, authorizationHeaderValue string) (*sip.Request, *sip.Response) {
	inviteRecipent := &sip.Uri{User: *to, Host: *sipUri}
	req := sip.NewRequest(sip.INVITE, inviteRecipent)
	req.SetDestination(*sipServer)
	req.SetBody(offer)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.AppendHeader(sip.NewHeader("Contact", fmt.Sprintf("<sip:livekit@%s:5060>", getLocalIP())))
	req.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, CANCEL, BYE, NOTIFY, REFER, MESSAGE, OPTIONS, INFO, SUBSCRIBE"))

	if authorizationHeaderValue != "" {
		req.AppendHeader(sip.NewHeader("Proxy-Authorization", authorizationHeaderValue))
	}

	tx, err := sipClient.TransactionRequest(req)
	if err != nil {
		panic(err)
	}
	defer tx.Terminate()

	return req, getResponse(tx)
}

func main() {
	flag.Parse()

	if *sipServer == "" {
		*sipServer = getLocalIP() + ":5060"
	}

	mediaConn := lksip.NewMediaConn()
	if *filePathSave != "" {
		saveDone := saveToFile(mediaConn, *filePathSave)
		defer func() {
			if err := saveDone(); err != nil {
				slog.Error("cannot save the file", err)
			}
		}()
	}
	if err := mediaConn.Start(0, 0, ""); err != nil {
		panic(err)
	}
	defer mediaConn.Close()
	slog.With("port", mediaConn.LocalAddr().Port).Info("media listener started")

	offer, err := createOffer(mediaConn.LocalAddr().Port)
	if err != nil {
		panic(err)
	}

	ua, err := sipgo.NewUA()
	if err != nil {
		panic(err)
	}

	sipClient, err := sipgo.NewClient(ua, sipgo.WithClientHostname(getLocalIP()))
	if err != nil {
		panic(err)
	}

	var (
		authorizationHeaderValue = ""
		inviteResponse           *sip.Response
		inviteRequest            *sip.Request
	)

	slog.Info("sending invite")
	for try := 1; ; try++ {
		if try > 1 {
			slog.With("attempt", try).Info("invite attempt")
		}
		inviteRequest, inviteResponse = attemptInvite(sipClient, offer, authorizationHeaderValue)

		if inviteResponse.StatusCode == 407 {
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
	slog.Info("invite success")

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
	mediaConn.SetDestAddr(dstAddr)

	sendAudioPackets(mediaConn)
	if !byeSent.Load() {
		sendBye()
	}
}
