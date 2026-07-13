# orchestrator setup: plan

motivation: a first-time bring-up of an orchestrator + one fc-agent took ~2 hours
of debugging, and every failure was a setup/ergonomics gap, not a real bug. this
plan closes those gaps. ordered by pain caused.

## what actually went wrong

1. the CLI was pointed at the fc-agent (`:8090`) instead of an orchestrator
   (`:8080`). the orchestrator was never running at all. nothing surfaced this.
2. `fuse connect` does no network call, so it printed `connected:` against a
   host that had no orchestrator on it. the failure only appeared one command
   later, as an unrelated-looking `Not found`.
3. `Not found` was the fc-agent 404ing `/v1/hosts`. indistinguishable from
   "that host id does not exist".
4. two different tokens (orchestrator bearer vs host-agent token) with no way to
   tell which one you are holding. a wrong-token attempt produced
   `Unauthorized`, a right-token attempt produced `Not found`, which reads like
   the token got worse.
5. no install path for the orchestrator. it had to be built from source and run
   under an ad-hoc `nohup`.

common thread: **every error was reported at the wrong layer.** the CLI cannot
tell "wrong url", "wrong token", "wrong service", and "not running" apart.

## p0: make the failure modes self-describing

### 1. probe on connect

`fuse connect` should hit the orchestrator before writing config, and refuse to
save a context that does not answer as one.

- `GET /v1/version` (new, unauthenticated) returns `{"service":"fuse-orchestrator","version":"..."}`.
- connect calls it and hard-fails if `service` is missing or wrong:
  - no response -> `no orchestrator at %s (connection refused). is it running?`
  - answers but not fuse -> `%s is not a fuse orchestrator (got Server: fc-agent/0.1). the orchestrator default port is 8080; 8090 is the host agent.`
  - answers, wrong token -> `orchestrator at %s rejected the token. this must be ORCH_AUTH_TOKEN, not the host-agent token.`
- `--no-verify` to skip, for scripted/offline setup.

this single change would have caught the whole incident at step one.

### 2. version endpoint

orchestrator exposes `/v1/version` unauthenticated. today there is no way to ask
a running process what it is or what build it is on. the fc-agent already
self-identifies via its `Server:` header; the orchestrator should too.

also set `Server: fuse-orchestrator/<version>` on all responses.

### 3. distinguish 404-route from 404-resource

a 404 on an unregistered _route_ must not render as `Not found`. add a catch-all
route that returns a structured error, so the CLI can say
`%s does not expose the fuse API (is this the orchestrator?)` instead.

### 4. fix the version string

`fuse --version` prints `sdks/go/v0.2.0-22-g1e0afaf`, derived from `git describe`
against the `sdks/go` tag lineage. it undersells the build and is actively
misleading when diagnosing staleness. stamp the CLI from its own tag.

## p1: an actual install path

### 5. ship the orchestrator as a release artifact

`cmd/orchestrator` is already a single static binary. add it to goreleaser
alongside the CLI: linux/amd64 + arm64 tarballs, attached to the github release.

### 6. systemd unit + install script

```
scripts/install-orchestrator.sh   # fetch binary, write unit, enable
deploy/fuse-orchestrator.service  # EnvironmentFile=/etc/fuse/orchestrator.env
```

the unit reads `/etc/fuse/orchestrator.env`:

```
ORCH_LISTEN=:8080
ORCH_AUTH_TOKEN=
TOKEN_ENCRYPTION_KEY=
DATABASE_URL=
```

the install script generates `ORCH_AUTH_TOKEN` and `TOKEN_ENCRYPTION_KEY` if
absent and prints the connect command to run, so the operator never has to know
which of the several tokens in play is the right one:

```
orchestrator installed and listening on :8080

  fuse connect http://<this-host>:8080 --token <generated> --master
```

### 7. `fuse quickstart`

wraps the two-step (connect, then register a host) that is currently
undiscoverable. prompts for orchestrator url + token, verifies, then prompts for
the host agent url + token, registers it, and selects it.

## p2: naming and docs

### 8. `fuse hosts` vs `fuse host`

the split is a trap. `hosts` holds only `list`; `host` holds `register`, `get`,
`cordon`, `uncordon`, `remove`, `metrics`, plus bare-id selection. everyone
reaches for `fuse hosts register` first, and it errors with `Unknown flag: --url`,
which points at the flag rather than at the command name.

fix: register every subcommand under both, or make `hosts` an alias of `host` and
keep `list` on both. at minimum, add an `Unknown command` hint.

### 9. token glossary in the docs

one table, near the top of the setup guide:

| token               | set on       | used by               | flag/env                                   |
| ------------------- | ------------ | --------------------- | ------------------------------------------ |
| orchestrator bearer | orchestrator | you -> orchestrator   | `ORCH_AUTH_TOKEN` / `fuse connect --token` |
| host agent token    | fc-agent     | orchestrator -> agent | `fuse host register --token`               |
| api key             | orchestrator | apps -> orchestrator  | `fuse apikeys create`                      |

state the direction of travel explicitly. "which token goes where" was the single
most confusing part of bring-up.

### 10. default ports, stated once

`8080` orchestrator, `8090` fc-agent. currently only discoverable by reading
`cmd/orchestrator/main.go:106`.

## sequencing

- p0 items 1-3 are the ones that matter. they are small, and they convert a
  two-hour debug into a one-line error message.
- item 4 is a one-liner.
- p1 turns "clone the repo and build it on the server" into "run one script".
- p2 is docs plus one naming decision.
