---
name: user-profile
description: "User's role, preferences, and working style for this project"
metadata: 
  node_type: memory
  type: user
  originSessionId: 3985f969-5856-416c-bbcb-d5b4c64ba8e9
---

Senior engineer, comfortable with Go, Docker, distributed systems, and CockroachDB concepts. Architectural thinking — prefers designs that are correct and idiomatic over quick patches.

**Style preferences:**
- No CLI subcommand trees for operational/data concerns — expose those via API (gRPC preferred over REST)
- No shelling out to Docker; use the Go Docker SDK for everything
- Follow russ (github.com/joshdurbin/russ) conventions: cobra/viper/zerolog/docker-sdk/pond/gofakeit
- Defaults should reflect the realistic production-like scenario (e.g. 9 nodes, not 3)
- Destroy should be destructive by default; preservation is opt-in (`--retain-data-volumes`)
- Workload should run as a container, not in-process in the CLI

**Feedback patterns:**
- Will call out when command syntax is stale in help text / quickstart output
- Pushes back on application-side caching when the DB model can solve it natively
- Wants demos that show real failure modes, not synthetic ones
- Prefers the PostgreSQL wire protocol for demoing DB features (shows portability)
