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
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"time"

	"github.com/at-wat/ebml-go"
	"github.com/at-wat/ebml-go/webm"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v2"
)

func startMediaListener() *net.UDPConn {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{
		Port: 0,
		IP:   net.ParseIP("0.0.0.0"),
	})
	if err != nil {
		panic(err)
	}

	return conn
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

func parseAnswer(in []byte) int {
	offer := sdp.SessionDescription{}
	if err := offer.Unmarshal(in); err != nil {
		panic(err)
	}

	return offer.MediaDescriptions[0].MediaName.Port.Value
}

func sendAudioPackets(conn *net.UDPConn, port int) {
	r, err := os.Open("audio.mkv")
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
	rtpPkt := &rtp.Packet{
		Header: rtp.Header{
			Version: 2,
			SSRC:    5000,
		},
	}

	dstAddr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", getLocalIP(), port))
	if err != nil {
		panic(err)
	}

	for range time.NewTicker(20 * time.Millisecond).C {
		if i >= len(audioFrames) {
			break
		}

		rtpPkt.Payload = audioFrames[i]

		raw, err := rtpPkt.Marshal()
		if err != nil {
			panic(err)
		}

		if _, err = conn.WriteTo(raw, dstAddr); err != nil {
			return
		}

		rtpPkt.Header.Timestamp += 160
		rtpPkt.Header.SequenceNumber += 1
		i++
	}
}

func main() {
	mediaConn := startMediaListener()
	offer, err := createOffer(mediaConn.LocalAddr().(*net.UDPAddr).Port)
	if err != nil {
		panic(err)
	}

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent("+15550100001"),
	)
	if err != nil {
		log.Fatal(err)
	}

	sipClient, err := sipgo.NewClient(ua)
	if err != nil {
		log.Fatal(err)
	}

	inviteRequest := sip.NewRequest(sip.INVITE, &sip.Uri{User: "+15550100000", Host: "example.pstn.twilio.com"})
	inviteRequest.SetDestination(getLocalIP() + ":5060")
	inviteRequest.SetBody(offer)

	tx, err := sipClient.TransactionRequest(inviteRequest)
	if err != nil {
		panic(err)
	}

	var inviteResponse *sip.Response
	select {
	case inviteResponse = <-tx.Responses():
	case <-tx.Done():
		panic("INVITE failed")
	}

	if inviteResponse.StatusCode != 200 {
		panic("INVITE rejected")
	}

	sendBye := func() {
		req := sip.NewByeRequest(inviteRequest, inviteResponse, nil)

		tx, err := sipClient.TransactionRequest(req)
		if err != nil {
			panic(err)
		}

		select {
		case <-tx.Responses():
		case <-tx.Done():
			panic("BYE failed")
		}

		mediaConn.Close()
	}

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)
	go func() {
		<-signalChan
		sendBye()
	}()

	sendAudioPackets(mediaConn, parseAnswer(inviteResponse.Body()))
	sendBye()

}
