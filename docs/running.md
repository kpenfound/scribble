# Running Scribble

This page covers the tailnet node, installing the PWA, managing the systemd user units, tuning the routing thresholds, recovery and limits. For first-time setup see [getting-started.md](getting-started.md).

## The tailnet

Scribble embeds a Tailscale node (tsnet). It joins your tailnet as its own machine, named by `listen.tailnet` (default `scribble`), and serves the app on it. It does not need `tailscaled` on the host.

- **First login.** Set `TS_AUTHKEY` in `~/.config/scribble/environment` before the first start. The node logs in with it and stores its state in `<root>/tailnet` (`~/.local/share/scribble/tailnet`). Later starts use that state, so the key can then be removed. Without a key, the node logs a login URL (`login_url` in the journal) that you can open to approve it, and `scribble status` shows the tailnet as degraded until you do.
- **HTTPS.** With `tailnet_https = true` the node serves on port 443 with a Tailscale certificate. Enable HTTPS certificates for your tailnet in the Tailscale admin console first. Microphone access needs HTTPS. `tailnet_https = false` serves plain HTTP on port 80.
- **The URL.** The app is at `https://scribble.<tailnet>.ts.net`, where `<tailnet>` is your tailnet's name. `scribble status` prints the exact URL once the node runs.
- **Access.** Tailnet access control is the authentication boundary. There is no login. Limit access to your own devices with tailnet grants or ACLs, and do not enable Funnel for this node.
- **Failure.** A tailnet failure at startup or later degrades only the tailnet listener. The service, the socket and `listen.web` keep running, and the reason shows in `scribble status`, `GET /v1/health` and the journal. A degraded node retries by itself.
- **Outbound traffic.** The node only listens. Jev, Claude and Whisper calls go out through the host's own network stack. See [transcription.md](transcription.md).

## Installing the PWA

Open `https://scribble.<tailnet>.ts.net` on your Android phone or desktop, signed in to the tailnet, and use the browser's install or Add to Home Screen action. Allow microphone access when recording.

- The app shell works offline. A capture is saved to IndexedDB on the device before it is sent and stays there until the server acknowledges it, so keep the browser's site data until it sends.
- Where the browser supports Background Sync, queued captures send in the background. Otherwise open the app; reconnecting and returning to the tab also trigger sending.
- An acknowledged capture is safely on the server even if the tab closes. The app shows pending and failed captures and the last completed write, with an Undo button for it.
- Recording stops by itself after two minutes.

## Managing the systemd user units

Scribble runs as the `scribble` systemd user unit under your own account. Obsidian Headless Sync (`ob sync --continuous`) runs as its own unit next to it under the same account; the lab deploys that from the Apiary repository. The `scribble` unit runs `scribble serve` with no `--root`, so the state root is `~/.local/share/scribble` and the config is `~/.config/scribble/config.toml`. The socket is `~/.local/share/scribble/scribble.sock`, mode 0600 and owned by you, so the CLI commands work from your own shell with no `--root`:

```sh
systemctl --user status scribble
journalctl --user -u scribble -f

scribble status
scribble doctor
scribble config
scribble retry <job-id>
scribble undo  <job-id>
```

**Lingering is required.** The user manager runs only while you are logged in unless lingering is on (`loginctl enable-linger "$USER"`, which needs root once or a polkit rule). Without it, Scribble and the sync unit stop when your last session ends and do not start at boot, so capture is down until you log in again. `loginctl show-user "$USER" --property=Linger` should say `Linger=yes`.

**Keep the XDG values the same.** The CLI and the unit must resolve the same root. A shell that sets `XDG_DATA_HOME` or `XDG_CONFIG_HOME` has to give the user manager the same values, for example with `Environment=` lines in a drop-in from `systemctl --user edit scribble`. Otherwise `scribble status` looks at a different root than the service and reports no service. Alternatively, pass `--root PATH` to every command and to the unit's `ExecStart`. See [cli.md](cli.md).

- **Apply a config change:** edit `~/.config/scribble/config.toml`, then `systemctl --user restart scribble`. `scribble config` keeps showing the previous configuration until the restart, because it prints what the running service holds.
- **Stop:** `systemctl --user stop scribble`. `scribble stop` shuts the service down the same way, but systemd then sees a clean exit and does not restart it.
- **Check the environment:** `scribble doctor` runs inside the service, so it reports the `PATH` and keys the service actually has. If `claude` or a key is missing, fix the unit's `Environment=PATH=…` or `~/.config/scribble/environment` and restart. The user manager reads the environment file as you, not as root, so it needs to be readable by you (mode 0600 is right). After editing a unit file, run `systemctl --user daemon-reload`.
- **Health probes:** `curl` the socket with `curl --unix-socket ~/.local/share/scribble/scribble.sock http://scribble/v1/health`. The endpoint is `GET /v1/health`, which reports build information and the tailnet state.
- **Logs:** the service writes JSON to stdout, which systemd sends to the user journal. Read it with `journalctl --user -u scribble`. Logs contain raw captured thoughts by design (`route.decision`), so set journald access and retention accordingly. A service started with `--detach` writes `<root>/service.log` instead.

Update the binary with `install -m 0755 bin/scribble ~/.local/bin/scribble` and `systemctl --user restart scribble`. Jobs survive restarts.

## Tuning the thresholds

The defaults are `jev.amend_threshold = 0.75`, `jev.link_threshold = 0.75` and `jev.tag_threshold = 0.6`. They compare against Jev's returned probabilities, not its separate confidence statistic:

- Today's selected target at or above the amend threshold is amended; below it, a new linked entry is written.
- A prior day's selected target is linked only at or above the link threshold. It is never amended.
- Each tag applies at or above the tag threshold. The same threshold gates proposing a new tag.
- A `none` target always adds a new entry.

Every thought logs one `route.decision` event and every undo logs `entry.undo` with the same `job_id`. The `thresholds` field of `route.decision` names the values in effect as `amend_confidence`, `link_confidence` and `tag_confidence`.

View decisions and undos:

```sh
journalctl --user -u scribble --since '7 days ago' -o cat \
  | jq -c 'select(.msg == "route.decision" or .msg == "entry.undo")'
```

Join undo signals to decisions:

```sh
journalctl --user -u scribble --since '30 days ago' -o cat \
  | jq -s '
      [.[] | select(.msg == "entry.undo") | .job_id] as $undone |
      [.[] | select(.msg == "route.decision") |
        . + {undone: (.job_id as $id | $undone | index($id) != null)}] |
      group_by(.action) |
      map({action: .[0].action, writes: length,
           undos: map(select(.undone)) | length})'
```

Inspect individual undone decisions and their `target_probability`, `tag_probabilities`, `new_tag_probability`, `thresholds` and latencies. Raise `amend_threshold` if unrelated thoughts merge. Lower it if genuine same-day follow-ups repeatedly become linked additions. Tune links and tags independently. Undo is a useful signal, not proof that classification was wrong. Edit `config.toml`, restart, and compare a later sample. Keep the defaults until enough real examples accumulate.

## Recovery

The queue uses SQLite WAL with full synchronous durability. Both the queue and the vault have process locks, so another Scribble process cannot write concurrently. Every note edit is journaled before file changes, uses a same-directory temp file, flushes to disk, checks for intervening edits and renames atomically. A restart replays the intent: an already-present resulting line or updated tag row is recognized rather than duplicated.

Unrelated note changes are preserved when rebuilding the edit. If someone changed the exact target line or tag row, the job fails instead of replacing their content. Inspect the indicated note or registry, then retry from the PWA or with `scribble retry`. A failure between the note and registry writes can leave the line present while the job reports failure; retry resumes the same intent and does not create another entry. A prepared write that did not land before midnight is routed again for the new processing day. An already-applied write finishes recovery without altering the past note. An externally changed target remains a visible conflict.

No local filesystem offers compare-and-swap rename against an uncooperative Sync writer. A small interval remains between the final hash check and the rename. Keep Obsidian Sync version history and backups; Scribble does not claim a distributed transaction with Sync.

Undo also journals its inverse edit. If recovery of an interrupted undo conflicts with an external edit, startup stops and the journal identifies the failure; preserve the database and resolve the note conflict before restarting.

**Do not delete `scribble.db` to clear a stuck job.** Doing so also forgets UUID deduplication and pending write intents. Use `scribble retry` for a failed job.

Back up the vault and the root's `scribble.db` together while Scribble is stopped, including any `-wal` and `-shm` files. Job IDs are retained indefinitely for retry deduplication; completed audio is released, while failed audio stays available for retry.

A tailnet node that cannot start does not need recovery of the data: fix the reason `status` shows and restart. To give the node a new identity, stop the service and remove `<root>/tailnet`, then supply a fresh `TS_AUTHKEY`.

## Limits

- Text captures are limited to 16 KiB (16384 bytes). Audio uploads are limited to 25 MiB, and the PWA stops recording after two minutes.
- Retrying a UUID returns the existing job without changing its payload.
- Jev allows 255 target options: `none`, `last`, and at most 253 entries in the context. Oversized context fails visibly; it is not silently truncated. Reduce `lookback_days` if necessary.
- `lookback_days` is at most 7. Only today's note is ever edited; earlier notes are read and linked to.
- Undo applies only to the most recent completed write, only today, and only if the line is unchanged in the note. The PWA shows pending and failed captures and only the last write; use Obsidian for history.
- Cross-origin writes are rejected on the web and tailnet listeners, and so is any `Host` header they do not serve.
- The state root's socket path must fit the 103-byte Unix socket limit.
