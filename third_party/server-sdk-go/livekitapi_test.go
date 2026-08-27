// Copyright 2026 LiveKit, Inc.
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

// API tests that drive the unified LiveKitAPI against the shared mock server
// (livekit/livekit cmd/test-server), using its X-Lk-Mock JSON control protocol.
// They skip when no server is reachable (see testServerURL). Because the mock
// enforces the same per-method grants as the real server, a call that succeeds
// also proves the SDK attached the right grants automatically.
//
// The smoke tests fully populate each request so they double as a reference for
// what a complete call looks like and exercise field serialization end to end.
package lksdk

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twitchtv/twirp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
)

const (
	testAPIKey    = "devkey"
	testAPISecret = "secret" // matches the mock's default (livekit-server --dev)
)

func newTestAPI(t *testing.T) *LiveKitAPI {
	api, err := NewLiveKitAPI(WithURL(testServerURL(t)), WithAPIKey(testAPIKey, testAPISecret))
	require.NoError(t, err)
	return api
}

// mockControl is the X-Lk-Mock JSON control header understood by the mock server
// (see cmd/test-server/config.go). Every field is optional; the zero value means
// "behave normally".
type mockControl struct {
	FailRegions   []int           `json:"failRegions,omitempty"`
	FailMode      string          `json:"failMode,omitempty"`
	FailStatus    int             `json:"failStatus,omitempty"`
	FailTwirpCode string          `json:"failTwirpCode,omitempty"`
	DelayMs       *int            `json:"delayMs,omitempty"`
	RegionsStatus int             `json:"regionsStatus,omitempty"`
	Response      json.RawMessage `json:"response,omitempty"`
	SkipAuth      bool            `json:"skipAuth,omitempty"`
	SIPStatus     *sipStatus      `json:"sipStatus,omitempty"`
}

// sipStatus fails a SIP dial method with a SIP response (code + optional reason).
type sipStatus struct {
	Code   int    `json:"code"`
	Status string `json:"status,omitempty"`
}

// withMock attaches the X-Lk-Mock control header to ctx as a twirp request
// header, which prepareContext preserves alongside the signed token.
func withMock(ctx context.Context, t *testing.T, m mockControl) context.Context {
	b, err := json.Marshal(m)
	require.NoError(t, err)
	ctx, err = twirp.WithHTTPRequestHeaders(ctx, http.Header{"X-Lk-Mock": {string(b)}})
	require.NoError(t, err)
	return ctx
}

func mockCtx(t *testing.T, m mockControl) context.Context {
	return withMock(context.Background(), t, m)
}

func intPtr(i int) *int { return &i }

// runCalls asserts that each named call succeeds — i.e. the mock accepted the
// SDK's auto-signed grants and the response decoded.
func runCalls(t *testing.T, calls map[string]func() error) {
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, call())
		})
	}
}

func TestAPI_RoomServiceSmoke(t *testing.T) {
	api := newTestAPI(t)
	ctx := context.Background()
	const room, identity = "test-room", "participant-42"
	runCalls(t, map[string]func() error{
		"ListRooms": func() error {
			_, e := api.Room().ListRooms(ctx, &livekit.ListRoomsRequest{Names: []string{room, "lobby"}})
			return e
		},
		"DeleteRoom": func() error {
			_, e := api.Room().DeleteRoom(ctx, &livekit.DeleteRoomRequest{Room: room})
			return e
		},
		"ListParticipants": func() error {
			_, e := api.Room().ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: room})
			return e
		},
		"GetParticipant": func() error {
			_, e := api.Room().GetParticipant(ctx, &livekit.RoomParticipantIdentity{Room: room, Identity: identity})
			return e
		},
		"RemoveParticipant": func() error {
			_, e := api.Room().RemoveParticipant(ctx, &livekit.RoomParticipantIdentity{Room: room, Identity: identity})
			return e
		},
		"ForwardParticipant": func() error {
			_, e := api.Room().ForwardParticipant(ctx, &livekit.ForwardParticipantRequest{
				Room: room, Identity: identity, DestinationRoom: "overflow-room",
			})
			return e
		},
		"MoveParticipant": func() error {
			_, e := api.Room().MoveParticipant(ctx, &livekit.MoveParticipantRequest{
				Room: room, Identity: identity, DestinationRoom: "overflow-room",
			})
			return e
		},
		"MutePublishedTrack": func() error {
			_, e := api.Room().MutePublishedTrack(ctx, &livekit.MuteRoomTrackRequest{
				Room: room, Identity: identity, TrackSid: "TR_abc123", Muted: true,
			})
			return e
		},
		"UpdateParticipant": func() error {
			_, e := api.Room().UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
				Room:       room,
				Identity:   identity,
				Name:       "Alice",
				Metadata:   `{"role":"host"}`,
				Attributes: map[string]string{"seat": "1A"},
				Permission: &livekit.ParticipantPermission{
					CanSubscribe:      true,
					CanPublish:        true,
					CanPublishData:    true,
					CanPublishSources: []livekit.TrackSource{livekit.TrackSource_MICROPHONE, livekit.TrackSource_CAMERA},
					CanUpdateMetadata: true,
				},
			})
			return e
		},
		"UpdateSubscriptions": func() error {
			_, e := api.Room().UpdateSubscriptions(ctx, &livekit.UpdateSubscriptionsRequest{
				Room:      room,
				Identity:  identity,
				TrackSids: []string{"TR_abc123"},
				Subscribe: true,
				ParticipantTracks: []*livekit.ParticipantTracks{
					{ParticipantSid: "PA_xyz789", TrackSids: []string{"TR_abc123"}},
				},
			})
			return e
		},
		"UpdateRoomMetadata": func() error {
			_, e := api.Room().UpdateRoomMetadata(ctx, &livekit.UpdateRoomMetadataRequest{
				Room: room, Metadata: `{"scene":"intro"}`,
			})
			return e
		},
		"SendData": func() error {
			_, e := api.Room().SendData(ctx, &livekit.SendDataRequest{
				Room:                  room,
				Data:                  []byte("hello world"),
				Kind:                  livekit.DataPacket_RELIABLE,
				DestinationIdentities: []string{identity},
				Topic:                 proto.String("chat"),
			})
			return e
		},
	})
}

// CreateRoom is exercised in depth: parameter round-trip and error propagation.
func TestAPI_CreateRoom(t *testing.T) {
	api := newTestAPI(t)

	t.Run("echoes request fields", func(t *testing.T) {
		req := &livekit.CreateRoomRequest{
			Name:             "test-room",
			Metadata:         `{"scene":"lobby"}`,
			EmptyTimeout:     300,
			DepartureTimeout: 60,
			MaxParticipants:  50,
			Tags:             map[string]string{"env": "staging"},
			MinPlayoutDelay:  200,
			MaxPlayoutDelay:  2000,
			SyncStreams:      true,
			Agents:           []*livekit.RoomAgentDispatch{{AgentName: "greeter", Metadata: `{"lang":"en"}`}},
		}
		room, err := api.Room().CreateRoom(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, req.Name, room.Name)
		require.Equal(t, req.Metadata, room.Metadata)
		require.Equal(t, req.EmptyTimeout, room.EmptyTimeout)
		require.Equal(t, req.MaxParticipants, room.MaxParticipants)
		require.NotEmpty(t, room.Sid) // placeholder assigned by the mock
	})

	t.Run("propagates twirp errors", func(t *testing.T) {
		ctx := mockCtx(t, mockControl{FailRegions: []int{0}, FailStatus: 400, FailTwirpCode: "invalid_argument"})
		_, err := api.Room().CreateRoom(ctx, &livekit.CreateRoomRequest{Name: "test-room"})
		var terr twirp.Error
		require.ErrorAs(t, err, &terr)
		require.Equal(t, twirp.InvalidArgument, terr.Code())
	})
}

func TestAPI_EgressSmoke(t *testing.T) {
	api := newTestAPI(t)
	ctx := context.Background()
	runCalls(t, map[string]func() error{
		"UpdateLayout": func() error {
			_, e := api.Egress().UpdateLayout(ctx, &livekit.UpdateLayoutRequest{EgressId: "EG_abc123", Layout: "speaker"})
			return e
		},
		"UpdateStream": func() error {
			_, e := api.Egress().UpdateStream(ctx, &livekit.UpdateStreamRequest{
				EgressId:         "EG_abc123",
				AddOutputUrls:    []string{"rtmps://a.example.com/live/key-new"},
				RemoveOutputUrls: []string{"rtmps://a.example.com/live/key-old"},
			})
			return e
		},
		"ListEgress": func() error {
			_, e := api.Egress().ListEgress(ctx, &livekit.ListEgressRequest{RoomName: "test-room", EgressId: "EG_abc123", Active: true})
			return e
		},
		"StopEgress": func() error {
			_, e := api.Egress().StopEgress(ctx, &livekit.StopEgressRequest{EgressId: "EG_abc123"})
			return e
		},
	})
}

// Egress start requests take several distinct shapes; cover the common ones with
// a representative output each.
func TestAPI_EgressStartSmoke(t *testing.T) {
	api := newTestAPI(t)
	ctx := context.Background()
	mp4 := func(path string) []*livekit.EncodedFileOutput {
		return []*livekit.EncodedFileOutput{{FileType: livekit.EncodedFileType_MP4, Filepath: path}}
	}
	runCalls(t, map[string]func() error{
		"Unified": func() error {
			_, e := api.Egress().StartEgress(ctx, &livekit.StartEgressRequest{
				RoomName: "test-room",
				Source:   &livekit.StartEgressRequest_Web{Web: &livekit.WebSource{Url: "https://example.com/scene"}},
				Outputs: []*livekit.Output{
					{Config: &livekit.Output_Stream{Stream: &livekit.StreamOutput{
						Protocol: livekit.StreamProtocol_RTMP,
						Urls:     []string{"rtmps://a.example.com/live/key"},
					}}},
				},
			})
			return e
		},
		"RoomComposite": func() error {
			_, e := api.Egress().StartRoomCompositeEgress(ctx, &livekit.RoomCompositeEgressRequest{
				RoomName:    "test-room",
				Layout:      "grid",
				FileOutputs: mp4("recordings/room-{room_name}.mp4"),
			})
			return e
		},
		"Web": func() error {
			_, e := api.Egress().StartWebEgress(ctx, &livekit.WebEgressRequest{
				Url: "https://example.com/scene",
				StreamOutputs: []*livekit.StreamOutput{
					{Protocol: livekit.StreamProtocol_RTMP, Urls: []string{"rtmps://a.example.com/live/key"}},
				},
			})
			return e
		},
		"Participant": func() error {
			_, e := api.Egress().StartParticipantEgress(ctx, &livekit.ParticipantEgressRequest{
				RoomName:    "test-room",
				Identity:    "participant-42",
				ScreenShare: true,
				FileOutputs: mp4("recordings/participant-{publisher_identity}.mp4"),
			})
			return e
		},
		"TrackComposite": func() error {
			_, e := api.Egress().StartTrackCompositeEgress(ctx, &livekit.TrackCompositeEgressRequest{
				RoomName:     "test-room",
				AudioTrackId: "TR_audio1",
				VideoTrackId: "TR_video1",
				FileOutputs:  mp4("recordings/track-composite.mp4"),
			})
			return e
		},
		"Track": func() error {
			_, e := api.Egress().StartTrackEgress(ctx, &livekit.TrackEgressRequest{
				RoomName: "test-room",
				TrackId:  "TR_video1",
				Output:   &livekit.TrackEgressRequest_File{File: &livekit.DirectFileOutput{Filepath: "recordings/track.mp4"}},
			})
			return e
		},
	})
}

func TestAPI_IngressSmoke(t *testing.T) {
	api := newTestAPI(t)
	ctx := context.Background()
	runCalls(t, map[string]func() error{
		"CreateIngress": func() error {
			_, e := api.Ingress().CreateIngress(ctx, &livekit.CreateIngressRequest{
				InputType:           livekit.IngressInput_RTMP_INPUT,
				Name:                "stream-input",
				RoomName:            "test-room",
				ParticipantIdentity: "ingress-bot",
				ParticipantName:     "Live Stream",
				ParticipantMetadata: `{"source":"rtmp"}`,
				EnableTranscoding:   proto.Bool(true),
				Audio: &livekit.IngressAudioOptions{
					Name:   "audio",
					Source: livekit.TrackSource_MICROPHONE,
					EncodingOptions: &livekit.IngressAudioOptions_Options{
						Options: &livekit.IngressAudioEncodingOptions{AudioCodec: livekit.AudioCodec_OPUS, Bitrate: 64000, Channels: 2},
					},
				},
				Video: &livekit.IngressVideoOptions{
					Name:   "video",
					Source: livekit.TrackSource_CAMERA,
					EncodingOptions: &livekit.IngressVideoOptions_Preset{
						Preset: livekit.IngressVideoEncodingPreset_H264_720P_30FPS_3_LAYERS,
					},
				},
			})
			return e
		},
		"UpdateIngress": func() error {
			_, e := api.Ingress().UpdateIngress(ctx, &livekit.UpdateIngressRequest{
				IngressId:           "IN_abc123",
				Name:                "stream-input-v2",
				RoomName:            "test-room",
				ParticipantIdentity: "ingress-bot",
				ParticipantName:     "Live Stream",
				Enabled:             proto.Bool(true),
			})
			return e
		},
		"ListIngress": func() error {
			_, e := api.Ingress().ListIngress(ctx, &livekit.ListIngressRequest{RoomName: "test-room", IngressId: "IN_abc123"})
			return e
		},
		"DeleteIngress": func() error {
			_, e := api.Ingress().DeleteIngress(ctx, &livekit.DeleteIngressRequest{IngressId: "IN_abc123"})
			return e
		},
	})
}

func TestAPI_SIPSmoke(t *testing.T) {
	api := newTestAPI(t)
	ctx := context.Background()
	runCalls(t, map[string]func() error{
		"CreateSIPInboundTrunk": func() error {
			_, e := api.SIP().CreateSIPInboundTrunk(ctx, &livekit.CreateSIPInboundTrunkRequest{
				Trunk: &livekit.SIPInboundTrunkInfo{
					Name:             "inbound",
					Metadata:         `{"provider":"telco"}`,
					Numbers:          []string{"+15105550100"},
					AllowedAddresses: []string{"203.0.113.0/24"},
					AllowedNumbers:   []string{"+15105550111"},
					AuthUsername:     "trunk-user",
					AuthPassword:     "trunk-pass",
					KrispEnabled:     true,
				},
			})
			return e
		},
		"CreateSIPOutboundTrunk": func() error {
			_, e := api.SIP().CreateSIPOutboundTrunk(ctx, &livekit.CreateSIPOutboundTrunkRequest{
				Trunk: &livekit.SIPOutboundTrunkInfo{
					Name:               "outbound",
					Address:            "sip.telco.example.com",
					Transport:          livekit.SIPTransport_SIP_TRANSPORT_TLS,
					DestinationCountry: "US",
					Numbers:            []string{"+15105550100"},
					AuthUsername:       "trunk-user",
					AuthPassword:       "trunk-pass",
				},
			})
			return e
		},
		"UpdateSIPInboundTrunk": func() error {
			_, e := api.SIP().UpdateSIPInboundTrunk(ctx, &livekit.UpdateSIPInboundTrunkRequest{
				SipTrunkId: "ST_abc123",
				Action: &livekit.UpdateSIPInboundTrunkRequest_Replace{
					Replace: &livekit.SIPInboundTrunkInfo{Name: "inbound-v2", Numbers: []string{"+15105550100"}},
				},
			})
			return e
		},
		"UpdateSIPOutboundTrunk": func() error {
			_, e := api.SIP().UpdateSIPOutboundTrunk(ctx, &livekit.UpdateSIPOutboundTrunkRequest{
				SipTrunkId: "ST_abc123",
				Action: &livekit.UpdateSIPOutboundTrunkRequest_Replace{
					Replace: &livekit.SIPOutboundTrunkInfo{
						Name: "outbound-v2", Address: "sip.telco.example.com",
						Transport: livekit.SIPTransport_SIP_TRANSPORT_TLS, Numbers: []string{"+15105550100"},
					},
				},
			})
			return e
		},
		"GetSIPInboundTrunksByIDs": func() error {
			_, e := api.SIP().GetSIPInboundTrunksByIDs(ctx, []string{"ST_abc123", "ST_def456"})
			return e
		},
		"GetSIPOutboundTrunksByIDs": func() error { _, e := api.SIP().GetSIPOutboundTrunksByIDs(ctx, []string{"ST_abc123"}); return e },
		"ListSIPTrunk":              func() error { _, e := api.SIP().ListSIPTrunk(ctx, &livekit.ListSIPTrunkRequest{}); return e },
		"ListSIPInboundTrunk": func() error {
			_, e := api.SIP().ListSIPInboundTrunk(ctx, &livekit.ListSIPInboundTrunkRequest{
				TrunkIds: []string{"ST_abc123"}, Numbers: []string{"+15105550100"},
			})
			return e
		},
		"ListSIPOutboundTrunk": func() error {
			_, e := api.SIP().ListSIPOutboundTrunk(ctx, &livekit.ListSIPOutboundTrunkRequest{
				TrunkIds: []string{"ST_abc123"}, Numbers: []string{"+15105550100"},
			})
			return e
		},
		"DeleteSIPTrunk": func() error {
			_, e := api.SIP().DeleteSIPTrunk(ctx, &livekit.DeleteSIPTrunkRequest{SipTrunkId: "ST_abc123"})
			return e
		},
		"CreateSIPDispatchRule": func() error {
			_, e := api.SIP().CreateSIPDispatchRule(ctx, &livekit.CreateSIPDispatchRuleRequest{
				DispatchRule: &livekit.SIPDispatchRuleInfo{
					Name:     "direct-to-support",
					Metadata: `{"team":"support"}`,
					TrunkIds: []string{"ST_abc123"},
					Rule: &livekit.SIPDispatchRule{
						Rule: &livekit.SIPDispatchRule_DispatchRuleDirect{
							DispatchRuleDirect: &livekit.SIPDispatchRuleDirect{RoomName: "support", Pin: "1234"},
						},
					},
				},
			})
			return e
		},
		"UpdateSIPDispatchRule": func() error {
			_, e := api.SIP().UpdateSIPDispatchRule(ctx, &livekit.UpdateSIPDispatchRuleRequest{
				SipDispatchRuleId: "SDR_abc123",
				Action: &livekit.UpdateSIPDispatchRuleRequest_Replace{
					Replace: &livekit.SIPDispatchRuleInfo{
						Name: "individual-v2",
						Rule: &livekit.SIPDispatchRule{
							Rule: &livekit.SIPDispatchRule_DispatchRuleIndividual{
								DispatchRuleIndividual: &livekit.SIPDispatchRuleIndividual{RoomPrefix: "call-"},
							},
						},
					},
				},
			})
			return e
		},
		"GetSIPDispatchRulesByIDs": func() error { _, e := api.SIP().GetSIPDispatchRulesByIDs(ctx, []string{"SDR_abc123"}); return e },
		"ListSIPDispatchRule": func() error {
			_, e := api.SIP().ListSIPDispatchRule(ctx, &livekit.ListSIPDispatchRuleRequest{
				DispatchRuleIds: []string{"SDR_abc123"}, TrunkIds: []string{"ST_abc123"},
			})
			return e
		},
		"DeleteSIPDispatchRule": func() error {
			_, e := api.SIP().DeleteSIPDispatchRule(ctx, &livekit.DeleteSIPDispatchRuleRequest{SipDispatchRuleId: "SDR_abc123"})
			return e
		},
	})
}

// CreateSIPParticipant/TransferSIPParticipant block until the call is answered
// in the mock, so skip that wait with delayMs:0 except in the timeout test below.
func TestAPI_SIPParticipant(t *testing.T) {
	api := newTestAPI(t)
	noDelay := mockCtx(t, mockControl{DelayMs: intPtr(0)})

	t.Run("CreateSIPParticipant echoes fields", func(t *testing.T) {
		resp, err := api.SIP().CreateSIPParticipant(context.Background(), &livekit.CreateSIPParticipantRequest{
			SipTrunkId:            "ST_abc123",
			SipCallTo:             "+15105550100",
			SipNumber:             "+15105550111",
			RoomName:              "test-room",
			ParticipantIdentity:   "sip-caller",
			ParticipantName:       "SIP Caller",
			ParticipantMetadata:   `{"source":"pstn"}`,
			ParticipantAttributes: map[string]string{"region": "us-west"},
			Dtmf:                  "1234#",
			PlayDialtone:          true,
			MaxCallDuration:       durationpb.New(time.Hour),
		})
		require.NoError(t, err)
		require.Equal(t, "test-room", resp.RoomName)
		require.Equal(t, "sip-caller", resp.ParticipantIdentity)
	})

	t.Run("CreateSIPParticipant waitUntilAnswered", func(t *testing.T) {
		_, err := api.SIP().CreateSIPParticipant(noDelay, &livekit.CreateSIPParticipantRequest{
			SipTrunkId:        "ST_abc123",
			SipCallTo:         "+15105550100",
			RoomName:          "test-room",
			WaitUntilAnswered: true,
			RingingTimeout:    durationpb.New(2 * time.Second),
		})
		require.NoError(t, err)
	})

	t.Run("TransferSIPParticipant", func(t *testing.T) {
		_, err := api.SIP().TransferSIPParticipant(noDelay, &livekit.TransferSIPParticipantRequest{
			RoomName:            "test-room",
			ParticipantIdentity: "sip-caller",
			TransferTo:          "tel:+15559876543",
			PlayDialtone:        true,
			RingingTimeout:      durationpb.New(2 * time.Second),
		})
		require.NoError(t, err)
	})
}

func TestAPI_ConnectorSmoke(t *testing.T) {
	api := newTestAPI(t)
	ctx := context.Background()
	offer := &livekit.SessionDescription{Type: "offer", Sdp: "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\n"}
	answer := &livekit.SessionDescription{Type: "answer", Sdp: "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\n"}
	runCalls(t, map[string]func() error{
		"DialWhatsAppCall": func() error {
			_, e := api.Connector().DialWhatsAppCall(ctx, &livekit.DialWhatsAppCallRequest{
				WhatsappPhoneNumberId:   "123456789012345",
				WhatsappToPhoneNumber:   "+15105550100",
				WhatsappApiKey:          "wa-secret-key",
				WhatsappCloudApiVersion: "23.0",
				RoomName:                "test-room",
				ParticipantIdentity:     "whatsapp-caller",
				ParticipantName:         "WhatsApp Caller",
				DestinationCountry:      "US",
				RingingTimeout:          durationpb.New(30 * time.Second),
			})
			return e
		},
		"AcceptWhatsAppCall": func() error {
			_, e := api.Connector().AcceptWhatsAppCall(ctx, &livekit.AcceptWhatsAppCallRequest{
				WhatsappPhoneNumberId:   "123456789012345",
				WhatsappApiKey:          "wa-secret-key",
				WhatsappCloudApiVersion: "23.0",
				WhatsappCallId:          "wacid.HBgLABC",
				Sdp:                     answer,
				RoomName:                "test-room",
				ParticipantIdentity:     "whatsapp-callee",
			})
			return e
		},
		"ConnectWhatsAppCall": func() error {
			_, e := api.Connector().ConnectWhatsAppCall(ctx, &livekit.ConnectWhatsAppCallRequest{
				WhatsappCallId: "wacid.HBgLABC", Sdp: offer,
			})
			return e
		},
		"DisconnectWhatsAppCall": func() error {
			_, e := api.Connector().DisconnectWhatsAppCall(ctx, &livekit.DisconnectWhatsAppCallRequest{
				WhatsappCallId:   "wacid.HBgLABC",
				WhatsappApiKey:   "wa-secret-key",
				DisconnectReason: livekit.DisconnectWhatsAppCallRequest_BUSINESS_INITIATED,
			})
			return e
		},
		"ConnectTwilioCall": func() error {
			_, e := api.Connector().ConnectTwilioCall(ctx, &livekit.ConnectTwilioCallRequest{
				TwilioCallDirection: livekit.ConnectTwilioCallRequest_TWILIO_CALL_DIRECTION_INBOUND,
				RoomName:            "test-room",
				ParticipantIdentity: "twilio-caller",
				DestinationCountry:  "US",
			})
			return e
		},
	})
}

func TestAPI_AgentDispatchSmoke(t *testing.T) {
	api := newTestAPI(t)
	ctx := context.Background()
	const room = "test-room"
	runCalls(t, map[string]func() error{
		"CreateDispatch": func() error {
			_, e := api.AgentDispatch().CreateDispatch(ctx, &livekit.CreateAgentDispatchRequest{
				Room: room, AgentName: "inbound-agent", Metadata: `{"lang":"en"}`,
			})
			return e
		},
		"DeleteDispatch": func() error {
			_, e := api.AgentDispatch().DeleteDispatch(ctx, &livekit.DeleteAgentDispatchRequest{DispatchId: "AD_abc123", Room: room})
			return e
		},
		"ListDispatch": func() error {
			_, e := api.AgentDispatch().ListDispatch(ctx, &livekit.ListAgentDispatchRequest{Room: room, DispatchId: "AD_abc123"})
			return e
		},
		"GetDispatch": func() error {
			_, e := api.AgentDispatch().GetDispatch(ctx, "AD_abc123", room)
			return e
		},
	})
}

// A pre-signed token is sent verbatim (no secret), enabling client-side use. The
// token must already carry the grants for the call.
func TestAPI_TokenAuth(t *testing.T) {
	url := testServerURL(t)
	token, err := auth.NewAccessToken(testAPIKey, testAPISecret).
		SetVideoGrant(&auth.VideoGrant{RoomCreate: true}).
		ToJWT()
	require.NoError(t, err)

	api, err := NewLiveKitAPI(WithURL(url), WithToken(token))
	require.NoError(t, err)
	room, err := api.Room().CreateRoom(context.Background(), &livekit.CreateRoomRequest{Name: "token-room"})
	require.NoError(t, err)
	require.Equal(t, "token-room", room.Name)
}

// Explicitly provided credentials must win over ambient environment variables:
// an env LIVEKIT_TOKEN must not hijack a WithAPIKey caller. The env fallback
// applies only when no auth was passed. No server needed—this checks resolution.
func TestAPI_ExplicitCredsBeatEnvToken(t *testing.T) {
	const url = "http://localhost:7880"
	t.Setenv("LIVEKIT_TOKEN", "ambient-token")

	// Explicit key/secret: the env token must be ignored.
	keyAPI, err := NewLiveKitAPI(WithURL(url), WithAPIKey(testAPIKey, testAPISecret))
	require.NoError(t, err)
	require.Equal(t, testAPIKey, keyAPI.Room().apiKey)
	require.Equal(t, testAPISecret, keyAPI.Room().apiSecret)
	require.Empty(t, keyAPI.Room().token)

	// No explicit auth: the env token is used.
	envAPI, err := NewLiveKitAPI(WithURL(url))
	require.NoError(t, err)
	require.Equal(t, "ambient-token", envAPI.Room().token)
}

// SIP dialing must outlast ringing: when the answer takes longer than the dial
// budget (ringing timeout + margin) the call times out with a deadline error,
// while a prompt answer within the budget succeeds.
func TestAPI_SIPDialTimeout(t *testing.T) {
	api := newTestAPI(t)

	t.Run("times out when the answer exceeds the dial budget", func(t *testing.T) {
		// ringing 1s -> ~3s dial budget; the mock delays the answer past it.
		ctx := mockCtx(t, mockControl{DelayMs: intPtr(4000)})
		_, err := api.SIP().CreateSIPParticipant(ctx, &livekit.CreateSIPParticipantRequest{
			SipTrunkId:        "ST_abc123",
			SipCallTo:         "+15105550100",
			RoomName:          "test-room",
			WaitUntilAnswered: true,
			RingingTimeout:    durationpb.New(time.Second),
		})
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("succeeds within the dial budget", func(t *testing.T) {
		ctx := mockCtx(t, mockControl{DelayMs: intPtr(200)}) // answered well within the budget
		_, err := api.SIP().CreateSIPParticipant(ctx, &livekit.CreateSIPParticipantRequest{
			SipTrunkId:        "ST_abc123",
			SipCallTo:         "+15105550100",
			RoomName:          "test-room",
			WaitUntilAnswered: true,
		})
		require.NoError(t, err)
	})
}

// A failed SIP dial returns a SIP status. The SIP client surfaces it as a gRPC
// status error with the mapped code, and the underlying SIP status (code + reason)
// is recoverable via SIPStatusFrom.
func TestAPI_SIPCallErrors(t *testing.T) {
	api := newTestAPI(t)

	cases := []struct {
		name     string
		sip      sipStatus
		grpcCode codes.Code
		sipCode  livekit.SIPStatusCode
	}{
		{"busy", sipStatus{Code: 486, Status: "Busy Here"}, codes.ResourceExhausted, livekit.SIPStatusCode_SIP_STATUS_BUSY_HERE},
		{"declined", sipStatus{Code: 603, Status: "Decline"}, codes.PermissionDenied, livekit.SIPStatusCode_SIP_STATUS_GLOBAL_DECLINE},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sip := tc.sip
			// delayMs:0 returns the SIP verdict immediately rather than blocking.
			ctx := mockCtx(t, mockControl{DelayMs: intPtr(0), SIPStatus: &sip})
			_, err := api.SIP().CreateSIPParticipant(ctx, &livekit.CreateSIPParticipantRequest{
				SipTrunkId: "ST_abc123",
				SipCallTo:  "+15105550100",
				RoomName:   "test-room",
			})
			require.Error(t, err)
			require.Equal(t, tc.grpcCode, status.Code(err))

			st := SIPStatusFrom(err)
			require.NotNil(t, st, "SIP status should be recoverable from the error")
			require.Equal(t, tc.sipCode, st.Code)
			require.Equal(t, tc.sip.Status, st.Status)
		})
	}
}

// When the callee doesn't answer within ringing_timeout, the server returns SIP
// 408 Request Timeout, surfaced as a DeadlineExceeded error.
func TestAPI_SIPNoAnswer(t *testing.T) {
	api := newTestAPI(t)
	// delayMs:0 stands in for the server enforcing ringing_timeout and returning
	// the no-answer verdict, rather than the mock blocking for the full window.
	ctx := mockCtx(t, mockControl{DelayMs: intPtr(0), SIPStatus: &sipStatus{Code: 408, Status: "Request Timeout"}})
	_, err := api.SIP().CreateSIPParticipant(ctx, &livekit.CreateSIPParticipantRequest{
		SipTrunkId:        "ST_abc123",
		SipCallTo:         "+15105550100",
		RoomName:          "test-room",
		WaitUntilAnswered: true,
		RingingTimeout:    durationpb.New(2 * time.Second),
	})
	require.Error(t, err)
	require.Equal(t, codes.DeadlineExceeded, status.Code(err))

	st := SIPStatusFrom(err)
	require.NotNil(t, st, "SIP status should be recoverable from the error")
	require.Equal(t, livekit.SIPStatusCode_SIP_STATUS_REQUEST_TIMEOUT, st.Code)
}
