# Voice transcription

Voice capture is optional. With `transcribe.url` empty, text capture works and a voice job fails with a retryable message that the PWA shows; retry it after you configure a server.

Scribble posts the recorded audio to an OpenAI-compatible transcription endpoint (`/v1/audio/transcriptions`) and files the returned text like a typed thought. The server can be faster-whisper behind [Speaches](https://speaches.ai/), whisper.cpp or a hosted API.

## How Scribble reaches the server

The Whisper server runs on a separate tailnet host, ideally one with a GPU. **The Scribble host reaches it through its own network**, not through Scribble's embedded tailnet node, which only listens. So `transcribe.url` is normally a MagicDNS name or a 100.x address of the Whisper host, and the Scribble host must be able to route to it, for example by running `tailscaled` itself:

```toml
[transcribe]
url = "http://whisper.<tailnet>.ts.net:8000/v1/audio/transcriptions"
model = "deepdml/faster-whisper-large-v3-turbo-ct2"
api_key_env = "WHISPER_API_KEY"
```

`url` is the full endpoint that audio is posted to. Tailnet traffic is already encrypted, so plain `http://` to a tailnet address is fine. Check the path from the Scribble host:

```sh
curl -I http://whisper.<tailnet>.ts.net:8000/v1/audio/transcriptions
scribble doctor
```

`doctor` runs in the service's environment and network and passes the `whisper` check when the URL answers over HTTP. Any HTTP status counts as reachable, since a transcription endpoint may refuse `HEAD`.

If the server requires a bearer key, put `WHISPER_API_KEY=…` in `~/.config/scribble/environment` (the variable name is `transcribe.api_key_env`) and run `systemctl --user restart scribble`.

## The Whisper server

The lab deploys the Whisper server from the Apiary repository. Whatever runs it needs to be an OpenAI-compatible `/v1/audio/transcriptions` endpoint, such as faster-whisper behind Speaches, on a tailnet host that the Scribble host can route to. Scribble does not ship or configure the server.

- The server has no authentication of its own unless you configure a bearer key. Bind it to the host's tailnet address rather than the LAN, and restrict who on the tailnet may reach its port with a grant or ACL.
- Verify it with a real sample from the Scribble host:

  ```sh
  curl --fail http://whisper.<tailnet>.ts.net:8000/v1/audio/transcriptions \
    -F 'file=@sample.wav' \
    -F 'model=deepdml/faster-whisper-large-v3-turbo-ct2'
  ```

  `deepdml/faster-whisper-large-v3-turbo-ct2` is the faster-whisper distribution of `large-v3-turbo`. Speaches uses the full model ID in its [transcription API](https://speaches.ai/usage/speech-to-text/). If your server offers the short `large-v3-turbo` alias, the default `transcribe.model` can stay.
- Set `[transcribe]` on the Scribble host as above and restart the service. Record a short thought and check its passive confirmation. Retry earlier failed voice jobs in the PWA or with `scribble retry <job-id>`.

Scribble sends browser-recorded WebM/Opus, MP4 or Ogg with the original MIME type and a matching filename extension. The server must support the browser's codec.

Audio uploads are limited to 25 MiB and the PWA stops recording after two minutes. Completed audio is released from the queue; failed audio stays available for retry.
