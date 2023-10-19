package main

import (
	"context"
	"fmt"
	"log"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

const (
	userAgent = "LiveKit"
	sipPort   = 5060
	httpPort  = 8080
)

var (
	contentTypeHeaderSDP = sip.ContentTypeHeader("application/sdp")
)

func main() {
	publicIp := getPublicIP()

	audioTrack := createWebRTCHandlers(publicIp)

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(userAgent),
	)
	if err != nil {
		log.Fatal(err)
	}

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		log.Fatal(err)
	}

	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		rtpListenerPort := createRTPListener(audioTrack)

		res := sip.NewResponseFromRequest(req, 200, "OK", generateAnswer(req.Body(), publicIp, rtpListenerPort))
		res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Host: publicIp, Port: sipPort}})
		res.AppendHeader(&contentTypeHeaderSDP)
		tx.Respond(res)
	})

	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})

	srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})

	panic(srv.ListenAndServe(context.TODO(), "udp", fmt.Sprintf("0.0.0.0:%d", sipPort)))
}
