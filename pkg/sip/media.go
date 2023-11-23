// Copyright 2023 LiveKit, Inc.
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
	"log"
	"time"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/pkg/media/ulaw"
	"github.com/livekit/sip/res"
)

const (
	channels      = 1
	sampleRate    = 8000
	sampleDur     = 20 * time.Millisecond
	sampleDurPart = int(time.Second / sampleDur)
	rtpPacketDur  = uint32(sampleRate / sampleDurPart)
)

type mediaRes struct {
	enterPin []media.PCM16Sample
	roomJoin []media.PCM16Sample
	wrongPin []media.PCM16Sample
}

func (s *Server) initMediaRes() {
	s.res.enterPin = readMkvAudioFile(res.EnterPinMkv)
	s.res.roomJoin = readMkvAudioFile(res.RoomJoinMkv)
	s.res.wrongPin = readMkvAudioFile(res.WrongPinMkv)
}

func (c *inboundCall) closeMedia() {
	c.audioHandler.Store(nil)
	if p := c.lkRoom.Load(); p != nil {
		p.Close()
		c.lkRoom.Store(nil)
	}
	c.rtpConn.Close()
	close(c.dtmf)
}

func (c *inboundCall) HandleRTP(p *rtp.Packet) error {
	if p.Marker && p.PayloadType == 101 {
		c.handleDTMF(p.Payload)
		return nil
	}
	// TODO: Audio data appears to be coming with PayloadType=0, so maybe enforce it?
	if h := c.audioHandler.Load(); h != nil {
		return (*h).HandleRTP(p)
	}
	return nil
}

var dtmfEventToChar = [256]byte{
	0: '0', 1: '1', 2: '2', 3: '3', 4: '4',
	5: '5', 6: '6', 7: '7', 8: '8', 9: '9',
	10: '*', 11: '#',
	12: 'a', 13: 'b', 14: 'c', 15: 'd',
}

func (c *inboundCall) handleDTMF(data []byte) { // RFC2833
	if len(data) < 4 {
		return
	}
	ev := data[0]
	b := dtmfEventToChar[ev]
	// We should have enough buffer here.
	select {
	case c.dtmf <- b:
	default:
	}
}

func (c *inboundCall) createLiveKitParticipant(roomName, participantIdentity string) error {
	room, err := ConnectToRoom(c.s.conf, roomName, participantIdentity)
	if err != nil {
		return err
	}
	local, err := room.NewParticipant()
	if err != nil {
		_ = room.Close()
		return err
	}
	c.lkRoom.Store(room)

	// Decoding pipeline (SIP -> LK)
	lpcm := media.DecodePCM(local)
	law := ulaw.Encode(lpcm)
	var h rtp.Handler = rtp.NewMediaStreamIn(law)
	c.audioHandler.Store(&h)

	// Encoding pipeline (LK -> SIP)
	s := rtp.NewMediaStreamOut[ulaw.Sample](c.rtpConn, rtpPacketDur)
	room.SetOutput(ulaw.Decode(s))

	return nil
}

func (c *inboundCall) joinRoom(roomName, identity string) {
	log.Printf("Bridging SIP call %q -> %q to room %q (as %q)\n", c.from.Address.User, c.to.Address.User, roomName, identity)
	c.playAudio(c.s.res.roomJoin)
	if err := c.createLiveKitParticipant(roomName, identity); err != nil {
		log.Println(err)
	}
}

func (c *inboundCall) playAudio(frames []media.PCM16Sample) {
	r := c.lkRoom.Load()
	t := r.NewTrack()
	defer t.Close()
	t.PlayAudio(frames)
}
