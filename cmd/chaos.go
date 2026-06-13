package cmd

import (
	"fmt"
	"strconv"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/joshdurbin/cockroach_testing/internal/chaos"
	"github.com/joshdurbin/cockroach_testing/internal/docker"
)

var chaosCmd = &cobra.Command{
	Use:   "chaos",
	Short: "Inject and manage network faults via Toxiproxy",
}

var chaosInjectCmd = &cobra.Command{
	Use:   "inject",
	Short: "Inject a network fault (choose a sub-command for the fault type)",
}

// ---------- status ----------

var chaosStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show active toxics on each node proxy",
	RunE: func(cmd *cobra.Command, args []string) error {
		c := chaosClient()
		status, err := c.Status()
		if err != nil {
			return err
		}
		if len(status) == 0 {
			fmt.Println("No cdbct proxies found. Is Toxiproxy running?")
			return nil
		}
		fmt.Printf("%-20s  %s\n", "PROXY", "ACTIVE TOXICS")
		for proxy, toxics := range status {
			fmt.Printf("%-20s  %v\n", proxy, toxics)
		}
		return nil
	},
}

// ---------- inject latency ----------

var chaosInjectLatencyCmd = &cobra.Command{
	Use:   "latency <node>",
	Short: "Add fixed latency (+ optional jitter) to a node's RPC path",
	Args:  cobra.ExactArgs(1),
	Example: `  cdbct chaos inject latency 2 --latency=200
  cdbct chaos inject latency 2 --latency=200 --jitter=50`,
	RunE: func(cmd *cobra.Command, args []string) error {
		node, err := nodeArg(args)
		if err != nil {
			return err
		}
		attrs := map[string]string{
			"latency": strconv.Itoa(viper.GetInt("chaos.inject.latency")),
			"jitter":  strconv.Itoa(viper.GetInt("chaos.inject.jitter")),
		}
		return inject(node, chaos.FaultLatency, attrs)
	},
}

// ---------- inject bandwidth ----------

var chaosInjectBandwidthCmd = &cobra.Command{
	Use:   "bandwidth <node>",
	Short: "Throttle bandwidth on a node's RPC path",
	Args:  cobra.ExactArgs(1),
	Example: `  cdbct chaos inject bandwidth 3 --rate=100`,
	RunE: func(cmd *cobra.Command, args []string) error {
		node, err := nodeArg(args)
		if err != nil {
			return err
		}
		attrs := map[string]string{
			"rate": strconv.Itoa(viper.GetInt("chaos.inject.rate")),
		}
		return inject(node, chaos.FaultBandwidth, attrs)
	},
}

// ---------- inject timeout ----------

var chaosInjectTimeoutCmd = &cobra.Command{
	Use:   "timeout <node>",
	Short: "Hang a node's connections for N ms then close them",
	Args:  cobra.ExactArgs(1),
	Example: `  cdbct chaos inject timeout 1 --timeout=3000`,
	RunE: func(cmd *cobra.Command, args []string) error {
		node, err := nodeArg(args)
		if err != nil {
			return err
		}
		attrs := map[string]string{
			"timeout": strconv.Itoa(viper.GetInt("chaos.inject.timeout")),
		}
		return inject(node, chaos.FaultTimeout, attrs)
	},
}

// ---------- inject partition ----------

var chaosInjectPartitionCmd = &cobra.Command{
	Use:   "partition <node>",
	Short: "Completely sever a node's RPC connectivity (disables the proxy)",
	Args:  cobra.ExactArgs(1),
	Example: `  cdbct chaos inject partition 2`,
	RunE: func(cmd *cobra.Command, args []string) error {
		node, err := nodeArg(args)
		if err != nil {
			return err
		}
		return inject(node, chaos.FaultPartition, nil)
	},
}

// ---------- inject reset ----------

var chaosInjectResetCmd = &cobra.Command{
	Use:   "reset <node>",
	Short: "Send TCP RST on every connection, forcing CRDB retries",
	Args:  cobra.ExactArgs(1),
	Example: `  cdbct chaos inject reset 1`,
	RunE: func(cmd *cobra.Command, args []string) error {
		node, err := nodeArg(args)
		if err != nil {
			return err
		}
		return inject(node, chaos.FaultReset, nil)
	},
}

// ---------- clear ----------

var chaosClearCmd = &cobra.Command{
	Use:   "clear [node]",
	Short: "Remove all faults from one node (or all nodes if no argument)",
	Args:  cobra.MaximumNArgs(1),
	Example: `  cdbct chaos clear       # clear all nodes (auto-discovered)
  cdbct chaos clear 2     # clear node 2 only`,
	RunE: func(cmd *cobra.Command, args []string) error {
		c := chaosClient()
		if len(args) == 1 {
			node, err := nodeArg(args)
			if err != nil {
				return err
			}
			if err := c.ClearFaults(node); err != nil {
				return err
			}
			fmt.Printf("Cleared faults on node %d\n", node)
		} else {
			if err := c.ClearAll(); err != nil {
				return err
			}
			fmt.Println("Cleared all faults")
		}
		return nil
	},
}

// ---------- regional ----------

var chaosRegionalCmd = &cobra.Command{
	Use:   "regional",
	Short: "Re-apply realistic inter-region latency baselines (run after chaos clear)",
	Long: `Injects the well-known one-way latency for each node's region onto its Toxiproxy proxy.
These baselines are applied automatically by quickstart and are removed by chaos clear.

  us-east  nodes (1,4,7) : 39ms ±5ms   avg of 33ms→us-west, 45ms→eu-central
  us-west  nodes (2,5,8) : 51ms ±8ms   avg of 33ms→us-east, 70ms→eu-central
  eu-central nodes (3,6,9): 57ms ±10ms  avg of 45ms→us-east, 70ms→us-west

Chaos faults injected after this command stack additively on top of these baselines.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := docker.NewManager()
		if err != nil {
			return err
		}
		defer m.Close()

		nodes := viper.GetInt("chaos.nodes")
		apiAddr := viper.GetString("chaos.api-addr")
		if err := m.ApplyRegionalLatencies(apiAddr, nodes); err != nil {
			return err
		}
		fmt.Printf("Regional latencies applied for %d nodes.\n", nodes)
		return nil
	},
}

// ---------- setup ----------

var chaosSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Ensure Toxiproxy is running and node proxies are registered",
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := docker.NewManager()
		if err != nil {
			return err
		}
		defer m.Close()

		cluster := viper.GetString("cluster.name")
		nodes := viper.GetInt("chaos.nodes")

		if err := m.EnsureToxiproxy(cmd.Context(), cluster, nodes); err != nil {
			return fmt.Errorf("start toxiproxy: %w", err)
		}

		c := chaosClient()
		if err := c.EnsureProxies(nodes); err != nil {
			return fmt.Errorf("register proxies: %w", err)
		}

		log.Info().Msg("toxiproxy ready")
		fmt.Printf("Toxiproxy API: http://localhost:%d\n", docker.ToxiproxyAPIHostPort)
		fmt.Printf("Node proxies registered for %d node(s)\n", nodes)
		return nil
	},
}

// ---------- helpers ----------

func nodeArg(args []string) (int, error) {
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 1 {
		return 0, fmt.Errorf("node must be a positive integer, got %q", args[0])
	}
	return n, nil
}

func inject(node int, fault chaos.FaultType, attrs map[string]string) error {
	c := chaosClient()
	if err := c.InjectFault(node, fault, attrs); err != nil {
		return err
	}
	log.Info().Int("node", node).Str("fault", string(fault)).Msg("fault injected")
	fmt.Printf("Fault %q injected on node %d\n", fault, node)
	return nil
}

func chaosClient() *chaos.Client {
	return chaos.New(viper.GetString("chaos.api-addr"))
}

// ---------- init ----------

func init() {
	rootCmd.AddCommand(chaosCmd)
	chaosCmd.AddCommand(chaosStatusCmd, chaosInjectCmd, chaosClearCmd, chaosRegionalCmd, chaosSetupCmd)
	chaosInjectCmd.AddCommand(
		chaosInjectLatencyCmd,
		chaosInjectBandwidthCmd,
		chaosInjectTimeoutCmd,
		chaosInjectPartitionCmd,
		chaosInjectResetCmd,
	)

	chaosCmd.PersistentFlags().String("api-addr", "localhost:8474", "Toxiproxy API address")
	viper.BindPFlag("chaos.api-addr", chaosCmd.PersistentFlags().Lookup("api-addr"))

	// --nodes is only used by chaos setup to register proxies.
	// status and clear-all discover proxies from Toxiproxy directly.
	chaosSetupCmd.Flags().Int("nodes", 9, "number of cluster nodes to register proxies for")
	viper.BindPFlag("chaos.nodes", chaosSetupCmd.Flags().Lookup("nodes"))

	chaosInjectLatencyCmd.Flags().Int("latency", 100, "latency in ms")
	chaosInjectLatencyCmd.Flags().Int("jitter", 0, "jitter in ms")
	viper.BindPFlag("chaos.inject.latency", chaosInjectLatencyCmd.Flags().Lookup("latency"))
	viper.BindPFlag("chaos.inject.jitter", chaosInjectLatencyCmd.Flags().Lookup("jitter"))

	chaosInjectBandwidthCmd.Flags().Int("rate", 100, "bandwidth limit in KB/s")
	viper.BindPFlag("chaos.inject.rate", chaosInjectBandwidthCmd.Flags().Lookup("rate"))

	chaosInjectTimeoutCmd.Flags().Int("timeout", 5000, "timeout in ms before connection is closed")
	viper.BindPFlag("chaos.inject.timeout", chaosInjectTimeoutCmd.Flags().Lookup("timeout"))

	chaosSetupCmd.Flags().String("cluster-name", docker.DefaultCluster, "cluster name")
	viper.BindPFlag("cluster.name", chaosSetupCmd.Flags().Lookup("cluster-name"))
}
