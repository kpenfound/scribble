# AGENTS.md

Scribble is a single Go service that files text and voice thoughts into today's Obsidian daily note. This file tells coding agents and contributors how to work in the repository. [CONTRIBUTING.md](CONTRIBUTING.md) covers the same ground for people.

## The design is the source of truth

[docs/design.md](docs/design.md) is the product contract: the daily note format, the routing pipeline, the queue, write safety, deployment. Read the part you are about to change first.

- If the code and the design disagree, decide which one is wrong. Fix the code, or change the design in the same change and say why. Do not leave them different.
- Behaviour the design locks does not change as a side effect of other work: routing and the threshold logic, write safety (`os.Root`, byte-preserving edits, rebase retries, the write-intent journal and replay), the single worker goroutine, FIFO order, backoff, UUID dedup, the undo rules, the vault and database locks, and the fields of the `route.decision` and `entry.undo` log events.
- The user-facing docs live in [docs/](docs/) and are listed in the [README](README.md). A change that alters a flag, key, default, route, exit code, log field or deployment step updates the doc that describes it in the same change.

## Tests: only through Dagger, only with fakes

- Run the checks with `dagger check --progress=report`. It runs Go tests with `-race`, Staticcheck, the PWA checks, the Playwright e2e suite, the release build check and the generated-file checks. `dagger check --env=flake` repeats the selected tests to expose flakes.
- **Never run `go test` on the host.** Not for a single package, not to iterate quickly. The Go version, the browser runtime and the generated-file checks are pinned in the Dagger workspace, and a host run proves nothing about the gate. See [docs/testing.md](docs/testing.md).
- Tests use fakes only. No test talks to a real tailnet, Jev, Claude or Whisper. Use agentcore's `jevtest` and `agenttest` doubles, fake HTTP servers, a fake tailnet backend and temporary vaults and roots. Sync never runs in tests.
- Every behaviour change comes with a test in the package that owns the code.
- A bug found in the e2e suite gets fixed in the owning package with a unit test. The e2e fixture does not paper over it.

## Code layout

| Package | Owns |
| --- | --- |
| `cmd/scribble` | A short shim that calls `cli.Run`. |
| `internal/cli` | Commands, flags, exit codes, the socket client, doctor, detached mode and the socket-only controls. |
| `internal/config` | TOML loading, validation, `DefaultConfig()`, `config.example.toml` and root path resolution. |
| `internal/api` | `Prefix`, the wire types and the error envelope and codes. |
| `internal/service` | `service.Routes`, handlers per listener, SSE and the single worker. |
| `internal/queue` | SQLite queue, audio, retries and write intents. |
| `internal/vault` | The byte-preserving editor, daily-note settings and the tag registry. |
| `internal/routing` | Jev routing and the threshold decision. |
| `internal/writer` | Claude through agentcore's restricted runner. |
| `internal/transcribe` | The Whisper client. |
| `internal/tailnet` | The embedded tsnet node and its state reporting. |
| `internal/buildinfo` | Version, commit and date stamped by the release build. |
| `internal/web` | The embedded PWA. Plain HTML, CSS and JavaScript with no bundle step. |
| `internal/openapi` | The generator for `docs/openapi.json`. |
| `tests/e2e` | The Playwright specs and the Go fixture that runs `scribble serve --root`. |

## Comments

- Comment why, not what. A comment states a constraint, an invariant or a reason the obvious approach is wrong. It does not restate the next line.
- Every exported identifier has a doc comment that starts with its name. A package has a package comment.
- Comments describe the code as it is now. Do not leave comments about past versions, removed options or work in progress.
- Never put a secret, a token or captured thought text in a comment, test name or error message. Config errors name the file and field, never the value.

## Docs

- Write plain, direct prose. Describe what the code does today. Say nothing about intended behaviour that is not built.
- Docs describe only the current code. They carry no history of earlier setups and no mention of removed options.
- `configuration.md`, `config.example.toml` and `DefaultConfig()` state the same defaults. Change all three together. `internal/config`'s tests compare the last two.
- `docs/cli.md` matches the implemented commands, flags and exit codes. Every command that talks to the service takes `--root`.

## Regenerating `docs/openapi.json`

`docs/openapi.json` is generated from `service.Routes` and the `internal/api` types. Never edit it by hand.

1. After changing a route, a request or response type or an error code, run `dagger generate`.
2. Commit the regenerated file with the change.
3. `dagger check` fails when the committed file is stale, so a forgotten regeneration cannot land.

Socket-only controls (loaded config, doctor, shutdown) are not public routes. They stay out of `service.Routes` and out of the OpenAPI file.

## Toolchain pins

The Go base image, the Playwright image tag and the npm `@playwright/test` version move together, as do the `go` directive in `go.mod` and the Dagger Go module settings in `dagger.toml`. Change them in one commit.

## Boundaries

- Outbound calls (Jev, Claude, Whisper) use the host's network stack. Nothing dials through the embedded tsnet node, which only listens.
- A tailnet failure degrades the tailnet listener only. The service, the socket and `listen.web` keep running.
- The socket-only controls are served on the Unix socket only. The web and tailnet listeners answer them with not found.
