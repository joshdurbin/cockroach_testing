---
name: go-docker-sdk-patterns
description: Non-obvious Docker SDK (docker/docker v27) patterns learned building cdbct
metadata: 
  node_type: memory
  type: project
  originSessionId: 3985f969-5856-416c-bbcb-d5b4c64ba8e9
---

## CopyToContainer beats busybox sidecars for config injection

The original obs.go used a busybox container to write prometheus.yml into a volume via a shell command. This was fragile (shell escaping, blocking ContainerWait, restart loops when it failed). The correct pattern:

```go
func copyFileToContainer(ctx context.Context, client *dockerclient.Client, containerID, dir, filename string, content []byte) error {
    var buf bytes.Buffer
    tw := tar.NewWriter(&buf)
    tw.WriteHeader(&tar.Header{Name: filename, Mode: 0644, Size: int64(len(content)), Typeflag: tar.TypeReg})
    tw.Write(content)
    tw.Close()
    return client.CopyToContainer(ctx, containerID, dir, &buf, container.CopyToContainerOptions{})
}
```

- Works on stopped containers (create, inject config, then start)
- Destination dir must exist in the image — `/etc/grafana/provisioning/dashboards/` exists; `/etc/grafana/dashboards/` does not
- The tar filename becomes the file in the container; the dir is where it's extracted

## stdcopy.StdCopy for exec output demux

Docker's exec attach response uses a multiplexed stream (8-byte frame headers for stdout/stderr). Bare `io.Copy` reads the raw frames including binary headers, producing garbage output. Always use stdcopy:

```go
import "github.com/docker/docker/pkg/stdcopy"

resp, _ := client.ContainerExecAttach(ctx, execID, container.ExecAttachOptions{})
defer resp.Close()
var stdout, stderr strings.Builder
stdcopy.StdCopy(&stdout, &stderr, resp.Reader)
```

## ContainerWait returns two channels, not one

In Docker SDK v27, `ContainerWait` returns `(resultC <-chan container.WaitResponse, errC <-chan error)`. A common mistake is assigning it to a single value:

```go
// WRONG — compile error or runtime panic
result, err := client.ContainerWait(ctx, id, condition)

// CORRECT
resultC, errC := client.ContainerWait(ctx, id, container.WaitConditionNotRunning)
select {
case <-resultC:
    // container exited
case err := <-errC:
    return err
}
```

## Building an image from source tar

To build the workload image from Go source without shelling out to `docker build`:

```go
resp, err := client.ImageBuild(ctx, tarReader, types.ImageBuildOptions{
    Tags:        []string{"cdbct-workload:latest"},
    Remove:      true,
    ForceRemove: true,
    Dockerfile:  "Dockerfile",
})
// Stream build output (JSON lines)
dec := json.NewDecoder(resp.Body)
for {
    var msg struct { Stream string `json:"stream"`; Error string `json:"error"` }
    if err := dec.Decode(&msg); err == io.EOF { break }
    if msg.Error != "" { return fmt.Errorf("%s", msg.Error) }
}
```

Build context is a tar stream containing all files. Create it by walking the source tree:
- Add `Dockerfile` first
- Add `go.mod`, `go.sum`, `main.go`
- Recursively add `cmd/` and `internal/` directories

## Label-based container discovery

All managed containers get labels at creation time. Discovery uses filter API — no container name parsing:

```go
const LabelManaged = "cdbct.managed"
const LabelRole    = "cdbct.role"
const LabelCluster = "cdbct.cluster"

// At create time
Labels: map[string]string{
    LabelManaged: "true",
    LabelRole:    "crdb",
    LabelCluster: clusterName,
}

// At lookup time
f := filters.NewArgs(
    filters.Arg("label", LabelManaged+"=true"),
    filters.Arg("label", LabelCluster+"="+cluster),
    filters.Arg("label", LabelRole+"=crdb"),
)
client.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
```

## image.PullOptions rename in Docker SDK v27+

`types.ImagePullOptions` was moved to `image.PullOptions`:

```go
import imagetypes "github.com/docker/docker/api/types/image"

rc, err := client.ImagePull(ctx, imageRef, imagetypes.PullOptions{})
```

## Network attachment at container creation

Pass a `NetworkingConfig` to `ContainerCreate` to attach the container to a named network with a DNS alias:

```go
func networkConfig(alias string) *network.NetworkingConfig {
    return &network.NetworkingConfig{
        EndpointsConfig: map[string]*network.EndpointSettings{
            "cdbct-net": {Aliases: []string{alias}},
        },
    }
}
// Pass as 3rd argument to ContainerCreate
```

Container DNS name `cdbct-crdb-1` resolves to its IP from any other container on `cdbct-net`.

## DestroyCluster volume deletion bug pattern

When removing containers and then trying to find volumes to delete, the volume lookup fails because it queries container labels — but the containers are already gone.

**Always collect volume names from container labels BEFORE removing containers:**

```go
// WRONG — containers are gone by the time you query for volumes
for _, role := range []string{RoleCRDB} {
    containers, _ := m.ListContainersByRole(ctx, cluster, role)
    for _, c := range containers { m.StopAndRemove(ctx, c.ID) }
}
// This returns empty — containers already deleted
nodes, _ := m.ListContainersByRole(ctx, cluster, RoleCRDB)
for _, c := range nodes { m.RemoveVolume(ctx, volName) }

// CORRECT — collect first, then remove
var volumesToDelete []string
nodes, _ := m.ListContainersByRole(ctx, cluster, RoleCRDB)
for _, c := range nodes {
    if idx, ok := c.Labels[LabelNodeIdx]; ok {
        volumesToDelete = append(volumesToDelete, fmt.Sprintf("cdbct-crdb-%s-data", idx))
    }
}
// Now remove containers
for _, role := range []string{RoleCRDB} { ... }
// Then delete volumes
for _, vol := range volumesToDelete { m.RemoveVolume(ctx, vol) }
```

This bug caused `cdbct destroy` to leave all data volumes intact for the entire project history.

## Docker SDK module split in v28+

Docker v28 split into separate modules (`github.com/moby/moby/api`, `github.com/docker/docker/client`). Stay on v27 (`github.com/docker/docker v27.1.2+incompatible`) for the monolithic module that matches russ conventions. The v28 split causes module path resolution errors with `go mod tidy`.
