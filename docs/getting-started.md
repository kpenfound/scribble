# Getting started

This page takes a Linux host from nothing to a running Scribble you can capture into from your phone. It covers the build, the service account, Obsidian Headless Sync, configuration and the first start. [running.md](running.md) covers day-to-day operation and [transcription.md](transcription.md) adds voice.

You need:

- A Linux host with systemd that stays on. Scribble embeds its own tailnet node, so the host does not need `tailscaled` to serve the app. Voice is the exception: the Whisper server is reached through the host's own network, so for a Whisper server on the tailnet the host needs a route to it, such as `tailscaled` on the host. See [transcription.md](transcription.md).
- An Obsidian Sync subscription, for [Obsidian Headless](https://obsidian.md/help/headless).
- A TypeSafe API key for Jev.
- [Claude Code](https://docs.anthropic.com/en/docs/claude-code) (the `claude` CLI), with an Anthropic API key or a login. It writes amendments and new tag definitions.
- A Tailscale auth key for the first login. Create a reusable-or-single-use key in the Tailscale admin console.

## 1. Build and install

Build with Go 1.26 or newer:

```sh
go build -trimpath -o bin/scribble ./cmd/scribble
sudo install -m 0755 bin/scribble /usr/local/bin/scribble
```

A plain `go build` reports version `dev`. The Dagger release build stamps the version, commit and date that `scribble --version` prints:

```sh
dagger api call scribble-dev build --version v0.1.0 --commit "$(git rev-parse --short HEAD)" --date "$(date -u +%Y-%m-%dT%H:%M:%SZ)" export --path ./bin/scribble-linux-amd64
```

## 2. Create the service account and directories

```sh
sudo useradd --system --create-home --home-dir /var/lib/scribble --shell /bin/bash scribble
sudo install -d -o scribble -g scribble -m 0700 /var/lib/scribble /srv/vault
sudo install -d -m 0750 /etc/scribble
sudo npm install -g obsidian-headless @anthropic-ai/claude-code
command -v ob
command -v claude
```

`/var/lib/scribble` is the state root and also the account's home. Claude Code keeps its login there. `/srv/vault` is the vault.

The supplied units expect executables in `/usr/local/bin`. If your Node installation uses another prefix, adjust `ExecStart` for `ob` in `obsidian-sync.service`, and add the directory holding `claude` to `PATH` in `scribble.service`. Run Scribble and Headless Sync as the same account so both can update the vault, and keep the state root outside the synced vault.

## 3. Connect Obsidian Headless

With an active Obsidian Sync subscription, configure the service account's vault:

```sh
sudo -u scribble -H ob login
sudo -u scribble -H ob sync-list-remote
sudo -u scribble -H ob sync-setup --vault 'Your Vault Name' --path /srv/vault
sudo -u scribble -H ob sync --path /srv/vault
```

Verify the first sync has finished and daily notes are present before starting Scribble. Use Headless Sync alone on this host; the desktop app stays on your Mac and Android devices. These commands follow the [Headless Sync documentation](https://obsidian.md/help/sync/headless), and [Headless requires Node 22+](https://obsidian.md/help/headless).

## 4. Configure Scribble

Copy the example config into the root and set the vault. Every other value is a default; see [configuration.md](configuration.md).

```sh
sudo -u scribble install -m 0600 internal/config/config.example.toml /var/lib/scribble/config.toml
sudo -u scribble ${EDITOR:-nano} /var/lib/scribble/config.toml
```

At minimum, check `vault = "/srv/vault"` and `timezone`. Leave `listen.tailnet = "scribble"` to get `https://scribble.<tailnet>.ts.net`.

Put the secrets in the unit's environment file:

```sh
sudo install -m 0600 /dev/null /etc/scribble/environment
sudoedit /etc/scribble/environment
```

```text
TYPESAFE_API_KEY=your-typesafe-key
ANTHROPIC_API_KEY=your-anthropic-key
TS_AUTHKEY=tskey-auth-your-key
```

systemd reads the file as root, so the `scribble` account does not need access to it. `TS_AUTHKEY` is used only for the node's first login; once `/var/lib/scribble/tailnet` holds the node's state you can remove it. Omit `ANTHROPIC_API_KEY` if you log in to Claude Code as `scribble` instead (`sudo -u scribble -H claude`); API key authentication is simpler for an unattended service. A missing Claude login shows up only as a failed job when an amendment or new tag is needed, so test it before relying on capture.

Jev is called at `jev.url` with the key named by `jev.api_key_env`. Claude runs through agentcore's restricted Claude Code runner with the model in `[profiles.default]`. It receives the relevant text in its prompt, runs in a temporary working directory and does not write the vault.

## 5. Start the units

```sh
sudo install -m 0644 deploy/scribble.service deploy/obsidian-sync.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now obsidian-sync scribble
sudo systemctl status scribble obsidian-sync
```

Check the service as the `scribble` account, since the socket belongs to it:

```sh
sudo -u scribble scribble status --root /var/lib/scribble
sudo -u scribble scribble doctor --root /var/lib/scribble
```

`status` shows `Health: ok` and a `Tailnet:` line. On the first start the node needs a moment to log in; the line moves from `starting` to `running https://scribble.<tailnet>.ts.net`. If it says `degraded`, the reason names the fix, such as enabling HTTPS certificates for your tailnet in the Tailscale admin console or supplying `TS_AUTHKEY`. `doctor` checks the vault, the daily-notes settings, `claude`, the Jev key, the Whisper URL and the tailnet in the service's own environment.

Open the app URL on your phone or desktop and install it. [running.md](running.md#installing-the-pwa) has the steps.

## Trying it locally without a tailnet

For development, or to see it work before joining a tailnet, use a throwaway root with only the loopback listener:

```sh
mkdir -p .local/vault
cat > .local/config.toml <<'TOML'
version = 1
vault = "/absolute/path/to/.local/vault"

[listen]
web = "127.0.0.1:8090"
tailnet = ""
TOML
export TYPESAFE_API_KEY='your-key'
export ANTHROPIC_API_KEY='your-key'
./bin/scribble serve --root .local
```

Open `http://127.0.0.1:8090`. Browsers allow the microphone on `localhost` but not on plain HTTP elsewhere. In another terminal, `./bin/scribble status --root .local` talks to the same service. No Whisper setup is needed for text; a voice job fails visibly with a retryable message and can be retried once [transcription](transcription.md) is configured.
