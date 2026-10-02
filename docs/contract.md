# Contract with gauger-server

gauger and [gauger-server](https://github.com/mach4-braai/gauger-server) share this contract. Change it in both repos together.

## Transport

- gauger joins the tailnet as an ephemeral `tag:gauger-ci` node and sends HTTP to `http://gauger-server:4318`. The ACL lets `tag:gauger-ci` reach only `tag:gauger-server:4318`.
- gauger calls `envknob.SetNoLogsNoSupport()` before constructing the `tsnet.Server`, so tsnet never uploads its own logs to `log.tailscale.com`.
- Every request carries `Authorization: Bearer <GitHub OIDC JWT>` with `aud` `gauger-server`. gauger fetches a new token at least 60 s before `exp`, so one job sends several tokens.

## Endpoints

| Method and path | Body | When |
|---|---|---|
| `POST /v1/jobs/start` | JSON lifecycle | Once, after the node joins. `time` is when sampling started, which can be earlier than the request. |
| `POST /v1/metrics` | OTLP/HTTP `ExportMetricsServiceRequest`, `Content-Type: application/x-protobuf` | One batch every 5 s, oldest first. Batches buffered while offline arrive late and in order. |
| `POST /v1/jobs/done` | JSON lifecycle | Once, after the final flush in the post step. |

gauger treats any 2xx as accepted. It retries 401, 403, 408, 429, 5xx (including the server's `503` for a job it does not know yet) and network errors on the next flush. It drops a batch the server rejects with another 4xx.

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

## Metrics

Sampled once a second. Sums are cumulative from the first sample of the job, so every job's counters start at 0.

| Metric | Type | Unit | Attributes |
|---|---|---|---|
| `system.cpu.utilization` | gauge, busy share of all CPUs since the previous sample: every mode except idle and iowait | `1` | none |
| `system.cpu.logical.count` | non-monotonic sum, once per batch | `{cpu}` | none |
| `system.memory.usage` | non-monotonic sum | `By` | `system.memory.state`: `used`, `free`, `buffers`, `cached` |
| `system.memory.limit` | non-monotonic sum, once per batch (`MemTotal`) | `By` | none |
| `system.linux.memory.available` | non-monotonic sum (`MemAvailable`) | `By` | none |
| `system.disk.io` | monotonic sum | `By` | `system.device`, `disk.io.direction`: `read`, `write` |
| `system.disk.operations` | monotonic sum | `{operation}` | `system.device`, `disk.io.direction` |
| `system.network.io` | monotonic sum | `By` | `network.interface.name`, `network.io.direction`: `receive`, `transmit` |

- `used` memory is `MemTotal - MemFree - Buffers - Cached - SReclaimable`, and `cached` includes `SReclaimable`, so the four states add up to `MemTotal`. For peak memory against `MemTotal`, use `MemTotal - system.linux.memory.available`.
- Disks are whole disks from `/sys/block`, leaving out loop and RAM devices. Interfaces are the ones backed by a device, which leaves out `lo`, `docker0` and veth pairs.

## Fallback artifact

- Name: `gauger-<check_run_id>`, or `gauger-<runner name>` with unsafe characters replaced by `-` when the check run ID is empty.
- Contents: the unsent batches, one `ExportMetricsServiceRequest` per file, named `<sequence>.pb` in send order.
- Retention: 7 days.
