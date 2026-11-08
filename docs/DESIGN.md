# scribble — design

Nov 5, 2026 · @Kyle Penfound

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

scribble is a tailnet-only Go service on the homelab box. It has a small PWA frontend and writes into a vault kept current by Obsidian Headless.

&#91;embedded content: scribble architecture · tailnet service + headless sync\]

The PWA talks only to scribble. scribble talks only to the vault on disk, the Jev and Claude APIs, and a whisper server elsewhere on the tailnet. Obsidian Sync carries every change to the Mac and Android apps.

- **Why not native Android:** the Obsidian app has no reliable API for amending a line in place, and an LLM call is needed for each thought anyway, so a backend is unavoidable.
- **Why a PWA:** one client for phone and desktop that installs to the Android home screen. HTTPS from `tailscale serve` is what makes mic access work.

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

scribble calls it through the Jev harness in agentcore.

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
| entry in today's note | ≥ `amend_confidence` | amend |
| entry in today's note | below | new + link to it |
| entry from an earlier day | ≥ `link_confidence` | new + link |
| entry from an earlier day | below | new |

An entry's tags are every tag with probability ≥ `tag_confidence`.

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

- All logs use Go `log/slog` with the JSON handler, written to stdout and collected by journald.
- Each thought emits one `route.decision` event with these fields:
  - `job_id`, `captured_at`, `input`;
  - Jev's target choice and its probability;
  - each tag's probability, and the new-tag probability;
  - the thresholds in effect;
  - the resulting `action`, `target_id`, `tags` and `new_tags`;
  - whether Claude was called;
  - the Jev and Claude latencies in milliseconds.
- Undo emits `entry.undo` with the same `job_id`. That makes undos a direct signal of bad decisions when tuning the thresholds.
- `amend_confidence`, `link_confidence` and `tag_confidence` are read from the config file. None are hardcoded.

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
- **Sync.** `ob sync --continuous` runs as a separate systemd unit next to scribble. Desktop sync is never run on this host.

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

scribble runs on the homelab Linux box as two systemd units, reachable only on the tailnet.

| Unit | Runs | Notes |
| --- | --- | --- |
| `obsidian-sync` | `ob sync --continuous --path /srv/vault` | Obsidian Headless (open beta), Node 22+ |
| `scribble` | Go binary serving the API and the PWA | `tailscale serve` provides HTTPS, which browsers require for mic access |

Transcription runs on a separate tailnet host (ideally one with a GPU) as an OpenAI-compatible transcription server backed by faster-whisper; speaches is one example. scribble calls \`/v1/audio/transcriptions\`, so the server can be swapped for whisper.cpp or a hosted API by changing the URL. The host is the RTX 3070 machine (8 GB, Ampere), running \`large-v3-turbo\` in float16 at about 3 GB of VRAM. CI is a Dagger module that builds the binary and the PWA.

```yaml
vault: /srv/vault
daily_dir: Daily
date_format: YYYY-MM-DD
timezone: America/Detroit
lookback_days: 7
jev:
  model: jev-latest
  amend_confidence: 0.75
  link_confidence: 0.75
  tag_confidence: 0.6
claude:
  model: claude-haiku-5-5
queue_db: /var/lib/scribble/queue.db
transcribe:
  url: https://whisper.<tailnet>.ts.net/v1/audio/transcriptions
  model: large-v3-turbo  # float16 on the RTX 3070 host
log_level: info
```

## Implementation notes

- **Model calls:** use [kpenfound/agentcore](https://github.com/kpenfound/agentcore) v1.1.0 to call both Jev, through its Jev harness, and Claude through its restricted Claude Code runner. The Claude CLI is a service dependency.
- **Reference project:** follow [kpenfound/osmia](https://github.com/kpenfound/osmia) for:
  - `tailscale serve` setup;
  - frontend style and components for the PWA.
- **CI:** Dagger. Use existing Dagger modules where they fit, including:
  - Go: build, test and lint;
  - Playwright: PWA end-to-end tests;
  - Android, if a native client is ever added.

**Testing:** no test, unit or e2e, calls a real Claude, Jev or whisper.

- Jev and Claude are replaced with the mocks from agentcore v1.1.0. Scripted Jev answers include probabilities, so the threshold logic is tested on both sides of each threshold.
- Whisper is replaced with a fake `/v1/audio/transcriptions` server in scribble's test tree that returns fixture text for each audio fixture.
- Playwright e2e tests run the real binary against fake Jev and Whisper HTTP endpoints, configured by URL, and agentcore's fake Claude CLI, configured by executable path, so the production client code paths are exercised.
- The vault is a temp directory with fixture daily notes. Sync is never run in tests.

## Follow-ups

Neither of these blocks implementation. Both must be covered step by step in the repo's setup instructions (README or `docs/setup.md`), and the defaults must work until they're done.

- [ ] **Whisper server.** Setup instructions: install the faster-whisper server on the RTX 3070 host with `float16` and `large-v3-turbo`, expose it with `tailscale serve`, and set `transcribe.url`. Until it exists, text input works and voice jobs fail visibly in the PWA.
- [ ] **Threshold tuning.** Setup instructions: explain the three Jev thresholds and how to query the `route.decision` and `entry.undo` logs (a `journalctl` + `jq` example). Ship with the defaults, and revisit once there's real data.

## Implementation decisions

The implementation plan and recovery details are in [IMPLEMENTATION.md](IMPLEMENTATION.md). [setup.md](setup.md) covers deployment, the optional Whisper host, and threshold tuning step by step.

- Pasted line breaks become spaces; the remaining input is preserved, trimmed.
- `tag_confidence` also gates creating a new tag.
- Routing identifiers include the daily-note path to distinguish block IDs repeated across different notes; written links retain the daily-note basename format above.
- A durable write intent precedes note and registry changes. Recovery recognizes applied lines and tag rows. An unapplied intent crossing midnight is routed again for the new processing day.
- Undo restores/removes the entry while keeping the registry's historical usage counts and tag definitions.
- Hash checks and atomic rename protect against partial writes and detect intervening edits. An external Sync process can still write in the interval between the final check and rename; this is not a distributed filesystem transaction.
