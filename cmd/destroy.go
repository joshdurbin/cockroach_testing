package cmd

import (
	"fmt"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/joshdurbin/cockroach_testing/internal/docker"
)

var destroyCmd = &cobra.Command{
	Use:   "destroy",
	Short: "Stop and remove everything: cluster, Toxiproxy, obs stack, and volumes",
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := docker.NewManager()
		if err != nil {
			return err
		}
		defer m.Close()

		cluster := viper.GetString("cluster.name")
		retainData := viper.GetBool("destroy.retain-data-volumes")
		purge := !retainData

		log.Info().Str("cluster", cluster).Bool("purge_volumes", purge).Msg("destroying environment")

		if err := m.StopWorkload(cmd.Context(), cluster); err != nil {
			log.Warn().Err(err).Msg("workload teardown error")
		}
		if err := m.DestroyCluster(cmd.Context(), cluster, purge); err != nil {
			log.Warn().Err(err).Msg("cluster destroy error")
		}
		if err := m.TeardownObs(cmd.Context(), cluster); err != nil {
			log.Warn().Err(err).Msg("obs teardown error")
		}
		if err := m.RemoveNetwork(cmd.Context()); err != nil {
			log.Warn().Err(err).Msg("network remove error")
		}

		if retainData {
			fmt.Printf("Environment %q destroyed (data volumes retained).\n", cluster)
		} else {
			fmt.Printf("Environment %q destroyed.\n", cluster)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(destroyCmd)

	destroyCmd.Flags().String("name", docker.DefaultCluster, "cluster name")
	destroyCmd.Flags().Bool("retain-data-volumes", false, "keep CockroachDB data volumes (migrations and tenant pool will be skipped on next start)")
	viper.BindPFlag("cluster.name", destroyCmd.Flags().Lookup("name"))
	viper.BindPFlag("destroy.retain-data-volumes", destroyCmd.Flags().Lookup("retain-data-volumes"))
}
