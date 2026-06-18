package cmd

import (
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/joshdurbin/cockroach_testing/internal/docker"
)

var quickstartCmd = &cobra.Command{
	Use:   "quickstart",
	Short: "Stand up a full demo environment (choose a cluster type)",
	Long: `quickstart provisions the complete stack — cluster, Toxiproxy, observability, and workload.

Choose a cluster installation:

  multi-geo-cluster      9-node, 3-region geo-distributed cluster with geo-partitioning,
                         realistic inter-region latency baselines, and full chaos support.

  single-region-cluster  3-node, single-region cluster (us-west) — simpler setup for
                         demonstrating RLS, QoS, and fault injection without geo complexity.`,
}

var quickstartMultiGeoCmd = &cobra.Command{
	Use:   "multi-geo-cluster",
	Short: "Stand up a 9-node, 3-region geo-distributed cluster",
	RunE: func(cmd *cobra.Command, args []string) error {
		nodes := viper.GetInt("quickstart.nodes")
		return runQuickstart(cmd, docker.MultiGeoTopology(nodes))
	},
}

var quickstartSingleRegionCmd = &cobra.Command{
	Use:   "single-region-cluster",
	Short: "Stand up a 3-node, single-region cluster (us-west)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runQuickstart(cmd, docker.SingleRegionTopology())
	},
}

func runQuickstart(cmd *cobra.Command, topo docker.ClusterTopology) error {
	m, err := docker.NewManager()
	if err != nil {
		return err
	}
	defer m.Close()

	interval := viper.GetDuration("quickstart.interval")
	batch := viper.GetInt("quickstart.batch")
	queryEvery := viper.GetDuration("quickstart.query-every")
	tenants := viper.GetInt("quickstart.tenants")
	cluster := viper.GetString("cluster.name")

	log.Info().
		Str("cluster", cluster).
		Int("nodes", topo.Nodes).
		Str("mode", string(topo.Mode)).
		Strs("regions", topo.Regions).
		Msg("quickstart: creating cluster")

	infos, err := m.CreateCluster(cmd.Context(), cluster, topo)
	if err != nil {
		return fmt.Errorf("cluster create: %w", err)
	}

	// Regional latency baselines only make sense for multi-geo clusters.
	if topo.Mode == docker.ModeMultiGeo && !viper.GetBool("quickstart.no-faults") {
		log.Info().Msg("quickstart: applying regional latencies")
		apiAddr := fmt.Sprintf("localhost:%d", docker.ToxiproxyAPIHostPort)
		if err := m.ApplyRegionalLatencies(apiAddr, topo); err != nil {
			log.Warn().Err(err).Msg("regional latency setup failed — cluster will run without baseline latency")
		}
	} else if topo.Mode == docker.ModeMultiGeo {
		log.Info().Msg("quickstart: skipping regional latencies (--no-faults)")
	}

	log.Info().Msg("quickstart: starting observability stack")
	if err := m.SetupObs(cmd.Context(), cluster, infos); err != nil {
		return fmt.Errorf("obs setup: %w", err)
	}

	log.Info().Msg("quickstart: building workload image")
	if err := m.BuildWorkloadImage(cmd.Context()); err != nil {
		return fmt.Errorf("build workload image: %w", err)
	}

	log.Info().Dur("interval", interval).Int("batch", batch).Dur("query_every", queryEvery).Int("tenants", tenants).Msg("quickstart: starting workload")
	if err := m.StartWorkload(cmd.Context(), docker.WorkloadOptions{
		Cluster:     cluster,
		Topology:    topo,
		Interval:    interval,
		BatchSize:   batch,
		QueryEvery:  queryEvery,
		TenantCount: tenants,
		MetricsPort: docker.WorkloadMetricsContainerPort,
	}); err != nil {
		return fmt.Errorf("start workload: %w", err)
	}

	printSummary(infos, topo)
	return nil
}

func printSummary(infos []docker.NodeInfo, topo docker.ClusterTopology) {
	fmt.Printf("\nEnvironment ready.\n\n")
	fmt.Printf("  %-8s  %-20s  %s\n", "NODE", "SQL", "ADMIN UI")
	for _, n := range infos {
		fmt.Printf("  %-8d  localhost:%-10d  http://localhost:%d\n",
			n.Index, n.SQLPort, n.HTTPPort)
	}
	fmt.Printf("\n  Prometheus : http://localhost:%d\n", docker.PrometheusPort)
	fmt.Printf("  Grafana    : http://localhost:%d\n", docker.GrafanaPort)
	fmt.Printf("  Workload metrics : http://localhost:%d/metrics\n", docker.WorkloadMetricsContainerPort)
	fmt.Printf("  Workload gRPC    : localhost:%d  (grpcurl -plaintext localhost:%d list)\n\n",
		docker.WorkloadGRPCContainerPort, docker.WorkloadGRPCContainerPort)

	if topo.Mode == docker.ModeMultiGeo {
		noFaults := viper.GetBool("quickstart.no-faults")
		if !noFaults {
			fmt.Printf("Regional latency baselines (applied now, always-on, bidirectional → RTT ≈ 2× value):\n")
			fmt.Printf("  us-east    nodes (1,4,7)  : 42ms ±5ms  → ~84ms RTT\n")
			fmt.Printf("  us-west    nodes (2,5,8)  : 55ms ±8ms  → ~110ms RTT\n")
			fmt.Printf("  eu-central nodes (3,6,9)  : 61ms ±10ms → ~122ms RTT\n\n")
			fmt.Printf("Inject chaos (stacks on regional latency):\n")
		} else {
			fmt.Printf("No faults active (--no-faults). Inject chaos:\n")
		}
		fmt.Printf("  cdbct chaos inject partition 1   # sever one us-east node\n")
		fmt.Printf("  cdbct chaos inject latency 2 --latency=300\n")
		fmt.Printf("  cdbct chaos inject reset 3\n")
		fmt.Printf("  cdbct chaos status\n")
		fmt.Printf("  cdbct chaos clear\n")
		if !noFaults {
			fmt.Printf("  cdbct chaos regional     # re-apply regional baselines after clear\n")
		}
	} else {
		fmt.Printf("Single-region cluster (us-west, %d nodes). No inter-region latency baselines.\n\n", topo.Nodes)
		fmt.Printf("Inject chaos:\n")
		fmt.Printf("  cdbct chaos inject partition 1\n")
		fmt.Printf("  cdbct chaos inject latency 2 --latency=300\n")
		fmt.Printf("  cdbct chaos inject reset 3\n")
		fmt.Printf("  cdbct chaos status\n")
		fmt.Printf("  cdbct chaos clear\n")
	}

	fmt.Printf("\nTear down:\n")
	fmt.Printf("  cdbct destroy\n\n")
}

func init() {
	rootCmd.AddCommand(quickstartCmd)
	quickstartCmd.AddCommand(quickstartMultiGeoCmd, quickstartSingleRegionCmd)

	// Shared workload flags on the parent so both subcommands inherit them.
	quickstartCmd.PersistentFlags().String("name", docker.DefaultCluster, "cluster name")
	quickstartCmd.PersistentFlags().Duration("interval", 10*time.Millisecond, "tick interval between insert batches")
	quickstartCmd.PersistentFlags().Int("batch", 10, "audit events inserted per tick")
	quickstartCmd.PersistentFlags().Duration("query-every", 50*time.Millisecond, "how often to run read queries")
	quickstartCmd.PersistentFlags().Int("tenants", 1000, "number of tenants to seed")

	viper.BindPFlag("cluster.name", quickstartCmd.PersistentFlags().Lookup("name"))
	viper.BindPFlag("quickstart.interval", quickstartCmd.PersistentFlags().Lookup("interval"))
	viper.BindPFlag("quickstart.batch", quickstartCmd.PersistentFlags().Lookup("batch"))
	viper.BindPFlag("quickstart.query-every", quickstartCmd.PersistentFlags().Lookup("query-every"))
	viper.BindPFlag("quickstart.tenants", quickstartCmd.PersistentFlags().Lookup("tenants"))

	// --nodes only applies to multi-geo (single-region is always 3).
	quickstartMultiGeoCmd.Flags().IntP("nodes", "n", 9, "number of CockroachDB nodes (9 = 3 per region, geo-partitions survive single-node failure)")
	viper.BindPFlag("quickstart.nodes", quickstartMultiGeoCmd.Flags().Lookup("nodes"))

	// --no-faults only applies to multi-geo (single-region has no regional latency baselines).
	quickstartMultiGeoCmd.Flags().Bool("no-faults", false, "skip Toxiproxy fault injection (no regional latency baselines)")
	viper.BindPFlag("quickstart.no-faults", quickstartMultiGeoCmd.Flags().Lookup("no-faults"))
}
