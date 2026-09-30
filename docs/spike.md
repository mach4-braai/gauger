# Spike results

Measured on GitHub-hosted `ubuntu-24.04` and `ubuntu-26.04` runners in [run 36744327825](https://github.com/mach4-braai/gauger/actions/runs/36744327825), by `.github/workflows/spike.yml`.

## 1. Is `job.check_run_id` valid in an action input's default?

Yes. A `node24` action with `default: ${{ job.check_run_id }}` received `109986525349` on 24.04 and `109986525743` on 26.04, in both `main` and `post`.

## 2. Does `check_run_id` equal the REST job ID?

Yes. `GET /repos/{owner}/{repo}/actions/runs/{run_id}/attempts/{attempt}/jobs` returned `id` `109986525349`, and its `check_run_url` ends in `/check-runs/109986525349`.

The OIDC token also carries a `check_run_id` claim with the same value, so gauger-server can check that a request's claimed job matches its token.

## 3. Does `ACTIONS_ID_TOKEN_REQUEST_TOKEN` still work late in a long job?

A process started at the beginning of the job and holding the request token from its environment fetched an OIDC token at +0 s, +420 s and +1500 s on both images. Each OIDC token had `exp - iat = 300` s.

The request token itself is a JWT with `exp - iat = 3000` s, and gauger's binary keeps the copy from its start environment. Whether a fetch still works past 50 minutes is not measured yet: the 75-minute run for it was cancelled to stay inside the free usage limits. If it does not, a job longer than about 50 minutes keeps sampling but can no longer upload, and the rest reaches gauger-server only through the fallback artifact.

## 4. How long does the tsnet join take, and how big is the binary?

The join took 1.4 s and 2.0 s on 24.04 and 26.04 in [CI run 36748702080](https://github.com/mach4-braai/gauger/actions/runs/36748702080), and 1.8 s and 2.3 s in two more `ubuntu-24.04` and `ubuntu-latest` runs.

The stripped binary is 34.3 MB for `linux/amd64` and 32.0 MB for `linux/arm64`.

## 5. How much time and CPU does sampling add compared with a baseline job?

[Overhead run 36749772448](https://github.com/mach4-braai/gauger/actions/runs/36749772448) built gauger from scratch three times (`go build -a`), with and without gauger sampling.

| Image | Mode | Wall s, 3 builds | gauger CPU | gauger peak RSS |
|---|---|---|---|---|
| 24.04 | baseline | 50.7, 49.7, 50.8 | | |
| 24.04 | gauger | 65.4, 65.2, 64.1 | 0.21 s user, 0.14 s sys | 60 MiB |
| 26.04 | baseline | 67.5, 68.3, 66.9 | | |
| 26.04 | gauger | 54.9, 54.7, 54.8 | 0.15 s user, 0.11 s sys | 49 MiB |

- The wall-time gaps go both ways by 15 to 25 %. They come from the VMs, not from gauger: over the same three to four minutes gauger itself used 0.26 to 0.35 s of CPU, about 0.2 % of one core.
- The main step took 0 to 1 s, including the 34 MB download.
- With gauger-server unreachable, the post step took 17 to 22 s, because the final drain retried for its whole 15 s window. The binary now gives up after 3 attempts, and the next runs took 3 to 4 s, including the fallback artifact upload.

## 6. Can each `Runner.Worker` child process be matched to a step?

Not well enough for per-step attribution.

- Each `run:` step the tracer caught was one direct `bash` child of `Runner.Worker`. It never caught the `node` process of the two JavaScript action steps. A step that runs for seconds can be matched by time.
- REST step timestamps have one-second resolution. Several steps started and ended inside the same second, so a process start time cannot tell them apart.
- Polling `/proc` every 100 ms saw 0 of 500 sub-second `/bin/true` runs.
- A background process left by a step is reparented to PID 1 (`systemd`) when the step's shell exits, so it drops out of the `Runner.Worker` tree. It kept running into later steps.
- Only the `docker run` client showed up under the step. The container's own processes belong to the Docker daemon, which is why the spec attributes containers from cgroups in phase 2.

v1 stays runner-level, as the spec says. Per-step attribution would need exec events (eBPF or the audit log) rather than polling, plus cgroups for containers.
