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
					Formats: []string{"0", "101"},
				},
				Attributes: []sdp.Attribute{
					sdp.Attribute{Key: "rtpmap", Value: "0 PCMU/8000"},
					sdp.Attribute{Key: "rtpmap", Value: "101 telephone-event/8000"},
					sdp.Attribute{Key: "fmtp", Value: "101 0-16"},
					sdp.Attribute{Key: "ptime", Value: "20"},
					sdp.Attribute{Key: "maxptime", Value: "150"},
					sdp.Attribute{Key: "sendrecv"},
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
		rtpPkt.Payload = audioFrames[i]

		raw, err := rtpPkt.Marshal()
		if err != nil {
			return
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

	client, err := sipgo.NewClient(ua)
	if err != nil {
		log.Fatal(err)
	}

	req := sip.NewRequest(sip.INVITE, &sip.Uri{User: "+15550100000", Host: "example.pstn.twilio.com"})
	req.SetDestination(getLocalIP() + ":5060")
	req.SetBody(offer)

	tx, err := client.TransactionRequest(req)
	if err != nil {
		panic(err)
	}
	defer tx.Terminate()

	var port int

	select {
	case res := <-tx.Responses():
		port = parseAnswer(res.Body())
	case <-tx.Done():
		panic("INVITE failed")
	}

	sendAudioPackets(mediaConn, port)
}
