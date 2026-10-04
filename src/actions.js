import { randomUUID } from "node:crypto";
import { appendFileSync } from "node:fs";
import { EOL } from "node:os";

function escape(value) {
  return String(value).replace(/%/g, "%25").replace(/\r/g, "%0D").replace(/\n/g, "%0A");
}

function issueCommand(command, message = "") {
  process.stdout.write(`::${command}::${escape(message)}${EOL}`);
}

export function getInput(name) {
  return (process.env[`INPUT_${name.replace(/ /g, "_").toUpperCase()}`] || "").trim();
}

export function getState(name) {
  return process.env[`STATE_${name}`] || "";
}

export function saveState(name, value) {
  const filePath = process.env.GITHUB_STATE;
  if (!filePath) throw new Error("GITHUB_STATE is not set");
  const delimiter = `ghadelimiter_${randomUUID()}`;
  appendFileSync(filePath, `${name}<<${delimiter}${EOL}${value}${EOL}${delimiter}${EOL}`, "utf8");
}

export function warning(message) {
  issueCommand("warning", message);
}

export function info(message) {
  process.stdout.write(message + EOL);
}

export function startGroup(name) {
  issueCommand("group", name);
}

export function endGroup() {
  issueCommand("endgroup");
}
