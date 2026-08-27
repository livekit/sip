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

//go:build mage
// +build mage

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/magefile/mage/sh"

	"github.com/livekit/mageutil"
)

var Default = Build

const livekitServerVersion = "master"

func Build() error {
	fmt.Println("building...")
	cmd := exec.Command("go", "build", ".")
	connectStd(cmd)
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

func connectStd(cmd *exec.Cmd) {
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
}

const (
	testApiKey = "devkey: secret"
)

func getLocalIPAddresses() ([]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	loopBacks := make([]string, 0)
	addresses := make([]string, 0)
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch typedAddr := addr.(type) {
			case *net.IPNet:
				ip = typedAddr.IP.To4()
			case *net.IPAddr:
				ip = typedAddr.IP.To4()
			default:
				continue
			}
			if ip == nil {
				continue
			}
			if ip.IsLoopback() {
				loopBacks = append(loopBacks, ip.String())
			} else {
				addresses = append(addresses, ip.String())
			}
		}
	}

	if len(addresses) > 0 {
		return addresses, nil
	}
	if len(loopBacks) > 0 {
		return loopBacks, nil
	}
	return nil, fmt.Errorf("could not find local IP address")
}

// runs golangci-lint
func Lint() error {
	if _, err := exec.LookPath("golangci-lint"); err != nil {
		return errors.New("golangci-lint is not installed, install instructions: https://golangci-lint.run/docs/welcome/install/")
	}
	return mageutil.Run(context.Background(), "golangci-lint run ./...")
}

func Test() error {
	fmt.Println("testing local packages...")
	if err := mageutil.Run(context.Background(), "go test -short -v ./pkg/... -count=1"); err != nil {
		return err
	}

	fmt.Println("starting livekit-server...")

	confString := `
port: 7880
bind_addresses:
    - ""
rtc:
    udp_port: 7882
    tcp_port: 7881
turn:
    enabled: true
    domain: ""
    cert_file: ""
    key_file: ""
    tls_port: 0
    udp_port: 3478
    relay_range_start: 30000
    relay_range_end: 30020
    external_tls: false
log_level: debug
logging:
    pion_level: info
`
	parameters := []string{
		`run`, `-e`, `LIVEKIT_KEYS=` + testApiKey, `-e`, `LIVEKIT_CONFIG=` + string(confString),
		`-d`, `--rm`, `-p7880:7880`, `-p7881:7881`, `-p7882:7882/udp`, `-p3478:3478/udp`,
	}

	for p := 30000; p <= 30020; p++ {
		parameters = append(parameters, fmt.Sprintf("-p%d:%d/udp", p, p))
	}
	parameters = append(parameters, []string{`--name`, `livekit-server`, `livekit/livekit-server:` + livekitServerVersion}...)
	if addresses, _ := getLocalIPAddresses(); len(addresses) > 0 {
		fmt.Println("set node ip", addresses[0])
		parameters = append(parameters, `--node-ip`, addresses[0])
	}

	if err := sh.RunV(`docker`, parameters...); err != nil {
		return err
	}
	defer func() {
		fmt.Println("stop livekit-server...")
		run(nil, "docker stop livekit-server")
	}()

	fmt.Println("testing...")
	testflags := os.Getenv("TestFlags")
	return run(map[string]string{"LIVEKIT_KEYS": testApiKey}, `go test -v -race `+testflags)
}

func run(env map[string]string, commands ...string) error {
	for _, command := range commands {
		args := strings.Split(command, " ")
		_, err := sh.Exec(env, os.Stdout, os.Stderr, args[0], args[1:]...)
		if err != nil {
			return err
		}
	}
	return nil
}
