import { existsSync, readFileSync } from "node:fs";
import path from "node:path";

import { DefaultArtifactClient } from "@actions/artifact";
import * as core from "@actions/core";

import { STOP_TIMEOUT_MS, alive, artifactName, sleep, spooledBatches } from "./lib.js";

async function stop(pid, statusFile) {
  try {
    process.kill(pid, "SIGTERM");
  } catch (error) {
    if (error.code === "ESRCH") {
      core.warning("gauger had already exited before the post step; the log below shows why.");
      return;
    }
    throw error;
  }
  const deadline = Date.now() + STOP_TIMEOUT_MS;
  while (Date.now() < deadline && alive(pid) && !existsSync(statusFile)) {
    await sleep(200);
  }
  if (!existsSync(statusFile) && alive(pid)) {
    process.kill(pid, "SIGKILL");
    core.warning(`gauger did not finish within ${STOP_TIMEOUT_MS / 1000} s and was killed.`);
  } else if (!existsSync(statusFile)) {
    core.warning("gauger exited without writing its status; the log below shows why.");
  }
}

function report(statusFile) {
  if (!existsSync(statusFile)) return;
  const status = JSON.parse(readFileSync(statusFile, "utf8"));
  for (const warning of status.warnings ?? []) {
    core.warning(warning);
  }
  const joined = status.join_ms ? `joined the tailnet in ${(status.join_ms / 1000).toFixed(1)} s, ` : "";
  core.info(`gauger ${status.version}: ${joined}sent ${status.sent_batches} batches, ${status.unsent_batches} unsent.`);
}

async function uploadUnsent(spoolDir, checkRunId) {
  const files = await spooledBatches(spoolDir);
  if (files.length === 0) return;
  const name = artifactName(checkRunId, process.env.RUNNER_NAME);
  await new DefaultArtifactClient().uploadArtifact(name, files, spoolDir, { retentionDays: 7 });
  core.info(`gauger uploaded ${files.length} unsent batches as artifact ${name}.`);
}

async function post() {
  const pid = Number(core.getState("pid"));
  if (!pid) return;
  const dir = core.getState("dir");
  const stateDir = path.join(dir, "state");
  const statusFile = path.join(stateDir, "status.json");

  await stop(pid, statusFile);
  report(statusFile);

  const log = path.join(dir, "gauger.log");
  if (existsSync(log)) {
    core.startGroup("gauger log");
    core.info(readFileSync(log, "utf8"));
    core.endGroup();
  }
  await uploadUnsent(path.join(stateDir, "spool"), core.getState("check-run-id"));
}

post().catch((error) => core.warning(`gauger post step failed: ${error.message}`));
