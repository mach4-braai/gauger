import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash, randomBytes } from "node:crypto";
import { existsSync, mkdtempSync, readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { mkdir } from "node:fs/promises";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";
import { test } from "node:test";
import { gzipSync } from "node:zlib";

import { artifactName, assetKey, download, readManifest, spooledBatches, summaryTable, uploadArtifact } from "../src/lib.js";

const tmp = () => mkdtempSync(path.join(tmpdir(), "gauger-test-"));

const serve = (body, status = 200) => async () => new Response(status === 200 ? body : "nope", { status });
function fakeRuntimeToken() {
  const header = Buffer.from(JSON.stringify({ alg: "none" })).toString("base64url");
  const payload = Buffer.from(
    JSON.stringify({ scp: "Actions.Results:11111111-1111-1111-1111-111111111111:22222222-2222-2222-2222-222222222222" }),
  ).toString("base64url");
  return `${header}.${payload}.`;
}

test("download keeps a binary whose sha256 matches and makes it executable", async () => {
  const body = Buffer.from("binary contents");
  const gz = gzipSync(body);
  const file = path.join(tmp(), "gauger");
  const bytes = await download("https://example.test/gauger", createHash("sha256").update(gz).digest("hex"), file, serve(gz));
  assert.deepEqual(readFileSync(file), body);
  assert.equal(statSync(file).mode & 0o777, 0o700);
  assert.equal(existsSync(`${file}.tmp`), false);
  assert.equal(bytes, gz.length);
});

test("download does not keep a binary, or its temp file, whose sha256 does not match", async () => {
  const file = path.join(tmp(), "gauger");
  await assert.rejects(download("https://example.test/gauger", "0".repeat(64), file, serve(gzipSync(Buffer.from("tampered")))), /sha256/);
  assert.equal(existsSync(file), false);
});

test("download fails the hash check on a tampered gzip before it decompresses", async () => {
  const body = Buffer.from("binary contents");
  const gz = gzipSync(body);
  const sum = createHash("sha256").update(gz).digest("hex");
  const tampered = Buffer.from(gz);
  tampered[tampered.length - 1] ^= 0xff;
  const file = path.join(tmp(), "gauger");
  await assert.rejects(download("https://example.test/gauger", sum, file, serve(tampered)), /sha256/);
  assert.equal(existsSync(file), false);
  assert.equal(existsSync(`${file}.tmp`), false);
});

test("download fails on an HTTP error", async () => {
  await assert.rejects(download("https://example.test/gauger", "0".repeat(64), path.join(tmp(), "g"), serve("", 404)), /HTTP 404/);
});

test("download survives a server that closes the connection after a large body", async () => {
  const body = randomBytes(32 << 20);
  const gz = gzipSync(body);
  const server = createServer((_req, res) => {
    res.writeHead(200, { "Content-Length": gz.length, Connection: "close" });
    res.end(gz);
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const url = `http://127.0.0.1:${server.address().port}/gauger`;
  const sum = createHash("sha256").update(gz).digest("hex");
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
    peaks: { cpu_utilization: 0.42, memory_used_bytes: 1024 * 1024 },
  });
  assert.match(table, /\| Batches unsent \| 4 \|/);
  assert.match(table, /\| Batches sent \| 0 \|/);
  assert.match(table, /\| Peak CPU utilization \| 42\.0% \|/);
  assert.match(table, /\| Peak memory used \| 1\.0 MiB \|/);
  assert.match(table, /\| Disk read \| 0\.0 MiB \|/);
});

test("summaryTable shows the totals for a healthy run", () => {
  const table = summaryTable({
    version: "v1.2.3",
    sent_batches: 7,
    unsent_batches: 0,
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
  assert.match(table, /\| Disk read \| 5\.0 MiB \|/);
  assert.match(table, /\| Network sent \| 0\.5 MiB \|/);
});

test("uploadArtifact zips, uploads and finalizes against a fake Twirp server", async () => {
  const dir = tmp();
  const spoolDir = path.join(dir, "spool");
  await mkdir(spoolDir);
  const batches = {
    "00000000000000000001.pb": Buffer.from("batch-one"),
    "00000000000000000002.pb": Buffer.from("batch-two"),
  };
  for (const [name, data] of Object.entries(batches)) {
    writeFileSync(path.join(spoolDir, name), data);
  }
  const files = await spooledBatches(spoolDir);

  let uploadedBody = Buffer.alloc(0);
  const server = createServer((req, res) => {
    const chunks = [];
    req.on("data", (chunk) => chunks.push(chunk));
    req.on("end", () => {
      const body = Buffer.concat(chunks);
      if (req.url.endsWith("/CreateArtifact")) {
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ ok: true, signed_upload_url: `http://127.0.0.1:${server.address().port}/blob` }));
      } else if (req.url === "/blob") {
        uploadedBody = body;
        res.writeHead(201);
        res.end();
      } else if (req.url.endsWith("/FinalizeArtifact")) {
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ ok: true, artifact_id: "1" }));
      } else {
        res.writeHead(404);
        res.end();
      }
    });
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  process.env.ACTIONS_RESULTS_URL = `http://127.0.0.1:${server.address().port}`;
  process.env.ACTIONS_RUNTIME_TOKEN = fakeRuntimeToken();
  try {
    assert.equal(await uploadArtifact("gauger-test", files, 7), true);

    const zipFile = path.join(dir, "artifact.zip");
    writeFileSync(zipFile, uploadedBody);
    const extractDir = path.join(dir, "extract");
    await mkdir(extractDir);
    execFileSync("unzip", ["-q", zipFile, "-d", extractDir]);
    assert.deepEqual(readdirSync(extractDir).sort(), Object.keys(batches).sort());
    for (const [name, data] of Object.entries(batches)) {
      assert.deepEqual(readFileSync(path.join(extractDir, name)), data);
    }
  } finally {
    server.close();
    delete process.env.ACTIONS_RESULTS_URL;
    delete process.env.ACTIONS_RUNTIME_TOKEN;
  }
});

test("uploadArtifact warns instead of throwing when the results service errors", async () => {
  const server = createServer((_req, res) => {
    res.writeHead(500);
    res.end("nope");
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const file = path.join(tmp(), "00000000000000000001.pb");
  writeFileSync(file, "x");
  process.env.ACTIONS_RESULTS_URL = `http://127.0.0.1:${server.address().port}`;
  process.env.ACTIONS_RUNTIME_TOKEN = fakeRuntimeToken();
  const originalWrite = process.stdout.write.bind(process.stdout);
  let output = "";
  process.stdout.write = (chunk, ...args) => {
    output += chunk;
    return originalWrite(chunk, ...args);
  };
  try {
    assert.equal(await uploadArtifact("gauger-test", [file], 7), false);
    assert.match(output, /::warning::/);
  } finally {
    process.stdout.write = originalWrite;
    server.close();
    delete process.env.ACTIONS_RESULTS_URL;
    delete process.env.ACTIONS_RUNTIME_TOKEN;
  }
});
