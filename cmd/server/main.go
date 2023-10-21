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

package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/redis"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/tracer"
	"github.com/livekit/psrpc"
	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/errors"
	"github.com/livekit/sip/pkg/service"
	"github.com/livekit/sip/version"
)

func main() {
	app := &cli.App{
		Name:        "SIP",
		Usage:       "LiveKit SIP",
		Version:     version.Version,
		Description: "SIP connectivity for LiveKit",
		Commands: []*cli.Command{
			{
				Name:        "run-handler",
				Description: "runs a request in a new process",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name: "info",
					},
					&cli.StringFlag{
						Name: "config-body",
					},
					&cli.StringFlag{
						Name: "token",
					},
					&cli.StringFlag{
						Name: "ws-url",
					},
					&cli.StringFlag{
						Name: "extra-params",
					},
				},
				Action: runHandler,
				Hidden: true,
			},
		},
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Usage:   "LiveKit SIP yaml config file",
				EnvVars: []string{"SIP_CONFIG_FILE"},
			},
			&cli.StringFlag{
				Name:    "config-body",
				Usage:   "LiveKit SIP yaml config body",
				EnvVars: []string{"SIP_CONFIG_BODY"},
			},
		},
		Action: runService,
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Println(err)
	}
}

func runService(c *cli.Context) error {
	conf, err := getConfig(c, true)
	if err != nil {
		return err
	}

	rc, err := redis.GetRedisClient(conf.Redis)
	if err != nil {
		return err
	}

	bus := psrpc.NewRedisMessageBus(rc)
	psrpcClient, err := rpc.NewIOInfoClient(conf.NodeID, bus)
	if err != nil {
		return err
	}

	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, syscall.SIGTERM, syscall.SIGQUIT)

	killChan := make(chan os.Signal, 1)
	signal.Notify(killChan, syscall.SIGINT)

	svc := service.NewService(conf, psrpcClient, bus)

	go func() {
		select {
		case sig := <-stopChan:
			logger.Infow("exit requested, finishing all SIP then shutting down", "signal", sig)
			svc.Stop(false)

		case sig := <-killChan:
			logger.Infow("exit requested, stopping all SIP and shutting down", "signal", sig)
			svc.Stop(true)

		}
	}()

	return svc.Run()
}

func runHandler(c *cli.Context) error {
	conf, err := getConfig(c, false)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctx, span := tracer.Start(ctx, "Handler.New")
	defer span.End()
	logger.Debugw("handler launched")

	token := c.String("token")

	var handler interface {
		Kill()
		HandleSIP(ctx context.Context, info any, wsUrl, token string, extraParams any)
	}

	handler = service.NewHandler(conf)

	killChan := make(chan os.Signal, 1)
	signal.Notify(killChan, syscall.SIGINT)

	go func() {
		sig := <-killChan
		logger.Infow("exit requested, stopping all SIP and shutting down", "signal", sig)
		handler.Kill()

		time.Sleep(10 * time.Second)
		// If handler didn't exit cleanly after 10s, cancel the context
		cancel()
	}()

	wsUrl := conf.WsUrl
	if c.String("ws-url") != "" {
		wsUrl = c.String("ws-url")
	}

	var ep any
	info := false

	handler.HandleSIP(ctx, info, wsUrl, token, ep)
	return nil
}

func getConfig(c *cli.Context, initialize bool) (*config.Config, error) {
	configFile := c.String("config")
	configBody := c.String("config-body")
	if configBody == "" {
		if configFile == "" {
			return nil, errors.ErrNoConfig
		}
		content, err := ioutil.ReadFile(configFile)
		if err != nil {
			return nil, err
		}
		configBody = string(content)
	}

	conf, err := config.NewConfig(configBody)
	if err != nil {
		return nil, err
	}

	if initialize {
		err = conf.Init()
		if err != nil {
			return nil, err
		}
	}

	return conf, nil
}
