# gauger spec

A GitHub Action that samples runner metrics during a job and streams them to [gauger-server](https://github.com/mach4-braai/gauger-server) over Tailscale.

## Shape

- **JavaScript action.** `runs.using: node24`, with a `main.js` and a `post.js`. Leave `post-if` at its default, `always()`.
- **`main.js`.**
  - Downloads the gauger binary for the runner's OS and arch from this repo's release, and checks its sha256.
  - Starts the binary detached, then returns.
  - Records the binary's PID for `post` in `$GITHUB_STATE`.
- **`post.js`.**
  - Signals the binary to flush and log out, then waits at most 30 s.
  - If the upload failed, writes the fallback artifact. That needs `@actions/artifact`, so bundle it (`ncc`).
- **Go binary.**
  - Samples `/proc` once a second: CPU, memory, disk and network, plus `nproc` and `MemTotal`.
  - Adds the job's identity to every record (see the contract).
  - Streams OTLP/HTTP protobuf batches every 5 to 10 s to `gauger-server:4318`.
  - Keeps a bounded on-disk buffer under `RUNNER_TEMP` for times when it's disconnected.
- **v1 scope.** Linux GitHub-hosted runners only. `ubuntu-latest` moves to Ubuntu 26 from 2026-10-19, so test on both.

## Tailnet

- **tsnet settings.**
  - `Ephemeral: true`
  - `AdvertiseTags: ["tag:gauger-ci"]`
  - `Dir` under `RUNNER_TEMP`
  - a unique `Hostname` per job
  - `ClientID` and `Audience` from the infra outputs `gauger_ci_client_id` and `gauger_ci_audience`
- **Required import.** Blank-import `tailscale.com/feature/identityfederation`. Without it, workload identity federation doesn't authenticate.
- **Join in the background.** Sample from the first second and buffer until the node has joined. The join must never delay the job's own steps.
- **Log out explicitly in `post`.** The Personal plan allows 1,000 ephemeral minutes a month, and current Actions volume is about 800 job-minutes a month.

## Contract with gauger-server

This contract is shared. Change it in both repos together.

- **Identity attributes on every record.** `github.run_id`, `github.run_attempt`, `github.check_run_id`, `github.repository`, `github.workflow`, `github.job` and `runner.name`. Reuse OTel CI/CD semantic-convention names where they exist, and make the same mapping in both repos.
- **Auth on every request.**
  - Send `Authorization: Bearer <GitHub OIDC JWT>` with audience `gauger-server`.
  - The JWT is short-lived. GitHub's example has `exp - iat = 300` s. Get a fresh one from `ACTIONS_ID_TOKEN_REQUEST_URL` with `ACTIONS_ID_TOKEN_REQUEST_TOKEN` at least 60 s before `exp`.
  - Retry a failed fetch at most 3 times with backoff, then buffer and keep trying.
  - A token fetched once in `main.js` would stop working about 5 minutes into the job.
- **Lifecycle.**
  - `POST /v1/jobs/start` when the sampler starts.
  - `POST /v1/jobs/done` after the final flush.
  - OTLP goes to `/v1/metrics`.
- **Job ID.**
  - Take it from action input `check-run-id`, defaulting to `${{ job.check_run_id }}`.
  - If that is empty, send `RUNNER_NAME` so the server can match on it.
- **Fallback artifact.**
  - Name it `gauger-<check_run_id>`.
  - It contains the unsent OTLP protobuf batches.
  - Upload it with `retention-days: 7`.

## Rules

- **Never fail the user's job.** Every gauger error is a `::warning::`, and the action exits 0.
- **v1 metrics are runner-level.** Each value is the runner's usage during a step's time window, and gauger labels it that way. It doesn't claim to be the step's own usage.
- **True per-step attribution waits on a spike.** It ships only if the spike shows that process-to-step mapping works, including background processes and processes that live under a second.
- **Docker.** Containers are parented by `dockerd`. Attribute them in phase 2, from cgroups.

## Spike before building

Run it on a real `ubuntu-latest` job. It answers:

1. Is `job.check_run_id` valid in an action input's default?
2. Does `check_run_id` equal the REST job ID?
3. Does `ACTIONS_ID_TOKEN_REQUEST_TOKEN` still work late in a long job?
4. How long does the tsnet join take, and how big is the binary?
5. How much time and CPU does sampling add compared with a baseline job?
6. Can each `Runner.Worker` child process be matched to a step?

## Done when

- A sample workflow in a `mach4-braai` repo streams to gauger-server.
- Cancelling the run still delivers the final flush.
- Killing the server mid-job produces the fallback artifact.
- Measured overhead is reported next to the baseline.
