#!/usr/bin/env python3
"""Fetch GitHub OIDC tokens at the given offsets in seconds and print their lifetimes."""

import base64
import json
import os
import sys
import time
import urllib.parse
import urllib.request

AUDIENCE = "gauger-server"


def claims(jwt):
    payload = jwt.split(".")[1]
    payload += "=" * (-len(payload) % 4)
    return json.loads(base64.urlsafe_b64decode(payload))


def fetch():
    url = os.environ["ACTIONS_ID_TOKEN_REQUEST_URL"] + "&audience=" + urllib.parse.quote(AUDIENCE)
    request = urllib.request.Request(
        url, headers={"Authorization": "bearer " + os.environ["ACTIONS_ID_TOKEN_REQUEST_TOKEN"]}
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)["value"]


def main():
    start = time.time()
    try:
        request_token = claims(os.environ["ACTIONS_ID_TOKEN_REQUEST_TOKEN"])
        issued = request_token.get("iat", request_token.get("nbf"))
        print(
            f"request token: exp - iat = {request_token['exp'] - issued} s, "
            f"expires {request_token['exp'] - start:.0f} s after this process started",
            flush=True,
        )
    except Exception as error:
        print(f"request token: not a readable JWT ({error})", flush=True)

    for offset in map(int, sys.argv[1:]):
        time.sleep(max(0.0, start + offset - time.time()))
        try:
            token = claims(fetch())
            print(
                f"+{offset}s: ok, exp - iat = {token['exp'] - token['iat']} s, aud = {token['aud']}, "
                f"claims = {' '.join(sorted(token))}",
                flush=True,
            )
            if offset == 0:
                for name in ("sub", "check_run_id", "job_check_run_id", "runner_environment", "repository_owner_id"):
                    print(f"  {name} = {token.get(name)}", flush=True)
        except Exception as error:
            print(f"+{offset}s: failed: {error}", flush=True)


if __name__ == "__main__":
    main()
