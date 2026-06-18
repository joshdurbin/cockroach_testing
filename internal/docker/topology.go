package docker

// ClusterMode identifies which installation variant was used to create the cluster.
type ClusterMode string

const (
	ModeMultiGeo     ClusterMode = "multi-geo"
	ModeSingleRegion ClusterMode = "single-region"

	LabelClusterMode = LabelPrefix + "cluster-mode"
)

// ClusterTopology describes the region and node layout of a CockroachDB cluster.
type ClusterTopology struct {
	Mode    ClusterMode
	Regions []string // geographic regions assigned to nodes in cyclic order
	Nodes   int
}

// MultiGeoTopology returns the standard 9-node, 3-region topology used for
// geo-partition and chaos demos.
func MultiGeoTopology(nodes int) ClusterTopology {
	return ClusterTopology{
		Mode:    ModeMultiGeo,
		Regions: []string{"us-east", "us-west", "eu-central"},
		Nodes:   nodes,
	}
}

// SingleRegionTopology returns a 3-node cluster confined to us-west.
// All nodes share the same locality tag so replicas stay local.
func SingleRegionTopology() ClusterTopology {
	return ClusterTopology{
		Mode:    ModeSingleRegion,
		Regions: []string{"us-west"},
		Nodes:   3,
	}
}

// NodeRegion returns the geographic region label for node idx (1-based).
// Cycles through the topology's regions for any cluster size.
func (t ClusterTopology) NodeRegion(idx int) string {
	return t.Regions[(idx-1)%len(t.Regions)]
}
