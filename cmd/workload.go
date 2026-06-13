package cmd

import (
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/joshdurbin/cockroach_testing/internal/docker"
	"github.com/joshdurbin/cockroach_testing/internal/workload"
)

var workloadCmd = &cobra.Command{
	Use:   "workload",
	Short: "Manage the load test workload container",
}

var workloadStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the workload (runs inside the container as cdbct workload start)",
	RunE: func(cmd *cobra.Command, args []string) error {
		interval := viper.GetDuration("workload.interval")
		batch := viper.GetInt("workload.batch")
		queryEvery := viper.GetDuration("workload.query-every")
		metricsAddr := viper.GetString("workload.metrics-addr")

		// If an explicit DSN is set (i.e. running inside the container), run in-process.
		dsn := viper.GetString("workload.dsn")
		if dsn != "" {
			fmt.Printf("Workload starting — 1 event per %s (batch %d), queries every %s\n",
				interval, batch, queryEvery)
			fmt.Printf("  DSN: %s\n", dsn)
			fmt.Printf("  Metrics: http://localhost%s/metrics\n\n", metricsAddr)
			tenants := viper.GetInt("workload.tenants")
			grpcAddr := viper.GetString("workload.grpc-addr")
			return workload.Run(cmd.Context(), workload.Config{
				DSN: dsn,
				RegionalDSNs: map[string]string{
					"us-east":    viper.GetString("workload.dsn-east"),
					"us-west":    viper.GetString("workload.dsn-west"),
					"eu-central": viper.GetString("workload.dsn-eu"),
				},
				Interval:    interval,
				BatchSize:   batch,
				QueryEvery:  queryEvery,
				TenantCount: tenants,
				MetricsAddr: metricsAddr,
				GRPCAddr:    grpcAddr,
			})
		}

		// Running on the host — build image and start container.
		nodes := viper.GetInt("workload.nodes")
		cluster := viper.GetString("cluster.name")

		m, err := docker.NewManager()
		if err != nil {
			return err
		}
		defer m.Close()

		if err := m.BuildWorkloadImage(cmd.Context()); err != nil {
			return fmt.Errorf("build workload image: %w", err)
		}

		tenants := viper.GetInt("workload.tenants")
		if err := m.StartWorkload(cmd.Context(), docker.WorkloadOptions{
			Cluster:     cluster,
			Nodes:       nodes,
			Interval:    interval,
			BatchSize:   batch,
			QueryEvery:  queryEvery,
			TenantCount: tenants,
			MetricsPort: docker.WorkloadMetricsContainerPort,
		}); err != nil {
			return err
		}

		log.Info().
			Str("metrics", fmt.Sprintf("http://localhost:%d/metrics", docker.WorkloadMetricsContainerPort)).
			Msg("workload container started")
		return nil
	},
}

var workloadStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop and remove the workload container",
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := docker.NewManager()
		if err != nil {
			return err
		}
		defer m.Close()
		cluster := viper.GetString("cluster.name")
		return m.StopWorkload(cmd.Context(), cluster)
	},
}

var workloadLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List workload containers",
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := docker.NewManager()
		if err != nil {
			return err
		}
		defer m.Close()
		cluster := viper.GetString("cluster.name")
		containers, err := m.ListContainersByRole(cmd.Context(), cluster, docker.RoleWorkload)
		if err != nil {
			return err
		}
		if len(containers) == 0 {
			fmt.Println("No workload containers running.")
			return nil
		}
		fmt.Printf("%-20s  %-12s  %s\n", "NAME", "STATUS", "ID")
		for _, c := range containers {
			fmt.Printf("%-20s  %-12s  %s\n", c.Names[0], c.State, c.ID[:12])
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(workloadCmd)
	workloadCmd.AddCommand(workloadStartCmd, workloadStopCmd, workloadLsCmd)

	workloadCmd.PersistentFlags().String("cluster-name", docker.DefaultCluster, "cluster name")
	viper.BindPFlag("cluster.name", workloadCmd.PersistentFlags().Lookup("cluster-name"))

	workloadStartCmd.Flags().Duration("interval", 100*time.Millisecond, "tick interval between insert batches")
	workloadStartCmd.Flags().Int("batch", 1, "audit events inserted per tick")
	workloadStartCmd.Flags().Duration("query-every", 500*time.Millisecond, "how often to run read queries")
	workloadStartCmd.Flags().Int("nodes", 3, "number of cluster nodes (for host-side DSN)")
	workloadStartCmd.Flags().Int("tenants", 10, "number of tenants to seed (10, 25, or 50)")
	workloadStartCmd.Flags().String("metrics-addr", ":9091", "Prometheus metrics listen address")
	workloadStartCmd.Flags().String("grpc-addr", ":9092", "gRPC server listen address")
	workloadStartCmd.Flags().String("dsn", "", "explicit DSN (set automatically when running inside container)")
	workloadStartCmd.Flags().String("dsn-east", "", "DSN for us-east nodes only (set automatically inside container)")
	workloadStartCmd.Flags().String("dsn-west", "", "DSN for us-west nodes only (set automatically inside container)")
	workloadStartCmd.Flags().String("dsn-eu", "", "DSN for eu-central nodes only (set automatically inside container)")

	viper.BindPFlag("workload.interval", workloadStartCmd.Flags().Lookup("interval"))
	viper.BindPFlag("workload.batch", workloadStartCmd.Flags().Lookup("batch"))
	viper.BindPFlag("workload.query-every", workloadStartCmd.Flags().Lookup("query-every"))
	viper.BindPFlag("workload.nodes", workloadStartCmd.Flags().Lookup("nodes"))
	viper.BindPFlag("workload.tenants", workloadStartCmd.Flags().Lookup("tenants"))
	viper.BindPFlag("workload.metrics-addr", workloadStartCmd.Flags().Lookup("metrics-addr"))
	viper.BindPFlag("workload.grpc-addr", workloadStartCmd.Flags().Lookup("grpc-addr"))
	viper.BindPFlag("workload.dsn", workloadStartCmd.Flags().Lookup("dsn"))
	viper.BindPFlag("workload.dsn-east", workloadStartCmd.Flags().Lookup("dsn-east"))
	viper.BindPFlag("workload.dsn-west", workloadStartCmd.Flags().Lookup("dsn-west"))
	viper.BindPFlag("workload.dsn-eu", workloadStartCmd.Flags().Lookup("dsn-eu"))
}
