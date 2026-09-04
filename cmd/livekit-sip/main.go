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
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/redis"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/tracer/jaeger"
	"github.com/livekit/psrpc"
	"github.com/livekit/psrpc/pkg/middleware/otelpsrpc"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/errors"
	"github.com/livekit/sip/pkg/service"
	"github.com/livekit/sip/pkg/sip"
	"github.com/livekit/sip/pkg/stats"
	"github.com/livekit/sip/version"
)

func main() {
	cmd := &cli.Command{
		Name:        "SIP",
		Usage:       "LiveKit SIP",
		Version:     version.Version,
		Description: "SIP connectivity for LiveKit",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Usage:   "LiveKit SIP yaml config file",
				Sources: cli.EnvVars("SIP_CONFIG_FILE"),
			},
			&cli.StringFlag{
				Name:    "config-body",
				Usage:   "LiveKit SIP yaml config body",
				Sources: cli.EnvVars("SIP_CONFIG_BODY"),
			},
		},
		Action: runService,
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		fmt.Println(err)
	}
}

func runService(ctx context.Context, c *cli.Command) error {
	conf, err := getConfig(c, true)
	if err != nil {
		return err
	}

	// Resolve LIVEKIT_NODE_IP_IFACE (if set) into conf.ListenIP before the
	// service starts. This mirrors the LiveKit media server behaviour: the env
	// var names the network interface whose IPv4 address should be used for SIP
	// signalling binding. It is useful in ECS/Fargate/Kubernetes where the
	// container IP lives on a known interface (e.g. eth0) and must be bound
	// explicitly rather than falling back to the 0.0.0.0 wildcard.
	if err = applyNodeIPIface(conf); err != nil {
		return err
	}

	log := logger.GetLogger()
	if conf.JaegerURL != "" {
		jaeger.Configure(ctx, conf.JaegerURL, conf.ServiceName)
	}

	rc, err := redis.GetRedisClient(conf.Redis)
	if err != nil {
		return err
	}

	bus := psrpc.NewRedisMessageBus(rc)
	psrpcClient, err := rpc.NewIOInfoClient(bus,
		otelpsrpc.ClientOptions(otelpsrpc.Config{}),
	)
	if err != nil {
		return err
	}

	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, syscall.SIGTERM, syscall.SIGQUIT)

	killChan := make(chan os.Signal, 1)
	signal.Notify(killChan, syscall.SIGINT)

	mon, err := stats.NewMonitor(conf)
	if err != nil {
		return err
	}

	sipsrv, err := sip.NewService("", conf, mon, log, func(projectID string, _ *rpc.SIPCallObservability, _ *livekit.SIPCallInfo) sip.StateHandler { return sip.NewRPCStateHandler(psrpcClient) })
	if err != nil {
		return err
	}
	svc := service.NewService(conf, log, sipsrv, sipsrv.Stop, sipsrv.ActiveCalls, psrpcClient, bus, mon)
	sipsrv.SetHandler(svc)

	if err = sipsrv.Start(); err != nil {
		return err
	}

	go func() {
		select {
		case sig := <-stopChan:
			log.Infow("exit requested, finishing all SIP then shutting down", "signal", sig)
			svc.Stop(false)
		case sig := <-killChan:
			log.Infow("exit requested, stopping all SIP and shutting down", "signal", sig)
			svc.Stop(true)
		}
	}()

	return svc.Run()
}

// applyNodeIPIface reads LIVEKIT_NODE_IP_IFACE and, if set, resolves the first
// IPv4 address of that interface into conf.ListenIP.
//
// The override is skipped when:
//   - the env var is absent or empty, or
//   - conf.ListenIP is already set to a specific (non-wildcard) address,
//     meaning the operator made an explicit choice in the config file.
func applyNodeIPIface(conf *config.Config) error {
	iface := os.Getenv("LIVEKIT_NODE_IP_IFACE")
	if iface == "" {
		return nil
	}

	log := logger.GetLogger()

	// Respect an explicit listen_ip that isn't the wildcard.
	if conf.ListenIP != "" && conf.ListenIP != "0.0.0.0" {
		log.Infow("LIVEKIT_NODE_IP_IFACE set but listen_ip already configured, skipping interface resolution",
			"iface", iface,
			"listen_ip", conf.ListenIP,
		)
		return nil
	}

	ip, err := ipv4FromIface(iface)
	if err != nil {
		return fmt.Errorf("LIVEKIT_NODE_IP_IFACE=%s: %w", iface, err)
	}

	log.Infow("resolved listen_ip from network interface", "iface", iface, "ip", ip)
	conf.ListenIP = ip
	return nil
}

// ipv4FromIface returns the first IPv4 address assigned to the named interface.
func ipv4FromIface(name string) (string, error) {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return "", fmt.Errorf("interface %q not found: %w", name, err)
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return "", fmt.Errorf("cannot list addresses for %q: %w", name, err)
	}
	if ip, ok := firstIPv4(addrs); ok {
		return ip, nil
	}
	return "", fmt.Errorf("no IPv4 address found on interface %q", name)
}

// firstIPv4 returns the first IPv4 address in addrs, if any.
func firstIPv4(addrs []net.Addr) (string, bool) {
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil {
			return ip4.String(), true
		}
	}
	return "", false
}

func getConfig(c *cli.Command, initialize bool) (*config.Config, error) {
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
