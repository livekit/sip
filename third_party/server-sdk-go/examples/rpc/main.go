package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

const (
	caller     = "caller"
	greeter    = "greeter"
	mathGenius = "math-genius"
)

var (
	host, apiKey, apiSecret string
)

func init() {
	flag.StringVar(&host, "host", "", "livekit server host")
	flag.StringVar(&apiKey, "api-key", "", "livekit api key")
	flag.StringVar(&apiSecret, "api-secret", "", "livekit api secret")
}

func performGreeting(room *lksdk.Room) {
	logger.Infow("[Caller] Letting the greeter know that I've arrived")
	res, err := room.LocalParticipant.PerformRpc(lksdk.PerformRpcParams{
		DestinationIdentity: "greeter",
		Method:              "arrival",
		Payload:             "Hello",
	})

	if err != nil {
		logger.Errorw("[Caller] RPC call failed: ", err)
		return
	}

	logger.Infow(fmt.Sprintf("[Caller] That's nice, the greeter said: %s", *res))
}

func performGreetingWithTimeout(room *lksdk.Room) {
	logger.Infow("[Caller] Let's try a greeting with a timeout")

	timeout := 10 * time.Millisecond

	res, err := room.LocalParticipant.PerformRpc(lksdk.PerformRpcParams{
		DestinationIdentity: "greeter",
		Method:              "arrival",
		Payload:             "Hello",
		ResponseTimeout:     &timeout,
	})

	if err != nil {
		logger.Errorw(fmt.Sprintf("[Caller] RPC call failed: %s", err), nil)
		return
	}

	logger.Infow(fmt.Sprintf("[Caller] That's nice, the greeter said: %s", *res))

}

func performDisconnection(room *lksdk.Room) {
	logger.Infow("[Caller] Checking back in on the greeter...")
	res, err := room.LocalParticipant.PerformRpc(lksdk.PerformRpcParams{
		DestinationIdentity: "greeter",
		Method:              "arrival",
		Payload:             "You still there?",
	})

	if err != nil {
		if err.(*lksdk.RpcError).Code == lksdk.RpcRecipientDisconnected {
			logger.Infow("[Caller] The greeter disconnected during the request.")
		} else {
			logger.Errorw("[Caller] Unexpected error: ", err)
		}
		return
	}

	logger.Infow(fmt.Sprintf("[Caller] That's nice, the greeter said: %s", *res))
}

func performSquareRoot(room *lksdk.Room) {
	logger.Infow("[Caller] What's the square root of 16?")

	number := 16
	payload, err := json.Marshal(number)
	if err != nil {
		logger.Errorw("Failed to marshal square root request", err)
		return
	}

	res, err := room.LocalParticipant.PerformRpc(lksdk.PerformRpcParams{
		DestinationIdentity: "math-genius",
		Method:              "square-root",
		Payload:             string(payload),
	})

	if err != nil {
		logger.Errorw("[Caller] RPC call failed:", err)
		return
	}

	var result float64
	err = json.Unmarshal([]byte(*res), &result)
	if err != nil {
		logger.Errorw("Failed to unmarshal square root result", err)
		return
	}
	logger.Infow(fmt.Sprintf("[Caller] Nice, the answer was: %v", result))
}

func performQuantumHypergeometricSeries(room *lksdk.Room) {
	logger.Infow("[Caller] What's the quantum hypergeometric series of 42?")

	number := 42
	payload, err := json.Marshal(number)
	if err != nil {
		logger.Errorw("Failed to marshal quantum hypergeometric series request", err)
		return
	}

	res, err := room.LocalParticipant.PerformRpc(lksdk.PerformRpcParams{
		DestinationIdentity: "math-genius",
		Method:              "quantum-hypergeometric-series",
		Payload:             string(payload),
	})

	if err != nil {
		if err.(*lksdk.RpcError).Code == lksdk.RpcUnsupportedMethod {
			logger.Infow("[Caller] Aww looks like the genius doesn't know that one.")
		} else {
			logger.Errorw("[Caller] Unexpected error:", err)
		}
		return
	}

	var result float64
	err = json.Unmarshal([]byte(*res), &result)
	if err != nil {
		logger.Errorw("Failed to unmarshal quantum hypergeometric series result", err)
		return
	}

	logger.Infow(fmt.Sprintf("[Caller] genius says %v!", result))
}

func performDivide(room *lksdk.Room) {
	logger.Infow("[Caller] Let's try dividing 10 by 0")

	numerator := 10
	denominator := 0

	payload, err := json.Marshal(struct {
		Numerator   float64 `json:"numerator"`
		Denominator float64 `json:"denominator"`
	}{
		Numerator:   float64(numerator),
		Denominator: float64(denominator),
	})
	if err != nil {
		logger.Errorw("Failed to marshal divide request", err)
		return
	}

	res, err := room.LocalParticipant.PerformRpc(lksdk.PerformRpcParams{
		DestinationIdentity: "math-genius",
		Method:              "divide",
		Payload:             string(payload),
	})

	if err != nil {
		if rpcErr, ok := err.(*lksdk.RpcError); ok {
			if rpcErr.Code == lksdk.RpcApplicationError {
				logger.Infow("[Caller] Oops! I guess that didn't work. Let's try something else.")
			} else {
				logger.Errorw(fmt.Sprintf("[Caller] Unexpected RPC error: %s", rpcErr.Error()), nil)
			}
		} else {
			logger.Errorw("[Caller] Unexpected error:", err)
		}
		return
	}

	var result float64
	err = json.Unmarshal([]byte(*res), &result)
	if err != nil {
		logger.Errorw("Failed to unmarshal divide result", err)
		return
	}

	logger.Infow(fmt.Sprintf("[Caller] The result is %v", result))
}

func registerReceiverMethods(rooms map[string]*lksdk.Room) error {
	greeterRoom := rooms[greeter]
	mathGeniusRoom := rooms[mathGenius]

	arrivalHandler := func(data lksdk.RpcInvocationData) (string, error) {
		resultChan := make(chan string)

		logger.Infow(fmt.Sprintf("[Greeter] Oh %s arrived and said %s", data.CallerIdentity, data.Payload))

		time.AfterFunc(2000*time.Millisecond, func() {
			resultChan <- "Welcome and have a wonderful day!"
		})

		return <-resultChan, nil
	}
	err := greeterRoom.RegisterRpcMethod("arrival", arrivalHandler)
	if err != nil {
		logger.Errorw("Failed to register arrival RPC method", err)
		return err
	}

	squareRootHandler := func(data lksdk.RpcInvocationData) (string, error) {
		resultChan := make(chan string)
		var number float64
		err := json.Unmarshal([]byte(data.Payload), &number)
		if err != nil {
			logger.Errorw("Failed to unmarshal square root request", err)
			return "", err
		}

		logger.Infow(fmt.Sprintf("[Math Genius] I guess %s wants the square root of %s. I've only got %v seconds to respond but I think I can pull it off ", data.CallerIdentity, data.Payload, data.ResponseTimeout.Seconds()))
		logger.Infow("[Math Genius] *doing math*...")

		time.AfterFunc(2000*time.Millisecond, func() {
			result := math.Sqrt(number)
			logger.Infow(fmt.Sprintf("[MathGenius] Aha! It's %v", result))

			jsonBytes, err := json.Marshal(result)
			if err != nil {
				logger.Errorw("Failed to marshal square root result", err)
				return
			}

			resultChan <- string(jsonBytes)
		})

		return <-resultChan, nil
	}
	err = mathGeniusRoom.RegisterRpcMethod("square-root", squareRootHandler)
	if err != nil {
		logger.Errorw("Failed to register square root RPC method", err)
		return err
	}

	divideHandler := func(data lksdk.RpcInvocationData) (string, error) {
		resultChan := make(chan string)

		type DivideRequest struct {
			Numerator   float64 `json:"numerator"`
			Denominator float64 `json:"denominator"`
		}
		var payload DivideRequest
		err := json.Unmarshal([]byte(data.Payload), &payload)
		if err != nil {
			logger.Errorw("Failed to unmarshal divide request", err)
			return "", err
		}

		logger.Infow(fmt.Sprintf("[Math Genius] %s wants to divide %v by %v. Let me think...", data.CallerIdentity, payload.Numerator, payload.Denominator))

		if payload.Denominator == 0 {
			return "", fmt.Errorf("cannot divide by zero")
		}

		time.AfterFunc(2000*time.Millisecond, func() {
			result := payload.Numerator / payload.Denominator
			logger.Infow(fmt.Sprintf("[MathGenius] %v / %v = %v", payload.Numerator, payload.Denominator, result))

			jsonBytes, err := json.Marshal(result)
			if err != nil {
				logger.Errorw("Failed to marshal divide result", err)
				return
			}

			resultChan <- string(jsonBytes)
		})

		return <-resultChan, nil
	}
	err = mathGeniusRoom.RegisterRpcMethod("divide", divideHandler)
	if err != nil {
		logger.Errorw("Failed to register divide RPC method", err)
		return err
	}
	return nil
}

func main() {
	logger.InitFromConfig(&logger.Config{Level: "info"}, "rpc-demo")
	lksdk.SetLogger(logger.GetLogger())
	flag.Parse()

	roomName := "rpc-demo" + uuid.New().String()[:8]
	logger.Infow("connecting participants to room", "roomName", roomName)

	participants := []string{caller, greeter, mathGenius}

	rooms := map[string]*lksdk.Room{}
	for _, p := range participants {
		room, err := lksdk.ConnectToRoom(host, lksdk.ConnectInfo{
			APIKey:              apiKey,
			APISecret:           apiSecret,
			RoomName:            roomName,
			ParticipantIdentity: p,
		}, nil)
		if err != nil {
			logger.Errorw("failed to connect to room", err, "participant", p)
			return
		}
		rooms[p] = room
	}
	defer func() {
		for _, room := range rooms {
			room.Disconnect()
		}
		logger.Infow("Participants disconnected. Example completed.")
	}()

	err := registerReceiverMethods(rooms)
	if err != nil {
		logger.Errorw("failed to register RPC methods", err)
		return
	}

	logger.Infow("participants connected to room, starting rpc demo", "roomName", roomName)

	logger.Infow("Running greeting example...")
	performGreeting(rooms[caller])
	performGreetingWithTimeout(rooms[caller])

	logger.Infow("Running error handling example...")
	performDivide(rooms[caller])

	logger.Infow("Running math example...")
	performSquareRoot(rooms[caller])
	time.Sleep(2000 * time.Millisecond)
	performQuantumHypergeometricSeries(rooms[caller])

	logger.Infow("Running disconnection example...")
	rooms[greeter].Disconnect()
	time.Sleep(1000 * time.Millisecond)
	performDisconnection(rooms[caller])

	logger.Infow("participants done, disconnecting")
}
