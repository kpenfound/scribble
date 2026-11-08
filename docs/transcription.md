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
sudo -u scribble scribble doctor --root /var/lib/scribble
```

`doctor` runs in the service's environment and network and passes the `whisper` check when the URL answers over HTTP. Any HTTP status counts as reachable, since a transcription endpoint may refuse `HEAD`.

If the server requires a bearer key, put `WHISPER_API_KEY=…` in `/etc/scribble/environment` (the variable name is `transcribe.api_key_env`) and restart.

## Setting up the server on an RTX 3070 host

The supplied Compose service, [`deploy/whisper.compose.yaml`](../deploy/whisper.compose.yaml), uses faster-whisper through Speaches, CUDA and `float16` for the 8 GB RTX 3070.

1. Install a current NVIDIA driver, Docker Engine with Compose and the [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html). Confirm `nvidia-smi` reports the GPU and Docker has GPU access. Join the host to the same tailnet.
2. Copy `deploy/whisper.compose.yaml` to the host. It publishes the server on loopback unless you set `WHISPER_BIND` to the host's tailnet address, which is what lets the Scribble host reach it:

   ```sh
   export WHISPER_BIND="$(tailscale ip -4)"
   docker compose -f whisper.compose.yaml up -d
   docker compose -f whisper.compose.yaml logs -f whisper
   ```

   The server has no authentication of its own. Binding it to the tailnet address keeps it off the LAN; restrict who on the tailnet may reach port 8000 with a grant or ACL.
3. Wait for the configured `deepdml/faster-whisper-large-v3-turbo-ct2` model download. This is the faster-whisper distribution of `large-v3-turbo`. `WHISPER__INFERENCE_DEVICE=cuda` and `WHISPER__COMPUTE_TYPE=float16` set the device and precision, and `STT_MODEL_TTL=-1` keeps the model loaded. See [Speaches installation](https://speaches.ai/installation/) and [configuration](https://speaches.ai/configuration/). The Compose file uses the upstream CUDA tag; pin its image digest on your host after validating it.
4. Verify with a real sample from the Whisper host, then from the Scribble host:

   ```sh
   curl --fail http://$WHISPER_BIND:8000/v1/audio/transcriptions \
     -F 'file=@sample.wav' \
     -F 'model=deepdml/faster-whisper-large-v3-turbo-ct2'
   ```

   Speaches uses the full model ID in its [transcription API](https://speaches.ai/usage/speech-to-text/). If your server offers the short `large-v3-turbo` alias, the default `transcribe.model` can stay.
5. Set `[transcribe]` on the Scribble host as above, restart the service, record a short thought and check its passive confirmation. Retry earlier failed voice jobs in the PWA or with `scribble retry <job-id>`.

The expected model footprint is roughly 3 GB, but measure on your GPU alongside other workloads with `nvidia-smi`. Scribble sends browser-recorded WebM/Opus, MP4 or Ogg with the original MIME type and a matching filename extension. The server must support the browser's codec.

Audio uploads are limited to 25 MiB and the PWA stops recording after two minutes. Completed audio is released from the queue; failed audio stays available for retry.
