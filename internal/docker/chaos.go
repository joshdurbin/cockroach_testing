package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"github.com/rs/zerolog/log"

	"github.com/joshdurbin/cockroach_testing/internal/chaos"
)

const (
	ToxiproxyImage       = "ghcr.io/shopify/toxiproxy:2.12.0"
	toxiproxyContainerName = "cdbct-toxiproxy"
	ToxiproxyAPIPort     = 8474
	ToxiproxyAPIHostPort = 8474
	// Base port for CRDB RPC proxies: node N gets port 26000+N.
	toxiRPCBase = 26000
)

// toxiRPCPort returns the Toxiproxy listen port for CRDB node idx (1-based).
func toxiRPCPort(idx int) int {
	return toxiRPCBase + idx
}

// EnsureToxiproxy starts the Toxiproxy container if not already running.
// It exposes one proxy port per CRDB node for RPC traffic.
func (m *Manager) EnsureToxiproxy(ctx context.Context, cluster string, nodes int) error {
	existing, err := m.ListContainersByRole(ctx, cluster, RoleToxiproxy)
	if err != nil {
		return err
	}
	// If already running with the right number of ports, leave it.
	if len(existing) > 0 {
		for _, c := range existing {
			if c.State == "running" {
				log.Debug().Msg("toxiproxy already running")
				return nil
			}
			// Container exists but stopped — remove and recreate.
			_ = m.StopAndRemove(ctx, c.ID)
		}
	}

	if err := m.PullImage(ctx, ToxiproxyImage); err != nil {
		return err
	}

	portBindings := nat.PortMap{
		nat.Port(fmt.Sprintf("%d/tcp", ToxiproxyAPIPort)): []nat.PortBinding{
			{HostIP: "0.0.0.0", HostPort: strconv.Itoa(ToxiproxyAPIHostPort)},
		},
	}
	exposedPorts := nat.PortSet{
		nat.Port(fmt.Sprintf("%d/tcp", ToxiproxyAPIPort)): {},
	}
	// Expose a port for each node's RPC proxy.
	for i := range nodes {
		p := nat.Port(fmt.Sprintf("%d/tcp", toxiRPCPort(i+1)))
		exposedPorts[p] = struct{}{}
		portBindings[p] = []nat.PortBinding{}
	}

	resp, err := m.client.ContainerCreate(ctx,
		&container.Config{
			Image:        ToxiproxyImage,
			ExposedPorts: exposedPorts,
			Labels:       managedLabels(cluster, RoleToxiproxy, ""),
			Hostname:     toxiproxyContainerName,
		},
		&container.HostConfig{
			PortBindings:  portBindings,
			RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
		},
		networkConfig(toxiproxyContainerName),
		nil,
		toxiproxyContainerName,
	)
	if err != nil {
		return fmt.Errorf("create toxiproxy: %w", err)
	}

	if err := m.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start toxiproxy: %w", err)
	}

	log.Info().Str("container", toxiproxyContainerName).Int("nodes", nodes).Msg("toxiproxy started")

	apiAddr := fmt.Sprintf("localhost:%d", ToxiproxyAPIHostPort)
	if err := waitForToxiproxyAPI(ctx, apiAddr); err != nil {
		return fmt.Errorf("toxiproxy api not ready: %w", err)
	}
	if err := registerToxiproxyProxies(apiAddr, nodes); err != nil {
		return fmt.Errorf("register toxiproxy proxies: %w", err)
	}
	return nil
}

// waitForToxiproxyAPI polls until the Toxiproxy HTTP API responds.
func waitForToxiproxyAPI(ctx context.Context, apiAddr string) error {
	log.Debug().Str("addr", apiAddr).Msg("waiting for toxiproxy api")
	deadline := time.Now().Add(30 * time.Second)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(fmt.Sprintf("http://%s/proxies", apiAddr))
		if err == nil {
			resp.Body.Close()
			log.Debug().Msg("toxiproxy api ready")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("toxiproxy api at %s did not become ready", apiAddr)
}

// registerToxiproxyProxies creates one TCP proxy per CRDB node.
// Each proxy listens on 0.0.0.0:2600N and forwards to cdbct-crdb-N:26357.
// Existing proxies (409 Conflict) are silently accepted.
func registerToxiproxyProxies(apiAddr string, nodes int) error {
	client := &http.Client{Timeout: 5 * time.Second}
	for i := range nodes {
		idx := i + 1
		body, _ := json.Marshal(map[string]any{
			"name":     fmt.Sprintf("crdb-node-%d", idx),
			"listen":   fmt.Sprintf("0.0.0.0:%d", toxiRPCPort(idx)),
			"upstream": fmt.Sprintf("cdbct-crdb-%d:%d", idx, crdbRPCPort),
			"enabled":  true,
		})
		resp, err := client.Post(
			fmt.Sprintf("http://%s/proxies", apiAddr),
			"application/json",
			bytes.NewReader(body),
		)
		if err != nil {
			return fmt.Errorf("register proxy for node %d: %w", idx, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
			return fmt.Errorf("unexpected status %d registering proxy for node %d", resp.StatusCode, idx)
		}
		log.Debug().Int("node", idx).Int("port", toxiRPCPort(idx)).Msg("toxiproxy proxy registered")
	}
	log.Info().Int("nodes", nodes).Msg("toxiproxy proxies registered")
	return nil
}

// regionalLatencyToxicName is the fixed name for the baseline regional
// latency toxic on each proxy. Using a distinct name lets it coexist with
// user-injected chaos latency toxics (they are additive in Toxiproxy).
func regionalLatencyToxicName(nodeIdx int) string {
	return fmt.Sprintf("crdb-node-%d-latency-regional", nodeIdx)
}

// ApplyRegionalLatencies injects realistic inter-region latency baselines
// into every Toxiproxy proxy based on the node's assigned region.
//
// Latency values model average one-way propagation delay from each region to
// the other two regions, derived from realistic cloud datacenter RTTs. Each
// toxic is applied in BOTH upstream and downstream directions so the effective
// Raft RTT through any follower proxy ≈ 2× the configured one-way value —
// correctly modeling TCP round-trip cost for CockroachDB's gRPC/Raft
// replication. See chaos.WellKnownLatencies for the per-region values.
//
// Two toxics per node are registered (name-up / name-down) so they coexist
// with user-injected chaos faults — all are applied additively by Toxiproxy.
// chaos clear removes them; chaos regional re-applies them.
func (m *Manager) ApplyRegionalLatencies(apiAddr string, n int) error {
	c := chaos.New(apiAddr)
	for i := range n {
		idx := i + 1
		region := nodeRegion(idx)
		lat, ok := chaos.WellKnownLatencies[region]
		if !ok {
			log.Warn().Int("node", idx).Str("region", region).Msg("no well-known latency for region, skipping")
			continue
		}
		name := regionalLatencyToxicName(idx)
		if err := c.InjectNamedLatency(idx, name, lat.LatencyMS, lat.JitterMS); err != nil {
			return fmt.Errorf("node %d (%s): %w", idx, region, err)
		}
		log.Debug().
			Int("node", idx).
			Str("region", region).
			Int("latency_ms", lat.LatencyMS).
			Int("jitter_ms", lat.JitterMS).
			Msg("regional latency applied")
	}
	log.Info().
		Int("nodes", n).
		Str("us_east_ms", "42±5 (RTT ~84ms)").
		Str("us_west_ms", "55±8 (RTT ~110ms)").
		Str("eu_central_ms", "61±10 (RTT ~122ms)").
		Msg("regional latencies applied (bidirectional)")
	return nil
}
