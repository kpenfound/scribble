# Implementation plan

The product contract is [DESIGN.md](DESIGN.md).

1. Build validated YAML configuration, Obsidian daily-note settings, and a byte-preserving vault editor. Test dates, section boundaries, collisions, concurrent edits, and undo conflicts.
2. Persist submissions, audio, retry schedules, write intents, and confirmation state in SQLite. Run one FIFO worker, including undo, and recover interrupted writes without duplicating them.
3. Integrate agentcore v1.1.0's Jev judge and restricted Claude runner, plus OpenAI-compatible Whisper transcription. Test calibrated probabilities using agentcore mocks and fake HTTP transcription.
4. Embed a small PWA with an IndexedDB outbox, Background Sync, microphone recording, SSE progress, passive confirmation, and undo. Follow Osmia's warm neutral palette and native controls.
5. Exercise the real binary with Playwright and fake providers. Add Dagger build/test/lint checks, systemd units, and step-by-step setup and tuning instructions.

## Resolved decisions

- Pin agentcore **v1.1.0**, which includes `jevtest` and `agenttest`.
- Claude runs through agentcore's Claude Code runner. Its test double is agentcore's CLI fixture; Jev and Whisper use HTTP fixtures. The CLI is a deployment dependency.
- Replace input line breaks with spaces so each entry remains one physical line; otherwise preserve trimmed input.
- Use configured `tag_confidence` for both existing-tag selection and creation of a new tag.
- Use calendar days in the configured timezone for the seven-day lookback, and qualify routing IDs with the note path because block IDs are unique only within a note.
- Retry the oldest unfinished job before advancing FIFO. After the retry limit, surface its failure and continue. Manual retry joins the end of the queue.
- Persist a write intent before changing files. Recovery recognizes the intended resulting line and tag rows. Conflicting user edits fail visibly instead of overwriting them. Undo follows the same journaled path.
- Only bind the API to loopback; Tailscale Serve supplies tailnet access and HTTPS. The vault has one service writer, protected by a process lock.
- CI uses native Dagger Cloud Checks for this repository, configured with `dagger cloud checks on`; the workspace's Go, Staticcheck, Playwright, and build checks are the CI contract.
- Filesystem rename cannot atomically compare a file against a concurrently writing external Sync process. Re-read immediately before rename and rebase unrelated changes; document this remaining external-writer race rather than promise a filesystem transaction that does not exist.
