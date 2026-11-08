package transcribe

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTranscriptionHTTPAndMissingConfiguration(t *testing.T) {
	m := New("", "large-v3-turbo", "WHISPER_API_KEY")
	j := struct {
		Audio []byte
		MIME  string
	}{[]byte("audio fixture"), "audio/webm"}
	if _, err := m.Transcribe(context.Background(), j.Audio, j.MIME); err == nil {
		t.Fatal("missing endpoint did not fail")
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Error("wrong path")
		}
		if err := r.ParseMultipartForm(1024); err != nil {
			t.Error(err)
		}
		defer r.MultipartForm.RemoveAll()
		f, h, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			http.Error(w, "missing file", 400)
			return
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		if string(b) != "audio fixture" || h.Header.Get("Content-Type") != "audio/webm" || r.FormValue("model") != "large-v3-turbo" {
			t.Error("invalid transcription request")
		}
		_, _ = io.WriteString(w, `{"text":"spoken\nthought"}`)
	}))
	defer endpoint.Close()
	m.URL = endpoint.URL + "/v1/audio/transcriptions"
	text, err := m.Transcribe(context.Background(), j.Audio, j.MIME)
	if err != nil || text != "spoken thought" {
		t.Fatalf("transcription %q %v", text, err)
	}
}
