// Package transcribe sends captured audio to an OpenAI-style Whisper endpoint.
package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strings"
	"time"

	"github.com/kpenfound/scribble/internal/queue"
)

// Transcriber is what the service needs from transcription; tests inject fakes.
type Transcriber interface {
	Transcribe(ctx context.Context, audio []byte, mime string) (string, error)
}

// Whisper calls the configured endpoint over the host's network stack.
type Whisper struct {
	URL       string
	Model     string
	APIKeyEnv string
	Client    *http.Client
}

func New(url, model, apiKeyEnv string) *Whisper {
	return &Whisper{URL: url, Model: model, APIKeyEnv: apiKeyEnv, Client: &http.Client{Timeout: 2 * time.Minute}}
}

func (m *Whisper) Transcribe(ctx context.Context, audio []byte, mime string) (string, error) {
	if m.URL == "" {
		return "", errors.New("voice is not configured: set transcribe.url; text capture is available")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	ext := "webm"
	switch strings.Split(mime, ";")[0] {
	case "audio/mp4":
		ext = "m4a"
	case "audio/ogg":
		ext = "ogg"
	case "audio/wav", "audio/x-wav":
		ext = "wav"
	case "audio/mpeg":
		ext = "mp3"
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="capture.`+ext+`"`)
	h.Set("Content-Type", mime)
	part, err := writer.CreatePart(h)
	if err != nil {
		return "", err
	}
	if _, err = part.Write(audio); err != nil {
		return "", err
	}
	if err = writer.WriteField("model", m.Model); err != nil {
		return "", err
	}
	if err = writer.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.URL, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if key := os.Getenv(m.APIKeyEnv); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := m.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("transcription returned HTTP %d", resp.StatusCode)
	}
	var result struct {
		Text string `json:"text"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	text := queue.NormalizeText(result.Text)
	if text == "" {
		return "", errors.New("transcription was empty")
	}
	if len(text) > queue.MaxText {
		return "", errors.New("transcription exceeds text limit")
	}
	return text, nil
}
