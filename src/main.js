import { spawn } from "node:child_process";
import { once } from "node:events";
import { mkdirSync, openSync } from "node:fs";
import path from "node:path";

import * as core from "@actions/core";

import { assetKey, defaultManifestPath, download, readManifest } from "./lib.js";

async function main() {
  const key = assetKey(process.platform, process.arch);
  if (!key) {
    core.warning(`gauger supports Linux x64 and arm64 runners only, not ${process.platform}/${process.arch}; it is not sampling this job.`);
    return;
  }
  if (!process.env.ACTIONS_ID_TOKEN_REQUEST_URL) {
    core.warning("gauger needs `permissions: id-token: write` on the job; it is not sampling this job.");
    return;
  }
  const manifest = readManifest(defaultManifestPath);
  const asset = manifest.assets[key];
  if (!asset) {
    throw new Error(`release ${manifest.version} has no ${key} binary`);
  }

  const dir = path.join(process.env.RUNNER_TEMP, "gauger");
  const stateDir = path.join(dir, "state");
  mkdirSync(stateDir, { recursive: true });
  const binary = path.join(dir, "gauger");
  const bytes = await download(asset.url, asset.sha256, binary);
  core.info(`downloaded ${bytes} bytes for ${key}`);

  const checkRunId = core.getInput("check-run-id");
  const args = [
    "-state-dir", stateDir,
    "-server", core.getInput("server"),
    "-check-run-id", checkRunId,
    "-ts-client-id", core.getInput("tailscale-client-id"),
    "-ts-audience", core.getInput("tailscale-audience"),
  ];
  const log = openSync(path.join(dir, "gauger.log"), "a");
  const child = spawn(binary, args, { detached: true, stdio: ["ignore", log, log] });
  await Promise.race([
    once(child, "spawn"),
    once(child, "error").then(([error]) => Promise.reject(error)),
  ]);
  child.unref();

  core.saveState("pid", String(child.pid));
  core.saveState("dir", dir);
  core.saveState("check-run-id", checkRunId);
  core.info(`gauger ${manifest.version} is sampling as PID ${child.pid}${checkRunId ? ` for check run ${checkRunId}` : ""}.`);
}

main().catch((error) => core.warning(`gauger did not start: ${error.message}`));
