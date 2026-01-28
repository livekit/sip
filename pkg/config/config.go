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
	"net/netip"
	"os"
	"time"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/logger/medialogutils"
	"github.com/livekit/protocol/redis"
	"github.com/livekit/protocol/utils/guid"
	"github.com/livekit/psrpc"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/livekit/sip/pkg/errors"
)

const (
	DefaultSIPPort    int = 5060
	DefaultSIPPortTLS int = 5061
)

var (
	DefaultRTPPortRange = rtcconfig.PortRange{Start: 10000, End: 20000}
)

type TLSCert struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type TLSConfig struct {
	Port       int       `yaml:"port"`        // announced SIP signaling port
	ListenPort int       `yaml:"port_listen"` // SIP signaling port to listen on
	Certs      []TLSCert `yaml:"certs"`
	KeyLog     string    `yaml:"key_log"`

	MinVersion string `yaml:"min_version"` // min TLS version, accepts: "tls1.0", "tls1.1", "tls1.2", "tls1.3"
	MaxVersion string `yaml:"max_version"` // max TLS version, accepts: "tls1.0", "tls1.1", "tls1.2", "tls1.3"

	// CipherSuites is an optional list of cipher suite names.
	// If not provided, Go's secure defaults are used.
	// Note: Only applies to TLS 1.0-1.2; TLS 1.3 cipher suites are not configurable.
	CipherSuites []string `yaml:"cipher_suites"`
}

type TCPConfig struct {
	DialPort rtcconfig.PortRange `yaml:"dial_port"`
}

type Config struct {
	Redis     *redis.RedisConfig `yaml:"redis"`      // required
	ApiKey    string             `yaml:"api_key"`    // required (env LIVEKIT_API_KEY)
	ApiSecret string             `yaml:"api_secret"` // required (env LIVEKIT_API_SECRET)
	WsUrl     string             `yaml:"ws_url"`     // required (env LIVEKIT_WS_URL)

	HealthPort         int                 `yaml:"health_port"`
	PrometheusPort     int                 `yaml:"prometheus_port"`
	PProfPort          int                 `yaml:"pprof_port"`
	SIPPort            int                 `yaml:"sip_port"`        // announced SIP signaling port
	SIPPortListen      int                 `yaml:"sip_port_listen"` // SIP signaling port to listen on
	SIPHostname        string              `yaml:"sip_hostname"`
	SIPRingingInterval time.Duration       `yaml:"sip_ringing_interval"` // from 1 sec up to 60 (default '1s')
	TCP                *TCPConfig          `yaml:"tcp"`
	TLS                *TLSConfig          `yaml:"tls"`
	RTPPort            rtcconfig.PortRange `yaml:"rtp_port"`
	Logging            logger.Config       `yaml:"logging"`
	ClusterID          string              `yaml:"cluster_id"` // cluster this instance belongs to
	MaxCpuUtilization  float64             `yaml:"max_cpu_utilization"`

	UseExternalIP bool   `yaml:"use_external_ip"`
	LocalNet      string `yaml:"local_net"` // local IP net to use, e.g. 192.168.0.0/24
	NAT1To1IP     string `yaml:"nat_1_to_1_ip"`
	ListenIP      string `yaml:"listen_ip"`

	// if different from signaling IP
	MediaUseExternalIP bool   `yaml:"media_use_external_ip"`
	MediaNAT1To1IP     string `yaml:"media_nat_1_to_1_ip"`

	MediaTimeout        time.Duration   `yaml:"media_timeout"`
	MediaTimeoutInitial time.Duration   `yaml:"media_timeout_initial"`
	Codecs              map[string]bool `yaml:"codecs"`

	// HideInboundPort controls how SIP endpoint responds to unverified inbound requests.
	// Setting it to true makes SIP server silently drop INVITE requests if it gets a negative Auth or Dispatch response.
	// Doing so hides our SIP endpoint from (a low effort) port scanners.
	HideInboundPort bool `yaml:"hide_inbound_port"`
	// AddRecordRoute forces SIP to add Record-Route headers to the responses.
	AddRecordRoute bool `yaml:"add_record_route"`

	// AudioDTMF forces SIP to generate audio DTMF tones in addition to digital.
	AudioDTMF              bool    `yaml:"audio_dtmf"`
	EnableJitterBuffer     bool    `yaml:"enable_jitter_buffer"`
	EnableJitterBufferProb float64 `yaml:"enable_jitter_buffer_prob"`

	// internal
	ServiceName string `yaml:"-"`
	NodeID      string // Do not provide, will be overwritten
	JaegerURL   string `yaml:"jaeger_url"` // for tracing

	// Experimental, these option might go away without notice.
	Experimental struct {
		// InboundWaitACK forces SIP to wait for an ACK to 200 OK before proceeding with the call.
		InboundWaitACK bool `yaml:"inbound_wait_ack"`
	} `yaml:"experimental"`
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

func (c *Config) Init() error {
	c.NodeID = guid.New("NE_")

	if c.SIPPort == 0 {
		c.SIPPort = DefaultSIPPort
	}
	if c.SIPPortListen == 0 {
		c.SIPPortListen = c.SIPPort
	}
	if tc := c.TLS; tc != nil {
		if tc.Port == 0 {
			tc.Port = DefaultSIPPortTLS
		}
		if tc.ListenPort == 0 {
			tc.ListenPort = tc.Port
		}
	}
	if c.RTPPort.Start == 0 {
		c.RTPPort.Start = DefaultRTPPortRange.Start
	}
	if c.RTPPort.End == 0 {
		c.RTPPort.End = DefaultRTPPortRange.End
	}
	if c.MaxCpuUtilization <= 0 || c.MaxCpuUtilization > 1 {
		c.MaxCpuUtilization = 0.9
	}

	if err := c.InitLogger(); err != nil {
		return err
	}

	if c.UseExternalIP && c.NAT1To1IP != "" {
		return fmt.Errorf("use_external_ip and nat_1_to_1_ip can not both be set")
	}

	if c.MediaUseExternalIP && c.MediaNAT1To1IP != "" {
		return fmt.Errorf("media_use_external_ip and media_nat_1_to_1_ip can not both be set")
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
	lksdk.SetLogger(medialogutils.NewOverrideLogger(nil))

	return nil
}

// To use with zap logger
func (c *Config) GetLoggerValues() []interface{} {
	if c.NodeID == "" {
		return nil
	}
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

func GetLocalIP() (netip.Addr, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return netip.Addr{}, nil
	}
	type Iface struct {
		Name string
		Addr netip.Addr
	}
	var candidates []Iface
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagRunning == 0 {
			continue
		}
		if ifc.Flags&(net.FlagPointToPoint|net.FlagLoopback) != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if ip4 := ipnet.IP.To4(); ip4 != nil {
				ip, _ := netip.AddrFromSlice(ip4)
				candidates = append(candidates, Iface{
					Name: ifc.Name, Addr: ip,
				})
				logger.Debugw("considering interface", "iface", ifc.Name, "ip", ip)
			}
		}
	}
	if len(candidates) == 0 {
		return netip.Addr{}, fmt.Errorf("No local IP found")
	}
	return candidates[0].Addr, nil
}
