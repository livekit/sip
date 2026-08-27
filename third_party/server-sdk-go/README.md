<!--BEGIN_BANNER_IMAGE-->

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="/.github/banner_dark.png">
  <source media="(prefers-color-scheme: light)" srcset="/.github/banner_light.png">
  <img style="width:100%;" alt="The LiveKit icon, the name of the repository and some sample code in the background." src="https://raw.githubusercontent.com/livekit/server-sdk-go/main/.github/banner_light.png">
</picture>

<!--END_BANNER_IMAGE-->

# LiveKit Go SDK

<!--BEGIN_DESCRIPTION-->
Use this SDK to interact with <a href="https://livekit.io/">LiveKit</a> server APIs and create access tokens from your Go backend.
<!--END_DESCRIPTION-->

[!NOTE]

Version 2 of the <platform> SDK contains a small set of breaking changes. Read the [migration guide](https://docs.livekit.io/guides/migrate-from-v1/) for a detailed overview of what has changed.

## Installation

```shell
go get github.com/livekit/server-sdk-go/v2
```

Note: since v1.0 release, this package requires Go 1.18+ in order to build.

## Token creation

```go
import (
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/protocol/auth"
)

func getJoinToken(apiKey, apiSecret, room, identity string) (string, error) {
	at := auth.NewAccessToken(apiKey, apiSecret)
	grant := &auth.VideoGrant{
		RoomJoin: true,
		Room:     room,
	}
	at.SetVideoGrant(grant).
		SetIdentity(identity).
		SetValidFor(time.Hour)

	return at.ToJWT()
}
```

## Authentication

Every request to the server APIs is authenticated. `LiveKitAPI` (and each service client) supports two modes:

- **API key & secret** — recommended for backend use. The SDK signs a short-lived token per request from your key and secret. Keep your API secret on the server; never ship it to a client.
- **Access token** — for frontend / client-side use, where the API secret must not be exposed. Pass a pre-signed [access token](https://docs.livekit.io/frontends/reference/tokens-grants/) that already carries the grants for the operations you'll perform; the SDK sends it verbatim. Mint it on your backend and hand it to the client.

```go
// Backend (API key & secret): set LIVEKIT_URL, LIVEKIT_API_KEY, and
// LIVEKIT_API_SECRET, then construct with no options...
api, _ := lksdk.NewLiveKitAPI()

// ...or pass any of them explicitly to override the corresponding env var:
api, _ := lksdk.NewLiveKitAPI(lksdk.WithURL(hostURL), lksdk.WithAPIKey("api-key", "api-secret"))

// Frontend (pre-signed access token): with LIVEKIT_URL set, pass just the token:
api, _ := lksdk.NewLiveKitAPI(lksdk.WithToken("token"))
```

## Server APIs

`LiveKitAPI` is a single entry point to every server API, exposing each service through an accessor: `Room()`, `Egress()`, `Ingress()`, `SIP()`, `AgentDispatch()`, and `Connector()`. Construct it with your credentials (see [Authentication](#authentication)); the url and credentials fall back to the `LIVEKIT_URL`, `LIVEKIT_API_KEY`, `LIVEKIT_API_SECRET`, and `LIVEKIT_TOKEN` environment variables. Values you pass explicitly take precedence; the environment variables are used only as a fallback for arguments you omit — an ambient `LIVEKIT_TOKEN`, for example, won't override an explicitly-provided API key and secret.

RoomService, reached via `api.Room()`, gives you complete control over rooms and participants within them, including selective track subscriptions and moderation capabilities.

```go
import (
	"context"

	lksdk "github.com/livekit/server-sdk-go/v2"
	livekit "github.com/livekit/protocol/livekit"
)

func main() {
	hostURL := "host-url" // ex: https://project-123456.livekit.cloud
	roomName := "myroom"
	identity := "participantIdentity"

	// authenticate with an API key and secret, or lksdk.WithToken("token") for a pre-signed token
	api, err := lksdk.NewLiveKitAPI(lksdk.WithURL(hostURL), lksdk.WithAPIKey("api-key", "api-secret"))
	if err != nil {
		panic(err)
	}

	// create a new room
	room, _ := api.Room().CreateRoom(context.Background(), &livekit.CreateRoomRequest{
		Name: roomName,
	})

	// list rooms
	rooms, _ := api.Room().ListRooms(context.Background(), &livekit.ListRoomsRequest{})

	// terminate a room and cause participants to leave
	api.Room().DeleteRoom(context.Background(), &livekit.DeleteRoomRequest{
		Room: roomName,
	})

	// list participants in a room
	participants, _ := api.Room().ListParticipants(context.Background(), &livekit.ListParticipantsRequest{
		Room: roomName,
	})

	// disconnect a participant from room
	api.Room().RemoveParticipant(context.Background(), &livekit.RoomParticipantIdentity{
		Room:     roomName,
		Identity: identity,
	})

	// mute/unmute a participant's track
	api.Room().MutePublishedTrack(context.Background(), &livekit.MuteRoomTrackRequest{
		Room:     roomName,
		Identity: identity,
		TrackSid: "track_sid",
		Muted:    true,
	})

	// other services are reached the same way, e.g. api.Egress(), api.SIP()
	_, _ = api.Egress().ListEgress(context.Background(), &livekit.ListEgressRequest{})

	_, _, _ = room, rooms, participants
}
```

### Agent dispatch

[Agent dispatch](https://docs.livekit.io/agents/server/agent-dispatch/) assigns an agent to a room. Explicit dispatch, via `api.AgentDispatch()`, gives you full control over when and how agents join and lets you pass job-specific metadata. The target agent is selected by its `agent_name`, and the room is created if it doesn't exist.

```go
// dispatch an agent into a room
dispatch, _ := api.AgentDispatch().CreateDispatch(context.Background(), &livekit.CreateAgentDispatchRequest{
	Room:      "myroom",
	AgentName: "my-agent",
	Metadata:  "{}",
})

// list dispatches in a room
dispatches, _ := api.AgentDispatch().ListDispatch(context.Background(), &livekit.ListAgentDispatchRequest{
	Room: "myroom",
})

// delete a dispatch
api.AgentDispatch().DeleteDispatch(context.Background(), &livekit.DeleteAgentDispatchRequest{
	DispatchId: dispatch.Id,
	Room:       "myroom",
})

_ = dispatches
```

### Error handling

A failed server API call returns a `lksdk.ServerError`, which exposes the error code, message, and any server-provided metadata. Use `errors.As` to extract it. SIP dialing calls can fail with a SIP response status; `lksdk.SIPStatusFrom` extracts it from the returned error:

```go
_, err := api.SIP().CreateSIPParticipant(context.Background(), &livekit.CreateSIPParticipantRequest{
	SipTrunkId:        "trunk-id",
	SipCallTo:         "+15105550100",
	RoomName:          "my-room",
	WaitUntilAnswered: true,
})
if err != nil {
	if status := lksdk.SIPStatusFrom(err); status != nil {
		fmt.Println(status.Code, status.Status) // e.g. 486 "Busy Here"
	}
	var se lksdk.ServerError
	if errors.As(err, &se) {
		fmt.Println(se.Code(), se.Msg())
	}
}
```

## Interacting as a participant

The Real-time SDK gives you access programmatic access as a client enabling you to publish and record audio/video/data to the room.

```go
import (
  lksdk "github.com/livekit/server-sdk-go/v2"
)

func main() {
  hostURL := "host-url"  // ex: https://project-123456.livekit.cloud
  apiKey := "api-key"
  apiSecret := "api-secret"
  roomName := "myroom"
  identity := "botuser"
  roomCB := &lksdk.RoomCallback{
	ParticipantCallback: lksdk.ParticipantCallback{
	  OnTrackSubscribed: trackSubscribed,
  	},
  }
  room, err := lksdk.ConnectToRoom(hostURL, lksdk.ConnectInfo{
  	APIKey:              apiKey,
  	APISecret:           apiSecret,
  	RoomName:            roomName,
  	ParticipantIdentity: identity,
  }, roomCB)
  if err != nil {
  	panic(err)
  }

  ...
  room.Disconnect()
}

func trackSubscribed(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {

}
```

For more advanced usage, see the [examples](https://github.com/livekit/server-sdk-go/tree/main/examples) directory.

## Publishing tracks to Room

With the Go SDK, you can publish existing files encoded in H.264, VP8, and Opus to the room.

First, you will need to encode media into the right format.

### VP8 / Opus

```bash
ffmpeg -i <input.mp4> \
  -c:v libvpx -keyint_min 120 -qmax 50 -maxrate 2M -b:v 1M <output.ivf> \
  -c:a libopus -page_duration 20000 -vn <output.ogg>
```

The above encodes VP8 at average 1Mbps / max 2Mbps with a minimum keyframe interval of 120.

### H.264 / Opus

```bash
ffmpeg -i <input.mp4> \
  -c:v libx264 -bsf:v h264_mp4toannexb -b:v 2M -profile baseline -pix_fmt yuv420p \
    -x264-params keyint=120 -max_delay 0 -bf 0 <output.h264> \
  -c:a libopus -page_duration 20000 -vn <output.ogg>
```

The above encodes H264 with CBS of 2Mbps with a minimum keyframe interval of 120.

### Publish from file

```go
file := "video.ivf"
videoWidth := 1920
videoHeight := 1080
track, err := lksdk.NewLocalFileTrack(file,
	// control FPS to ensure synchronization
	lksdk.ReaderTrackWithFrameDuration(33 * time.Millisecond),
	lksdk.ReaderTrackWithOnWriteComplete(func() { fmt.Println("track finished") }),
)
if err != nil {
    return err
}
if _, err = room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
	Name: file,
	VideoWidth: videoWidth,
	VideoHeight: videoHeight,
}); err != nil {
    return err
}
```

### Publish from io.ReadCloser implementation

```go
// - `in` implements io.ReadCloser, such as buffer or file
// - `mime` has to be one of webrtc.MimeType...
track, err := lksdk.NewLocalReaderTrack(in, mime,
	lksdk.ReaderTrackWithFrameDuration(33 * time.Millisecond),
	lksdk.ReaderTrackWithOnWriteComplete(func() { fmt.Println("track finished") }),
)
if err != nil {
	return err
}
if _, err = room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{}); err != nil {
    return err
}
```

For a full working example, refer to [filesender](https://github.com/livekit/server-sdk-go/blob/main/examples/filesender/main.go). This
example sends all audio/video files in the current directory.

### Publishing audio from PCM16 Samples

In order to publish audio from PCM16 Samples, you can use the NewPCMLocalTrack API as follows:

```go
import (
	...
	lkmedia "github.com/livekit/server-sdk-go/v2/pkg/media"
)

...

publishTrack, err := lkmedia.NewPCMLocalTrack(sourceSampleRate, sourceChannels, logger.GetLogger())
if err != nil {
	return err
}

if _, err = room.LocalParticipant.PublishTrack(publishTrack, &lksdk.TrackPublicationOptions{
	Name: "test",
}); err != nil {
	return err
}
```

You can then write PCM16 samples to the `publishTrack` like:

```go
err = publishTrack.WriteSample(sample)
if err != nil {
	logger.Errorw("error writing sample", err)
}
```

The SDK will encode the sample to Opus and write it to the track. If the sourceSampleRate is not 48000, resampling is also handled internally.

To close the track, you can call `Close()` on the `publishTrack`, this will stop accepting samples, write the existing buffer and then close the track. If you wish to clear the buffer manually, use the `ClearQeuue()` on the track. There's also a `WaitForPlayout()` API if you want to wait for existing buffer to be written before writing something to the track.

**Note**: Stereo audio is currently not supported, it may result in unpleasant audio.

### Publish from other sources

In order to publish from non-file sources, you will have to implement your own `SampleProvider`, that could provide frames of data with a `NextSample` method.

The SDK takes care of sending the samples to the room.

### Using a pacer

With video publishing, keyframes can be an order of magnitude larger than delta frames.
This size difference can cause a significant increase in bitrate when a keyframe is transmitted, leading to a surge in packet flow.
Such spikes might result in packet loss at the forwarding layer. To maintain a consistent packet flow,
you can enable the use of a [pacer](https://chromium.googlesource.com/external/webrtc/+/master/modules/pacing/g3doc/index.md).

```go
import "github.com/livekit/mediatransportutil/pkg/pacer"

// Control total output bitrate to 10Mbps with 1s max latency
pf := pacer.NewPacerFactory(
	pacer.LeakyBucketPacer,
	pacer.WithBitrate(10000000),
	pacer.WithMaxLatency(time.Second),
)

room, err := lksdk.ConnectToRoom(hostURL, lksdk.ConnectInfo{
    APIKey:              apiKey,
    APISecret:           apiSecret,
    RoomName:            roomName,
    ParticipantIdentity: identity,
}, &lksdk.RoomCallback{
    ParticipantCallback: lksdk.ParticipantCallback{
        OnTrackSubscribed: onTrackSubscribed,
    },
}, lksdk.WithPacer(pf))
```

## Receiving tracks from Room

With the Go SDK, you can accept media from the room.

For a full working example, refer to [filesaver](https://github.com/livekit/server-sdk-go/blob/main/examples/filesaver/main.go). This
example saves the audio/video in the LiveKit room to the local disk.

### Decoding an Opus track to PCM16

To get PCM audio out of a remote Opus audio track, you can use the following API:

```go
import (
	...
	media "github.com/livekit/media-sdk"
	lkmedia "github.com/livekit/server-sdk-go/v2/pkg/media"
)

type PCM16Writer struct {
	closed atomic.Bool
}

func (w *PCM16Writer) WriteSample(sample media.PCM16Sample) error {
	if !w.closed.Load() {
		// Use the sample however you want
	}
}

func (w *PCM16Writer) Close() error {
	w.closed.Store(true)
	// close the writer
}

...

writer := &PCM16Writer{}
pcmTrack, err := lkmedia.NewPCMRemoteTrack(remoteTrack, writer)
if err != nil {
	return err
}
```

The SDK will then read the provided remote track, decode the audio and write the PCM16 samples to the provided writer. By defeault, it pushes out 48kHz mono audio. The output sample rate and channels can also be configured by passsing as an option:

```go
pcmTrack, err := lkmedia.NewPCMRemoteTrack(remoteTrack, writer, lkmedia.WithTargetSampleRate(24000), lkmedia.WithTargetChannels(2))
```

Resampling to the target sample rate is handled internally, and so is upmixing/downmixing to the target channel count.

The API also provides an option to handle jitter, this is enabled by default but you can disable it using:

```go
pcmTrack, err := lkmedia.NewPCMRemoteTrack(remoteTrack, writer, lkmedia.WithHandleJitter(false))
```

## Receiving webhooks

The Go SDK helps you to verify and decode webhook callbacks to ensure their authenticity.
See [webhooks guide](https://docs.livekit.io/guides/webhooks) for configuration.

```go
import (
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"
)

var authProvider = auth.NewSimpleKeyProvider(
	apiKey, apiSecret,
)

func ServeHTTP(w http.ResponseWriter, r *http.Request) {
  // event is a livekit.WebhookEvent{} object
	event, err := webhook.ReceiveWebhookEvent(r, authProvider)
	if err != nil {
		// could not validate, handle error
		return
	}

	// consume WebhookEvent
}
```

<!--BEGIN_REPO_NAV-->
<br/><table>
<thead><tr><th colspan="2">LiveKit Ecosystem</th></tr></thead>
<tbody>
<tr><td>LiveKit SDKs</td><td><a href="https://github.com/livekit/client-sdk-js">Browser</a> · <a href="https://github.com/livekit/client-sdk-swift">iOS/macOS/visionOS</a> · <a href="https://github.com/livekit/client-sdk-android">Android</a> · <a href="https://github.com/livekit/client-sdk-flutter">Flutter</a> · <a href="https://github.com/livekit/client-sdk-react-native">React Native</a> · <a href="https://github.com/livekit/rust-sdks">Rust</a> · <a href="https://github.com/livekit/node-sdks">Node.js</a> · <a href="https://github.com/livekit/python-sdks">Python</a> · <a href="https://github.com/livekit/client-sdk-unity">Unity</a> · <a href="https://github.com/livekit/client-sdk-unity-web">Unity (WebGL)</a> · <a href="https://github.com/livekit/client-sdk-esp32">ESP32</a></td></tr><tr></tr>
<tr><td>Server APIs</td><td><a href="https://github.com/livekit/node-sdks">Node.js</a> · <b>Golang</b> · <a href="https://github.com/livekit/server-sdk-ruby">Ruby</a> · <a href="https://github.com/livekit/server-sdk-kotlin">Java/Kotlin</a> · <a href="https://github.com/livekit/python-sdks">Python</a> · <a href="https://github.com/livekit/rust-sdks">Rust</a> · <a href="https://github.com/agence104/livekit-server-sdk-php">PHP (community)</a> · <a href="https://github.com/pabloFuente/livekit-server-sdk-dotnet">.NET (community)</a></td></tr><tr></tr>
<tr><td>UI Components</td><td><a href="https://github.com/livekit/components-js">React</a> · <a href="https://github.com/livekit/components-android">Android Compose</a> · <a href="https://github.com/livekit/components-swift">SwiftUI</a> · <a href="https://github.com/livekit/components-flutter">Flutter</a></td></tr><tr></tr>
<tr><td>Agents Frameworks</td><td><a href="https://github.com/livekit/agents">Python</a> · <a href="https://github.com/livekit/agents-js">Node.js</a> · <a href="https://github.com/livekit/agent-playground">Playground</a></td></tr><tr></tr>
<tr><td>Services</td><td><a href="https://github.com/livekit/livekit">LiveKit server</a> · <a href="https://github.com/livekit/egress">Egress</a> · <a href="https://github.com/livekit/ingress">Ingress</a> · <a href="https://github.com/livekit/sip">SIP</a></td></tr><tr></tr>
<tr><td>Resources</td><td><a href="https://docs.livekit.io">Docs</a> · <a href="https://github.com/livekit-examples">Example apps</a> · <a href="https://livekit.io/cloud">Cloud</a> · <a href="https://docs.livekit.io/home/self-hosting/deployment">Self-hosting</a> · <a href="https://github.com/livekit/livekit-cli">CLI</a></td></tr>
</tbody>
</table>
<!--END_REPO_NAV-->
