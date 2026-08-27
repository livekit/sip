package lksdk

import (
	"time"

	"github.com/livekit/protocol/utils/guid"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
)

// Data types

type DataPacket interface {
	ToProto() *livekit.DataPacket
}

// Compile-time assertion for all supported data packet types.
var (
	_ DataPacket = (*UserDataPacket)(nil)
	_ DataPacket = (*livekit.SipDTMF)(nil)     // implemented in the protocol package
	_ DataPacket = (*livekit.ChatMessage)(nil) // implemented in the protocol package
)

// UserData creates a UserDataPacket with opaque bytes that can be sent via WebRTC.
func UserData(data []byte) *UserDataPacket {
	return &UserDataPacket{Payload: data}
}

// UserDataPacket is a custom user data that can be sent via WebRTC on a custom topic.
type UserDataPacket struct {
	Payload []byte
	Topic   string // optional
}

// ToProto converts the UserDataPacket to a protobuf DataPacket.
func (p *UserDataPacket) ToProto() *livekit.DataPacket {
	var topic *string
	if p.Topic != "" {
		topic = proto.String(p.Topic)
	}
	return &livekit.DataPacket{Value: &livekit.DataPacket_User{
		User: &livekit.UserPacket{
			Payload: p.Payload,
			Topic:   topic,
		},
	}}
}

// ChatMessage creates a chat message that can be sent via WebRTC.
// If timestamp is zero, current time will be used.
func ChatMessage(ts time.Time, text string) *livekit.ChatMessage {
	if ts.IsZero() {
		ts = time.Now()
	}
	return &livekit.ChatMessage{
		Id:        guid.New("MSG_"),
		Timestamp: ts.UnixMilli(),
		Message:   text,
	}
}

// receiving

type DataReceiveParams struct {
	Sender         *RemoteParticipant
	SenderIdentity string
	Topic          string // Deprecated: Use UserDataPacket.Topic
}

// publishing

type dataPublishOptions struct {
	Reliable              *bool
	DestinationIdentities []string
	Topic                 string
}

type DataPublishOption func(*dataPublishOptions)

func WithDataPublishTopic(topic string) DataPublishOption {
	return func(o *dataPublishOptions) {
		o.Topic = topic
	}
}

func WithDataPublishReliable(reliable bool) DataPublishOption {
	return func(o *dataPublishOptions) {
		o.Reliable = &reliable
	}
}

// WithDataPublishDestination sets specific participant identities to send data to.
// If not set, data will be sent to all participants.
func WithDataPublishDestination(identities []string) DataPublishOption {
	return func(o *dataPublishOptions) {
		o.DestinationIdentities = identities
	}
}
