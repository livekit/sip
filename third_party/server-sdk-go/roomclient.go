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

package lksdk

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/twitchtv/twirp"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/xtwirp"
	"github.com/livekit/server-sdk-go/v2/signalling"
)

type RoomServiceClient struct {
	roomService livekit.RoomService
	authBase
}

func NewRoomServiceClient(url string, apiKey string, secretKey string, opts ...twirp.ClientOption) *RoomServiceClient {
	return newRoomServiceClient(url, authBase{apiKey: apiKey, apiSecret: secretKey}, newAPIHTTPClient(), opts...)
}

func newRoomServiceClient(url string, auth authBase, httpClient *http.Client, opts ...twirp.ClientOption) *RoomServiceClient {
	opts = append(opts, xtwirp.DefaultClientOptions()...)
	url = signalling.ToHttpURL(url)
	client := livekit.NewRoomServiceProtobufClient(url, httpClient, opts...)
	return &RoomServiceClient{
		roomService: client,
		authBase:    auth,
	}
}

func (c *RoomServiceClient) CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest) (*livekit.Room, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomCreate: true})
	if err != nil {
		return nil, err
	}

	return c.roomService.CreateRoom(ctx, req)
}

func (c *RoomServiceClient) ListRooms(ctx context.Context, req *livekit.ListRoomsRequest) (*livekit.ListRoomsResponse, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomList: true})
	if err != nil {
		return nil, err
	}

	return c.roomService.ListRooms(ctx, req)
}

func (c *RoomServiceClient) DeleteRoom(ctx context.Context, req *livekit.DeleteRoomRequest) (*livekit.DeleteRoomResponse, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomCreate: true})
	if err != nil {
		return nil, err
	}

	return c.roomService.DeleteRoom(ctx, req)
}

func (c *RoomServiceClient) ListParticipants(ctx context.Context, req *livekit.ListParticipantsRequest) (*livekit.ListParticipantsResponse, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomAdmin: true, Room: req.Room})
	if err != nil {
		return nil, err
	}

	return c.roomService.ListParticipants(ctx, req)
}

func (c *RoomServiceClient) GetParticipant(ctx context.Context, req *livekit.RoomParticipantIdentity) (*livekit.ParticipantInfo, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomAdmin: true, Room: req.Room})
	if err != nil {
		return nil, err
	}

	return c.roomService.GetParticipant(ctx, req)
}

func (c *RoomServiceClient) RemoveParticipant(ctx context.Context, req *livekit.RoomParticipantIdentity) (*livekit.RemoveParticipantResponse, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomAdmin: true, Room: req.Room})
	if err != nil {
		return nil, err
	}

	return c.roomService.RemoveParticipant(ctx, req)
}

// Forward a participant's track(s) to another room. Requires `roomAdmin` and `destinationRoom`. The forwarding will
// stop when the participant leaves the room or `RemoveParticipant` has been called in the destination room.
// A participant can be forwarded to multiple rooms. The destination room will be created if it does not exist.
func (c *RoomServiceClient) ForwardParticipant(ctx context.Context, req *livekit.ForwardParticipantRequest) (*livekit.ForwardParticipantResponse, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomAdmin: true, Room: req.Room, DestinationRoom: req.DestinationRoom})
	if err != nil {
		return nil, err
	}
	return c.roomService.ForwardParticipant(ctx, req)
}

// Move a connected participant to a different room. Requires `roomAdmin` and `destinationRoom`.
// The participant will be removed from the current room and added to the destination room.
// From other observers' perspective, the participant would've disconnected from the previous room and joined the new one.
func (c *RoomServiceClient) MoveParticipant(ctx context.Context, req *livekit.MoveParticipantRequest) (*livekit.MoveParticipantResponse, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomAdmin: true, Room: req.Room, DestinationRoom: req.DestinationRoom})
	if err != nil {
		return nil, err
	}
	return c.roomService.MoveParticipant(ctx, req)
}

func (c *RoomServiceClient) MutePublishedTrack(ctx context.Context, req *livekit.MuteRoomTrackRequest) (*livekit.MuteRoomTrackResponse, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomAdmin: true, Room: req.Room})
	if err != nil {
		return nil, err
	}

	return c.roomService.MutePublishedTrack(ctx, req)
}

func (c *RoomServiceClient) UpdateParticipant(ctx context.Context, req *livekit.UpdateParticipantRequest) (*livekit.ParticipantInfo, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomAdmin: true, Room: req.Room})
	if err != nil {
		return nil, err
	}
	return c.roomService.UpdateParticipant(ctx, req)
}

func (c *RoomServiceClient) UpdateSubscriptions(ctx context.Context, req *livekit.UpdateSubscriptionsRequest) (*livekit.UpdateSubscriptionsResponse, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomAdmin: true, Room: req.Room})
	if err != nil {
		return nil, err
	}
	return c.roomService.UpdateSubscriptions(ctx, req)
}

func (c *RoomServiceClient) UpdateRoomMetadata(ctx context.Context, req *livekit.UpdateRoomMetadataRequest) (*livekit.Room, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomAdmin: true, Room: req.Room})
	if err != nil {
		return nil, err
	}
	return c.roomService.UpdateRoomMetadata(ctx, req)
}

func (c *RoomServiceClient) SendData(ctx context.Context, req *livekit.SendDataRequest) (*livekit.SendDataResponse, error) {
	ctx, err := c.prepareContext(ctx, withVideoGrant{RoomAdmin: true, Room: req.Room})
	if err != nil {
		return nil, err
	}
	// add a nonce to enable receiver to de-dupe
	bytes, err := uuid.New().MarshalBinary()
	if err == nil {
		req.Nonce = bytes
	}
	return c.roomService.SendData(ctx, req)
}

// CreateToken returns an AccessToken seeded with this client's API key and
// secret, ready for you to add grants and call ToJWT.
//
// This requires API key/secret authentication (NewRoomServiceClient, or
// NewLiveKitAPI with WithAPIKey). If the client was created with token-based
// auth (WithToken) there is no secret to sign with, so the returned token's
// ToJWT reports auth.ErrKeysMissing ("missing API key or secret key").
func (c *RoomServiceClient) CreateToken() *auth.AccessToken {
	return auth.NewAccessToken(c.apiKey, c.apiSecret)
}
