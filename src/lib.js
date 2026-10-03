import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { readdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { crc32 } from "node:zlib";

import * as core from "./actions.js";

// src/<entry>.js reads dist/manifest.json, which the release workflow writes.
export const defaultManifestPath = path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "dist", "manifest.json");

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

function createZip(entries) {
  const localParts = [];
  const centralParts = [];
  const { date, time } = dosDateTime(new Date());
  let offset = 0;
  for (const { name, data } of entries) {
    const nameBuf = Buffer.from(name, "utf8");
    const crc = crc32(data) >>> 0;
    const local = Buffer.alloc(30);
    local.writeUInt32LE(0x04034b50, 0);
    local.writeUInt16LE(20, 4);
    local.writeUInt16LE(0, 6);
    local.writeUInt16LE(0, 8);
    local.writeUInt16LE(time, 10);
    local.writeUInt16LE(date, 12);
    local.writeUInt32LE(crc, 14);
    local.writeUInt32LE(data.length, 18);
    local.writeUInt32LE(data.length, 22);
    local.writeUInt16LE(nameBuf.length, 26);
    local.writeUInt16LE(0, 28);
    localParts.push(local, nameBuf, data);

    const central = Buffer.alloc(46);
    central.writeUInt32LE(0x02014b50, 0);
    central.writeUInt16LE(20, 4);
    central.writeUInt16LE(20, 6);
    central.writeUInt16LE(0, 8);
    central.writeUInt16LE(0, 10);
    central.writeUInt16LE(time, 12);
    central.writeUInt16LE(date, 14);
    central.writeUInt32LE(crc, 16);
    central.writeUInt32LE(data.length, 20);
    central.writeUInt32LE(data.length, 24);
    central.writeUInt16LE(nameBuf.length, 28);
    central.writeUInt16LE(0, 30);
    central.writeUInt16LE(0, 32);
    central.writeUInt16LE(0, 34);
    central.writeUInt16LE(0, 36);
    central.writeUInt32LE((0o100644 << 16) >>> 0, 38);
    central.writeUInt32LE(offset, 42);
    centralParts.push(central, nameBuf);

    offset += local.length + nameBuf.length + data.length;
  }
  const centralDir = Buffer.concat(centralParts);
  const end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0);
  end.writeUInt16LE(0, 4);
  end.writeUInt16LE(0, 6);
  end.writeUInt16LE(entries.length, 8);
  end.writeUInt16LE(entries.length, 10);
  end.writeUInt32LE(centralDir.length, 12);
  end.writeUInt32LE(offset, 16);
  end.writeUInt16LE(0, 20);
  return Buffer.concat([...localParts, centralDir, end]);
}

function dosDateTime(d) {
  const date = (Math.max(d.getFullYear(), 1980) - 1980 << 9) | ((d.getMonth() + 1) << 5) | d.getDate();
  const time = (d.getHours() << 11) | (d.getMinutes() << 5) | (d.getSeconds() >> 1);
  return { date, time };
}

function getBackendIds(runtimeToken) {
  const payload = runtimeToken.split(".")[1];
  const decoded = JSON.parse(Buffer.from(payload, "base64url").toString("utf8"));
  for (const scope of (decoded.scp ?? "").split(" ")) {
    const parts = scope.split(":");
    if (parts[0] === "Actions.Results" && parts.length === 3) {
      return { workflowRunBackendId: parts[1], workflowJobRunBackendId: parts[2] };
    }
  }
  throw new Error("ACTIONS_RUNTIME_TOKEN has no Actions.Results scope");
}

async function twirpRequest(resultsUrl, runtimeToken, method, body) {
  const url = new URL(`/twirp/github.actions.results.api.v1.ArtifactService/${method}`, resultsUrl).href;
  const response = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${runtimeToken}` },
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(30_000),
  });
  const text = await response.text();
  let data;
  try {
    data = JSON.parse(text);
  } catch {
    throw new Error(`${method}: HTTP ${response.status}, non-JSON response`);
  }
  if (!response.ok || !data.ok) {
    throw new Error(`${method}: HTTP ${response.status}${data.msg ? `: ${data.msg}` : ""}`);
  }
  return data;
}

export async function uploadArtifact(name, files, retentionDays) {
  try {
    const runtimeToken = process.env.ACTIONS_RUNTIME_TOKEN;
    if (!runtimeToken) throw new Error("ACTIONS_RUNTIME_TOKEN is not set");
    const resultsUrl = process.env.ACTIONS_RESULTS_URL;
    if (!resultsUrl) throw new Error("ACTIONS_RESULTS_URL is not set");
    const ids = getBackendIds(runtimeToken);

    const entries = await Promise.all(files.map(async (file) => ({ name: path.basename(file), data: await readFile(file) })));
    const zip = createZip(entries);

    const expiresAt = new Date(Date.now() + retentionDays * 24 * 60 * 60 * 1000).toISOString();
    const created = await twirpRequest(resultsUrl, runtimeToken, "CreateArtifact", {
      workflow_run_backend_id: ids.workflowRunBackendId,
      workflow_job_run_backend_id: ids.workflowJobRunBackendId,
      name,
      expires_at: expiresAt,
      version: 7,
      mime_type: "application/zip",
    });

    const uploadResponse = await fetch(created.signed_upload_url, {
      method: "PUT",
      headers: { "x-ms-blob-type": "BlockBlob", "Content-Type": "application/zip" },
      body: zip,
      signal: AbortSignal.timeout(60_000),
    });
    if (!uploadResponse.ok) {
      throw new Error(`blob upload: HTTP ${uploadResponse.status}`);
    }
    const hash = createHash("sha256").update(zip).digest("hex");

    await twirpRequest(resultsUrl, runtimeToken, "FinalizeArtifact", {
      workflow_run_backend_id: ids.workflowRunBackendId,
      workflow_job_run_backend_id: ids.workflowJobRunBackendId,
      name,
      size: String(zip.length),
      hash: `sha256:${hash}`,
    });
    return true;
  } catch (error) {
    core.warning(`gauger could not upload the fallback artifact: ${error.message}`);
    return false;
  }
}
export const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
