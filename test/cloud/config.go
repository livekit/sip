package cloud

import (
	"os"

	"github.com/livekit/sip/pkg/config"
)

const (
	Host      = "integration.sip.livekit.cloud"
	ProjectID = "sip_integration"
	Uri       = "integration.pstn.twilio.com"
)

type IntegrationConfig struct {
	*config.Config

	RoomName string
}

func NewIntegrationConfig() (*IntegrationConfig, error) {
	c := &IntegrationConfig{
		Config: &config.Config{
			ApiKey:             os.Getenv("LIVEKIT_API_KEY"),
			ApiSecret:          os.Getenv("LIVEKIT_API_SECRET"),
			WsUrl:              os.Getenv("LIVEKIT_WS_URL"),
			ServiceName:        "sip",
			EnableJitterBuffer: true,
		},
		RoomName: "test",
	}
	if err := c.Config.Init(); err != nil {
		return nil, err
	}
	return c, nil
}
