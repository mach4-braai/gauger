# gauger

GitHub Action that samples runner CPU, memory, disk and network during a job and streams them over Tailscale to [gauger-server](https://github.com/mach4-braai/gauger-server).

## How it works

- `main.js` downloads the gauger binary pinned in `dist/manifest.json`, checks its sha256, starts it detached and returns.
- The binary samples `/proc` once a second and tags every batch with the job's identity. It joins the tailnet in the background as an ephemeral `tag:gauger-ci` node, and sends an OTLP batch every 5 s. Batches wait in a bounded buffer under `RUNNER_TEMP` until they are sent.
- `post.js` stops the binary, which flushes, sends `done` and logs out of the tailnet. Batches it could not send go into the artifact `gauger-<check_run_id>`, kept for 7 days.
- The values are runner-level. A step's window shows the whole runner's usage during that step, not what the step itself used.
- gauger never fails the job. Every error is a warning.

## Usage

Pin the commit of a release tag, with the version in a comment, and let Renovate bump both. No release is published yet.

```yaml
jobs:
  build:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      id-token: write
    steps:
      - uses: mach4-braai/gauger@<full commit sha> # vX.Y.Z
      - uses: actions/checkout@v7
      # ...
```

Put it first, so sampling covers the whole job. It runs on Linux x64 and arm64 GitHub-hosted runners.

Pin the commit the `vX.Y.Z` tag points to, not a `master` SHA. The release commit is the only one that carries `dist/manifest.json`, and no branch contains it. In a clone of gauger:

```sh
git fetch --tags
git rev-list -n1 vX.Y.Z
```

| Input | Default | |
|---|---|---|
| `check-run-id` | `${{ job.check_run_id }}` | Job ID that gauger-server matches the samples on. |
| `server` | `http://gauger-server:4318` | gauger-server on the tailnet. |
| `tailscale-client-id` | infra output `gauger_ci_client_id` | Tailscale federated identity for `tag:gauger-ci`. |
| `tailscale-audience` | infra output `gauger_ci_audience` | Its OIDC audience. |

`docs/contract.md` is the wire contract with gauger-server. `docs/spike.md` records what the spike measured on real runners.

## Development

`mise run check` runs the Go checks, and `mise run e2e` runs the binary against a fake server on Linux. `npm test` tests the action code, and `npm run build` rebuilds `dist/`, which is committed.

The Release workflow builds the binaries, tags a commit that pins them in `dist/manifest.json` and publishes the release. Release tags are immutable, so there is no moving major tag. Branches have no manifest, so `uses: mach4-braai/gauger@master` warns and does nothing.
