const { appendFileSync } = require("node:fs");

const value = process.env["INPUT_CHECK-RUN-ID"] ?? "";
console.log(`main: input check-run-id = ${JSON.stringify(value)}`);
console.log(`main: ACTIONS_ID_TOKEN_REQUEST_URL set = ${Boolean(process.env.ACTIONS_ID_TOKEN_REQUEST_URL)}`);
console.log(`main: ACTIONS_ID_TOKEN_REQUEST_TOKEN set = ${Boolean(process.env.ACTIONS_ID_TOKEN_REQUEST_TOKEN)}`);
const names = Object.keys(process.env)
  .filter((name) => /^(GITHUB|ACTIONS|RUNNER)_/.test(name))
  .sort();
console.log(`main: environment names: ${names.join(" ")}`);

appendFileSync(process.env.GITHUB_OUTPUT, `check-run-id=${value}\n`);
appendFileSync(process.env.GITHUB_STATE, `check-run-id=${value}\n`);
