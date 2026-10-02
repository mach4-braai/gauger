import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { readdir, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { gunzipSync } from "node:zlib";

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

// download fetches a gzipped url into file and fails unless the sha256 of the
// compressed bytes matches. It gunzips only after the hash check passes, and
// returns the number of compressed bytes it read. The body is read whole:
// undici asserts and crashes the process when a server closes the connection
// while a streamed body is paused for backpressure.
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
  await writeFile(file, gunzipSync(data), { mode: 0o755 });
  return data.length;
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
