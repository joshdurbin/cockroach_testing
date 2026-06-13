package docker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/go-connections/nat"
	"github.com/rs/zerolog/log"
)

// NodeSQLPort returns the host-mapped SQL port for node idx (1-based).
func NodeSQLPort(idx int) int { return crdbSQLPort - 1 + idx }

// nodeRegion maps a 1-based node index to a geographic region label.
// Cycles through us-east → us-west → eu-central for any cluster size.
func nodeRegion(idx int) string {
	regions := []string{"us-east", "us-west", "eu-central"}
	return regions[(idx-1)%len(regions)]
}

// NodeRegion is the exported form for use in workload configuration.
func NodeRegion(idx int) string { return nodeRegion(idx) }

// NodeHTTPPort returns the host-mapped Admin UI port for node idx (1-based).
func NodeHTTPPort(idx int) int { return crdbHTTPPort - 1 + idx }

// NodeRPCPort returns the host-mapped RPC port for node idx (1-based).
// Base: 26356 so node 1=26357 (matching the container-internal port).
func NodeRPCPort(idx int) int { return crdbRPCPort - 1 + idx }

const (
	CRDBImage    = "cockroachdb/cockroach:v26.2.2"
	crdbRPCPort  = 26357
	crdbSQLPort  = 26257
	crdbHTTPPort = 8080

	// CRDBInternalSQLPort is the SQL port as seen from INSIDE a container.
	// Always 26257 regardless of host-side port mapping. Use this for
	// Exec-based commands (cockroach node status, cockroach init, etc.).
	CRDBInternalSQLPort = crdbSQLPort
)

// NodeInfo describes a running CockroachDB node.
type NodeInfo struct {
	ContainerID string
	Name        string
	Index       int
	SQLPort     int
	HTTPPort    int
	SQLAddr     string // host:port for pgx
}

// CreateCluster starts N CockroachDB nodes plus Toxiproxy, then initialises the cluster.
// Nodes advertise their RPC address through Toxiproxy; SQL clients connect directly.
func (m *Manager) CreateCluster(ctx context.Context, cluster string, nodes int) ([]NodeInfo, error) {
	if err := m.PullImage(ctx, CRDBImage); err != nil {
		return nil, err
	}
	if _, err := m.EnsureNetwork(ctx); err != nil {
		return nil, err
	}

	// Start Toxiproxy first so CRDB nodes can advertise through it.
	if err := m.EnsureToxiproxy(ctx, cluster, nodes); err != nil {
		return nil, fmt.Errorf("toxiproxy setup: %w", err)
	}

	// Build join list pointing through Toxiproxy.
	joinParts := make([]string, nodes)
	for i := range nodes {
		joinParts[i] = fmt.Sprintf("cdbct-toxiproxy:%d", toxiRPCPort(i+1))
	}
	joinList := strings.Join(joinParts, ",")

	var infos []NodeInfo
	for i := range nodes {
		info, err := m.startCRDBNode(ctx, cluster, i+1, nodes, joinList)
		if err != nil {
			return nil, fmt.Errorf("start node %d: %w", i+1, err)
		}
		infos = append(infos, info)
		log.Info().Int("node", i+1).Str("sql", info.SQLAddr).Msg("crdb node started")
	}

	if err := m.waitForNodes(ctx, infos); err != nil {
		return nil, fmt.Errorf("waiting for nodes: %w", err)
	}

	if err := m.initCluster(ctx, infos[0].ContainerID); err != nil {
		return nil, fmt.Errorf("cluster init: %w", err)
	}

	return infos, nil
}

// AddNode adds a node to an existing cluster (hot scale-up).
func (m *Manager) AddNode(ctx context.Context, cluster string) (NodeInfo, error) {
	existing, err := m.ListContainersByRole(ctx, cluster, RoleCRDB)
	if err != nil {
		return NodeInfo{}, err
	}
	if len(existing) == 0 {
		return NodeInfo{}, fmt.Errorf("no existing nodes in cluster %q", cluster)
	}

	// Determine next index.
	maxIdx := 0
	for _, c := range existing {
		if s, ok := c.Labels[LabelNodeIdx]; ok {
			if n, _ := strconv.Atoi(s); n > maxIdx {
				maxIdx = n
			}
		}
	}
	newIdx := maxIdx + 1
	totalNodes := newIdx

	// Rebuild Toxiproxy proxies to include new node.
	if err := m.EnsureToxiproxy(ctx, cluster, totalNodes); err != nil {
		return NodeInfo{}, fmt.Errorf("toxiproxy update: %w", err)
	}

	// Build join list from existing nodes (via Toxiproxy).
	joinParts := make([]string, maxIdx)
	for i := range maxIdx {
		joinParts[i] = fmt.Sprintf("cdbct-toxiproxy:%d", toxiRPCPort(i+1))
	}

	info, err := m.startCRDBNode(ctx, cluster, newIdx, totalNodes, strings.Join(joinParts, ","))
	if err != nil {
		return NodeInfo{}, err
	}

	log.Info().Int("node", newIdx).Str("sql", info.SQLAddr).Msg("node added to cluster")
	return info, nil
}

func (m *Manager) startCRDBNode(ctx context.Context, cluster string, idx, totalNodes int, joinList string) (NodeInfo, error) {
	name := fmt.Sprintf("cdbct-crdb-%d", idx)
	volName := fmt.Sprintf("cdbct-crdb-%d-data", idx)

	if err := m.EnsureVolume(ctx, volName); err != nil {
		return NodeInfo{}, fmt.Errorf("volume %s: %w", volName, err)
	}

	sqlHostPort  := strconv.Itoa(crdbSQLPort - 1 + idx)  // 26257, 26258, 26259, ...
	httpHostPort := strconv.Itoa(crdbHTTPPort - 1 + idx) // 8080, 8081, 8082, ...
	rpcHostPort  := strconv.Itoa(crdbRPCPort - 1 + idx)  // 26357, 26358, 26359, ...

	portBindings := nat.PortMap{
		nat.Port(fmt.Sprintf("%d/tcp", crdbSQLPort)):  []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: sqlHostPort}},
		nat.Port(fmt.Sprintf("%d/tcp", crdbHTTPPort)): []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: httpHostPort}},
		nat.Port(fmt.Sprintf("%d/tcp", crdbRPCPort)):  []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: rpcHostPort}},
	}
	exposedPorts := nat.PortSet{
		nat.Port(fmt.Sprintf("%d/tcp", crdbSQLPort)):  {},
		nat.Port(fmt.Sprintf("%d/tcp", crdbHTTPPort)): {},
		nat.Port(fmt.Sprintf("%d/tcp", crdbRPCPort)):  {},
	}

	cmd := []string{
		"start",
		"--insecure",
		fmt.Sprintf("--listen-addr=0.0.0.0:%d", crdbRPCPort),
		fmt.Sprintf("--sql-addr=0.0.0.0:%d", crdbSQLPort),
		fmt.Sprintf("--http-addr=0.0.0.0:%d", crdbHTTPPort),
		// Advertise RPC through Toxiproxy so inter-node traffic is injectable.
		fmt.Sprintf("--advertise-addr=cdbct-toxiproxy:%d", toxiRPCPort(idx)),
		// Advertise SQL directly (clients bypass chaos layer).
		fmt.Sprintf("--advertise-sql-addr=%s:%d", name, crdbSQLPort),
		"--join=" + joinList,
		"--store=/cockroach/cockroach-data",
		// Cap memory so three nodes + obs stack fit in a constrained Colima VM.
		// Default is 128MiB cache / 25% of RAM SQL; these explicit limits prevent
		// memory pressure that spikes Raft latency and triggers clock-offset restarts.
		"--cache=128MiB",
		"--max-sql-memory=256MiB",
		// Locality tags enable zone config constraints and lease_preferences to
		// resolve against physical nodes. Node N cycles through the three
		// canonical regions so zone configs work correctly in a single-VM cluster.
		fmt.Sprintf("--locality=region=%s,az=az1", nodeRegion(idx)),
	}

	resp, err := m.client.ContainerCreate(ctx,
		&container.Config{
			Image:        CRDBImage,
			Cmd:          cmd,
			ExposedPorts: exposedPorts,
			Labels:       managedLabels(cluster, RoleCRDB, strconv.Itoa(idx)),
			Hostname:     name,
		},
		&container.HostConfig{
			PortBindings: portBindings,
			Mounts: []mount.Mount{{
				Type:   mount.TypeVolume,
				Source: volName,
				Target: "/cockroach/cockroach-data",
			}},
			RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
		},
		networkConfig(name),
		nil,
		name,
	)
	if err != nil {
		return NodeInfo{}, fmt.Errorf("container create: %w", err)
	}

	if err := m.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return NodeInfo{}, fmt.Errorf("container start: %w", err)
	}

	return NodeInfo{
		ContainerID: resp.ID,
		Name:        name,
		Index:       idx,
		SQLPort:     crdbSQLPort - 1 + idx,
		HTTPPort:    crdbHTTPPort - 1 + idx,
		SQLAddr:     fmt.Sprintf("localhost:%d", crdbSQLPort-1+idx),
	}, nil
}

// waitForNodes waits until every node's RPC port (26357) accepts a TCP connection.
// That is the gRPC port that cockroach init needs, so this is the right signal.
// We also check the HTTP /health endpoint as a secondary confirmation.
func (m *Manager) waitForNodes(ctx context.Context, nodes []NodeInfo) error {
	log.Info().Msg("waiting for crdb nodes to become ready")
	httpClient := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		allReady := true
		for _, n := range nodes {
			// Primary: can we reach the HTTP liveness endpoint?
			url := fmt.Sprintf("http://localhost:%d/health", n.HTTPPort)
			resp, err := httpClient.Get(url)
			if err != nil {
				log.Debug().Int("node", n.Index).Err(err).Msg("http not up yet")
				allReady = false
				break
			}
			resp.Body.Close()

			// Secondary: can we open a TCP connection to the RPC port?
			rpcAddr := fmt.Sprintf("localhost:%d", NodeRPCPort(n.Index))
			conn, err := net.DialTimeout("tcp", rpcAddr, time.Second)
			if err != nil {
				log.Debug().Int("node", n.Index).Err(err).Msg("rpc port not up yet")
				allReady = false
				break
			}
			conn.Close()
			log.Debug().Int("node", n.Index).Int("http", resp.StatusCode).Msg("node ready")
		}
		if allReady {
			log.Info().Msg("all crdb nodes ready")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("crdb nodes did not become ready within 120s")
}

func (m *Manager) initCluster(ctx context.Context, containerID string) error {
	log.Info().Msg("initialising crdb cluster")
	// cockroach init speaks gRPC and must target the RPC/listen port, not the SQL port.
	out, code, err := m.Exec(ctx, containerID, []string{
		"cockroach", "init", "--insecure",
		fmt.Sprintf("--host=localhost:%d", crdbRPCPort),
	})
	if err != nil {
		return err
	}
	// Exit code 1 with "already initialized" is fine.
	if code != 0 && !strings.Contains(out, "already been initialized") {
		return fmt.Errorf("cockroach init failed (exit %d): %s", code, out)
	}
	log.Info().Msg("cluster initialised")
	return nil
}

// GetClusterNodes returns NodeInfo for all running CRDB nodes in the cluster.
func (m *Manager) GetClusterNodes(ctx context.Context, cluster string) ([]NodeInfo, error) {
	containers, err := m.ListContainersByRole(ctx, cluster, RoleCRDB)
	if err != nil {
		return nil, err
	}
	var infos []NodeInfo
	for _, c := range containers {
		idx, _ := strconv.Atoi(c.Labels[LabelNodeIdx])
		infos = append(infos, NodeInfo{
			ContainerID: c.ID,
			Name:        strings.TrimPrefix(c.Names[0], "/"),
			Index:       idx,
			SQLPort:     crdbSQLPort - 1 + idx,
			HTTPPort:    crdbHTTPPort - 1 + idx,
			SQLAddr:     fmt.Sprintf("localhost:%d", crdbSQLPort-1+idx),
		})
	}
	return infos, nil
}

// DestroyCluster stops and removes all cluster containers. Volumes are removed if purge=true.
func (m *Manager) DestroyCluster(ctx context.Context, cluster string, purge bool) error {
	// Collect volume names BEFORE removing containers — ListContainersByRole
	// returns nothing once the containers are gone.
	var volumesToDelete []string
	if purge {
		nodes, _ := m.ListContainersByRole(ctx, cluster, RoleCRDB)
		for _, c := range nodes {
			if idx, ok := c.Labels[LabelNodeIdx]; ok {
				volumesToDelete = append(volumesToDelete, fmt.Sprintf("cdbct-crdb-%s-data", idx))
			}
		}
	}

	for _, role := range []string{RoleCRDB, RoleToxiproxy} {
		containers, err := m.ListContainersByRole(ctx, cluster, role)
		if err != nil {
			return err
		}
		for _, c := range containers {
			log.Info().Str("name", c.Names[0]).Msg("removing container")
			if err := m.StopAndRemove(ctx, c.ID); err != nil {
				log.Warn().Err(err).Str("id", c.ID[:12]).Msg("remove failed")
			}
		}
	}

	for _, vol := range volumesToDelete {
		log.Info().Str("volume", vol).Msg("removing volume")
		_ = m.RemoveVolume(ctx, vol)
	}

	return nil
}
