// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"fmt"
	"net"
	"os"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/redis"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/psrpc"
	lksdk "github.com/livekit/server-sdk-go"

	"github.com/livekit/sip/pkg/errors"
)

const (
	DefaultSIPPort int = 5060
)

var (
	DefaultRTPPortRange = rtcconfig.PortRange{Start: 10000, End: 20000}
)

type Config struct {
	Redis     *redis.RedisConfig `yaml:"redis"`      // required
	ApiKey    string             `yaml:"api_key"`    // required (env LIVEKIT_API_KEY)
	ApiSecret string             `yaml:"api_secret"` // required (env LIVEKIT_API_SECRET)
	WsUrl     string             `yaml:"ws_url"`     // required (env LIVEKIT_WS_URL)

	HealthPort     int                 `yaml:"health_port"`
	PrometheusPort int                 `yaml:"prometheus_port"`
	SIPPort        int                 `yaml:"sip_port"`
	RTPPort        rtcconfig.PortRange `yaml:"rtp_port"`
	Logging        logger.Config       `yaml:"logging"`
	ClusterID      string              `yaml:"cluster_id"` // cluster this instance belongs to

	UseExternalIP bool   `yaml:"use_external_ip"`
	NAT1To1IP     string `yaml:"nat_1_to_1_ip"`

	// internal
	ServiceName string `yaml:"-"`
	NodeID      string // Do not provide, will be overwritten
}

func NewConfig(confString string) (*Config, error) {
	conf := &Config{
		ApiKey:      os.Getenv("LIVEKIT_API_KEY"),
		ApiSecret:   os.Getenv("LIVEKIT_API_SECRET"),
		WsUrl:       os.Getenv("LIVEKIT_WS_URL"),
		ServiceName: "sip",
	}
	if confString != "" {
		if err := yaml.Unmarshal([]byte(confString), conf); err != nil {
			return nil, errors.ErrCouldNotParseConfig(err)
		}
	}

	if conf.Redis == nil {
		return nil, psrpc.NewErrorf(psrpc.InvalidArgument, "redis configuration is required")
	}

	return conf, nil
}

func (conf *Config) Init() error {
	conf.NodeID = utils.NewGuid("NE_")

	if conf.SIPPort == 0 {
		conf.SIPPort = DefaultSIPPort
	}
	if conf.RTPPort.Start == 0 {
		conf.RTPPort.Start = DefaultRTPPortRange.Start
	}
	if conf.RTPPort.End == 0 {
		conf.RTPPort.End = DefaultRTPPortRange.End
	}

	if err := conf.InitLogger(); err != nil {
		return err
	}

	if conf.UseExternalIP && conf.NAT1To1IP != "" {
		return fmt.Errorf("use_external_ip and nat_1_to_1_ip can not both be set")
	}

	return nil
}

func (c *Config) InitLogger(values ...interface{}) error {
	zl, err := logger.NewZapLogger(&c.Logging)
	if err != nil {
		return err
	}

	values = append(c.GetLoggerValues(), values...)
	l := zl.WithValues(values...)
	logger.SetLogger(l, c.ServiceName)
	lksdk.SetLogger(l)

	return nil
}

// To use with zap logger
func (c *Config) GetLoggerValues() []interface{} {
	return []interface{}{"nodeID", c.NodeID}
}

// To use with logrus
func (c *Config) GetLoggerFields() logrus.Fields {
	fields := logrus.Fields{
		"logger": c.ServiceName,
	}
	v := c.GetLoggerValues()
	for i := 0; i < len(v); i += 2 {
		fields[v[i].(string)] = v[i+1]
	}

	return fields
}

func GetLocalIP() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", nil
	}
	for _, address := range addrs {
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String(), nil
			}
		}
	}

	return "", fmt.Errorf("No local IP found")
}
