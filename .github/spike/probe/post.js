console.log(`post: input check-run-id = ${JSON.stringify(process.env["INPUT_CHECK-RUN-ID"] ?? "")}`);
console.log(`post: state check-run-id = ${JSON.stringify(process.env["STATE_check-run-id"] ?? "")}`);
console.log(`post: ACTIONS_ID_TOKEN_REQUEST_URL set = ${Boolean(process.env.ACTIONS_ID_TOKEN_REQUEST_URL)}`);
console.log(`post: ACTIONS_ID_TOKEN_REQUEST_TOKEN set = ${Boolean(process.env.ACTIONS_ID_TOKEN_REQUEST_TOKEN)}`);
