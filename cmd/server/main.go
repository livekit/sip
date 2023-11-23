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
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v2"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/redis"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/errors"
	"github.com/livekit/sip/pkg/service"
	"github.com/livekit/sip/pkg/sip"
	"github.com/livekit/sip/version"
)

func main() {
	app := &cli.App{
		Name:        "SIP",
		Usage:       "LiveKit SIP",
		Version:     version.Version,
		Description: "SIP connectivity for LiveKit",
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
	psrpcClient, err := rpc.NewIOInfoClient(bus)
	if err != nil {
		return err
	}

	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, syscall.SIGTERM, syscall.SIGQUIT)

	killChan := make(chan os.Signal, 1)
	signal.Notify(killChan, syscall.SIGINT)

	sipCli := sip.NewClient(conf)
	if err = sipCli.Start(); err != nil {
		return err
	}

	svc := service.NewService(conf, sipCli, psrpcClient, bus)

	sipSrv := sip.NewServer(conf, svc.HandleTrunkAuthentication, svc.HandleDispatchRules)
	if err = sipSrv.Start(); err != nil {
		return err
	}

	go func() {
		select {
		case sig := <-stopChan:
			logger.Infow("exit requested, finishing all SIP then shutting down", "signal", sig)
			svc.Stop(false)

		case sig := <-killChan:
			logger.Infow("exit requested, stopping all SIP and shutting down", "signal", sig)
			svc.Stop(true)
			if err = sipCli.Stop(); err != nil {
				log.Println(err)
			}
			if err = sipSrv.Stop(); err != nil {
				log.Println(err)
			}

		}
	}()

	return svc.Run()
}

func getConfig(c *cli.Context, initialize bool) (*config.Config, error) {
	configFile := c.String("config")
	configBody := c.String("config-body")
	if configBody == "" {
		if configFile == "" {
			return nil, errors.ErrNoConfig
		}
		content, err := os.ReadFile(configFile)
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
