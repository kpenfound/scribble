# scribble — design

Nov 5, 2026 · Kyle Penfound

> **Scribble is your scribe for random thoughts.**

## Overview

scribble is an input-only capture app. You send it a raw thought by text or voice, and it files that thought as a tagged entry in today's Obsidian daily note. Obsidian is where you read, search and browse; scribble never displays entries back except the last write.

**Goals**

- One-step capture from phone or desktop, text or voice.
- Each thought is classified and tagged, with tags kept consistent over time.
- A follow-up to an earlier thought from the same day amends that entry instead of adding a new one.
- A follow-up to a thought from the last 7 days adds a cross-link to it.
- Everything runs on the tailnet.

**Non-goals**

- Browsing, searching or reviewing entries. Obsidian does that.
- Editing any daily note other than today's.
- Applying your daily note template.
- A Hearsay integration in v1. Seven days of notes fits in one prompt, and the vault is already the memory.

## Architecture

scribble is a tailnet-only Go service on the homelab box. It embeds its own tailnet node, serves a small PWA frontend and writes into a vault kept current by Obsidian Headless.

```
phone / desktop (PWA) ──tailnet HTTPS──▶ scribble ──▶ vault on disk ◀── ob sync --continuous ──▶ Obsidian Sync
                                           │
                                           ├──▶ Jev API, Claude CLI   (host network)
                                           └──▶ whisper server        (host network, a tailnet address)
```

The PWA talks only to scribble. scribble talks only to the vault on disk, the Jev and Claude APIs, and a whisper server elsewhere on the tailnet. Obsidian Sync carries every change to the Mac and Android apps.

- **Why not native Android:** the Obsidian app has no reliable API for amending a line in place, and an LLM call is needed for each thought anyway, so a backend is unavoidable.
- **Why a PWA:** one client for phone and desktop that installs to the Android home screen. HTTPS from the embedded tailnet node's Tailscale certificate is what makes mic access work.

## Daily note format

Every entry is one bullet under `## Scribbles`, ending in its tags and an Obsidian block ID. The block ID makes the entry addressable, both for amending it today and for linking to it on later days.

```markdown
## Scribbles
- LinkedIn post: overpaying for CI talk — mention Dagger test splitting #idea/linkedin #dagger ^scr-k3f9
- Build a Dagger module for ctest #dagger/module ^scr-p21a
- Factory: support GitLab repos (see [[2026-11-03#^scr-x8q2]]) #project/busybees #feature ^scr-m7c0
```

- **Path:** `Daily/YYYY-MM-DD.md`. If the daily notes config syncs, scribble reads the folder and date format from `.obsidian/daily-notes.json`; otherwise both come from scribble's own config.
- **Today:** the date in America/Detroit, not the server's UTC.
- **Block ID:** `^scr-` plus 4 random base36 characters, unique within the note.
- **Tags:** hierarchical, such as `#idea/linkedin` and `#dagger/module`, placed before the block ID.
- **Cross-link:** `[[YYYY-MM-DD#^scr-xxxx]]`, which points at an entry in an earlier note.

## Processing pipeline

Jev makes every routing and tagging decision. Claude only writes text, and only when an amend or a new tag needs it. Most thoughts never reach Claude.

[Jev](https://typesafe.ai/blog/introducing-system-one-models-and-jev) is TypeSafe's decision model:

- it returns typed answers (a boolean, a choice or a score) with a calibrated probability, and never free text;
- it responds in 70–500 ms;
- it has a 32K context window;
- it costs about $0.042 per million input tokens, and output is free.

scribble calls it through the Jev harness in agentcore. Its calls leave through the host's network, never through the embedded tailnet node.

1. **Transcribe** the input if it's voice, via the tailnet whisper server.
2. **Gather context:**
   - every `^scr-` entry in today's note;
   - entries from the previous 7 daily notes;
   - the tag registry;
   - the last completed write.
3. **Ask Jev**, as one call with parallel atomic questions:
   - *Target (choice):* which earlier scribble does this thought add to or correct? The options are `none`, every candidate entry ID, and the last write.
   - *Tags (one boolean per registry tag):* does `#tag` apply? Each question includes the tag's meaning.
   - *New tag (boolean):* is the thought's main topic missing from the existing tags?
4. **Decide in code:**

| Jev target | Probability | Action |
| --- | --- | --- |
| `none` | any | new |
| entry in today's note | ≥ `amend_threshold` | amend |
| entry in today's note | below | new + link to it |
| entry from an earlier day | ≥ `link_threshold` | new + link |
| entry from an earlier day | below | new |

An entry's tags are every tag with probability ≥ `tag_threshold`. The same threshold gates proposing a new tag.

5. **Call Claude only if needed:**
   - *Amend:* rewrite the target line to merge in the new thought. Keep its tags and links unless the correction changes them.
   - *New tag:* produce a hierarchical tag name, reusing existing prefixes, plus a one-line meaning.
   - *Otherwise:* a new entry uses the input text as-is, trimmed. No LLM writes it.
6. **Write** the note (see Write safety) and update the tag registry.
7. **Return** the resulting line for passive confirmation.

Corrections need no special handling. For "tag that as a talk idea", Jev picks the last write as the target, which triggers an amend, and Claude's rewrite changes the tag.

All three thresholds compare against Jev's calibrated probabilities, so logged values mean the same thing over time and can be tuned from real data.

## Queues and decision logging

Submitting never waits on processing. Thoughts are queued on the phone and on the server, and the server works through them strictly in order.

**PWA offline queue**

- Each thought gets a client-generated UUID and a capture timestamp, and is saved to IndexedDB before it's sent.
- The service worker sends queued items using Background Sync where it's available, and otherwise when the app is opened or the network comes back.
- An item leaves the queue only after the server acknowledges it. The UUID makes retries idempotent, so a retry can't create a duplicate.

**Backend queue**

- `POST /v1/thoughts` persists the job to SQLite and returns `202` with the job ID immediately. Voice uploads are stored with the job.
- A single worker processes jobs FIFO. The queue is non-blocking for the client but serial in processing, because a follow-up must see the entry it refers to.
- Job states are `queued → transcribing → routing → written`, or `failed`. Failed jobs retry with backoff, then stop and are shown in the PWA.
- The PWA follows job state over SSE, which is how the last-write card updates.
- Jobs survive restarts.
- A thought is filed into the note for the day it's processed. If it was captured on an earlier day, it can still link to that day's entries, but past notes are never edited.

**Decision logging**

- All logs use Go `log/slog` with the JSON handler, written to stdout. systemd sends them to journald; a detached service writes them to `<root>/service.log`.
- Each thought emits one `route.decision` event with these fields:
  - `job_id`, `captured_at`, `input`;
  - Jev's target choice and its probability;
  - each tag's probability, and the new-tag probability;
  - the thresholds in effect;
  - the resulting `action`, `target_id`, `tags` and `new_tags`;
  - whether Claude was called;
  - the Jev and Claude latencies in milliseconds.
- Undo emits `entry.undo` with the same `job_id`. That makes undos a direct signal of bad decisions when tuning the thresholds.
- `amend_threshold`, `link_threshold` and `tag_threshold` are read from the `[jev]` table of the config file. None are hardcoded. The `thresholds` field of `route.decision` keeps the keys `amend_confidence`, `link_confidence` and `tag_confidence`, so logged data stays comparable over time.

## Tag registry

The tag registry lives in the vault at `Scribble/Tags.md`, so you can see it and edit it in Obsidian. It's a markdown table:

```markdown
| Tag | Meaning | Uses | Last used |
| --- | --- | --- | --- |
| #idea/linkedin | LinkedIn post ideas | 14 | 2026-11-05 |
| #dagger/module | New Dagger module ideas | 6 | 2026-11-05 |
```

- **Reuse first.** The prompt tells the LLM to prefer an existing tag, and gives it every tag's meaning so it can match on intent, not spelling.
- **New tags** are appended with a one-line meaning written by the LLM.
- **Your edits win.** scribble re-reads the file before every write, so renaming a tag or changing its meaning in Obsidian takes effect on the next thought.
- **Usage counts** are updated with each write. Rarely used tags stay in the registry until you delete them.

## Write safety

scribble never removes or rewrites content it didn't write, and it never touches a note other than today's.

- **Create only if missing.** The file is opened with `O_CREAT|O_EXCL`. A note that already exists is never truncated or replaced. A new note contains only `## Scribbles`.
- **Append the heading.** If today's note exists without `## Scribbles`, the heading is appended at the end of the file and nothing above it changes.
- **Scoped edits.**
  - A **new** entry is inserted as the last bullet of the Scribbles section.
  - An **amend** replaces exactly one line: the one ending in the matching `^scr-` ID.
  - Every other byte of the file stays identical. Before writing, scribble checks this by comparing the old and new file with the edited line removed.
- **Compare-and-swap.** The file is re-read immediately before writing. If its hash differs from the one read at the start of processing, the edit is rebuilt against the new content and retried.
- **Atomic replace.** scribble writes a temp file in the same directory and renames it over the note, so Sync never sees a half-written note.
- **One writer.** All writes go through a single goroutine.
- **Past notes are read-only.** The 7-day window is only read, never written.
- **Sync.** `ob sync --continuous` runs as a separate systemd unit next to scribble, under the same account. Desktop sync is never run on this host.

## Passive confirmation and corrections

scribble writes immediately and never asks first. Under the input box, the PWA shows the result of the last write, and you can ignore it.

```
✎ Amended · 12:41
LinkedIn post: overpaying for CI talk — mention Dagger test splitting
#idea/linkedin #dagger · ^scr-k3f9
```

The card shows:

- the action taken: **Added**, **Amended**, or **Added + linked**;
- the full resulting line, including tags and cross-links;
- for an amend, the previous text struck through, so the change is visible;
- an **Undo** button, which restores the line's previous version or removes a new entry. Undo only works on today's note.

The card stays until the next submission replaces it. Nothing pops up, and you don't have to dismiss it.

While a thought is queued or being processed, the card shows it as pending with its job state, and updates in place when the write lands. Undo always applies to the most recent completed write. `Scribble/Tags.md` starts empty and grows as tags are created.

**Corrections are just more input.** The last entry written is always in the routing context, so typing "no, that's a busybees feature" or "tag that as a talk idea" amends it. There's no separate edit mode.

## Deployment and config

scribble runs on the homelab Linux box as two systemd units under one dedicated `scribble` account, reachable only on the tailnet.

| Unit | Runs | Notes |
| --- | --- | --- |
| `obsidian-sync` | `ob sync --continuous --path /srv/vault` | Obsidian Headless (open beta), Node 22+ |
| `scribble` | `scribble serve --root /var/lib/scribble` | The API, the PWA and its own tailnet node |

Both units run as the same account so both can update the vault. The state root must be outside the synced vault.

### State root

Everything scribble keeps lives in one root directory. `scribble serve --root PATH` makes PATH a self-contained root that holds `config.toml`. Without `--root`, the config is `$XDG_CONFIG_HOME/scribble/config.toml` (falling back to `~/.config`) and the root is `$XDG_DATA_HOME/scribble` (falling back to `~/.local/share`). The systemd unit uses `--root /var/lib/scribble`.

| Path under the root | Holds |
| --- | --- |
| `config.toml` | The configuration (with `--root`). |
| `scribble.db`, `-wal`, `-shm` | The SQLite queue, audio, idempotency keys, retries and write intents. |
| `scribble.db.lock` | The database process lock. |
| `scribble.sock` | The Unix socket the CLI uses. Mode 0600, owned by the service's account. |
| `sessions/` | Claude sessions, removed after successful calls. |
| `writer-*` | Temporary working directories for Claude. |
| `tailnet/` | The embedded node's state. |
| `service.log` | The log of a detached service. |

### Listeners

- **Unix socket** `<root>/scribble.sock`: always on. It serves the public routes with no browser checks, plus the socket-only controls the CLI needs: the loaded configuration, the doctor checks and shutdown. The controls are not public API and are not in `docs/openapi.json`.
- **`listen.web`**: optional, loopback `host:port` only, for local use and tests. It serves the public routes with browser checks.
- **Tailnet**: when `listen.tailnet` is set, scribble starts an embedded tsnet node with that hostname. It serves the same public routes with the same browser checks, on port 443 with a Tailscale certificate when `tailnet_https = true` and on port 80 otherwise. The app is at `https://scribble.<tailnet>.ts.net`. `TS_AUTHKEY` is read for the first login; later starts use the state in `<root>/tailnet`.

Browser checks on the web and tailnet listeners: non-GET requests with a cross-site `Origin` or `Sec-Fetch-Site` are refused, and so is any request whose `Host` header the listener does not serve, which closes DNS rebinding. The Content-Security-Policy stays strict. The socket applies neither check.

A tailnet failure at startup or at runtime degrades only the tailnet listener. The service, the socket and `listen.web` keep running, and `scribble status` and `GET /v1/health` show the tailnet as degraded with a reason.

The embedded node only listens. Jev, Claude and Whisper traffic goes out through the host's network stack, never through the node. `transcribe.url` is therefore normally a MagicDNS name or a 100.x address that the host itself can reach.

### Config

The config is TOML with `version = 1`. Unknown keys, a wrong version, thresholds outside [0,1], `lookback_days` outside 1 to 7 and a non-loopback `listen.web` are rejected with an error of the form `<file>: <field>: <reason>`. Durations are strings. [configuration.md](configuration.md) lists every key and default, and `internal/config/config.example.toml` is the commented example.

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

The `claude` CLI is found on `PATH`; there is no binary setting. Secrets (`TYPESAFE_API_KEY`, `ANTHROPIC_API_KEY`, `TS_AUTHKEY`, `WHISPER_API_KEY`) come from the environment, which systemd fills from an `EnvironmentFile`.

### HTTP API

The public routes are listed in `service.Routes` under the `/v1` prefix, and `docs/openapi.json` is generated from that table by `dagger generate`:

`POST /v1/thoughts`, `GET /v1/thoughts/{id}`, `POST /v1/thoughts/{id}/retry`, `POST /v1/thoughts/{id}/undo`, `GET /v1/status`, `GET /v1/events` (SSE) and `GET /v1/health`.

Failures answer `{"error":{"code","message"}}` with a matching HTTP status. The codes are `validation`, `forbidden`, `not_found`, `conflict`, `unsupported_media_type`, `unavailable` and `internal`.

### CLI

`scribble serve [--detach] [--root PATH]` runs the service. `stop`, `status`, `retry`, `undo`, `config` and `doctor` talk to a running service over its socket and accept `--root PATH` to pick which service. [cli.md](cli.md) documents them.

### Transcription and CI

Transcription runs on a separate tailnet host (ideally one with a GPU) as an OpenAI-compatible transcription server backed by faster-whisper; speaches is one example. scribble posts to the configured endpoint, so the server can be swapped for whisper.cpp or a hosted API by changing the URL. The host is the RTX 3070 machine (8 GB, Ampere), running `large-v3-turbo` in float16 at about 3 GB of VRAM. [transcription.md](transcription.md) has the setup.

CI is Dagger: the workspace's Go, Staticcheck, Playwright and release build checks are the CI contract, run natively by Dagger Cloud Checks (`dagger cloud checks on`).

## Implementation notes

- **Model calls:** use [kpenfound/agentcore](https://github.com/kpenfound/agentcore) v1.2.0 to call both Jev, through its Jev harness, and Claude through its restricted Claude Code runner. The Claude CLI is a service dependency found on `PATH`.
- **Reference project:** follow [kpenfound/osmia](https://github.com/kpenfound/osmia) for:
  - the CLI, the config and state root, the embedded tailnet node, the versioned API and its error envelope;
  - frontend style and components for the PWA.
- **CI:** Dagger. The workspace reuses the shared Go, Staticcheck and Playwright modules, with a local `scribble-dev` module for the release build and browser runtime.

**Testing:** no test, unit or e2e, calls a real Claude, Jev or whisper.

- Jev and Claude are replaced with the mocks from agentcore v1.2.0. Scripted Jev answers include probabilities, so the threshold logic is tested on both sides of each threshold.
- Whisper is replaced with a fake `/v1/audio/transcriptions` server in scribble's test tree that returns fixture text for each audio fixture.
- Playwright e2e tests run the real binary as `scribble serve --root <tmp>` against fake Jev and Whisper HTTP endpoints, configured by URL, and agentcore's fake Claude CLI, placed on `PATH`, so the production client code paths are exercised. The tailnet listener is off. The specs also drive the CLI against that running service.
- No test uses a real tailnet.
- The vault is a temp directory with fixture daily notes. Sync is never run in tests.

## Implementation decisions

Decisions made while building scribble. They are part of the contract.

- Pin agentcore **v1.2.0**, which includes `jevtest` and `agenttest`.
- Claude runs through agentcore's Claude Code runner. Its test double is agentcore's CLI fixture; Jev and Whisper use HTTP fixtures. The CLI is a deployment dependency found on `PATH`.
- Pasted line breaks become spaces so each entry stays one physical line; the remaining input is preserved, trimmed.
- `tag_threshold` gates both the selection of an existing tag and the creation of a new tag.
- The lookback uses calendar days in the configured timezone. Routing identifiers include the daily-note path, because block IDs are unique only within a note; written links keep the daily-note basename format above.
- The oldest unfinished job is retried before FIFO advances. After the retry limit its failure is surfaced and the queue continues. A manual retry joins the end of the queue.
- A durable write intent is persisted before note and registry changes. Recovery recognizes the intended resulting line and tag rows. Conflicting user edits fail visibly instead of being overwritten. An unapplied intent crossing midnight is routed again for the new processing day. Undo follows the same journaled path and keeps the registry's historical usage counts and tag definitions.
- Scribble listens on a Unix socket, an optional loopback address and its own tailnet node. Tailnet access control is the authentication boundary. There is no public listener, login or Funnel. The vault has one service writer, protected by a process lock.
- Hash checks and atomic rename protect against partial writes and detect intervening edits. The file is re-read immediately before the rename and unrelated changes are rebased. A filesystem cannot compare-and-swap a rename against an uncooperative external Sync writer, so a small interval between the final check and the rename remains. This is documented, not hidden: scribble does not claim a distributed filesystem transaction. Keep Obsidian Sync version history.
- CI is the workspace checks under Dagger Cloud Checks.
