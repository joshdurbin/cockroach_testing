package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/joshdurbin/cockroach_testing/internal/docker"
)

var obsCmd = &cobra.Command{
	Use:   "obs",
	Short: "Manage the observability stack (Prometheus + Grafana)",
}

var obsSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Start Prometheus and Grafana wired to the cluster",
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := docker.NewManager()
		if err != nil {
			return err
		}
		defer m.Close()

		cluster := viper.GetString("cluster.name")
		nodeCount := viper.GetInt("obs.nodes")

		nodes, err := m.GetClusterNodes(cmd.Context(), cluster)
		if err != nil {
			return err
		}
		if len(nodes) == 0 {
			// Build synthetic NodeInfo for the requested count when cluster may
			// not yet be running (e.g. called before cluster create).
			nodes = make([]docker.NodeInfo, nodeCount)
			for i := range nodeCount {
				nodes[i] = docker.NodeInfo{
					Index:    i + 1,
					HTTPPort: docker.NodeHTTPPort(i + 1),
				}
			}
		}

		if err := m.SetupObs(cmd.Context(), cluster, nodes); err != nil {
			return err
		}

		fmt.Printf("\nObservability stack ready:\n")
		fmt.Printf("  Prometheus : http://localhost:%d\n", docker.PrometheusPort)
		fmt.Printf("  Grafana    : http://localhost:%d\n\n", docker.GrafanaPort)
		fmt.Printf("CockroachDB Admin UI (cluster-wide):\n")
		for i := range nodeCount {
			fmt.Printf("  Node %d: http://localhost:%d\n", i+1, docker.NodeHTTPPort(i+1))
		}
		fmt.Println()
		return nil
	},
}

var obsTeardownCmd = &cobra.Command{
	Use:   "teardown",
	Short: "Stop Prometheus and Grafana",
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := docker.NewManager()
		if err != nil {
			return err
		}
		defer m.Close()
		cluster := viper.GetString("cluster.name")
		return m.TeardownObs(cmd.Context(), cluster)
	},
}

func init() {
	rootCmd.AddCommand(obsCmd)
	obsCmd.AddCommand(obsSetupCmd, obsTeardownCmd)

	obsSetupCmd.Flags().Int("nodes", 3, "number of cluster nodes to scrape")
	viper.BindPFlag("obs.nodes", obsSetupCmd.Flags().Lookup("nodes"))

	obsSetupCmd.Flags().String("cluster-name", docker.DefaultCluster, "cluster name")
	viper.BindPFlag("cluster.name", obsSetupCmd.Flags().Lookup("cluster-name"))
}
