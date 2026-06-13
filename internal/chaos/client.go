package chaos

import (
	"fmt"
	"strconv"

	toxiclient "github.com/Shopify/toxiproxy/v2/client"
)

// FaultType enumerates the kinds of network fault injectable via Toxiproxy.
type FaultType string

const (
	FaultLatency   FaultType = "latency"
	FaultBandwidth FaultType = "bandwidth"
	FaultTimeout   FaultType = "timeout"
	FaultReset     FaultType = "reset_peer"
	// FaultPartition disables the proxy entirely — no TCP connections are
	// accepted. This is a complete network partition, stronger than FaultDown
	// (which accepts the connection and then drops data).
	FaultPartition FaultType = "partition"
)

// Client wraps the Toxiproxy API for CockroachDB inter-node chaos injection.
type Client struct {
	tc *toxiclient.Client
}

// New creates a Client pointed at the Toxiproxy API running on the given host:port.
func New(addr string) *Client {
	return &Client{tc: toxiclient.NewClient(addr)}
}

func proxyName(nodeIdx int) string {
	return fmt.Sprintf("crdb-node-%d", nodeIdx)
}

// RegionalLatency describes the realistic one-way network delay for a region.
type RegionalLatency struct {
	LatencyMS int
	JitterMS  int
}

// WellKnownLatencies maps region names to their average one-way delay to all
// other regions in the cluster. Values are derived from realistic cloud
// datacenter RTTs between the named regions, divided by two:
//
//	us-east  ↔ us-west:    ~72ms RTT → 36ms one-way
//	us-east  ↔ eu-central: ~95ms RTT → 48ms one-way
//	us-west  ↔ eu-central: ~150ms RTT → 75ms one-way
//
// Each region stores the average one-way delay to the other two regions.
// Applied in BOTH upstream and downstream directions, so the effective Raft
// RTT through a follower's proxy ≈ 2 × the configured value:
//
//	us-east  follower: ~84ms RTT
//	us-west  follower: ~110ms RTT
//	eu-central follower: ~122ms RTT
var WellKnownLatencies = map[string]RegionalLatency{
	"us-east":    {LatencyMS: 42, JitterMS: 5},  // avg(36, 48) ms
	"us-west":    {LatencyMS: 55, JitterMS: 8},  // avg(36, 75) ms
	"eu-central": {LatencyMS: 61, JitterMS: 10}, // avg(48, 75) ms
}

// InjectNamedLatency injects latency toxics in BOTH upstream and downstream
// directions on the proxy for nodeIdx. Two toxics are created: name+"-up"
// and name+"-down", each adding latencyMS delay. This makes the effective
// per-proxy RTT ≈ 2×latencyMS, correctly modeling TCP round-trip propagation
// rather than one-directional delay. CockroachDB's Raft replication is TCP
// (gRPC) and requires quorum ACKs, so the full RTT must be modeled.
func (c *Client) InjectNamedLatency(nodeIdx int, name string, latencyMS, jitterMS int) error {
	p, err := c.tc.Proxy(proxyName(nodeIdx))
	if err != nil {
		return fmt.Errorf("proxy for node %d: %w", nodeIdx, err)
	}
	attrs := toxiclient.Attributes{
		"latency": latencyMS,
		"jitter":  jitterMS,
	}
	if _, err = p.AddToxic(name+"-up", "latency", "upstream", 1.0, attrs); err != nil {
		return err
	}
	_, err = p.AddToxic(name+"-down", "latency", "downstream", 1.0, attrs)
	return err
}

// EnsureProxies creates Toxiproxy proxies for each CockroachDB node (1-based).
func (c *Client) EnsureProxies(n int) error {
	for i := range n {
		idx := i + 1
		name := proxyName(idx)
		listen := fmt.Sprintf("0.0.0.0:%d", 26000+idx)
		upstream := fmt.Sprintf("cdbct-crdb-%d:26357", idx)

		if _, err := c.tc.Proxy(name); err == nil {
			continue
		}
		if _, err := c.tc.CreateProxy(name, listen, upstream); err != nil {
			return fmt.Errorf("create proxy for node %d: %w", idx, err)
		}
	}
	return nil
}

// InjectFault applies a fault on the proxy for nodeIdx.
func (c *Client) InjectFault(nodeIdx int, fault FaultType, attrs map[string]string) error {
	p, err := c.tc.Proxy(proxyName(nodeIdx))
	if err != nil {
		return fmt.Errorf("proxy for node %d: %w", nodeIdx, err)
	}

	// FaultPartition disables the proxy rather than adding a toxic.
	if fault == FaultPartition {
		p.Enabled = false
		return p.Save()
	}

	toxicName := fmt.Sprintf("%s-%s", proxyName(nodeIdx), fault)

	switch fault {
	case FaultLatency:
		latency, _ := strconv.Atoi(attrs["latency"])
		jitter, _ := strconv.Atoi(attrs["jitter"])
		_, err = p.AddToxic(toxicName, "latency", "downstream", 1.0, toxiclient.Attributes{
			"latency": latency,
			"jitter":  jitter,
		})
	case FaultBandwidth:
		rate, _ := strconv.Atoi(attrs["rate"])
		_, err = p.AddToxic(toxicName, "bandwidth", "downstream", 1.0, toxiclient.Attributes{
			"rate": rate,
		})
	case FaultTimeout:
		timeout, _ := strconv.Atoi(attrs["timeout"])
		_, err = p.AddToxic(toxicName, "timeout", "downstream", 1.0, toxiclient.Attributes{
			"timeout": timeout,
		})
	case FaultReset:
		_, err = p.AddToxic(toxicName, "reset_peer", "downstream", 1.0, toxiclient.Attributes{})
	default:
		return fmt.Errorf("unknown fault type %q", fault)
	}

	return err
}

// ClearFaults removes all toxics from the proxy for nodeIdx and re-enables
// it if it was partitioned.
func (c *Client) ClearFaults(nodeIdx int) error {
	p, err := c.tc.Proxy(proxyName(nodeIdx))
	if err != nil {
		return fmt.Errorf("proxy for node %d: %w", nodeIdx, err)
	}

	// Re-enable if partitioned.
	if !p.Enabled {
		p.Enabled = true
		if err := p.Save(); err != nil {
			return fmt.Errorf("re-enable proxy: %w", err)
		}
	}

	toxics, err := p.Toxics()
	if err != nil {
		return err
	}
	for _, t := range toxics {
		if err := p.RemoveToxic(t.Name); err != nil {
			return fmt.Errorf("remove toxic %q: %w", t.Name, err)
		}
	}
	return nil
}

// ClearAll removes all faults from every registered cdbct proxy.
// It discovers the proxy list from Toxiproxy directly so the node count
// never needs to be specified — works correctly for any cluster size.
func (c *Client) ClearAll() error {
	proxies, err := c.tc.Proxies()
	if err != nil {
		return fmt.Errorf("list proxies: %w", err)
	}
	for name := range proxies {
		if !isCRDBProxy(name) {
			continue
		}
		p, err := c.tc.Proxy(name)
		if err != nil {
			continue
		}
		if !p.Enabled {
			p.Enabled = true
			_ = p.Save()
		}
		toxics, _ := p.Toxics()
		for _, t := range toxics {
			_ = p.RemoveToxic(t.Name)
		}
	}
	return nil
}

// Status returns active toxics (or partition state) for all registered cdbct
// proxies, discovered dynamically from Toxiproxy. Works for any cluster size.
func (c *Client) Status() (map[string][]string, error) {
	proxies, err := c.tc.Proxies()
	if err != nil {
		return nil, fmt.Errorf("list proxies: %w", err)
	}
	result := make(map[string][]string, len(proxies))
	for name, p := range proxies {
		if !isCRDBProxy(name) {
			continue
		}
		if !p.Enabled {
			result[name] = []string{"PARTITIONED (proxy disabled)"}
			continue
		}
		toxics, err := p.Toxics()
		if err != nil {
			result[name] = []string{err.Error()}
			continue
		}
		labels := make([]string, 0, len(toxics))
		for _, t := range toxics {
			labels = append(labels, formatToxic(t))
		}
		if len(labels) == 0 {
			labels = []string{"clean"}
		}
		result[name] = labels
	}
	return result, nil
}

// isCRDBProxy reports whether a proxy name belongs to this tool.
func isCRDBProxy(name string) bool {
	return len(name) > len("crdb-node-") && name[:len("crdb-node-")] == "crdb-node-"
}

// formatToxic renders a toxic as "type(attrs)" so status output shows
// the actual injected parameters rather than just the auto-generated name.
func formatToxic(t toxiclient.Toxic) string {
	switch t.Type {
	case "latency":
		latency := intAttr(t.Attributes, "latency")
		jitter := intAttr(t.Attributes, "jitter")
		if jitter > 0 {
			return fmt.Sprintf("latency(%dms ±%dms)", latency, jitter)
		}
		return fmt.Sprintf("latency(%dms)", latency)
	case "bandwidth":
		return fmt.Sprintf("bandwidth(%d KB/s)", intAttr(t.Attributes, "rate"))
	case "timeout":
		return fmt.Sprintf("timeout(%dms)", intAttr(t.Attributes, "timeout"))
	case "reset_peer":
		return "reset"
	case "limit_data":
		return "down(data dropped)"
	default:
		return fmt.Sprintf("%s(%.0f%%)", t.Type, t.Toxicity*100)
	}
}

func intAttr(attrs toxiclient.Attributes, key string) int {
	v, ok := attrs[key]
	if !ok {
		return 0
	}
	switch val := v.(type) {
	case float64:
		return int(val)
	case int:
		return val
	case int64:
		return int(val)
	}
	return 0
}
