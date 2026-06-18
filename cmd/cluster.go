package cmd

import (
	"fmt"
	"strconv"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/joshdurbin/cockroach_testing/internal/docker"
)

var clusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "Manage the CockroachDB cluster",
}

var clusterCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Stand up a new CockroachDB cluster",
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := docker.NewManager()
		if err != nil {
			return err
		}
		defer m.Close()

		nodes := viper.GetInt("cluster.nodes")
		cluster := viper.GetString("cluster.name")
		mode := viper.GetString("cluster.mode")

		var topo docker.ClusterTopology
		switch docker.ClusterMode(mode) {
		case docker.ModeSingleRegion:
			topo = docker.SingleRegionTopology()
			if cmd.Flags().Changed("nodes") {
				topo.Nodes = nodes
			}
		default:
			topo = docker.MultiGeoTopology(nodes)
		}

		log.Info().Str("cluster", cluster).Int("nodes", topo.Nodes).Str("mode", string(topo.Mode)).Msg("creating cluster")

		infos, err := m.CreateCluster(cmd.Context(), cluster, topo)
		if err != nil {
			return err
		}

		fmt.Printf("\nCluster %q ready (%d nodes, %s)\n\n", cluster, len(infos), topo.Mode)
		fmt.Printf("  %-8s  %-22s  %s\n", "NODE", "SQL", "ADMIN UI")
		for _, n := range infos {
			fmt.Printf("  %-8d  localhost:%-12d  http://localhost:%d\n",
				n.Index, n.SQLPort, n.HTTPPort)
		}
		fmt.Println()
		return nil
	},
}

var clusterScaleCmd = &cobra.Command{
	Use:   "scale",
	Short: "Add a node to the running cluster",
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := docker.NewManager()
		if err != nil {
			return err
		}
		defer m.Close()

		cluster := viper.GetString("cluster.name")
		info, err := m.AddNode(cmd.Context(), cluster)
		if err != nil {
			return err
		}
		log.Info().Int("node", info.Index).Str("sql", info.SQLAddr).Msg("node joined cluster")
		fmt.Printf("Node %d added: SQL localhost:%d  Admin UI http://localhost:%d\n",
			info.Index, info.SQLPort, info.HTTPPort)
		return nil
	},
}

var clusterLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List cluster nodes",
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := docker.NewManager()
		if err != nil {
			return err
		}
		defer m.Close()

		cluster := viper.GetString("cluster.name")
		nodes, err := m.GetClusterNodes(cmd.Context(), cluster)
		if err != nil {
			return err
		}
		if len(nodes) == 0 {
			fmt.Printf("No nodes found in cluster %q\n", cluster)
			return nil
		}
		fmt.Printf("%-8s  %-22s  %-30s  %s\n", "NODE", "CONTAINER", "SQL", "ADMIN UI")
		for _, n := range nodes {
			fmt.Printf("%-8s  %-22s  localhost:%-12d  http://localhost:%d\n",
				strconv.Itoa(n.Index), n.Name, n.SQLPort, n.HTTPPort)
		}
		return nil
	},
}

var clusterRmCmd = &cobra.Command{
	Use:   "rm",
	Short: "Stop and remove the cluster",
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := docker.NewManager()
		if err != nil {
			return err
		}
		defer m.Close()

		cluster := viper.GetString("cluster.name")
		purge := viper.GetBool("cluster.purge")

		log.Info().Str("cluster", cluster).Bool("purge", purge).Msg("destroying cluster")
		if err := m.DestroyCluster(cmd.Context(), cluster, purge); err != nil {
			return err
		}
		if err := m.TeardownObs(cmd.Context(), cluster); err != nil {
			log.Warn().Err(err).Msg("obs teardown")
		}
		if purge {
			_ = m.RemoveNetwork(cmd.Context())
		}
		fmt.Printf("Cluster %q removed\n", cluster)
		return nil
	},
}

var clusterStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show cluster health",
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := docker.NewManager()
		if err != nil {
			return err
		}
		defer m.Close()

		cluster := viper.GetString("cluster.name")
		nodes, err := m.GetClusterNodes(cmd.Context(), cluster)
		if err != nil {
			return err
		}
		if len(nodes) == 0 {
			fmt.Printf("Cluster %q: no nodes running\n", cluster)
			return nil
		}

		fmt.Printf("Cluster %q: %d node(s)\n\n", cluster, len(nodes))

		// Run `cockroach node status` inside node 1 using the container-internal
		// SQL port (always 26257 inside the container — n.SQLPort is the host-side
		// mapped port and doesn't exist inside the container).
		// `cockroach node status` is cluster-aware: one exec shows all nodes.
		out, code, err := m.Exec(cmd.Context(), nodes[0].ContainerID, []string{
			"cockroach", "node", "status", "--insecure",
			fmt.Sprintf("--host=localhost:%d", docker.CRDBInternalSQLPort),
			"--format=table",
		})
		if err != nil || code != 0 {
			fmt.Printf("  cluster unreachable: %v\n%s\n", err, out)
			return nil
		}
		fmt.Println(out)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(clusterCmd)
	clusterCmd.AddCommand(clusterCreateCmd, clusterScaleCmd, clusterLsCmd, clusterRmCmd, clusterStatusCmd)

	clusterCreateCmd.Flags().IntP("nodes", "n", 9, "number of CockroachDB nodes (9 = 3 per region, geo-partitions survive single-node failure)")
	clusterCreateCmd.Flags().String("mode", "multi-geo", "cluster mode: multi-geo (3-region) or single-region (us-west)")
	viper.BindPFlag("cluster.nodes", clusterCreateCmd.Flags().Lookup("nodes"))
	viper.BindPFlag("cluster.mode", clusterCreateCmd.Flags().Lookup("mode"))

	clusterRmCmd.Flags().Bool("purge", false, "also delete data volumes")
	viper.BindPFlag("cluster.purge", clusterRmCmd.Flags().Lookup("purge"))

	// Shared cluster name flag on the parent so all sub-commands inherit it.
	clusterCmd.PersistentFlags().String("name", docker.DefaultCluster, "cluster name")
	viper.BindPFlag("cluster.name", clusterCmd.PersistentFlags().Lookup("name"))
}
