# hunky-cli

The client for **Hunky Cloud** — the (real, intentionally stupid) hosted version
of [`hunky`](../). It mirrors the local tool's command set 1:1, except instead of
doing the work on your machine — instantly, free, privately — it ships your diff
to a server, where the exact same `hunky` binary runs it, and hands back the git
commands you then run locally, plus an unsolicited code review from **Brawny-1**.

This is strictly worse than just running `hunky`. That is the product. See
[`../HUNKY_CLOUD.md`](../HUNKY_CLOUD.md) for the full bit.

## Install

```
go install github.com/whiskeytuesday/hunky/hunky-cli@latest
```

## Account (management plane)

Logs you in and manages your account — talks to the token-authed
`/v1/{signup,login,account,keys,plan}` endpoints.

```
hunky-cli signup                  # create an account (interactive)
hunky-cli login                   # log in → stores a 1h session token
hunky-cli logout                  # forget the session token
hunky-cli account                 # show plan, credit, keys
hunky-cli keys                    # list API keys
hunky-cli keys create <name>      # mint a key (stored as your active key)
hunky-cli keys revoke <prefix>    # revoke a key
hunky-cli plan <plan>             # pretty_boy | mamas_boy | whole_package
hunky-cli credits                 # the backer wall, in three descending tiers
hunky-cli auth <key>              # store an API key directly
```

Plan tiers unlock Brawny review modes — pass `--review <mode>` when staging:
`standard` (free), `terse`/`verbose` ($1+), `haiku`/`sonnet` ($5+), and `mythos`
($10) which returns the empty string for national-security reasons. Locked modes
fall back to `standard`.

`signup`/`login` are interactive (hidden password prompt) but scriptable:
pass `--email`/`--password` (and `--name`/`--code` for signup) or set
`HUNKY_EMAIL`/`HUNKY_PASSWORD`. So the whole bootstrap is one non-interactive line:

```
HUNKY_EMAIL=you@co.dev HUNKY_PASSWORD=… hunky-cli signup && hunky-cli keys create ci
```

## Stage (use plane)

```
hunky-cli --list <file>           # list hunks (computed in the cloud)
hunky-cli --count <file>          # print the number of hunks
hunky-cli <file> <hunk>...        # stage by number — confirm, runs git apply locally
hunky-cli --invert <file> <hunk>...    # stage every hunk EXCEPT the listed ones
hunky-cli --lines START-END <file>     # whole hunks overlapping a line range
hunky-cli --pick-lines A-B <file>      # only the +/- lines within A-B (splits a hunk)
hunky-cli --dry-run <file> <hunk>...   # print the patch instead of staging
hunky-cli --stdin <file> <hunk>...     # read the diff from stdin (--yes to apply)
hunky-cli --version
```

`--yes`/`-y` skips the apply confirmation (required with `--stdin`, which has no TTY).

Env:
- `HUNKY_CLOUD_URL` — endpoint (default `https://api.hunky.dev`)
- `HUNKY_CLOUD_KEY` — API key (overrides the stored credential)
- `HUNKY_CLOUD_TOKEN` — management session token (overrides the stored one)
- `HUNKY_EMAIL` / `HUNKY_PASSWORD` — non-interactive `signup`/`login`

## Protocol

A two-call async handshake against `/v1/pretty`:

1. `POST /v1/pretty` with `{mode, file, hunks|lines|pick_lines, invert, diff}`,
   where `diff` is `base64(gzip(text))` ("military-grade compression"). Returns
   `202 {request_key, poll_after_ms}`.
2. `GET /v1/pretty/<request_key>` — `202` while the 1080 Ti warms up, then `200`
   with `{output, commands, review, meta}`.

The cloud only ever sees your **diff**, and only ever runs `hunky` read-only
(`--list`/`--count`/`--dry-run`). It never touches your working tree — for staging
it returns a `git apply --cached` command that the CLI runs locally after you
confirm.

## Local development

`mock/` is a zero-infra stand-in for the backend: it implements the handshake,
shells out to the real `hunky` binary (exactly as the eventual Elixir/Fly backend
will), and returns a canned Brawny-1 review.

```
go build -o hunky ..                                  # the real tool, on PATH
HUNKY_BIN=$PWD/hunky PORT=8787 go run ./mock           # start the mock

HUNKY_CLOUD_URL=http://localhost:8787 \
HUNKY_CLOUD_KEY=test \
  go run . --list somefile.go                          # drive it
```
