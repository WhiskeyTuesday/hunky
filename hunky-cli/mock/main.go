// hunky-cloud-mock — a local stand-in for the Hunky Cloud backend.
//
// Implements the /v1/pretty two-call handshake so `hunky-cli` can be developed
// end-to-end with zero infra. It does the genuinely-real part (shells out to the
// real `hunky` binary on the uploaded diff, exactly as the eventual Elixir/Fly
// backend will) and stubs the part that needs a key (Brawny-1's review — canned
// lines here; OpenRouter later).
//
//	HUNKY_BIN  path to the hunky binary (default: "hunky" on PATH)
//	PORT       listen port (default 8787)
//	MOCK_402   if set, every /v1/pretty submit returns 402 payment_required
//	           (also per-request via the X-Mock-402 header) — exercises the
//	           CLI's billing path
//
//	go build -o hunky . && PATH=.:$PATH go run ./hunky-cli/mock
//
// The wire contract here mirrors the real backend's PrettyController
// (cloud/app/lib/hunky_web/controllers/pretty_controller.ex): same handshake,
// status codes, and JSON error shapes. Keep the two in sync.
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type prettyRequest struct {
	Mode      string `json:"mode"`
	File      string `json:"file"`
	Hunks     []int  `json:"hunks"`
	Lines     string `json:"lines"`
	PickLines string `json:"pick_lines"`
	Invert    bool   `json:"invert"`
	Diff      string `json:"diff"`
}

type prettyResult struct {
	Output   string   `json:"output"`
	Commands []string `json:"commands"`
	Review   string   `json:"review"`
	Meta     struct {
		Model  string `json:"model"`
		Millis int    `json:"millis"`
	} `json:"meta"`
}

type job struct {
	readyAt time.Time
	result  prettyResult
}

var (
	mu      sync.Mutex
	jobs    = map[string]job{}
	seq     int
	reviews = []string{
		"Hey there, pretty mama. You pulled that fix out as clean as I part my hair. Ship it.",
		"Whoa — that refactor is almost as good-looking as me. Almost. Five stars, gorgeous.",
		"I read your diff. I also read my reflection in the monitor. Both looking sharp. Approved.",
		"Tidy little change. You're learning. (I, of course, was born this tidy.) Send it, mama.",
	}
)

func main() {
	port := getenv("PORT", "8787")
	http.HandleFunc("/v1/pretty", submit)
	http.HandleFunc("/v1/pretty/", retrieve)
	fmt.Printf("hunky-cloud-mock listening on :%s (HUNKY_BIN=%s)\n", port, getenv("HUNKY_BIN", "hunky"))
	_ = http.ListenAndServe(":"+port, nil)
}

func submit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	// Billing path: opt-in 402 so the CLI's payment-required handling can be
	// exercised (mirrors the real backend's overdraft refusal).
	if r.Header.Get("X-Mock-402") != "" || os.Getenv("MOCK_402") != "" {
		jsonError(w, http.StatusPaymentRequired, map[string]any{
			"error":        "balance exhausted",
			"detail":       "Your usage balance has hit the overdraft floor. Top up to keep staging.",
			"recharge_url": "http://localhost:" + getenv("PORT", "8787") + "/billing",
		})
		return
	}
	var req prettyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, map[string]any{"error": "could not decode request"})
		return
	}
	diff, err := decompress(req.Diff)
	if err != nil {
		jsonError(w, http.StatusBadRequest, map[string]any{"error": "could not decode diff"})
		return
	}

	start := time.Now()
	output, runErr := runHunky(req, diff)
	if runErr != nil {
		jsonError(w, http.StatusUnprocessableEntity, map[string]any{"error": "hunky failed", "detail": runErr.Error()})
		return
	}

	var res prettyResult
	res.Review = pickReview()
	res.Meta.Model = "Brawny-1"
	switch req.Mode {
	case "stage":
		res.Commands = []string{heredoc(output)}
	default: // list, count, dryrun
		res.Output = output
	}
	res.Meta.Millis = int(time.Since(start).Milliseconds())

	mu.Lock()
	seq++
	key := fmt.Sprintf("req_%08x", seq*2654435761)
	jobs[key] = job{readyAt: time.Now().Add(600 * time.Millisecond), result: res}
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"request_key": key, "poll_after_ms": 600})
}

func retrieve(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/v1/pretty/")
	mu.Lock()
	j, ok := jobs[key]
	mu.Unlock()
	if !ok {
		jsonError(w, http.StatusNotFound, map[string]any{"error": "unknown request key"})
		return
	}
	if time.Now().Before(j.readyAt) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(j.result)
}

// runHunky shells out to the real hunky binary on the uploaded diff — exactly
// what the production backend will do. It always runs read-only (--list or
// --dry-run); the CLI applies the patch locally.
func runHunky(req prettyRequest, diff []byte) (string, error) {
	args := []string{"--stdin"}
	switch req.Mode {
	case "list":
		args = append(args, "--list")
	case "count":
		args = append(args, "--count")
	case "dryrun", "stage":
		args = append(args, "--dry-run")
	}
	if req.Invert {
		args = append(args, "--invert")
	}
	if req.Lines != "" {
		args = append(args, "--lines", req.Lines)
	}
	if req.PickLines != "" {
		args = append(args, "--pick-lines", req.PickLines)
	}
	args = append(args, req.File)
	for _, h := range req.Hunks {
		args = append(args, strconv.Itoa(h))
	}

	cmd := exec.Command(getenv("HUNKY_BIN", "hunky"), args...)
	cmd.Stdin = bytes.NewReader(diff)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", msg)
	}
	return out.String(), nil
}

func heredoc(patch string) string {
	if !strings.HasSuffix(patch, "\n") {
		patch += "\n"
	}
	return "git apply --cached <<'HUNKYPATCH'\n" + patch + "HUNKYPATCH"
}

func decompress(b64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

func pickReview() string {
	mu.Lock()
	defer mu.Unlock()
	return reviews[seq%len(reviews)]
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// jsonError writes a JSON error body matching the real backend's shape
// (`{"error": ...}`, plus optional `detail` / `recharge_url`).
func jsonError(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
