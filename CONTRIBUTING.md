# Contributing

Scribble is a small service with a locked design. Changes are welcome when they keep the design's guarantees and come with tests and docs.

## Before you start

1. Read [docs/design.md](docs/design.md). It is the source of truth for behaviour. If your change alters what the design says, update the design in the same change and explain why.
2. Read [AGENTS.md](AGENTS.md) for the rules every change follows. They apply to people and to coding agents alike.

## Setting up

You need [Dagger](https://docs.dagger.io/getting-started/installation/) at the version in `dagger.toml` (`engineVersion`) and a container runtime. Everything else, including the Go toolchain, Node and the browser, runs inside Dagger. See [docs/testing.md](docs/testing.md).

To run the service locally, follow [docs/getting-started.md](docs/getting-started.md). A local root with `listen.tailnet = ""` and `listen.web = "127.0.0.1:8090"` needs no tailnet.

## Running the checks

```sh
dagger check --progress=report
```

This is the only way to run the tests. Do not run `go test` on the host. The gate runs Go tests with `-race`, Staticcheck, the PWA validation, the Playwright e2e suite, the release build check and the generated-file checks.

To look for flaky tests, repeat the selected tests:

```sh
dagger check --env=flake --progress=report
```

## Tests

- Tests use fakes only: agentcore's `jevtest` and `agenttest`, fake HTTP servers for Whisper, a fake tailnet backend, and temporary vaults and roots. No test uses a real tailnet, Jev, Claude or Whisper.
- Put each test in the package that owns the behaviour. A test that drives the whole service belongs in `internal/service`; the CLI against a running service is covered by the e2e suite.
- Locked behaviour (threshold boundaries, write-intent replay, FIFO and retry order, dedup, undo rules, locks, rebase retries, symlink escapes) keeps its tests and assertions. Add to them; do not weaken them.
- Fix a bug in the package that owns it and add a unit test there.

## Generated files

If you change `service.Routes`, a request or response type or an error code, run:

```sh
dagger generate
```

and commit the updated `docs/openapi.json`. The checks fail when it is stale.

## Docs and defaults

When you change a flag, config key, default, route, exit code, log field or deployment step, update the doc that describes it. Defaults are stated three times: `DefaultConfig()`, `internal/config/config.example.toml` and [docs/configuration.md](docs/configuration.md). Change all three.

Docs describe the code as built, in plain prose, with no history of earlier setups.

## Toolchain versions

The Go base image, the Playwright image tag and the npm `@playwright/test` version are pinned to match. Update them together, along with the `go` directive in `go.mod`.

## Pull requests

- Keep a change focused. One behaviour change per pull request.
- State what changed and how you checked it. Paste the `dagger check --progress=report` result.
- Do not commit secrets, vault contents or captured thoughts. Logs contain raw captured text by design, so redact them before sharing.
