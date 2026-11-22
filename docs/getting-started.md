# Getting started

This page takes a Linux host from nothing to a running Scribble you can capture into from your phone. It covers the build, Obsidian Headless Sync, configuration, lingering and the first start, all under your own account. [running.md](running.md) covers day-to-day operation and [transcription.md](transcription.md) adds voice.

You need:

- A Linux host with systemd that stays on, and an account on it that you can run services under. Scribble embeds its own tailnet node, so the host does not need `tailscaled` to serve the app. Voice is the exception: the Whisper server is reached through the host's own network, so for a Whisper server on the tailnet the host needs a route to it, such as `tailscaled` on the host. See [transcription.md](transcription.md).
- An Obsidian Sync subscription, for [Obsidian Headless](https://obsidian.md/help/headless).
- A TypeSafe API key for Jev.
- [Claude Code](https://docs.anthropic.com/en/docs/claude-code) (the `claude` CLI), with an Anthropic API key or a login. It writes amendments and new tag definitions.
- A Tailscale auth key for the first login. Create a reusable-or-single-use key in the Tailscale admin console.

## 1. Build and install

Build with Go 1.26 or newer and install the binary under your home directory:

```sh
go build -trimpath -o bin/scribble ./cmd/scribble
install -d -m 0755 ~/.local/bin
install -m 0755 bin/scribble ~/.local/bin/scribble
```

Make sure `~/.local/bin` is on your shell's `PATH`, so the plain `scribble` commands below find the binary. The unit names the binary by its full path.

A plain `go build` reports version `dev`. The Dagger release build stamps the version, commit and date that `scribble --version` prints:

```sh
dagger api call scribble-dev build --version v0.1.0 --commit "$(git rev-parse --short HEAD)" --date "$(date -u +%Y-%m-%dT%H:%M:%SZ)" export --path ./bin/scribble-linux-amd64
```

## 2. Create directories and install the tools

Everything lives under your home directory and runs as you. No root and no extra account are needed:

```sh
install -d -m 0700 ~/vault ~/.config/scribble
npm config set prefix ~/.local
npm install -g obsidian-headless @anthropic-ai/claude-code
command -v ob
command -v claude
```

`~/vault` is the vault. The state root is `~/.local/share/scribble`, which Scribble creates on first start. Your own Claude Code login stays in your home directory.

`npm config set prefix ~/.local` makes global installs land in `~/.local/bin`, which is where `scribble.service` looks. If Node uses another prefix, for example a version manager, or you installed the tools globally under `/usr/local`, add the directory holding `claude` to `PATH` in `scribble.service`. Whatever runs `ob sync --continuous` needs to find `ob` the same way; `command -v ob` prints its path. Scribble and the sync service run as you, so both can update the vault. Keep the state root outside the synced vault.

## 3. Connect Obsidian Headless

With an active Obsidian Sync subscription, configure the vault as yourself:

```sh
ob login
ob sync-list-remote
ob sync-setup --vault 'Your Vault Name' --path ~/vault
ob sync --path ~/vault
```

Verify the first sync has finished and daily notes are present before starting Scribble. This is the one-time setup; keeping the vault current afterwards is a separate continuous sync service. The lab deploys Obsidian Headless Sync from the Apiary repository. Whatever runs it needs to run `ob sync --continuous --path ~/vault` as the same account as Scribble, after the setup above. Use Headless Sync alone on this host; the desktop app stays on your Mac and Android devices. These commands follow the [Headless Sync documentation](https://obsidian.md/help/sync/headless), and [Headless requires Node 22+](https://obsidian.md/help/headless).

## 4. Configure Scribble

Copy the example config to `~/.config/scribble/config.toml` and set the vault. Every other value is a default; see [configuration.md](configuration.md).

```sh
install -m 0600 internal/config/config.example.toml ~/.config/scribble/config.toml
${EDITOR:-nano} ~/.config/scribble/config.toml
```

The example says `vault = "/home/<you>/vault"`. Substitute your own home directory for `/home/<you>`; `vault` must be an absolute path, so `~` does not work. Also check `timezone`. Leave `listen.tailnet = "scribble"` to get `https://scribble.<tailnet>.ts.net`.

Put the secrets in the unit's environment file:

```sh
install -m 0600 /dev/null ~/.config/scribble/environment
${EDITOR:-nano} ~/.config/scribble/environment
```

```text
TYPESAFE_API_KEY=your-typesafe-key
ANTHROPIC_API_KEY=your-anthropic-key
TS_AUTHKEY=tskey-auth-your-key
```

The file is mode 0600 and owned by you. The systemd user manager runs as you, not as root, and reads it as you. `TS_AUTHKEY` is used only for the node's first login; once `~/.local/share/scribble/tailnet` holds the node's state you can remove it. Omit `ANTHROPIC_API_KEY` if you log in to Claude Code instead by running `claude` as yourself; API key authentication is simpler for an unattended service. A missing Claude login shows up only as a failed job when an amendment or new tag is needed, so test it before relying on capture.

Jev is called at `jev.url` with the key named by `jev.api_key_env`. Claude runs through agentcore's restricted Claude Code runner with the model in `[profiles.default]`. It receives the relevant text in its prompt, runs in a temporary working directory and does not write the vault.

## 5. Enable lingering

A user manager normally exists only while you are logged in. Lingering keeps it running without a login, so the services start at boot and survive logout:

```sh
loginctl enable-linger "$USER"
```

This is the one privileged step. It needs root once, or a polkit rule that lets you do it yourself; if your account is not allowed, ask an administrator to run it for you. [running.md](running.md#managing-the-systemd-user-units) says what breaks without it.

## 6. Start the service

```sh
install -d -m 0755 ~/.config/systemd/user
install -m 0644 deploy/scribble.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now scribble
systemctl --user status scribble
```

The unit runs `scribble serve` with no `--root`, so the service uses the default XDG root and config. If your shell sets `XDG_DATA_HOME` or `XDG_CONFIG_HOME`, the user manager must see the same values, or your shell and the service resolve different roots and `scribble status` reaches nothing. Either set the variables for the user manager too (`systemctl --user import-environment XDG_DATA_HOME XDG_CONFIG_HOME` for the running session, or `Environment=` lines in a drop-in made with `systemctl --user edit scribble`), or pass the same `--root PATH` to every command and in the unit's `ExecStart`.

Check the service from your own shell, since the socket is yours:

```sh
scribble status
scribble doctor
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
