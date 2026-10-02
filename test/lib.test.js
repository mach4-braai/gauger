import assert from "node:assert/strict";
import { createHash, randomBytes } from "node:crypto";
import { existsSync, mkdtempSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { mkdir } from "node:fs/promises";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";
import { test } from "node:test";

import { artifactName, assetKey, download, readManifest, spooledBatches, summaryTable } from "../src/lib.js";

const tmp = () => mkdtempSync(path.join(tmpdir(), "gauger-test-"));

const serve = (body, status = 200) => async () => new Response(status === 200 ? body : "nope", { status });

test("download keeps a binary whose sha256 matches and makes it executable", async () => {
  const body = Buffer.from("binary contents");
  const file = path.join(tmp(), "gauger");
  await download("https://example.test/gauger", createHash("sha256").update(body).digest("hex"), file, serve(body));
  assert.deepEqual(readFileSync(file), body);
  assert.equal(statSync(file).mode & 0o777, 0o755);
});

test("download does not keep a binary whose sha256 does not match", async () => {
  const file = path.join(tmp(), "gauger");
  await assert.rejects(download("https://example.test/gauger", "0".repeat(64), file, serve(Buffer.from("tampered"))), /sha256/);
  assert.equal(existsSync(file), false);
});

test("download fails on an HTTP error", async () => {
  await assert.rejects(download("https://example.test/gauger", "0".repeat(64), path.join(tmp(), "g"), serve("", 404)), /HTTP 404/);
});

test("download survives a server that closes the connection after a large body", async () => {
  const body = randomBytes(32 << 20);
  const server = createServer((_req, res) => {
    res.writeHead(200, { "Content-Length": body.length, Connection: "close" });
    res.end(body);
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const url = `http://127.0.0.1:${server.address().port}/gauger`;
  const sum = createHash("sha256").update(body).digest("hex");
  try {
    for (let i = 0; i < 5; i++) {
      const file = path.join(tmp(), "gauger");
      await download(url, sum, file);
      assert.equal(statSync(file).size, body.length);
    }
  } finally {
    server.close();
  }
});

test("assetKey supports Linux x64 and arm64 only", () => {
  assert.equal(assetKey("linux", "x64"), "linux-x64");
  assert.equal(assetKey("linux", "arm64"), "linux-arm64");
  assert.equal(assetKey("darwin", "arm64"), null);
  assert.equal(assetKey("linux", "ia32"), null);
});

test("readManifest explains a ref without a release", () => {
  assert.throws(() => readManifest(path.join(tmp(), "manifest.json")), /release tag/);
});

test("readManifest rejects a manifest without assets", () => {
  const file = path.join(tmp(), "manifest.json");
  writeFileSync(file, JSON.stringify({ version: "v1.0.0" }));
  assert.throws(() => readManifest(file), /no version or assets/);
});

test("artifactName uses the check run ID and falls back to the runner name", () => {
  assert.equal(artifactName("123456", "GitHub Actions 3"), "gauger-123456");
  assert.equal(artifactName("", "GitHub Actions 3"), "gauger-GitHub-Actions-3");
});

test("spooledBatches lists complete batches in order", async () => {
  const dir = path.join(tmp(), "spool");
  await mkdir(dir);
  for (const name of ["00000000000000000002.pb", ".tmp-1", "00000000000000000001.pb"]) {
    writeFileSync(path.join(dir, name), "x");
  }
  assert.deepEqual(
    (await spooledBatches(dir)).map((file) => path.basename(file)),
    ["00000000000000000001.pb", "00000000000000000002.pb"],
  );
  assert.deepEqual(await spooledBatches(path.join(dir, "missing")), []);
});

test("summaryTable renders the unsent count when the server was unreachable", () => {
  const table = summaryTable({
    version: "v1.2.3",
    sent_batches: 0,
    unsent_batches: 4,
    join_ms: 0,
    peaks: { cpu_utilization: 0.42, memory_used_bytes: 1024 * 1024 },
  });
  assert.match(table, /\| Batches unsent \| 4 \|/);
  assert.match(table, /\| Batches sent \| 0 \|/);
  assert.match(table, /\| Joined the tailnet \| no \|/);
  assert.match(table, /\| Peak CPU utilization \| 42\.0% \|/);
  assert.match(table, /\| Peak memory used \| 1\.0 MiB \|/);
  assert.match(table, /\| Disk read \| 0\.0 MiB \|/);
});

test("summaryTable shows the join time and totals for a healthy run", () => {
  const table = summaryTable({
    version: "v1.2.3",
    sent_batches: 7,
    unsent_batches: 0,
    join_ms: 2500,
    peaks: {
      cpu_utilization: 0.8,
      memory_used_bytes: 2 * 1024 * 1024,
      disk_read_bytes: 5 * 1024 * 1024,
      disk_write_bytes: 3 * 1024 * 1024,
      network_rx_bytes: 1024 * 1024,
      network_tx_bytes: 512 * 1024,
    },
  });
  assert.match(table, /\| Batches sent \| 7 \|/);
  assert.match(table, /\| Joined the tailnet \| 2\.5 s \|/);
  assert.match(table, /\| Disk read \| 5\.0 MiB \|/);
  assert.match(table, /\| Network sent \| 0\.5 MiB \|/);
});
