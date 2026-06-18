package docker

import (
	"archive/tar"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"github.com/rs/zerolog/log"
)

//go:embed workload.Dockerfile
var workloadDockerfileContent []byte

const (
	WorkloadImageTag             = "cdbct-workload:latest"
	workloadContainer            = "cdbct-workload"
	WorkloadMetricsContainerPort = 9091
	RoleWorkload                 = "workload"
)

// WorkloadOptions configures the workload container.
type WorkloadOptions struct {
	Cluster     string
	Topology    ClusterTopology
	Interval    time.Duration
	BatchSize   int
	QueryEvery  time.Duration
	TenantCount int
	MetricsPort int
	GRPCPort    int
}

const WorkloadGRPCContainerPort = 9092

// InternalDSN builds a DSN using container hostnames on cdbct-net — used
// inside the workload container where localhost ports are not accessible.
func InternalDSN(nodes int) string {
	hosts := make([]string, nodes)
	for i := range nodes {
		hosts[i] = fmt.Sprintf("cdbct-crdb-%d:%d", i+1, crdbSQLPort)
	}
	return fmt.Sprintf(
		"postgresql://root@%s/defaultdb?sslmode=disable&load_balance_hosts=random",
		strings.Join(hosts, ","),
	)
}

// RegionalDSN builds a DSN scoped to nodes in a specific geographic region.
// The workload uses per-region pools so that geo-partitioned writes are
// routed through a home-region SQL gateway node, avoiding cross-region hops
// before the write even reaches the leaseholder.
func RegionalDSN(topo ClusterTopology, region string) string {
	var hosts []string
	for i := 1; i <= topo.Nodes; i++ {
		if topo.NodeRegion(i) == region {
			hosts = append(hosts, fmt.Sprintf("cdbct-crdb-%d:%d", i, crdbSQLPort))
		}
	}
	if len(hosts) == 0 {
		return InternalDSN(topo.Nodes)
	}
	return fmt.Sprintf(
		"postgresql://root@%s/defaultdb?sslmode=disable&load_balance_hosts=random",
		strings.Join(hosts, ","),
	)
}

// regionDSNFlag maps a region name to its workload --dsn-<x> flag name.
func regionDSNFlag(region string) string {
	switch region {
	case "us-east":
		return "--dsn-east"
	case "us-west":
		return "--dsn-west"
	case "eu-central":
		return "--dsn-eu"
	default:
		return ""
	}
}

// BuildWorkloadImage builds the cdbct-workload Docker image from the project
// source. It follows russ's pattern: tar up go.mod/go.sum + source tree, then
// call ImageBuild with the embedded multi-stage Dockerfile.
func (m *Manager) BuildWorkloadImage(ctx context.Context) error {
	log.Info().Str("image", WorkloadImageTag).Msg("building workload image")

	ctx2, err := buildContext()
	if err != nil {
		return fmt.Errorf("build context: %w", err)
	}

	resp, err := m.client.ImageBuild(ctx, ctx2, types.ImageBuildOptions{
		Tags:        []string{WorkloadImageTag},
		Remove:      true,
		ForceRemove: true,
		Dockerfile:  "Dockerfile",
	})
	if err != nil {
		return fmt.Errorf("image build: %w", err)
	}
	defer resp.Body.Close()

	// Stream build output so the user can see progress.
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Stream string `json:"stream"`
			Error  string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if msg.Error != "" {
			return fmt.Errorf("image build: %s", msg.Error)
		}
		if s := strings.TrimSpace(msg.Stream); s != "" {
			log.Debug().Msg(s)
		}
	}

	log.Info().Str("image", WorkloadImageTag).Msg("workload image built")
	return nil
}

// StartWorkload starts the workload container on the cdbct-net network.
// DSN flags are built dynamically from the topology's active regions.
func (m *Manager) StartWorkload(ctx context.Context, opts WorkloadOptions) error {
	// Remove any pre-existing workload container.
	existing, _ := m.ListContainersByRole(ctx, opts.Cluster, RoleWorkload)
	for _, c := range existing {
		log.Debug().Str("id", c.ID[:12]).Msg("removing existing workload container")
		_ = m.StopAndRemove(ctx, c.ID)
	}

	metricsPort := opts.MetricsPort
	if metricsPort == 0 {
		metricsPort = WorkloadMetricsContainerPort
	}

	metricsPortSpec := nat.Port(fmt.Sprintf("%d/tcp", WorkloadMetricsContainerPort))
	grpcPortSpec := nat.Port(fmt.Sprintf("%d/tcp", WorkloadGRPCContainerPort))
	grpcPort := opts.GRPCPort
	if grpcPort == 0 {
		grpcPort = WorkloadGRPCContainerPort
	}
	tenantCount := opts.TenantCount
	if tenantCount == 0 {
		tenantCount = 10
	}

	// Base command with the global DSN (all nodes).
	cmd := []string{
		"workload", "start",
		"--dsn", InternalDSN(opts.Topology.Nodes),
	}

	// Append a regional DSN flag for each active region in the topology.
	for _, region := range opts.Topology.Regions {
		flag := regionDSNFlag(region)
		if flag == "" {
			log.Warn().Str("region", region).Msg("no --dsn flag mapping for region, skipping")
			continue
		}
		cmd = append(cmd, flag, RegionalDSN(opts.Topology, region))
	}

	cmd = append(cmd,
		"--interval", opts.Interval.String(),
		"--batch", strconv.Itoa(opts.BatchSize),
		"--query-every", opts.QueryEvery.String(),
		"--tenants", strconv.Itoa(tenantCount),
		"--metrics-addr", fmt.Sprintf(":%d", WorkloadMetricsContainerPort),
		"--grpc-addr", fmt.Sprintf(":%d", WorkloadGRPCContainerPort),
	)

	resp, err := m.client.ContainerCreate(ctx,
		&container.Config{
			Image:  WorkloadImageTag,
			Cmd:    cmd,
			Labels: managedLabels(opts.Cluster, RoleWorkload, ""),
			ExposedPorts: nat.PortSet{
				metricsPortSpec: {},
				grpcPortSpec:    {},
			},
		},
		&container.HostConfig{
			NetworkMode: container.NetworkMode(NetworkName),
			PortBindings: nat.PortMap{
				metricsPortSpec: []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: strconv.Itoa(metricsPort)}},
				grpcPortSpec:    []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: strconv.Itoa(grpcPort)}},
			},
			RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
		},
		nil, nil,
		workloadContainer,
	)
	if err != nil {
		return fmt.Errorf("container create: %w", err)
	}
	if err := m.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("container start: %w", err)
	}

	log.Info().
		Str("container", workloadContainer).
		Dur("interval", opts.Interval).
		Int("batch", opts.BatchSize).
		Dur("query_every", opts.QueryEvery).
		Int("tenants", opts.TenantCount).
		Str("mode", string(opts.Topology.Mode)).
		Strs("regions", opts.Topology.Regions).
		Msg("workload container started")
	return nil
}

// StopWorkload stops and removes the workload container.
func (m *Manager) StopWorkload(ctx context.Context, cluster string) error {
	containers, err := m.ListContainersByRole(ctx, cluster, RoleWorkload)
	if err != nil {
		return err
	}
	for _, c := range containers {
		log.Info().Str("name", c.Names[0]).Msg("stopping workload container")
		_ = m.StopAndRemove(ctx, c.ID)
	}
	return nil
}

// buildContext creates a tar archive containing the project source files needed
// to build the cdbct-workload image: Dockerfile, go.mod, go.sum, and all .go
// files under cmd/ and internal/.
func buildContext() (io.Reader, error) {
	root, err := projectRoot()
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// Dockerfile first.
	if err := addBytes(tw, "Dockerfile", workloadDockerfileContent); err != nil {
		return nil, err
	}

	// Root-level module files.
	for _, name := range []string{"go.mod", "go.sum"} {
		if err := addFile(tw, root, name); err != nil {
			return nil, err
		}
	}

	// Add main.go.
	if err := addFile(tw, root, "main.go"); err != nil {
		return nil, err
	}

	// Recursively add cmd/ and internal/.
	for _, dir := range []string{"cmd", "internal"} {
		if err := addDir(tw, root, dir); err != nil {
			return nil, err
		}
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}

func addBytes(tw *tar.Writer, name string, content []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(content)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	_, err := tw.Write(content)
	return err
}

func addFile(tw *tar.Writer, root, rel string) error {
	full := filepath.Join(root, rel)
	info, err := os.Stat(full)
	if err != nil {
		return err
	}
	f, err := os.Open(full)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := tw.WriteHeader(&tar.Header{
		Name:     rel,
		Mode:     int64(info.Mode()),
		Size:     info.Size(),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

func addDir(tw *tar.Writer, root, dir string) error {
	return filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		return addFile(tw, root, rel)
	})
}

// projectRoot walks up from the executable's location looking for go.mod.
func projectRoot() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(exe)
	// When run via `go run` the executable is in a temp dir; fall back to cwd.
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		dir, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	// Walk up to find go.mod.
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find project root (no go.mod found)")
		}
		dir = parent
	}
}
