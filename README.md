# gauger

GitHub Action that samples runner CPU and memory per step and streams it over Tailscale to [gauger-server](https://github.com/mach4-braai/gauger-server).

## How it works

- `main.js` starts the gauger binary and returns.
- The binary samples host metrics, tags them with `GITHUB_RUN_ID`, `GITHUB_RUN_ATTEMPT` and `RUNNER_NAME`, joins the tailnet with `tsnet` as an ephemeral `tag:gauger-ci` node, and streams OTLP batches every 5 to 10 s.
- `post.js` flushes, logs out of the tailnet and writes a fallback artifact if the upload failed.

## Usage

```yaml
permissions:
  id-token: write
steps:
  - uses: mach4-braai/gauger@v1
```

## Status

Design only. No code yet.
