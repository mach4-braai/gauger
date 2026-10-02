import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { test } from "node:test";

import { endGroup, getInput, getState, info, saveState, startGroup, warning } from "../src/actions.js";

// captureStdout intercepts process.stdout.write for the duration of fn and
// returns what the action would have printed as a workflow command.
async function captureStdout(fn) {
  const original = process.stdout.write.bind(process.stdout);
  let output = "";
  process.stdout.write = (chunk) => {
    output += chunk;
    return true;
  };
  try {
    await fn();
  } finally {
    process.stdout.write = original;
  }
  return output;
}

test("warning escapes % and newlines the way workflow commands require", async () => {
  const output = await captureStdout(() => warning("50% off\nnext line"));
  assert.equal(output.trim(), "::warning::50%25 off%0Anext line");
});

test("info writes the message as-is", async () => {
  const output = await captureStdout(() => info("plain message"));
  assert.equal(output.trim(), "plain message");
});

test("startGroup and endGroup issue group commands", async () => {
  assert.equal((await captureStdout(() => startGroup("gauger log"))).trim(), "::group::gauger log");
  assert.equal((await captureStdout(() => endGroup())).trim(), "::endgroup::");
});

test("getInput reads INPUT_<NAME> with spaces turned to underscores and trims", () => {
  process.env["INPUT_CHECK-RUN-ID"] = " 123 ";
  try {
    assert.equal(getInput("check-run-id"), "123");
  } finally {
    delete process.env["INPUT_CHECK-RUN-ID"];
  }
});

test("getInput returns an empty string when the input is unset", () => {
  assert.equal(getInput("missing input"), "");
});

test("saveState writes the multiline delimiter format to GITHUB_STATE, and getState reads it back", () => {
  const file = path.join(mkdtempSync(path.join(tmpdir(), "gauger-test-")), "state");
  writeFileSync(file, "");
  process.env.GITHUB_STATE = file;
  try {
    saveState("pid", "4242");
    const written = readFileSync(file, "utf8");
    assert.match(written, /^pid<<ghadelimiter_[0-9a-f-]+\n4242\nghadelimiter_[0-9a-f-]+\n$/);

    process.env.STATE_pid = "4242";
    assert.equal(getState("pid"), "4242");
  } finally {
    delete process.env.GITHUB_STATE;
    delete process.env.STATE_pid;
  }
});
