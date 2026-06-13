package docker

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	imagetypes "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	dockerclient "github.com/docker/docker/client"
	"github.com/rs/zerolog/log"
)

const (
	NetworkName  = "cdbct-net"
	LabelPrefix  = "cdbct."
	LabelManaged = LabelPrefix + "managed"
	LabelRole    = LabelPrefix + "role"
	LabelCluster = LabelPrefix + "cluster"
	LabelNodeIdx = LabelPrefix + "node-index"

	RoleCRDB       = "crdb"
	RoleToxiproxy  = "toxiproxy"
	RolePrometheus = "prometheus"
	RoleGrafana    = "grafana"

	DefaultCluster = "default"
)

type Manager struct {
	client *dockerclient.Client
}

func NewManager() (*Manager, error) {
	c, err := dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Manager{client: c}, nil
}

func (m *Manager) Close() error {
	return m.client.Close()
}

func (m *Manager) EnsureNetwork(ctx context.Context) (string, error) {
	nets, err := m.client.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", NetworkName)),
	})
	if err != nil {
		return "", fmt.Errorf("list networks: %w", err)
	}
	for _, n := range nets {
		if n.Name == NetworkName {
			log.Debug().Str("id", n.ID[:12]).Msg("network already exists")
			return n.ID, nil
		}
	}

	resp, err := m.client.NetworkCreate(ctx, NetworkName, network.CreateOptions{
		Driver: "bridge",
		Labels: map[string]string{LabelManaged: "true"},
	})
	if err != nil {
		return "", fmt.Errorf("create network: %w", err)
	}
	log.Debug().Str("id", resp.ID[:12]).Msg("created network")
	return resp.ID, nil
}

func (m *Manager) RemoveNetwork(ctx context.Context) error {
	nets, err := m.client.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", NetworkName)),
	})
	if err != nil {
		return err
	}
	for _, n := range nets {
		if n.Name == NetworkName {
			return m.client.NetworkRemove(ctx, n.ID)
		}
	}
	return nil
}

func (m *Manager) EnsureVolume(ctx context.Context, name string) error {
	vols, err := m.client.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return err
	}
	for _, v := range vols.Volumes {
		if v.Name == name {
			return nil
		}
	}
	_, err = m.client.VolumeCreate(ctx, volume.CreateOptions{
		Name:   name,
		Labels: map[string]string{LabelManaged: "true"},
	})
	return err
}

func (m *Manager) RemoveVolume(ctx context.Context, name string) error {
	return m.client.VolumeRemove(ctx, name, false)
}

func (m *Manager) PullImage(ctx context.Context, image string) error {
	log.Info().Str("image", image).Msg("pulling image")
	rc, err := m.client.ImagePull(ctx, image, imagetypes.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	defer rc.Close()
	_, err = io.Copy(io.Discard, rc)
	return err
}

func (m *Manager) ListContainersByRole(ctx context.Context, cluster, role string) ([]types.Container, error) {
	f := filters.NewArgs(
		filters.Arg("label", LabelManaged+"=true"),
		filters.Arg("label", LabelCluster+"="+cluster),
		filters.Arg("label", LabelRole+"="+role),
	)
	return m.client.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
}

func (m *Manager) StopAndRemove(ctx context.Context, containerID string) error {
	timeout := 10
	_ = m.client.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout})
	return m.client.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

// Exec runs a command inside a running container and returns combined output.
// It uses stdcopy to properly demultiplex Docker's framed stdout/stderr stream.
func (m *Manager) Exec(ctx context.Context, containerID string, cmd []string) (string, int, error) {
	execID, err := m.client.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", -1, fmt.Errorf("exec create: %w", err)
	}

	resp, err := m.client.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", -1, fmt.Errorf("exec attach: %w", err)
	}
	defer resp.Close()

	var stdout, stderr strings.Builder
	_, _ = stdcopy.StdCopy(&stdout, &stderr, resp.Reader)

	inspect, err := m.client.ContainerExecInspect(ctx, execID.ID)
	if err != nil {
		return stdout.String(), -1, err
	}
	combined := stdout.String() + stderr.String()
	return combined, inspect.ExitCode, nil
}

func managedLabels(cluster, role, nodeIdx string) map[string]string {
	l := map[string]string{
		LabelManaged: "true",
		LabelCluster: cluster,
		LabelRole:    role,
	}
	if nodeIdx != "" {
		l[LabelNodeIdx] = nodeIdx
	}
	return l
}
