package main

import (
	"net/http"
	"net/url"
	"os"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"

	lksdk "github.com/livekit/server-sdk-go/v2"
)

func loadEnv() {
	err := godotenv.Load()
	if err != nil {
		panic(err)
	}
}

func connectToRealtimeAPI() (*websocket.Conn, string, error) {
	wssUrl := os.Getenv("WSS_URL")
	apiKey := os.Getenv("API_KEY")
	model := os.Getenv("MODEL")

	u, err := url.Parse(wssUrl)
	if err != nil {
		panic(err)
	}

	q := u.Query()
	q.Add("model", model)
	u.RawQuery = q.Encode()

	header := http.Header{}
	header.Add("Authorization", "Bearer "+apiKey)
	header.Add("OpenAI-Beta", "realtime=v1")

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), header)
	if err != nil {
		return nil, "", err
	}

	return conn, wssUrl, nil
}

func connectToLKRoom(cb *lksdk.RoomCallback) (*lksdk.Room, error) {
	host := os.Getenv("LK_HOST")
	apiKey := os.Getenv("LK_API_KEY")
	apiSecret := os.Getenv("LK_API_SECRET")
	roomName := os.Getenv("LK_ROOM_NAME")
	participantIdentity := os.Getenv("LK_PARTICIPANT_IDENTITY")

	room, err := lksdk.ConnectToRoom(host, lksdk.ConnectInfo{
		APIKey:              apiKey,
		APISecret:           apiSecret,
		RoomName:            roomName,
		ParticipantIdentity: participantIdentity,
	}, cb)
	if err != nil {
		return nil, err
	}
	return room, nil
}
