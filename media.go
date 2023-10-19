package main

import (
	"net"

	"github.com/pion/webrtc/v3"
)

func createRTPListener(audioTrack *webrtc.TrackLocalStaticRTP) int {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{
		Port: 0,
		IP:   net.ParseIP("0.0.0.0"),
	})
	if err != nil {
		panic(err)
	}

	go func() {
		buff := make([]byte, 1500)

		for {
			n, _, err := conn.ReadFromUDP(buff)
			if err != nil {
				panic(err)
			}

			audioTrack.Write(buff[:n])
		}
	}()

	return conn.LocalAddr().(*net.UDPAddr).Port
}
