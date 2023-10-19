package main

import (
	"fmt"
	"io"
	"net/http"

	"github.com/pion/webrtc/v3"
)

var (
	audioTrack *webrtc.TrackLocalStaticRTP

	api *webrtc.API
)

const indexHtml = `
<html>
  <head>
    <title>sip-webrtc-bridge</title>
  </head>

  <body>
    <video style="width: 1280" controls autoplay id="video"> </video>
  </body>

  <script>
    let pc = new RTCPeerConnection()
    pc.addTransceiver('audio')

    pc.ontrack = event => {
      document.getElementById("video").srcObject = event.streams[0];
    };

    pc.createOffer()
      .then(offer => {
        pc.setLocalDescription(offer)

        return resp = fetch('/whep', {
          method: 'POST',
          headers: {
            'Content-Type': 'application/sdp'
          },
          body: offer.sdp
        })
      })
      .then(res => res.text())
      .then(res => pc.setRemoteDescription({type: 'answer', sdp: res}))
      .catch(alert)
  </script>
</html>
`

func whep(w http.ResponseWriter, r *http.Request) {
	peerConnection, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		panic(err)
	}

	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		if connectionState == webrtc.ICEConnectionStateFailed {
			peerConnection.Close()
		}
	})

	if _, err = peerConnection.AddTrack(audioTrack); err != nil {
		panic(err)
	}

	offer, err := io.ReadAll(r.Body)
	if err != nil {
		panic(err)
	}

	if err = peerConnection.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  string(offer)}); err != nil {
		panic(err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	} else if err = peerConnection.SetLocalDescription(answer); err != nil {
		panic(err)
	}
	<-gatherComplete

	if _, err := w.Write([]byte(peerConnection.LocalDescription().SDP)); err != nil {
		panic(err)
	}
}

func createWebRTCHandlers(publicIp string) *webrtc.TrackLocalStaticRTP {
	var err error
	audioTrack, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU}, "audio", "audio")
	if err != nil {
		panic(err)
	}

	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetNAT1To1IPs([]string{publicIp}, webrtc.ICECandidateTypeHost)

	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		panic(err)
	}

	api = webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine), webrtc.WithMediaEngine(m))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, indexHtml)
	})
	http.HandleFunc("/whep", whep)

	go func() {
		panic(http.ListenAndServe(fmt.Sprintf("0.0.0.0:%d", httpPort), nil))
	}()

	return audioTrack
}
