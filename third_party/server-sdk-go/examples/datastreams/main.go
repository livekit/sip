package main

import (
	"flag"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

const (
	sender   = "sender"
	receiver = "receiver"
)

var (
	host, apiKey, apiSecret string

	testFileName = "test.mp4"
)

func init() {
	flag.StringVar(&host, "host", "", "livekit server host")
	flag.StringVar(&apiKey, "api-key", "", "livekit api key")
	flag.StringVar(&apiSecret, "api-secret", "", "livekit api secret")
}

func downloadTestFile() {
	res, err := http.Get("https://www.w3schools.com/html/mov_bbb.mp4")
	if err != nil {
		logger.Errorw("failed to download test file", err)
		return
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		logger.Errorw("failed to read test file", err)
		return
	}
	os.WriteFile(testFileName, body, 0644)
}

func main() {
	logger.InitFromConfig(&logger.Config{Level: "info"}, "ds-demo")
	lksdk.SetLogger(logger.GetLogger())
	flag.Parse()

	roomName := "datastreams-demo-" + uuid.New().String()[:8]

	receiverRoom, err := lksdk.ConnectToRoom(host, lksdk.ConnectInfo{
		APIKey:              apiKey,
		APISecret:           apiSecret,
		RoomName:            roomName,
		ParticipantIdentity: receiver,
	}, nil)
	if err != nil {
		logger.Errorw("failed to connect to room", err, "participant", receiver)
		return
	}

	senderRoom, err := lksdk.ConnectToRoom(host, lksdk.ConnectInfo{
		APIKey:              apiKey,
		APISecret:           apiSecret,
		RoomName:            roomName,
		ParticipantIdentity: sender,
	}, nil)
	if err != nil {
		logger.Errorw("failed to connect to room", err, "participant", sender)
		return
	}

	var wg sync.WaitGroup

	defer func() {
		senderRoom.UnregisterTextStreamHandler("text-read-iter")
		senderRoom.UnregisterTextStreamHandler("text-read-all")
		senderRoom.UnregisterByteStreamHandler("file-read-all")

		senderRoom.Disconnect()
		receiverRoom.Disconnect()
	}()

	// reading text in chunks
	receiverRoom.RegisterTextStreamHandler("text-read-iter", func(reader *lksdk.TextStreamReader, participantIdentity string) {
		logger.Infow("received text-read-iter text stream")
		res := ""
		for {
			word, err := reader.ReadString(' ')
			logger.Infow("read word", "word", word)
			res += word
			if err != nil {
				// EOF represents the end of the stream
				if err == io.EOF {
					break
				} else {
					logger.Errorw("failed to read text-read-iter text stream", err)
					break
				}
			}
		}
		logger.Infow("read text-read-iter text stream", "text", res)
		wg.Done()
	})

	// reading text as a whole
	receiverRoom.RegisterTextStreamHandler("text-read-all", func(reader *lksdk.TextStreamReader, participantIdentity string) {
		logger.Infow("received text-read-all text stream")
		res := reader.ReadAll()
		logger.Infow("read text-read-all text stream", "text", res)
		wg.Done()
	})

	receiverRoom.RegisterByteStreamHandler("file-read-all", func(reader *lksdk.ByteStreamReader, participantIdentity string) {
		logger.Infow("received file-read-all file stream")
		streamBytes := reader.ReadAll()

		fileName := "received"
		ext, _ := mime.ExtensionsByType(reader.Info.MimeType)
		fileName += ext[1]
		os.WriteFile(fileName, streamBytes, 0644)
		logger.Infow("wrote file-read-all file stream to received.mp4")
		wg.Done()
	})

	// sending text as a whole
	wg.Add(1)
	text := "Lorem ipsum dolor sit amet..."
	topic := "text-read-iter"
	senderRoom.LocalParticipant.SendText(text, lksdk.StreamTextOptions{
		Topic: topic,
	})

	// sending text in chunks
	wg.Add(1)
	topic = "text-read-all"
	writer := senderRoom.LocalParticipant.StreamText(lksdk.StreamTextOptions{
		Topic: topic,
	})

	chunks := strings.Split(text, " ")
	for i, chunk := range chunks {
		isLast := i == len(chunks)-1
		if !isLast {
			chunk += " "
		}

		onDone := func() {
			if isLast {
				writer.Close()
			}
		}
		writer.Write(chunk, &onDone)
	}

	// sending file as a whole
	wg.Add(1)
	downloadTestFile()
	topic = "file-read-all"
	senderRoom.LocalParticipant.SendFile(testFileName, lksdk.StreamBytesOptions{
		Topic:    topic,
		FileName: &testFileName,
	})

	wg.Wait()
}
