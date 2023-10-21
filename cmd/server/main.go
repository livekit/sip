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
	"io/ioutil"
	"os"

	"github.com/urfave/cli/v2"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/errors"
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

func runHandler(c *cli.Context) error {

	return nil
}

func runService(c *cli.Context) error {

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
