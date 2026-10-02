import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { readdir, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

// dist/<entry>/index.js reads dist/manifest.json, which the release workflow writes.
export const defaultManifestPath = path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "manifest.json");

export const STOP_TIMEOUT_MS = 30_000;

const ARCHES = { x64: "linux-x64", arm64: "linux-arm64" };

// assetKey returns the manifest key for this runner, or null when gauger does
// not support it.
export function assetKey(platform, arch) {
  return platform === "linux" ? (ARCHES[arch] ?? null) : null;
}

export function readManifest(file) {
  let manifest;
  try {
    manifest = JSON.parse(readFileSync(file, "utf8"));
  } catch (error) {
    if (error.code === "ENOENT") {
      throw new Error("this ref has no dist/manifest.json; pin the commit of a release tag");
    }
    throw error;
  }
  if (!manifest.version || typeof manifest.assets !== "object") {
    throw new Error("dist/manifest.json has no version or assets");
  }
  return manifest;
}

// download fetches url into file and fails unless its sha256 matches. The body
// is read whole: undici asserts and crashes the process when a server closes
// the connection while a streamed body is paused for backpressure.
export async function download(url, sha256, file, fetchImpl = fetch) {
  const response = await fetchImpl(url, { redirect: "follow", signal: AbortSignal.timeout(120_000) });
  if (!response.ok) {
    throw new Error(`download ${url}: HTTP ${response.status}`);
  }
  const data = Buffer.from(await response.arrayBuffer());
  const actual = createHash("sha256").update(data).digest("hex");
  if (actual !== sha256.toLowerCase()) {
    throw new Error(`${url} has sha256 ${actual}, the manifest expects ${sha256}`);
  }
  await writeFile(file, data, { mode: 0o755 });
}

// artifactName is the name gauger-server looks for when samples never arrive.
export function artifactName(checkRunId, runnerName) {
  const id = checkRunId || runnerName || "unknown";
  return `gauger-${id.replace(/[^A-Za-z0-9._-]+/g, "-")}`;
}

export async function spooledBatches(spoolDir) {
  let names;
  try {
    names = await readdir(spoolDir);
  } catch (error) {
    if (error.code === "ENOENT") return [];
    throw error;
  }
  return names
    .filter((name) => name.endsWith(".pb"))
    .sort()
    .map((name) => path.join(spoolDir, name));
}

export function alive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    return error.code === "EPERM";
  }
}

export const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function mib(bytes) {
  return `${((bytes ?? 0) / (1024 * 1024)).toFixed(1)} MiB`;
}

// summaryTable renders a status.json object as a markdown table for the job
// summary: peak CPU and memory, disk and network totals, batches sent and
// unsent, and tailnet join time. It renders the same way whether or not
// gauger-server was reachable, so a job with no server access still gets a
// table.
export function summaryTable(status) {
  const peaks = status.peaks ?? {};
  const rows = [
    ["Peak CPU utilization", `${((peaks.cpu_utilization ?? 0) * 100).toFixed(1)}%`],
    ["Peak memory used", mib(peaks.memory_used_bytes)],
    ["Disk read", mib(peaks.disk_read_bytes)],
    ["Disk write", mib(peaks.disk_write_bytes)],
    ["Network received", mib(peaks.network_rx_bytes)],
    ["Network sent", mib(peaks.network_tx_bytes)],
    ["Batches sent", `${status.sent_batches ?? 0}`],
    ["Batches unsent", `${status.unsent_batches ?? 0}`],
    ["Joined the tailnet", status.join_ms ? `${(status.join_ms / 1000).toFixed(1)} s` : "no"],
  ];
  return ["| Metric | Value |", "| --- | --- |", ...rows.map(([key, value]) => `| ${key} | ${value} |`)].join("\n");
}
