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
	"fmt"
	"go/build"
	"os"
	"os/exec"
	"strings"

	"github.com/livekit/mageutil"
)

var Default = Build

const (
	imageName = "livekit/sip"
)

var packages = []string{"pkg-config", "opus", "opusfile"}

type packageInfo struct {
	Dir string
}

func Bootstrap() error {
	brewPrefix, err := getBrewPrefix()
	if err != nil {
		return err
	}

	for _, plugin := range packages {
		if _, err := os.Stat(fmt.Sprintf("%s%s", brewPrefix, plugin)); err != nil {
			if err = run(fmt.Sprintf("brew install %s", plugin)); err != nil {
				return err
			}
		}
	}

	return nil
}

func Build() error {
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		gopath = build.Default.GOPATH
	}

	return run(fmt.Sprintf("go build -a -o %s/bin/sip ./cmd/livekit-sip", gopath))
}

func Test() error {
	return run("go test -v ./pkg/...")
}

func Integration() error {
	return run("go test -v ./test/integration/...")
}

func BuildDocker() error {
	return mageutil.Run(context.Background(),
		fmt.Sprintf("docker build -t %s:latest -f build/sip/Dockerfile .", imageName),
	)
}

func BuildDockerLinux() error {
	return mageutil.Run(context.Background(),
		fmt.Sprintf("docker build --platform linux/amd64 -t %s:latest -f build/sip/Dockerfile .", imageName),
	)
}

func SipClient() error {
	return run("go build -C ./test/sip-client/ ./...")
}

// helpers

func getBrewPrefix() (string, error) {
	out, err := exec.Command("brew", "--prefix").Output()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/Cellar/", strings.TrimSpace(string(out))), nil
}

func run(commands ...string) error {
	for _, command := range commands {
		args := strings.Split(command, " ")
		if err := runArgs(args...); err != nil {
			return err
		}
	}
	return nil
}

func runArgs(args ...string) error {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
