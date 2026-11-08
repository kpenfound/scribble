package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteErrorEnvelopeAndStatus(t *testing.T) {
	for code, status := range map[Code]int{
		Validation:       http.StatusBadRequest,
		Forbidden:        http.StatusForbidden,
		NotFound:         http.StatusNotFound,
		Conflict:         http.StatusConflict,
		UnsupportedMedia: http.StatusUnsupportedMediaType,
		Unavailable:      http.StatusServiceUnavailable,
		Internal:         http.StatusInternalServerError,
	} {
		w := httptest.NewRecorder()
		WriteError(w, code, "because")
		if w.Code != status {
			t.Errorf("%s: status %d, want %d", code, w.Code, status)
		}
		if w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: content type %q", code, w.Header().Get("Content-Type"))
		}
		var raw map[string]map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		if len(raw) != 1 || len(raw["error"]) != 2 || raw["error"]["code"] != string(code) || raw["error"]["message"] != "because" {
			t.Errorf("%s: body %s", code, w.Body.String())
		}
	}
}

func TestPrefixAndErrorString(t *testing.T) {
	if Prefix != "/v1" {
		t.Fatal(Prefix)
	}
	if got := Errorf(Conflict, "no %s", "way").Error(); got != "conflict: no way" {
		t.Fatal(got)
	}
	if Code("unknown").Status() != http.StatusInternalServerError {
		t.Fatal("unknown code should be 500")
	}
}
