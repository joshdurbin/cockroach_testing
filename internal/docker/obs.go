package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"text/template"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/go-connections/nat"
	"github.com/rs/zerolog/log"

	"github.com/joshdurbin/cockroach_testing/internal/docker/grafana"
)

const (
	PrometheusImage     = "prom/prometheus:latest"
	GrafanaImage        = "grafana/grafana:latest"
	prometheusContainer = "cdbct-prometheus"
	grafanaContainer    = "cdbct-grafana"
	PrometheusPort      = 9090
	GrafanaPort         = 3000
	WorkloadMetricsPort = 9091
)

// SetupObs starts Prometheus and Grafana wired to the given CockroachDB nodes.
func (m *Manager) SetupObs(ctx context.Context, cluster string, nodes []NodeInfo) error {
	for _, img := range []string{PrometheusImage, GrafanaImage} {
		if err := m.PullImage(ctx, img); err != nil {
			return err
		}
	}
	if err := m.startPrometheus(ctx, cluster, nodes); err != nil {
		return fmt.Errorf("prometheus: %w", err)
	}
	if err := m.startGrafana(ctx, cluster); err != nil {
		return fmt.Errorf("grafana: %w", err)
	}
	log.Info().
		Str("prometheus", fmt.Sprintf("http://localhost:%d", PrometheusPort)).
		Str("grafana", fmt.Sprintf("http://localhost:%d", GrafanaPort)).
		Msg("observability stack ready")
	return nil
}

func (m *Manager) startPrometheus(ctx context.Context, cluster string, nodes []NodeInfo) error {
	volName := "cdbct-prometheus-data"
	if err := m.EnsureVolume(ctx, volName); err != nil {
		return err
	}

	portBindings := nat.PortMap{
		nat.Port(fmt.Sprintf("%d/tcp", PrometheusPort)): []nat.PortBinding{
			{HostIP: "0.0.0.0", HostPort: strconv.Itoa(PrometheusPort)},
		},
	}

	resp, err := m.client.ContainerCreate(ctx,
		&container.Config{
			Image:    PrometheusImage,
			Labels:   managedLabels(cluster, RolePrometheus, ""),
			Hostname: prometheusContainer,
			Cmd: []string{
				"--config.file=/etc/prometheus/prometheus.yml",
				"--storage.tsdb.path=/prometheus",
				"--web.enable-lifecycle",
			},
		},
		&container.HostConfig{
			PortBindings:  portBindings,
			RestartPolicy: container.RestartPolicy{Name: "on-failure", MaximumRetryCount: 3},
			Mounts: []mount.Mount{
				{Type: mount.TypeVolume, Source: volName, Target: "/prometheus"},
			},
		},
		networkConfig(prometheusContainer),
		nil,
		prometheusContainer,
	)
	if err != nil {
		return fmt.Errorf("container create: %w", err)
	}

	promCfg, err := renderPromConfig(nodes)
	if err != nil {
		return fmt.Errorf("render prometheus config: %w", err)
	}
	if err := m.copyFileToContainer(ctx, resp.ID, "/etc/prometheus/", "prometheus.yml", []byte(promCfg)); err != nil {
		return fmt.Errorf("copy prometheus config: %w", err)
	}

	if err := m.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("container start: %w", err)
	}
	log.Info().Str("container", prometheusContainer).Msg("prometheus started")
	return nil
}

func (m *Manager) startGrafana(ctx context.Context, cluster string) error {
	volName := "cdbct-grafana-data"
	if err := m.EnsureVolume(ctx, volName); err != nil {
		return err
	}

	portBindings := nat.PortMap{
		nat.Port(fmt.Sprintf("%d/tcp", GrafanaPort)): []nat.PortBinding{
			{HostIP: "0.0.0.0", HostPort: strconv.Itoa(GrafanaPort)},
		},
	}

	resp, err := m.client.ContainerCreate(ctx,
		&container.Config{
			Image:    GrafanaImage,
			Labels:   managedLabels(cluster, RoleGrafana, ""),
			Hostname: grafanaContainer,
			Env: []string{
				// Anonymous admin access — appropriate for local dev only.
				"GF_AUTH_ANONYMOUS_ENABLED=true",
				"GF_AUTH_ANONYMOUS_ORG_ROLE=Admin",
				"GF_AUTH_DISABLE_LOGIN_FORM=true",
				"GF_ANALYTICS_REPORTING_ENABLED=false",
				"GF_ANALYTICS_CHECK_FOR_UPDATES=false",
				"GF_PATHS_PROVISIONING=/etc/grafana/provisioning",
			},
		},
		&container.HostConfig{
			PortBindings:  portBindings,
			RestartPolicy: container.RestartPolicy{Name: "on-failure", MaximumRetryCount: 3},
			Mounts: []mount.Mount{
				{Type: mount.TypeVolume, Source: volName, Target: "/var/lib/grafana"},
			},
		},
		networkConfig(grafanaContainer),
		nil,
		grafanaContainer,
	)
	if err != nil {
		return fmt.Errorf("container create: %w", err)
	}

	// Inject all provisioning files from the embedded FS.
	// All files land in paths that exist in the Grafana base image.
	// Dashboard JSONs go alongside dashboards.yml so the provider finds them
	// without needing any extra directory creation.
	provisionFiles := map[string]string{
		"datasource.yml":     "/etc/grafana/provisioning/datasources/",
		"dashboards.yml":     "/etc/grafana/provisioning/dashboards/",
		"crdb_overview.json": "/etc/grafana/provisioning/dashboards/",
		"workload.json":      "/etc/grafana/provisioning/dashboards/",
	}
	for filename, dir := range provisionFiles {
		content, err := grafana.FS.ReadFile(filename)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", filename, err)
		}
		if err := m.copyFileToContainer(ctx, resp.ID, dir, filename, content); err != nil {
			return fmt.Errorf("copy %s: %w", filename, err)
		}
	}

	if err := m.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("container start: %w", err)
	}
	log.Info().Str("container", grafanaContainer).Msg("grafana started")
	return nil
}

// copyFileToContainer injects a single file into a container via Docker's tar API.
func (m *Manager) copyFileToContainer(ctx context.Context, containerID, dir, filename string, content []byte) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     filename,
		Mode:     0644,
		Size:     int64(len(content)),
		ModTime:  time.Now(),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	if _, err := tw.Write(content); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return m.client.CopyToContainer(ctx, containerID, dir, &buf, container.CopyToContainerOptions{})
}

// Prometheus scrape config.
// CockroachDB nodes and Toxiproxy are addressed by container hostname on the
// shared cdbct-net Docker network. Node HTTP port is always 8080 internally.
var promConfigTmpl = template.Must(template.New("prom").Parse(`global:
  scrape_interval: 10s
  evaluation_interval: 10s

scrape_configs:
  - job_name: cockroachdb
    metrics_path: /_status/vars
    scheme: http
    static_configs:
      - targets:
{{- range .Nodes}}
        - cdbct-crdb-{{.Index}}:8080
{{- end}}
    relabel_configs:
      - source_labels: [__address__]
        target_label: instance

  - job_name: cdbct_workload
    metrics_path: /metrics
    static_configs:
      - targets:
        - cdbct-workload:{{.WorkloadPort}}
`))

func renderPromConfig(nodes []NodeInfo) (string, error) {
	var buf bytes.Buffer
	err := promConfigTmpl.Execute(&buf, map[string]any{
		"Nodes":        nodes,
		"WorkloadPort": WorkloadMetricsPort,
	})
	return buf.String(), err
}

// TeardownObs removes Prometheus and Grafana containers and their volumes.
func (m *Manager) TeardownObs(ctx context.Context, cluster string) error {
	for _, role := range []string{RolePrometheus, RoleGrafana} {
		containers, err := m.ListContainersByRole(ctx, cluster, role)
		if err != nil {
			return err
		}
		for _, c := range containers {
			log.Info().Str("name", c.Names[0]).Msg("removing obs container")
			_ = m.StopAndRemove(ctx, c.ID)
		}
	}
	for _, vol := range []string{"cdbct-prometheus-data", "cdbct-grafana-data"} {
		_ = m.RemoveVolume(ctx, vol)
	}
	return nil
}

var _ io.Reader = (*bytes.Buffer)(nil)
