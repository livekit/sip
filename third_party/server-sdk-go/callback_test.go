package lksdk_test

import (
	"fmt"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

// ExampleRoomCallback demonstrates usage of RoomCallback to handle various room events
func ExampleRoomCallback() {
	// Create a new callback handler
	cb := lksdk.NewRoomCallback()

	// Handle data packets received from other participants
	cb.OnDataPacket = func(data lksdk.DataPacket, params lksdk.DataReceiveParams) {
		// handle DTMF
		switch val := data.(type) {
		case *livekit.SipDTMF:
			fmt.Printf("Received DTMF from %s: %s (%d)\n", params.SenderIdentity, val.Digit, val.Code)
		case *lksdk.UserDataPacket:
			fmt.Printf("Received user data from %s: %s\n", params.SenderIdentity, string(val.Payload))
		}
	}

	// Handle participant metadata changes
	cb.OnAttributesChanged = func(changed map[string]string, p lksdk.Participant) {
		fmt.Printf("Participant %s attributes changed: %v\n", p.Identity(), changed)
	}

	// Handle when current participant becomes disconnected
	cb.OnDisconnectedWithReason = func(reason lksdk.DisconnectionReason) {
		fmt.Printf("Disconnected from room: %s\n", reason)
	}

	// Create a new room with the callback
	room := lksdk.NewRoom(cb)
	room.JoinWithToken("wss://myproject.livekit.cloud", "my-token")
}
