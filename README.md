# Scribble

Your scribe for random thoughts. Capture text or voice from an installable PWA; Scribble files it in today's Obsidian daily note. Jev selects tags and follow-ups, Claude writes only amendments or new tag definitions, and Obsidian handles reading and search.

One Go binary embeds the frontend and its own tailnet node. SQLite persists the FIFO queue, audio uploads, idempotency keys, retries and recoverable write intents. The PWA has an IndexedDB outbox, an offline app shell, SSE confirmation and undo of the most recent write. Scribble is reachable only on your tailnet, at `https://scribble.<tailnet>.ts.net`.

```sh
scribble serve --root /var/lib/scribble     # the service
scribble status --root /var/lib/scribble    # health, tailnet, pending and failed jobs, last write
scribble doctor --root /var/lib/scribble    # check the vault, claude, keys, Whisper and the tailnet
```

## Documentation

| Page | Covers |
| --- | --- |
| [Getting started](docs/getting-started.md) | Build, service account, Obsidian Headless, config and the first start. |
| [Configuration](docs/configuration.md) | The TOML file, its location, every key and default. |
| [Command line](docs/cli.md) | Every command, `--root`, `--json` and exit codes. |
| [Running](docs/running.md) | The tailnet, installing the PWA, systemd, threshold tuning, recovery and limits. |
| [Voice transcription](docs/transcription.md) | Whisper on a tailnet host. |
| [Testing](docs/testing.md) | `dagger check`, fakes, the e2e suite and regenerating files. |
| [Design](docs/design.md) | The product contract and the decisions behind it. |
| [OpenAPI](docs/openapi.json) | The public `/v1` API, generated from the routes. |

Contributors: read [AGENTS.md](AGENTS.md) and [CONTRIBUTING.md](CONTRIBUTING.md). Tests run only through `dagger check --progress=report`.
