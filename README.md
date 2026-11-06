# Scribble

Your scribe for random thoughts. Capture text or voice from an installable PWA; Scribble files it in today's Obsidian daily note. Jev selects tags and follow-ups, Claude writes only amendments or new tag definitions, and Obsidian handles reading and search.

The Go binary embeds the frontend. SQLite persists the FIFO queue, audio uploads, idempotency keys, retries, and recoverable write intents. The frontend has an IndexedDB outbox, offline app shell, Background Sync where supported, SSE confirmation, and undo of the most recent completed write.

## Run

Requires Go 1.25+, a local vault, a TypeSafe API key, and Claude Code for amendments and new tags. See [setup](docs/setup.md) for the complete Linux, Obsidian Headless, Tailscale, and optional Whisper setup.

```sh
mkdir -p .local/vault .local/state
cp config.example.yaml .local/config.yaml
# Edit vault and queue_db to absolute paths under .local.
# Keep listen: 127.0.0.1:8090.
export TYPESAFE_API_KEY='your-key'
export ANTHROPIC_API_KEY='your-key'
go build -trimpath -o bin/scribble ./cmd/scribble
./bin/scribble -config .local/config.yaml
```

Open `http://127.0.0.1:8090` locally. Use Tailscale Serve HTTPS for phone access and microphone permission. No Whisper setup is needed for text input; an unconfigured voice job fails visibly and can be retried after configuration.

## Develop and test

```sh
go test -race ./...
go vet ./...
npm ci
npm run build
npx playwright install chromium
npm run test:e2e
```

The frontend is plain HTML/CSS/JavaScript, with no runtime dependencies or bundle step. `npm run build` validates JavaScript and the install manifest; `go build` embeds the assets. `go run scripts/icons.go` regenerates the PNG app icons from a simple geometric mark.

Tests never call a real model or Whisper service. Backend tests use agentcore **v1.1.0**'s `jevtest` and `agenttest`; Playwright starts the real binary against agentcore's fake Jev HTTP server, a scripted Claude CLI, and fake transcription HTTP. Vaults and databases are temporary. Sync never runs in tests.

[Dagger](dagger.toml) reuses the Go, Staticcheck, and Playwright modules, with a local module for the release binary and browser runtime:

```sh
dagger check
# Export the Linux amd64 binary:
dagger api call scribble build export --path ./bin/scribble-linux-amd64
```

Use Dagger `v1.0.0-beta.15` for the checked-in module pins. CI uses native Dagger Cloud Checks, following the [Dagger quickstart](https://docs.dagger.io/getting-started/quickstart/) and [Cloud Checks setup](https://docs.dagger.io/cloud-checks/):

```sh
dagger cloud checks on
```

The command connects the repository identified by `origin`. If Dagger reports a missing account or GitHub integration, complete the prerequisite it prints and rerun it. Once enabled, pushes and pull requests run the same workspace checks. After pushing, inspect the results with `dagger workspace activity`.

The [implementation plan](docs/IMPLEMENTATION.md) records the decisions made while implementing [the design](docs/DESIGN.md).

## API

| Endpoint | Behavior |
| --- | --- |
| `POST /thoughts` | Persist a text JSON body or audio multipart upload; return `202` and the job immediately. |
| `GET /thoughts/{id}` | Status of a known job. |
| `GET /status` | Pending/failed jobs, the last completed write, and today's local date. |
| `GET /events` | SSE `status` snapshots; reconnects get current durable state. |
| `POST /thoughts/{id}/undo` | Undo only the most recent completed write, only today. |
| `POST /thoughts/{id}/retry` | Requeue a failed job at the back of the queue. |
| `GET /healthz` | Service/database health. |

Text body: `{"id":"<client UUID>","captured_at":"2026-11-05T12:00:00Z","text":"A thought"}`. Audio multipart uses `id`, `captured_at`, and an `audio` file with its audio content type. Text is limited to 16 KiB; audio to 25 MiB. The PWA stops recording after two minutes. Retrying a UUID returns the existing job without changing its payload.

The service binds only to a loopback IP. Tailnet access control is the authentication boundary; there is no public listener, login, or entry browser. Cross-origin writes are rejected.
