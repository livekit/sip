package sip

import (
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"github.com/pion/sdp/v2"
)

func (s *Server) handleInviteAuth(req *sip.Request, tx sip.ServerTransaction, from, username, password string) (ok bool) {
	if username == "" || password == "" {
		return true
	}

	var inviteState *inProgressInvite
	for i := range s.inProgressInvites {
		if s.inProgressInvites[i].from == from {
			inviteState = s.inProgressInvites[i]
		}
	}

	if inviteState == nil {
		if len(s.inProgressInvites) >= digestLimit {
			s.inProgressInvites = s.inProgressInvites[1:]
		}

		inviteState = &inProgressInvite{from: from}
		s.inProgressInvites = append(s.inProgressInvites, inviteState)
	}

	h := req.GetHeader("Proxy-Authorization")
	if h == nil {
		inviteState.challenge = digest.Challenge{
			Realm:     userAgent,
			Nonce:     fmt.Sprintf("%d", time.Now().UnixMicro()),
			Algorithm: "MD5",
		}

		res := sip.NewResponseFromRequest(req, 407, "Unauthorized", nil)
		res.AppendHeader(sip.NewHeader("Proxy-Authenticate", inviteState.challenge.String()))
		logOnError(tx.Respond(res))
		return false
	}

	cred, err := digest.ParseCredentials(h.Value())
	if err != nil {
		logOnError(tx.Respond(sip.NewResponseFromRequest(req, 401, "Bad credentials", nil)))
		return false
	}

	digCred, err := digest.Digest(&inviteState.challenge, digest.Options{
		Method:   req.Method.String(),
		URI:      cred.URI,
		Username: cred.Username,
		Password: password,
	})

	if err != nil {
		logOnError(tx.Respond(sip.NewResponseFromRequest(req, 401, "Bad credentials", nil)))
		return false
	}

	if cred.Response != digCred.Response {
		// 	fmt.Println(cred.Response, digCred.Response)
		logOnError(tx.Respond(sip.NewResponseFromRequest(req, 401, "Unauthorized", nil)))
		return false
	}

	return true
}

func (s *Server) onInvite(req *sip.Request, tx sip.ServerTransaction) {
	tag, err := getTagValue(req)
	if err != nil {
		sipErrorResponse(tx, req)
		return
	}

	from, ok := req.From()
	if !ok {
		sipErrorResponse(tx, req)
		return
	}

	to, ok := req.To()
	if !ok {
		sipErrorResponse(tx, req)
		return
	}
	src := req.Source()

	username, password, err := s.authenticationHandler(from.Address.User, to.Address.User, src)
	if err != nil {
		sipErrorResponse(tx, req)
		return
	}
	if !s.handleInviteAuth(req, tx, from.Address.User, username, password) {
		// handleInviteAuth will generate the SIP Response as needed
		return
	}
	call := s.newInboundCall(tag, from, to, src)
	call.handleInvite(req, tx)
}

type inboundCall struct {
	s     *Server
	tag   string
	from  *sip.FromHeader
	to    *sip.ToHeader
	src   string
	media mediaData
	done  atomic.Bool
}

func (s *Server) newInboundCall(tag string, from *sip.FromHeader, to *sip.ToHeader, src string) *inboundCall {
	c := &inboundCall{
		s:    s,
		tag:  tag,
		from: from,
		to:   to,
		src:  src,
	}
	c.initMedia()
	return c
}

func (c *inboundCall) handleInvite(req *sip.Request, tx sip.ServerTransaction) {
	// Send initial request. In the best case scenario, we will immediately get a room name to join.
	// Otherwise, we could ever learn that this number is not allowed and reject the call, or ask for pin if required.
	roomName, identity, requirePin, rejectInvite := c.s.dispatchRuleHandler(c.from.Address.User, c.to.Address.User, c.src, "", false)
	if rejectInvite {
		sipErrorResponse(tx, req)
		return
	}

	// We need to start media first, otherwise we won't be able to send audio prompts to the caller, or receive DTMF.
	answerData, err := c.runMedia(req.Body())
	if err != nil {
		sipErrorResponse(tx, req)
		return
	}
	c.s.cmu.Lock()
	c.s.activeCalls[c.tag] = c
	c.s.cmu.Unlock()

	res := sip.NewResponseFromRequest(req, 200, "OK", answerData)
	res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Host: c.s.publicIp, Port: c.s.conf.SIPPort}})
	res.AppendHeader(&contentTypeHeaderSDP)
	if err = tx.Respond(res); err != nil {
		log.Println(err)
		// TODO: should we close the call in this case?
		return
	}
	// We own this goroutine, so can freely block.
	if requirePin {
		c.pinPrompt()
	} else {
		c.joinRoom(roomName, identity)
	}
}

func (c *inboundCall) runMedia(offerData []byte) ([]byte, error) {
	addr, err := c.createMediaSession()
	if err != nil {
		return nil, err
	}
	offer := sdp.SessionDescription{}
	if err := offer.Unmarshal(offerData); err != nil {
		return nil, err
	}
	return generateAnswer(offer, c.s.publicIp, addr.Port)
}

func (c *inboundCall) pinPrompt() {
	log.Printf("Requesting Pin for SIP call %q -> %q\n", c.from.Address.User, c.to.Address.User)
	const pinLimit = 16
	c.playAudio(c.s.res.enterPin)
	pin := ""
	noPin := false
	for {
		select {
		case b, ok := <-c.media.dtmf:
			if !ok {
				c.Close()
				return
			}
			if b == 0 {
				continue // unrecognized
			}
			if b == '#' {
				// End of the pin
				noPin = pin == ""

				log.Printf("Checking Pin for SIP call %q -> %q = %q (noPin = %v)\n", c.from.Address.User, c.to.Address.User, pin, noPin)
				roomName, identity, requirePin, reject := c.s.dispatchRuleHandler(c.from.Address.User, c.to.Address.User, c.src, pin, noPin)
				if reject || requirePin || roomName == "" {
					c.playAudio(c.s.res.wrongPin)
					c.Close()
					return
				}
				c.joinRoom(roomName, identity)
				return
			}
			// Gather pin numbers
			pin += string(b)
			if len(pin) > pinLimit {
				c.playAudio(c.s.res.wrongPin)
				c.Close()
				return
			}
		}
	}
}

func (c *inboundCall) Close() error {
	if c.done.CompareAndSwap(false, true) {
		return nil
	}
	c.s.cmu.Lock()
	delete(c.s.activeCalls, c.tag)
	c.s.cmu.Unlock()
	c.closeMedia()
	// FIXME: drop the actual call
	return nil
}
