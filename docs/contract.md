# Contract with gauger-server

gauger and [gauger-server](https://github.com/mach4-braai/gauger-server) share this contract. Change it in both repos together.

## Transport

- gauger sends HTTPS to gauger-server's public runner listener, `https://<host>.<tailnet>.ts.net:10000`, a Tailscale Funnel port. The action's default is `https://gauger-server.taila8b8af.ts.net:10000`. gauger does not join the tailnet.
- Every request carries `Authorization: Bearer <GitHub OIDC JWT>` with `aud` `gauger-server`, and the token is the only credential. gauger fetches a new token at least 60 s before `exp`, so one job sends several tokens.
- The identity attributes in every body must name the job the token was issued to: its `repository`, `run_id`, `run_attempt` and `check_run_id` claims. A mismatch gets `403`.
- A job or client address over its rate gets `429` with `Retry-After`.

## Endpoints

| Method and path | Body | When |
|---|---|---|
| `POST /v1/jobs/start` | JSON lifecycle | Once, when gauger starts. `time` is when sampling started, which can be earlier than the request. |
| `POST /v1/metrics` | OTLP/HTTP `ExportMetricsServiceRequest`, `Content-Type: application/x-protobuf` | One batch every 5 s, oldest first. Batches buffered while offline arrive late and in order. |
| `POST /v1/jobs/done` | JSON lifecycle | Once, after the final flush in the post step. |

gauger treats any 2xx as accepted. It retries 401, 403, 408, 429, 5xx (including the server's `503` for a job it does not know yet) and network errors on the next flush, without reading `Retry-After`. It drops a batch the server rejects with another 4xx.

### Lifecycle body

One flat JSON object: the identity attributes, `time` in RFC 3339, and on `done` the batch counts.

```json
{
  "github.run_id": "18239012345",
  "github.run_attempt": "1",
  "github.check_run_id": "51837460912",
  "github.repository": "mach4-braai/gauger",
  "github.workflow": "CI",
  "github.job": "build",
  "runner.name": "GitHub Actions 1000000123",
  "gauger.metrics.scope": "runner",
  "time": "2026-09-30T15:00:00.123Z",
  "unsent_batches": 0,
  "dropped_batches": 0
}
```

A non-zero `unsent_batches` on `done` means those batches are in the fallback artifact.

## Identity attributes

The same keys and values are the OTLP resource attributes of every batch.

| Key | Source |
|---|---|
| `github.run_id` | `GITHUB_RUN_ID` |
| `github.run_attempt` | `GITHUB_RUN_ATTEMPT` |
| `github.check_run_id` | action input `check-run-id`, default `${{ job.check_run_id }}`, which equals the REST job ID |
| `github.repository` | `GITHUB_REPOSITORY`, as `owner/name` |
| `github.workflow` | `GITHUB_WORKFLOW` |
| `github.job` | `GITHUB_JOB`, the job's ID in the workflow file |
| `runner.name` | `RUNNER_NAME` |

- These are the spec's names as written, not OTel CI/CD semantic-convention names, because gauger-server parses them this way.
- An empty value is left out. When `github.check_run_id` is missing, gauger-server matches the job on `runner.name`.
- `gauger.metrics.scope` is always `runner`. Every value is the whole runner's usage, not one step's own usage.
- Batches also carry `service.name` `gauger`, `service.version` and `os.type` `linux`.

## Runner resource attributes

Batches also carry these as OTLP resource attributes. They describe the machine, not the job, so they are not part of the lifecycle body, and gauger-server has nowhere to store them per job yet: it ignores them until it does.

| Key | Source |
|---|---|
| `host.cpu.model.name` | `/proc/cpuinfo` `model name`, or on arm64, which has no `model name` line, `CPU implementer` and `CPU part` |
| `os.image` | `ImageOS` |
| `github.runner.image_version` | `ImageVersion` |
| `github.runner.environment` | `RUNNER_ENVIRONMENT` |

An empty value is left out, same as the identity attributes.

## Metrics

Sampled once a second. Sums are cumulative from the first sample of the job, so every job's counters start at 0.

| Metric | Type | Unit | Attributes |
|---|---|---|---|
| `system.cpu.utilization` | gauge, busy share of all CPUs since the previous sample: every mode except idle and iowait | `1` | none |
| `system.cpu.time` | monotonic sum, cumulative seconds since the first sample | `s` | `cpu.mode`: `user`, `nice`, `system`, `idle`, `iowait`, `interrupt`, `softirq`, `steal` |
| `system.process.count` | gauge | `{process}` | `process.state`: `running`, `blocked` |
| `system.cpu.logical.count` | non-monotonic sum, once per batch | `{cpu}` | none |
| `system.memory.usage` | non-monotonic sum | `By` | `system.memory.state`: `used`, `free`, `buffers`, `cached` |
| `system.memory.limit` | non-monotonic sum, once per batch (`MemTotal`) | `By` | none |
| `system.linux.memory.available` | non-monotonic sum (`MemAvailable`) | `By` | none |
| `system.paging.usage` | non-monotonic sum, left out when `SwapTotal` is 0 | `By` | `system.paging.state`: `used`, `free` |
| `system.disk.io` | monotonic sum | `By` | `system.device`, `disk.io.direction`: `read`, `write` |
| `system.disk.operations` | monotonic sum | `{operation}` | `system.device`, `disk.io.direction` |
| `system.network.io` | monotonic sum | `By` | `network.interface.name`, `network.io.direction`: `receive`, `transmit` |
| `system.linux.pressure.stall.time` | monotonic sum | `us` | `system.pressure.resource`: `cpu`, `memory`, `io`; `system.pressure.type`: `some`, `full` |
| `system.filesystem.usage` | non-monotonic sum | `By` | `system.filesystem.mountpoint`, `system.filesystem.state`: `used`, `free` |
| `container.cpu.time` | monotonic sum, cumulative since the container's first sample (cgroup v2 `cpu.stat` `usage_usec`) | `s` | `container.id`, `container.image.name` (when known) |
| `container.memory.usage` | non-monotonic sum (cgroup v2 `memory.current`) | `By` | `container.id`, `container.image.name` (when known) |
| `process.cpu.time` | gauge, CPU seconds accrued since the previous walk, only on samples that walk `/proc/<pid>`, every 5th sample at the default rate | `s` | `process.executable.name` |
| `process.memory.usage` | gauge, only on samples that walk `/proc/<pid>` | `By` | `process.executable.name` |

- `used` memory is `MemTotal - MemFree - Buffers - Cached - SReclaimable`, and `cached` includes `SReclaimable`, so the four states add up to `MemTotal`. For peak memory against `MemTotal`, use `MemTotal - system.linux.memory.available`.
- `used` swap is `SwapTotal - SwapFree`.
- Disks are whole disks from `/sys/block`, leaving out loop and RAM devices. Interfaces are the ones backed by a device, which leaves out `lo`, `docker0` and veth pairs, and leaves out any interface with a `/sys/class/net/<name>/master` link, such as a virtual function enslaved to a netvsc interface on Azure.
- Container metrics come from cgroup v2 cgroups at `/sys/fs/cgroup/system.slice/docker-<id>.scope`, one series per container running directly on the runner (service containers, `docker run`, `container:` jobs). A runner without Docker, or without the cgroup v2 unified hierarchy, sends none. `container.image.name` is present only when gauger can reach the Docker Engine API's Unix socket without root; otherwise a container still reports with `container.id` alone.
- Filesystem usage covers the filesystem holding `/` and `$GITHUB_WORKSPACE`, once each when they're the same filesystem. A failing `statfs` leaves the metric out of that sample rather than failing it.
- Pressure stall time comes from `/proc/pressure/{cpu,memory,io}`: the kernel's own cumulative microsecond counters since boot, but gauger counts from the job's first sample like every other sum here, not from boot. A resource is left out of the batch entirely when its file is missing, which happens on kernels built without `CONFIG_PSI`. Current kernels write a `full` line for `cpu` that always reads zero, since a stall of every runnable task also stalls the thing that would resume them; older kernels omit that line instead. Either way, gauger only sends `system.pressure.type=full` for `cpu` when the kernel's file has the line.
- `process.cpu.time` and `process.memory.usage` name only the top 5 processes by CPU time accrued since the previous walk, and the top 5 by current RSS, with no PID and no command line, so the series stay bounded and never leak arguments. gauger's first walk only records CPU baselines, since there's no previous walk to diff against, so that walk sends no `process.cpu.time` points; `process.memory.usage` is unaffected, since RSS is a current reading, not a diff. A PID gauger sees for the first time on a later walk is assumed to have started after the previous walk, so its lifetime CPU time is also its time since then, and its point uses that total. Several points can share the same `process.executable.name` in the same batch, and the same name can belong to a different process from one walk to the next: gauger-server must treat each point as a standalone observation, not diff or sum them across samples the way it does for the system-wide sums above.

## Fallback artifact

- Name: `gauger-<check_run_id>`, or `gauger-<runner name>` with unsafe characters replaced by `-` when the check run ID is empty.
- Contents: the unsent batches, one `ExportMetricsServiceRequest` per file, named `<sequence>.pb` in send order.
- Retention: 7 days.
