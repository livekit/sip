package integration

import (
	"fmt"
	"log"
	"strings"

	"github.com/ory/dockertest/v3/docker"
)

func requireNoLeftoverSIPTestDocker() {
	containers, networks, err := listSIPTestLeftovers()
	if err != nil {
		log.Fatalf("Could not list siptest docker resources: %s", err)
	}
	if len(containers) == 0 && len(networks) == 0 {
		return
	}

	var b strings.Builder
	b.WriteString("siptest docker resources already exist (possible parallel runs):\n")
	if len(containers) > 0 {
		fmt.Fprintf(&b, "  containers: %s\n", strings.Join(containers, ", "))
	}
	if len(networks) > 0 {
		fmt.Fprintf(&b, "  networks:   %s\n", strings.Join(networks, ", "))
	}
	b.WriteString("\nPurge with:\n")
	if len(containers) > 0 {
		fmt.Fprintf(&b, "  docker rm -f %s\n", strings.Join(containers, " "))
	}
	if len(networks) > 0 {
		fmt.Fprintf(&b, "  docker network rm %s\n", strings.Join(networks, " "))
	}
	log.Fatal(b.String())
}

func listSIPTestLeftovers() (containers, networks []string, err error) {
	listed, err := Docker.Client.ListContainers(docker.ListContainersOptions{All: true})
	if err != nil {
		return nil, nil, err
	}
	for _, c := range listed {
		if name, ok := sipTestContainerName(c); ok {
			containers = append(containers, name)
		}
	}

	listedNets, err := Docker.Client.ListNetworks()
	if err != nil {
		return nil, nil, err
	}
	for _, n := range listedNets {
		if isSIPTestName(n.Name) {
			networks = append(networks, n.Name)
		}
	}
	return containers, networks, nil
}

func sipTestContainerName(c docker.APIContainers) (string, bool) {
	for _, name := range c.Names {
		name = strings.TrimPrefix(name, "/")
		if isSIPTestName(name) {
			return name, true
		}
	}
	return "", false
}

func isSIPTestName(name string) bool {
	return strings.HasPrefix(strings.TrimPrefix(name, "/"), dockerPrefix)
}
