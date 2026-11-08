# Testing

Tests run only through Dagger:

```sh
dagger check --progress=report
```

Never run `go test` on the host. The Go version, the browser runtime and the generated-file checks are pinned in the Dagger workspace ([`dagger.toml`](../dagger.toml)), so the gate is the only run that counts. You need Dagger at the `engineVersion` in `dagger.toml` and a container runtime; no Go, Node or browser on the host.

## What the checks run

The workspace reuses the shared Go, Staticcheck and Playwright module sources that osmia's workspace uses, plus a local `scribble-dev` module in `.dagger/modules/scribble-dev`.

- **Go tests** for every package, with the race detector on by default (`goflags = "-race -count=1"`; `-count=1` turns off result caching).
- **Staticcheck** over the Go code.
- **PWA validation.** `npm run build` checks the JavaScript syntax and the install manifest. The frontend is plain HTML, CSS and JavaScript with no bundle step; `go build` embeds the assets.
- **Playwright e2e** in a browser container, against the real binary.
- **Release build check.** The release binary compiles, and a build stamped with a version, commit and date prints them from `scribble --version`.
- **Generated files.** `dagger generate` writes `docs/openapi.json` from `service.Routes`. The check fails when the committed file differs from what the generator produces.

### Flake hunting

```sh
dagger check --env=flake --progress=report
```

The `flake` environment repeats each selected test five times (`-count=5`) with `-race` still on. Use it to confirm a fix for a flaky test or to vet a new concurrent test.

### Regenerating `docs/openapi.json`

After changing `service.Routes`, a request or response type or an error code:

```sh
dagger generate
```

Commit the regenerated file. Do not edit it by hand.

## Fakes only

No test uses a real tailnet, Jev, Claude or Whisper, and Sync never runs in tests. Vaults, roots and databases are temporary.

| Dependency | Fake |
| --- | --- |
| Jev | agentcore v1.2.0 `jevtest`, with scripted typed answers and probabilities. Threshold logic is tested on both sides of each threshold. |
| Claude | agentcore v1.2.0 `agenttest`, the scripted CLI double. In e2e it is the `claude` found first on the service's `PATH`. |
| Whisper | A fake `/v1/audio/transcriptions` HTTP server that returns fixture text. |
| Tailnet | `tailnet.Fake` (a status the service reports) and `tailnet.FakeBackend` (a node that logs in, fails or drops) in `internal/tailnet`. Tests cover state reporting (starting, running, degraded, disabled) and failure handling without a real node. |
| Vault | A temporary directory with fixture daily notes. |

Each package tests the behaviour it owns:

| Package | Locks down |
| --- | --- |
| `internal/queue` | FIFO order, backoff, retry going to the back, UUID dedup, the database lock, write-intent persistence. |
| `internal/vault` | Byte-preserving edits, dates and section boundaries, collisions, concurrent edits and rebase retries, symlink escapes, the vault lock, replay. |
| `internal/routing` | Jev routing and the threshold boundaries. |
| `internal/writer` | The Claude runner with the default profile. |
| `internal/transcribe` | The Whisper client against a fake server. |
| `internal/config` | TOML validation, the `<file>: <field>: <reason>` error form, path resolution and that `DefaultConfig()` matches `config.example.toml`. |
| `internal/api` | The error envelope, codes and statuses. |
| `internal/service` | The `/v1` routes served on each listener, the envelope on every failure, `/v1/health` build and tailnet state, browser checks on web and tailnet handlers only, recovery after restart and midnight, undo rules. |
| `internal/tailnet` | The node lifecycle with a fake backend. |
| `internal/cli` | Commands, flags including `--root`, exit codes, `--json` output, doctor in the service versus local, `stop`, and two services under different roots. |
| `internal/buildinfo` | Version formatting. |

A test that drives the whole service lives in `internal/service`. A bug the e2e suite finds gets fixed in the owning package with a unit test there.

## The e2e suite

Playwright builds `bin/scribble`, then starts a Go fixture (`tests/e2e/server_test.go`, run only when `SCRIBBLE_E2E=1`) that:

1. writes a TOML config with `listen.web` on loopback and `listen.tailnet` empty, and a fixture vault with a prior day's note;
2. starts fake Jev and Whisper HTTP servers and puts an agentcore `agenttest` `claude` first on `PATH`;
3. runs `scribble serve --root <tmp>/root` and waits for `GET /v1/health`.

`tests/e2e/capture.spec.js` drives the PWA: capture, amendment, linking, voice, offline outbox, retry and undo. `tests/e2e/cli.spec.js` then runs the CLI against the same running service, every command given `--root <tmp>/root`: `status` (the `Tailnet:` line reads `disabled`), `retry` and `undo` of a job, `config`, `doctor` and finally `stop`, which shuts the service down. The CLI runs with an environment that has neither the fake `claude` nor the Jev key, which shows that `doctor` reports the service's environment rather than its own.

## Toolchain pins

The Go base image (`golang:1.26-bookworm` in `dagger.toml` and the `scribble-dev` module), the Playwright image tag and the npm `@playwright/test` version in `package.json` move together, as does the `go` directive in `go.mod`. Change them in one commit.

## CI

CI is Dagger Cloud Checks, running the same workspace checks on pushes and pull requests:

```sh
dagger cloud checks on
```

The command connects the repository identified by `origin`. If Dagger reports a missing account or GitHub integration, complete the prerequisite it prints and run it again. After pushing, inspect results with `dagger workspace activity`.
