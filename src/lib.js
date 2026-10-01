import { createHash } from "node:crypto";
import { createWriteStream, readFileSync } from "node:fs";
import { chmod, readdir, rm } from "node:fs/promises";
import path from "node:path";
import { Readable, Transform } from "node:stream";
import { pipeline } from "node:stream/promises";
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
      throw new Error("this ref has no dist/manifest.json; use a release tag such as mach4-braai/gauger@v1");
    }
    throw error;
  }
  if (!manifest.version || typeof manifest.assets !== "object") {
    throw new Error("dist/manifest.json has no version or assets");
  }
  return manifest;
}

// download streams url to file and fails unless its sha256 matches. A file
// that fails the check is deleted.
export async function download(url, sha256, file, fetchImpl = fetch) {
  const response = await fetchImpl(url, { redirect: "follow", signal: AbortSignal.timeout(120_000) });
  if (!response.ok || !response.body) {
    throw new Error(`download ${url}: HTTP ${response.status}`);
  }
  const hash = createHash("sha256");
  const tee = new Transform({
    transform(chunk, _encoding, callback) {
      hash.update(chunk);
      callback(null, chunk);
    },
  });
  await pipeline(Readable.fromWeb(response.body), tee, createWriteStream(file, { mode: 0o700 }));
  const actual = hash.digest("hex");
  if (actual !== sha256.toLowerCase()) {
    await rm(file, { force: true });
    throw new Error(`${url} has sha256 ${actual}, the manifest expects ${sha256}`);
  }
  await chmod(file, 0o755);
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
