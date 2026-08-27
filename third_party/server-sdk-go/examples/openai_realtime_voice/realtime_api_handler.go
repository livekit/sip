package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/gorilla/websocket"
	"github.com/livekit/media-sdk"
	"github.com/livekit/protocol/logger"
)

type RealtimeAPIHandler struct {
	conn *websocket.Conn
	url  string
	cb   *RealtimeAPIHandlerCallbacks
}

type RealtimeAPIHandlerCallbacks struct {
	OnAudioReceived func(audio media.PCM16Sample)
}

func NewRealtimeAPIHandler(cb *RealtimeAPIHandlerCallbacks) (*RealtimeAPIHandler, error) {
	conn, url, err := connectToRealtimeAPI()
	if err != nil {
		return nil, err
	}

	ws := &RealtimeAPIHandler{
		conn: conn,
		url:  url,
		cb:   cb,
	}

	go ws.readMessages()
	return ws, nil
}

func (h *RealtimeAPIHandler) Url() string {
	return h.url
}

func (h *RealtimeAPIHandler) sendMessage(messageType string, key string, value string) error {
	message := fmt.Sprintf(`{"type": "%s", "%s": "%s"}`, messageType, key, value)
	err := h.conn.WriteMessage(websocket.TextMessage, []byte(message))
	if err != nil {
		return err
	}

	return nil
}

func (h *RealtimeAPIHandler) SendAudioChunk(sample media.PCM16Sample) error {
	bytes := make([]byte, len(sample)*2)
	for i, s := range sample {
		binary.LittleEndian.PutUint16(bytes[i*2:], uint16(s))
	}
	encodedSample := base64.StdEncoding.EncodeToString(bytes)
	return h.sendMessage("input_audio_buffer.append", "audio", encodedSample)
}

func (h *RealtimeAPIHandler) readMessages() {
	for {
		_, message, err := h.conn.ReadMessage()
		if err != nil {
			logger.Errorw("Error reading message", err)
			h.Close()
			break
		}

		h.handleMessage(string(message))
	}
}

func (h *RealtimeAPIHandler) handleMessage(message string) {
	var data map[string]interface{}
	err := json.Unmarshal([]byte(message), &data)
	if err != nil {
		logger.Errorw("Failed to unmarshal message", err)
		return
	}

	switch data["type"] {
	case "response.audio.delta":
		audioBase64 := data["delta"].(string)
		audioBytes, err := base64.StdEncoding.DecodeString(audioBase64)
		audioPCM16 := make(media.PCM16Sample, len(audioBytes)/2)
		for i := 0; i < len(audioBytes); i += 2 {
			audioPCM16[i/2] = int16(binary.LittleEndian.Uint16(audioBytes[i : i+2]))
		}
		if err != nil {
			logger.Errorw("Failed to decode audio", err)
			return
		}
		h.cb.OnAudioReceived(audioPCM16)
	default:
		logger.Errorw("Unknown message type", nil, "type", data["type"])
	}
}

func (h *RealtimeAPIHandler) Close() error {
	return h.conn.Close()
}
