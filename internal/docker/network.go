package docker

import "github.com/docker/docker/api/types/network"

// networkConfig returns a NetworkingConfig that puts the container on cdbct-net
// with the given DNS alias.
func networkConfig(alias string) *network.NetworkingConfig {
	return &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			NetworkName: {Aliases: []string{alias}},
		},
	}
}
