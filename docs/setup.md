# Homelab setup

## 1. Build and prepare the service account

Install Go 1.25+, Node.js 22+, and Tailscale on the Linux host. Build from this repository:

```sh
go build -trimpath -o bin/scribble ./cmd/scribble
sudo install -m 0755 bin/scribble /usr/local/bin/scribble
sudo useradd --system --create-home --home-dir /var/lib/scribble --shell /bin/bash scribble
sudo install -d -o scribble -g scribble -m 0700 /var/lib/scribble /srv/vault
sudo install -d -m 0750 /etc/scribble
sudo npm install -g obsidian-headless @anthropic-ai/claude-code
command -v ob
command -v claude
```

The supplied units expect executables in `/usr/local/bin`. Adjust `ExecStart` or `claude.binary` if your Node installation uses another prefix. Run Scribble and Headless Sync as the same account so both can update the vault. Do not place `queue_db` inside the synced vault.

## 2. Connect Obsidian Headless

With an active Obsidian Sync subscription, configure the service account's vault:

```sh
sudo -u scribble -H ob login
sudo -u scribble -H ob sync-list-remote
sudo -u scribble -H ob sync-setup --vault 'Your Vault Name' --path /srv/vault
sudo -u scribble -H ob sync --path /srv/vault
```

Verify the first sync has finished and daily notes are present before starting Scribble. Use Headless Sync alone on this host; the desktop app remains on the Mac and Android devices. These commands follow the [Headless Sync documentation](https://obsidian.md/help/sync/headless); [Headless requires Node 22+](https://obsidian.md/help/headless).

## 3. Configure Scribble and model credentials

```sh
sudo install -m 0644 config.example.yaml /etc/scribble/config.yaml
sudo install -m 0600 /dev/null /etc/scribble/environment
sudoedit /etc/scribble/environment
```

Add credentials as systemd environment assignments:

```text
TYPESAFE_API_KEY=your-typesafe-key
ANTHROPIC_API_KEY=your-anthropic-key
```

Jev uses `https://api.typesafe.ai/v1/systemone` via agentcore's Jev judge. `jev.url` is the API base, without `/v1/systemone`. `jev.api_key_env` names the credential variable. Claude runs through agentcore's restricted Claude Code runner, using the configured `claude.model` and `claude.binary`. It receives the relevant text in its prompt and runs in a temporary working directory; it does not write the vault. Temporary sessions are removed after successful calls.

Alternatively, authenticate Claude Code as `scribble` using its supported account login. Keep its home at `/var/lib/scribble`, matching the unit. API key authentication is simpler for an unattended service. Test the installed CLI/account before relying on capture; missing Claude authentication appears as a failed job when a new tag or amendment is needed.

Review `/etc/scribble/config.yaml`:

- `vault`: `/srv/vault`; `queue_db`: `/var/lib/scribble/queue.db`.
- `timezone`: `America/Detroit`. Jobs use the day they are processed, including offline captures.
- `daily_dir` and `date_format`: fallback values when `.obsidian/daily-notes.json` is absent. If present, its `folder` and `format` take precedence. Supported date tokens are `YYYY`, `MM`, `DD`, separators, and bracketed literals; nested date directories work. Unsupported formats fail visibly rather than write under a wrong date.
- `lookback_days`: seven calendar days before today. Existing daily-note templates are left untouched. A missing note gets only the Scribbles heading and its new entry.
- `max_attempts: 3`, `retry_seconds: 5`: initial attempt, then retries after 5 and 10 seconds. The oldest unfinished job holds FIFO during backoff. A terminal failure allows later jobs through; manual retry goes to the back.
- `transcribe.url`: leave empty until the Whisper server is ready. Text processing still works.

`Scribble/Tags.md` starts as an empty markdown table. It grows with model-created tags; edit names and meanings in Obsidian. Keep the four columns and valid nonnegative usage counts. Meanings cannot contain an unescaped table delimiter; use words instead of `|`. Counts measure successful write uses, including amendments; undo does not erase historical usage counts or delete tag definitions.

Restart Scribble after changing its YAML configuration. Daily-note settings and the tag registry are re-read during processing.

## 4. Start the units and expose HTTPS on the tailnet

```sh
sudo install -m 0644 deploy/scribble.service deploy/obsidian-sync.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now obsidian-sync scribble
sudo systemctl status scribble obsidian-sync
curl --fail http://127.0.0.1:8090/v1/health
sudo tailscale serve --bg http://127.0.0.1:8090
tailscale serve status
```

Enable the tailnet HTTPS certificate feature if prompted. [Tailscale Serve](https://tailscale.com/docs/reference/tailscale-cli/serve) proxies this loopback service to tailnet HTTPS. Limit access to your own devices with tailnet grants/ACLs. Do not enable Funnel for this service.

Open the HTTPS URL on Android or desktop, then use the browser's install/Add to Home Screen action. Allow microphone access when recording. On browsers without Background Sync, keep/open the app to drain the outbox; reconnect and visibility changes also trigger sending. An acknowledged job is safely on the server even if the tab closes. A capture saved only on the device remains in IndexedDB until acknowledged, so retain browser site data until it sends.

## 5. Add Whisper on the RTX 3070 host

This step is optional for text capture. The supplied Compose service uses faster-whisper through Speaches, CUDA, and `float16` for the 8 GB RTX 3070.

1. Install a current NVIDIA driver, Docker Engine with Compose, and the [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html). Confirm `nvidia-smi` reports the 3070 and Docker has GPU access. Join this host to the same tailnet.
2. Copy `deploy/whisper.compose.yaml` to that host. Its listener is published only on loopback. Start it:

   ```sh
   docker compose -f whisper.compose.yaml up -d
   docker compose -f whisper.compose.yaml logs -f whisper
   ```

3. Wait for the configured `deepdml/faster-whisper-large-v3-turbo-ct2` model download. This is the faster-whisper distribution of `large-v3-turbo`. `WHISPER__INFERENCE_DEVICE=cuda` and `WHISPER__COMPUTE_TYPE=float16` set the device and precision; `STT_MODEL_TTL=-1` keeps it loaded. See [Speaches installation](https://speaches.ai/installation/) and [configuration](https://speaches.ai/configuration/). The Compose file uses the upstream CUDA tag; pin its image digest on your host after validating it.
4. Verify an actual sample and expose it:

   ```sh
   curl --fail http://127.0.0.1:8000/v1/audio/transcriptions \
     -F 'file=@sample.wav' \
     -F 'model=deepdml/faster-whisper-large-v3-turbo-ct2'
   sudo tailscale serve --bg http://127.0.0.1:8000
   tailscale serve status
   ```

   Speaches uses the full model ID in its [transcription API](https://speaches.ai/usage/speech-to-text/). If your chosen server offers the short `large-v3-turbo` alias, the default model name can stay.
5. On the Scribble host, update:

   ```yaml
   transcribe:
     url: https://whisper.YOUR-TAILNET.ts.net/v1/audio/transcriptions
     model: deepdml/faster-whisper-large-v3-turbo-ct2
     api_key_env: WHISPER_API_KEY
   ```

6. Restart `scribble`, record a short thought, and check its passive confirmation. Retry earlier failed voice jobs in the PWA. If the transcription server requires a bearer key, put `WHISPER_API_KEY=...` in `/etc/scribble/environment`.

The expected model footprint is roughly 3 GB, but measure on this GPU alongside other workloads with `nvidia-smi`. Scribble sends browser-recorded WebM/Opus, MP4, or Ogg with the original MIME type and a matching filename extension. The server must support the browser's codec.

## 6. Tune routing with real usage

Defaults are `amend_confidence: 0.75`, `link_confidence: 0.75`, and `tag_confidence: 0.6`, all under `jev`. These compare against Jev's returned probabilities, not its separate confidence statistic:

- Today's selected target at or above the amend threshold is amended; below it, a new linked entry is written.
- A prior day's selected target is linked only at or above the link threshold. It is never amended.
- Each tag applies at or above the tag threshold. The same threshold gates proposing a new tag.
- A `none` target always adds a new entry.

View decisions and undos:

```sh
journalctl -u scribble --since '7 days ago' -o cat \
  | jq -c 'select(.msg == "route.decision" or .msg == "entry.undo")'
```

Join undo signals to decisions:

```sh
journalctl -u scribble --since '30 days ago' -o cat \
  | jq -s '
      [.[] | select(.msg == "entry.undo") | .job_id] as $undone |
      [.[] | select(.msg == "route.decision") |
        . + {undone: (.job_id as $id | $undone | index($id) != null)}] |
      group_by(.action) |
      map({action: .[0].action, writes: length,
           undos: map(select(.undone)) | length})'
```

Inspect individual undone decisions and their `target_probability`, `tag_probabilities`, `new_tag_probability`, `thresholds`, and latencies. Increase the amend threshold if unrelated thoughts merge. Lower it if genuine same-day follow-ups repeatedly become linked additions. Tune links and tags independently. Undo is a useful signal, not proof that classification was wrong. Restart after changing thresholds and compare a later sample. Keep the defaults until enough real examples accumulate.

## Recovery and operational limits

The queue uses SQLite WAL with full synchronous durability. Both the queue and vault have process locks, so another Scribble process cannot write concurrently. Every note edit is journaled before file changes, uses a same-directory temp file, flushes to disk, checks for intervening edits, and renames atomically. A restart replays the intent: an already-present resulting line or updated tag row is recognized rather than duplicated.

Unrelated note changes are preserved when rebuilding the edit. If someone changed the exact target line or tag row, the job fails instead of replacing their content. Inspect the indicated note/registry before retrying. A failure between the note and registry writes can leave the line present while the job reports failure; retry resumes the same intent and does not create another entry. A prepared write that did not land before midnight is routed again for the new processing day. An already-applied write finishes recovery without altering the past note. An externally changed target remains a visible conflict.

No local filesystem offers compare-and-swap rename against an uncooperative Sync writer. There remains a small interval between the final hash check and rename. Keep Obsidian Sync version history/backups; the service does not claim a distributed transaction with Sync.

Undo also journals its inverse edit. If recovery of an interrupted undo conflicts with an external edit, startup stops and journald identifies the failure; preserve the DB and resolve the note conflict before restarting. Do not delete the database to clear a job: doing so also forgets UUID deduplication and pending write intents.

Back up the vault and queue together while Scribble is stopped, including any SQLite WAL files. Job IDs are retained indefinitely for retry deduplication; completed audio is released, while failed audio stays available for retry. Logs deliberately contain captured thoughts per the design, so set journald access and retention accordingly.

The current Jev API permits 255 target options: `none`, `last`, and at most 253 entries in the context. Oversized context fails visibly; it is not silently truncated. Reduce the configured lookback if necessary. The PWA displays pending/failed captures and only the last completed write; use Obsidian for history.
