package grafana

import "embed"

//go:embed datasource.yml dashboards.yml crdb_overview.json workload.json
var FS embed.FS
