package main

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPrettyHandshake(t *testing.T) {
	diff := []byte("synthetic diff containing a unicode character: 家\n")
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer hk_live_test" {
			t.Error("missing API key")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/pretty":
			var request prettyRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			compressed, err := base64.StdEncoding.DecodeString(request.Diff)
			if err != nil {
				t.Error(err)
				return
			}
			reader, err := gzip.NewReader(strings.NewReader(string(compressed)))
			if err != nil {
				t.Error(err)
				return
			}
			defer reader.Close()
			text, err := io.ReadAll(reader)
			if err != nil || string(text) != string(diff) {
				t.Errorf("incorrect diff upload: %q, %v", text, err)
			}
			if request.Mode != "dryrun" || request.File != "hello.txt" {
				t.Error("options lost")
			}
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"request_key":"req_test","poll_after_ms":1}`)
		case "GET /v1/pretty/req_test":
			if polls.Add(1) == 1 {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			io.WriteString(w, `{"output":"patch","commands":[],"review":"Looking good.","meta":{"model":"Brawny-1","millis":1}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	t.Setenv("HUNKY_CLOUD_URL", server.URL)
	t.Setenv("HUNKY_CLOUD_KEY", "hk_live_test")
	result, err := pretty(prettyRequest{Mode: "dryrun", File: "hello.txt", Diff: compress(diff)}, diff)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "patch" || result.Review != "Looking good." || polls.Load() != 2 {
		t.Fatalf("incorrect result: %#v", result)
	}
}

func TestPrettyReportsUnavailableCreditWithoutPolling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Error("unexpected polling after failure")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		io.WriteString(w, `{"error":"balance exhausted","detail":"Preview credit exhausted","recharge_url":"https://example.invalid/billing"}`)
	}))
	defer server.Close()
	t.Setenv("HUNKY_CLOUD_URL", server.URL)
	t.Setenv("HUNKY_CLOUD_KEY", "hk_live_test")
	_, err := pretty(prettyRequest{Diff: compress([]byte("diff"))}, []byte("diff"))
	if err == nil || !strings.Contains(err.Error(), "Preview credit exhausted") {
		t.Fatalf("unexpected error %v", err)
	}
}
