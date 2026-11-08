# Command line

```text
scribble serve [--detach] [--root PATH]
scribble stop    [--root PATH] [--json]
scribble status  [--root PATH] [--json]
scribble retry <job-id> [--root PATH] [--json]
scribble undo  <job-id> [--root PATH] [--json]
scribble config  [--root PATH] [--json]
scribble doctor  [--root PATH] [--json]
scribble --version
```

Flags may come before or after the job id. `scribble help` prints the usage.

## Choosing the service: `--root`

`serve` and every command that talks to the service accept `--root PATH`, resolved the same way everywhere:

- With `--root PATH`, the root is `PATH`, the config is `PATH/config.toml` and the service listens on `PATH/scribble.sock`.
- Without it, the root is `$XDG_DATA_HOME/scribble` (default `~/.local/share/scribble`), the config is `$XDG_CONFIG_HOME/scribble/config.toml` and the socket is in that root.

So after `scribble serve --root /var/lib/scribble`, `scribble status --root /var/lib/scribble` reaches that service and no other. With two services under different roots, each command acts on, and reports, only the service of the root it names. Pass the same `--root` that started the service. A command without `--root` looks only at the default root's socket.

The socket is mode 0600 and owned by the account that runs the service. Run the commands as that account; see [running.md](running.md#managing-the-systemd-service).

A socket path longer than the Unix limit (103 bytes) is rejected, so choose a short root.

## `serve`

Runs the service in the foreground until `SIGINT`, `SIGTERM` or `scribble stop`. It loads `config.toml`, opens the queue, listens on `<root>/scribble.sock`, on `listen.web` when set and on the tailnet node when `listen.tailnet` is set, and writes JSON logs to stdout.

A tailnet failure degrades only the tailnet listener; the service, the socket and `listen.web` keep running. A failure to load the config, to open the database or to bind the socket or `listen.web` ends the command.

`--detach` starts the service in the background, logging to `<root>/service.log`, and returns once it answers on its socket. It prints the pid and the log path. Use it outside systemd; under systemd run `serve` in the foreground.

`serve` has no `--json`.

## `status`

Prints the service's health, a `Tailnet:` line, today's note date, pending and failed jobs and the last write:

```text
Health: ok
Version: v0.1.0
Tailnet: running https://scribble.example.ts.net
Today: 2026-11-07
Pending: 1
  <job-id>  routing
Failed: 0
Last write: 2026-11-07T12:41:08-05:00 amend <job-id>
  - LinkedIn post: overpaying for CI talk ^scr-k3f9
```

The tailnet line is `disabled`, `starting`, `running <url>` or `degraded: <reason>`. A degraded tailnet does not change `Health`. `--json` prints `{"health": …, "status": …}`: the bodies of `GET /v1/health` and `GET /v1/status`.

## `retry <job-id>` and `undo <job-id>`

The same actions as the PWA, through `POST /v1/thoughts/{id}/retry` and `/undo`.

- `retry` requeues a failed job at the back of the queue. Prints `<id>: queued`.
- `undo` undoes the most recent completed write, only if it was written today and its line is unchanged in the note. Prints `<id>: undone`.

`--json` prints `{"id": …, "state": …}`. A refusal, such as retrying a job that has not failed, is a conflict.

## `config`

Prints the configuration the running service holds in memory, as TOML, with a header naming the file and root it came from. `--json` prints the same as JSON, including the resolved `paths`. Editing `config.toml` after the service started does not change the output; restart the service to apply a change.

## `doctor`

Runs six checks and reports each as `ok`, `warn` or `fail`:

| Check | Passes when |
| --- | --- |
| `vault` | The vault is a readable, writable directory. |
| `daily-notes.json` | `.obsidian/daily-notes.json` is absent or valid and its date format is supported. |
| `claude` | `claude` is on `PATH`. |
| `jev-key` | The environment variable named by `jev.api_key_env` is set. |
| `whisper` | `transcribe.url` is unset (a warning) or answers over the network. Any HTTP answer counts. |
| `tailnet` | The tailnet is disabled or running. A degraded node fails. |

`doctor` runs in the running service's environment: its `PATH`, environment variables and network, because that is where a missing `claude` or key would bite. The output says `Doctor (service)`.

When no service answers on the socket, `doctor` runs the same checks in the CLI process against the config it resolves from `--root`, and says `Doctor (local)` with the socket it tried. Local results reflect the CLI's own `PATH` and environment, not a service's. When `listen.tailnet` is set, the tailnet check can then only inspect its inputs (node state and `TS_AUTHKEY`) and warns. In `--json`, the `source` field is `service` or `local`.

It exits 5 when any check fails.

## `stop`

Asks the service to shut down over its socket. The service stops its listeners, lets the worker finish and closes the database, as it does on a signal, and `stop` returns once the service has gone (it waits for the socket file to disappear, up to 30 seconds). Prints `scribble service on <socket> stopped`; `--json` prints `{"state":"stopped"}`.

Under systemd, use `systemctl stop scribble` instead; `Restart=on-failure` does not restart a clean stop.

## `--version`

Prints the build information that the release build stamps into `internal/buildinfo`: `scribble v0.1.0 (<commit>, <date>)`. An unstamped build prints `scribble dev`.

## `--json` and errors

With `--json`, a successful command prints JSON on stdout. A failure prints the API error envelope on stderr:

```json
{"error": {"code": "not_found", "message": "…"}}
```

Without `--json`, a failure prints `scribble: <message>` on stderr. When the service is not running, the error says no service answers on the socket and which socket it tried.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Success. |
| `2` | Usage error: an unknown command or flag, or the wrong number of arguments. |
| `4` | A `validation`, `not_found`, `forbidden` or `unsupported_media_type` failure, or a config file that failed validation. |
| `5` | A `conflict`, internal or `unavailable` failure, no service answering on the socket, or a failed `doctor` check. |
