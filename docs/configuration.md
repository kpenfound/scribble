# Configuration

Scribble reads one TOML file, `config.toml`. [`internal/config/config.example.toml`](../internal/config/config.example.toml) is the commented example. It states every default below, with a placeholder `vault`.

## Where the file lives

| How you start | Config file | State root |
| --- | --- | --- |
| `scribble serve --root PATH` | `PATH/config.toml` | `PATH` |
| `scribble serve` | `$XDG_CONFIG_HOME/scribble/config.toml`, else `~/.config/scribble/config.toml` | `$XDG_DATA_HOME/scribble`, else `~/.local/share/scribble` |

An XDG variable counts only when it is an absolute path. The systemd unit uses `--root /var/lib/scribble`, so its config is `/var/lib/scribble/config.toml`. `scribble config` prints the configuration a running service loaded, including these resolved paths.

Restart the service after changing the file. Daily-note settings and the tag registry are re-read during processing; nothing else is.

## Rules

- The file must say `version = 1`.
- Only the keys below exist. An unknown key is rejected, named by its full path such as `jev.amend_confidence`.
- Durations are strings such as `"5s"` or `"90s"` and must be positive.
- Absent keys keep their defaults. `vault` has no default and is required.
- Errors name the file and the field and never quote values: `<file>: <field>: <reason>`. For example `/var/lib/scribble/config.toml: jev.tag_threshold: must be between 0 and 1`.

## Keys and defaults

### Top level

| Key | Default | Meaning |
| --- | --- | --- |
| `version` | `1` | The config format version. Must be `1`. |
| `vault` | none, required | Absolute path of the Obsidian vault. It must be writable by the service's account. Keep the state root outside it, so Obsidian Sync never carries the database. |
| `timezone` | `"America/Detroit"` | IANA time zone. A job is filed into the note for the day it is processed in this zone, including captures made offline on an earlier day. |
| `lookback_days` | `7` | Calendar days before today whose notes are read for routing context. From 1 to 7. |
| `log_level` | `"info"` | `debug`, `info`, `warn` or `error`. |

### `[daily]`

Used only when `.obsidian/daily-notes.json` is absent. When the file exists, its `folder` and `format` take precedence.

| Key | Default | Meaning |
| --- | --- | --- |
| `folder` | `"Daily"` | Folder of the daily notes, relative to the vault. |
| `format` | `"YYYY-MM-DD"` | Date format of the note name. Supported tokens are `YYYY`, `MM`, `DD`, separators and bracketed literals. Nested date directories work. An unsupported format fails visibly rather than write under a wrong date. |

### `[listen]`

The Unix socket is always `<root>/scribble.sock` and is not configurable.

| Key | Default | Meaning |
| --- | --- | --- |
| `web` | `""` | Optional loopback `host:port`, such as `"127.0.0.1:8090"`. The host must be `localhost` or a loopback address. Empty turns the listener off. |
| `tailnet` | `"scribble"` | Hostname of the embedded tailnet node, one DNS label of lowercase letters, digits and hyphens. With the default, the app is at `https://scribble.<tailnet>.ts.net`, where `<tailnet>` is your tailnet's name. Empty turns the node off. |
| `tailnet_https` | `true` | Serve on port 443 with a Tailscale certificate. `false` serves plain HTTP on port 80, which browsers do not treat as secure for microphone access. |

### `[queue]`

| Key | Default | Meaning |
| --- | --- | --- |
| `max_attempts` | `3` | Automatic attempts per job before it is marked failed. From 1 to 10. |
| `retry_backoff` | `"5s"` | Delay after the first failed attempt. It doubles for each later attempt: with the defaults, retries come after 5 and 10 seconds. |

The oldest unfinished job holds the FIFO during backoff. A terminal failure lets later jobs through, and a manual retry puts the job at the back.

### `[jev]`

| Key | Default | Meaning |
| --- | --- | --- |
| `url` | `"https://api.typesafe.ai"` | The Jev API base, without `/v1/systemone`. |
| `model` | `"jev-latest"` | Jev model. |
| `api_key_env` | `"TYPESAFE_API_KEY"` | Name of the environment variable holding the Jev key. |
| `timeout` | `"10s"` | Timeout of one Jev call. |
| `amend_threshold` | `0.75` | Probability at or above which a selected target in today's note is amended. Below it, a new entry linked to the target is written. |
| `link_threshold` | `0.75` | Probability at or above which a selected target from an earlier day is linked. It is never amended. |
| `tag_threshold` | `0.6` | Probability at or above which an existing tag applies. The same value gates proposing a new tag. |

Thresholds are probabilities from 0 to 1 and compare against Jev's returned probabilities. See [running.md](running.md#tuning-the-thresholds).

### `[profiles.default]`

The writer role, which writes amendments and new tag definitions, uses this profile.

| Key | Default | Meaning |
| --- | --- | --- |
| `agent` | `"claude"` | Only `claude` is accepted. The `claude` CLI is found on the service's `PATH`; there is no binary setting. |
| `model` | `"claude-haiku-5-5"` | Model passed to Claude Code. |
| `timeout` | `"90s"` | Timeout of one Claude call. |

### `[transcribe]`

| Key | Default | Meaning |
| --- | --- | --- |
| `url` | `""` | The full transcription endpoint that audio is posted to, such as `http://whisper.<tailnet>.ts.net:8000/v1/audio/transcriptions`. Empty means voice jobs fail with a retryable message while text capture works. Normally a MagicDNS name or a 100.x address the host can reach. |
| `model` | `"large-v3-turbo"` | Model name sent with each request. |
| `api_key_env` | `"WHISPER_API_KEY"` | Name of the environment variable holding a bearer key, if the server wants one. |

See [transcription.md](transcription.md).

## Environment

Secrets are never in the config file. The service reads them from its environment, which systemd fills from the unit's `EnvironmentFile`:

| Variable | Used for |
| --- | --- |
| `TYPESAFE_API_KEY` | Jev routing. The name is `jev.api_key_env`. |
| `ANTHROPIC_API_KEY` | The `claude` CLI, unless the account is logged in to Claude Code. |
| `TS_AUTHKEY` | The embedded node's first login. Later starts use `<root>/tailnet`. |
| `WHISPER_API_KEY` | The transcription server, when it needs a bearer key. The name is `transcribe.api_key_env`. |

## Full example

```toml
version = 1
vault = "/srv/vault"
timezone = "America/Detroit"
lookback_days = 7
log_level = "info"

[daily]
folder = "Daily"
format = "YYYY-MM-DD"

[listen]
web = ""
tailnet = "scribble"
tailnet_https = true

[queue]
max_attempts = 3
retry_backoff = "5s"

[jev]
url = "https://api.typesafe.ai"
model = "jev-latest"
api_key_env = "TYPESAFE_API_KEY"
timeout = "10s"
amend_threshold = 0.75
link_threshold = 0.75
tag_threshold = 0.6

[profiles.default]
agent = "claude"
model = "claude-haiku-5-5"
timeout = "90s"

[transcribe]
url = ""
model = "large-v3-turbo"
api_key_env = "WHISPER_API_KEY"
```

## The tag registry

`Scribble/Tags.md` in the vault starts as an empty markdown table and grows with model-created tags. Edit names and meanings in Obsidian; the next thought uses them. Keep the four columns and valid nonnegative usage counts. A meaning cannot contain an unescaped table delimiter; use words instead of `|`. Counts measure successful write uses, including amendments. Undo does not erase historical counts or delete tag definitions.
