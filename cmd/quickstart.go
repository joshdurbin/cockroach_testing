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
	Short: "Stand up the full environment: cluster + Toxiproxy + obs + workload container",
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := docker.NewManager()
		if err != nil {
			return err
		}
		defer m.Close()

		nodes := viper.GetInt("quickstart.nodes")
		interval := viper.GetDuration("quickstart.interval")
		batch := viper.GetInt("quickstart.batch")
		queryEvery := viper.GetDuration("quickstart.query-every")
		tenants := viper.GetInt("quickstart.tenants")
		cluster := viper.GetString("cluster.name")

		log.Info().Str("cluster", cluster).Int("nodes", nodes).Msg("quickstart: creating cluster")
		infos, err := m.CreateCluster(cmd.Context(), cluster, nodes)
		if err != nil {
			return fmt.Errorf("cluster create: %w", err)
		}

		if !viper.GetBool("quickstart.no-faults") {
			log.Info().Msg("quickstart: applying regional latencies")
			apiAddr := fmt.Sprintf("localhost:%d", docker.ToxiproxyAPIHostPort)
			if err := m.ApplyRegionalLatencies(apiAddr, nodes); err != nil {
				log.Warn().Err(err).Msg("regional latency setup failed — cluster will run without baseline latency")
			}
		} else {
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
			Nodes:       nodes,
			Interval:    interval,
			BatchSize:   batch,
			QueryEvery:  queryEvery,
			TenantCount: tenants,
			MetricsPort: docker.WorkloadMetricsContainerPort,
		}); err != nil {
			return fmt.Errorf("start workload: %w", err)
		}

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
		if !viper.GetBool("quickstart.no-faults") {
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
		if !viper.GetBool("quickstart.no-faults") {
			fmt.Printf("  cdbct chaos regional     # re-apply regional baselines after clear\n")
		}
		fmt.Printf("\n")
		fmt.Printf("Tear down:\n")
		fmt.Printf("  cdbct destroy\n\n")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(quickstartCmd)

	quickstartCmd.Flags().IntP("nodes", "n", 9, "number of CockroachDB nodes (9 = 3 per region, geo-partitions survive single-node failure)")
	quickstartCmd.Flags().String("name", docker.DefaultCluster, "cluster name")
	quickstartCmd.Flags().Duration("interval", 10*time.Millisecond, "tick interval between insert batches")
	quickstartCmd.Flags().Int("batch", 10, "audit events inserted per tick")
	quickstartCmd.Flags().Duration("query-every", 50*time.Millisecond, "how often to run read queries")
	quickstartCmd.Flags().Int("tenants", 1000, "number of tenants to seed")
	quickstartCmd.Flags().Bool("no-faults", false, "skip Toxiproxy fault injection (no regional latency baselines)")

	viper.BindPFlag("quickstart.nodes", quickstartCmd.Flags().Lookup("nodes"))
	viper.BindPFlag("cluster.name", quickstartCmd.Flags().Lookup("name"))
	viper.BindPFlag("quickstart.interval", quickstartCmd.Flags().Lookup("interval"))
	viper.BindPFlag("quickstart.batch", quickstartCmd.Flags().Lookup("batch"))
	viper.BindPFlag("quickstart.query-every", quickstartCmd.Flags().Lookup("query-every"))
	viper.BindPFlag("quickstart.tenants", quickstartCmd.Flags().Lookup("tenants"))
	viper.BindPFlag("quickstart.no-faults", quickstartCmd.Flags().Lookup("no-faults"))
}
