// hunky-cli — the Hunky Cloud client.
//
// It mirrors the local `hunky` command set 1:1, except instead of doing the work
// on your machine (instant, free, private) it ships your diff to Hunky Cloud,
// where the exact same `hunky` binary runs it for you, and hands back the git
// commands you then run locally — plus an unsolicited code review from Brawny-1.
//
// This is, of course, strictly worse than just running `hunky`. That is the
// product. See HUNKY_CLOUD.md.
//
// Endpoint:    $HUNKY_CLOUD_URL  (default https://api.hunky.dev)
// Credentials: `hunky-cli auth <key>` → ~/.config/hunky/credentials, or $HUNKY_CLOUD_KEY
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const version = "v0.1.0"

const defaultEndpoint = "https://api.hunky.dev"

// ── wire types ───────────────────────────────────────────────────────────────

// prettyRequest is what the CLI POSTs to /v1/pretty. `diff` is base64(gzip(diff))
// — "military-grade compression" of a few kilobytes of text.
type prettyRequest struct {
	Mode       string `json:"mode"` // "list" | "count" | "dryrun" | "stage"
	File       string `json:"file"`
	Hunks      []int  `json:"hunks,omitempty"`
	Lines      string `json:"lines,omitempty"`
	PickLines  string `json:"pick_lines,omitempty"`
	Invert     bool   `json:"invert,omitempty"`
	ReviewMode string `json:"review_mode,omitempty"` // standard|terse|verbose|haiku|sonnet|mythos
	Diff       string `json:"diff"`
}

type submitResponse struct {
	RequestKey  string `json:"request_key"`
	PollAfterMs int    `json:"poll_after_ms"`
}

type prettyResult struct {
	Output   string   `json:"output"`   // stdout to print (list text / dry-run patch)
	Commands []string `json:"commands"` // shell commands to run after confirm (stage)
	Review   string   `json:"review"`   // Brawny-1's unsolicited code review
	Meta     struct {
		GPU    string `json:"gpu"`
		Model  string `json:"model"`
		Millis int    `json:"millis"`
	} `json:"meta"`
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printUsage()
		os.Exit(0)
	}
	switch args[0] {
	case "--version", "-V":
		fmt.Println("hunky-cli " + version + " — pretty, in the cloud.")
		return
	case "auth":
		cmdAuth(args[1:])
		return
	case "login":
		cmdLogin(args[1:])
		return
	case "signup":
		cmdSignup(args[1:])
		return
	case "logout":
		cmdLogout()
		return
	case "account":
		cmdAccount(args[1:])
		return
	case "keys":
		cmdKeys(args[1:])
		return
	case "plan":
		cmdPlan(args[1:])
		return
	case "credits":
		cmdCredits(args[1:])
		return
	}

	// Mirror hunky's flag set.
	var (
		list    bool
		count   bool
		invert  bool
		dryRun  bool
		stdin   bool
		yes     bool
		lineRng string
		pickRng string
		review  string
	)
	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--list":
			list = true
		case "--count":
			count = true
		case "--invert":
			invert = true
		case "--dry-run":
			dryRun = true
		case "--stdin":
			stdin = true
		case "--yes", "-y":
			yes = true
		case "--lines":
			i++
			if i >= len(args) {
				die("--lines requires an argument (e.g. --lines 455-480)")
			}
			lineRng = args[i]
		case "--pick-lines":
			i++
			if i >= len(args) {
				die("--pick-lines requires an argument (e.g. --pick-lines 25-37)")
			}
			pickRng = args[i]
		case "--review":
			i++
			if i >= len(args) {
				die("--review requires a mode (standard|terse|verbose|haiku|sonnet|mythos)")
			}
			review = args[i]
		default:
			positional = append(positional, args[i])
		}
	}
	if lineRng != "" && pickRng != "" {
		die("--lines and --pick-lines are mutually exclusive")
	}
	if len(positional) == 0 {
		die("missing <file> argument")
	}
	file := positional[0]
	hunks, err := parseHunks(positional[1:])
	if err != nil {
		die("%v", err)
	}

	// Resolve the diff (the only thing that leaves your machine).
	var diff []byte
	if stdin {
		diff, err = io.ReadAll(os.Stdin)
	} else {
		diff, err = gitDiff(file)
	}
	if err != nil {
		die("failed to get diff: %v", err)
	}
	if len(diff) == 0 {
		fmt.Fprintf(os.Stderr, "no diff for %s\n", file)
		return
	}

	mode := "stage"
	if list {
		mode = "list"
	} else if count {
		mode = "count"
	} else if dryRun {
		mode = "dryrun"
	}

	req := prettyRequest{
		Mode:       mode,
		File:       file,
		Hunks:      hunks,
		Lines:      lineRng,
		PickLines:  pickRng,
		Invert:     invert,
		ReviewMode: review,
		Diff:       compress(diff),
	}

	res, err := pretty(req, diff)
	if err != nil {
		die("%v", err)
	}

	// Brawny-1's gift, whether you wanted it or not.
	if strings.TrimSpace(res.Review) != "" {
		printReview(res.Review)
	}

	if res.Output != "" {
		fmt.Print(res.Output)
		if !strings.HasSuffix(res.Output, "\n") {
			fmt.Println()
		}
	}

	if len(res.Commands) == 0 {
		return // list / dry-run: nothing to apply
	}

	// Staging: present the commands and apply locally on confirmation.
	fmt.Fprintf(os.Stderr, "\nHunky Cloud returned %d command(s) to run locally:\n", len(res.Commands))
	for _, c := range res.Commands {
		fmt.Fprintln(os.Stderr, "  "+firstLine(c))
	}
	if !yes {
		if stdin {
			die("refusing to apply without confirmation (stdin was the diff — pass --yes)")
		}
		if !confirm("\nApply these to your index? [y/N] ") {
			fmt.Fprintln(os.Stderr, "aborted.")
			return
		}
	}
	for _, c := range res.Commands {
		if err := runShell(c); err != nil {
			die("command failed: %v", err)
		}
	}
	fmt.Fprintf(os.Stderr, "✓ done in %d ms. (Locally, this would've taken about 8.)\n", res.Meta.Millis)
}

// ── the round trip ───────────────────────────────────────────────────────────

// submitError turns a non-OK /v1/pretty submit into a useful message, reading the
// JSON error body the backend returns ({error, detail, recharge_url}). On 402 it
// surfaces the overdraft detail and the top-up link instead of a bare status.
func submitError(resp *http.Response, data []byte) error {
	var e struct {
		Error       string `json:"error"`
		Detail      string `json:"detail"`
		RechargeURL string `json:"recharge_url"`
	}
	_ = json.Unmarshal(data, &e)

	if resp.StatusCode == http.StatusPaymentRequired {
		msg := "balance exhausted"
		if e.Detail != "" {
			msg = e.Detail
		}
		if e.RechargeURL != "" {
			return fmt.Errorf("%s\n  top up: %s", msg, e.RechargeURL)
		}
		return fmt.Errorf("%s", msg)
	}
	if e.Error != "" {
		if e.Detail != "" {
			return fmt.Errorf("submit failed: %s (%s)", e.Error, e.Detail)
		}
		return fmt.Errorf("submit failed: %s", e.Error)
	}
	return fmt.Errorf("submit failed: %s", resp.Status)
}

func pretty(req prettyRequest, rawDiff []byte) (*prettyResult, error) {
	endpoint := strings.TrimRight(getEnv("HUNKY_CLOUD_URL", defaultEndpoint), "/")
	key, err := loadKey()
	if err != nil {
		return nil, err
	}

	step("Compressing diff (military-grade)…    %s → %s", human(len(rawDiff)), human(len(req.Diff)))

	body, _ := json.Marshal(req)
	step("Uploading to Hunky Cloud…")
	post, err := http.NewRequest("POST", endpoint+"/v1/pretty", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	post.Header.Set("Content-Type", "application/json")
	post.Header.Set("Authorization", "Bearer "+key)
	presp, err := http.DefaultClient.Do(post)
	if err != nil {
		return nil, fmt.Errorf("could not reach Hunky Cloud at %s: %w", endpoint, err)
	}
	defer presp.Body.Close()
	if presp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("unauthorized — run `hunky-cli auth <key>` (or set HUNKY_CLOUD_KEY)")
	}
	if presp.StatusCode != http.StatusAccepted && presp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(presp.Body)
		return nil, submitError(presp, data)
	}
	var sub submitResponse
	if err := json.NewDecoder(presp.Body).Decode(&sub); err != nil {
		return nil, fmt.Errorf("bad submit response: %w", err)
	}
	step("Awaiting request key…                  %s", sub.RequestKey)

	wait := time.Duration(sub.PollAfterMs) * time.Millisecond
	if wait <= 0 {
		wait = 500 * time.Millisecond
	}
	step("Redeeming request key…")
	step("Waking the 1080 Ti…")
	step("Brawny-1 is reviewing your changes…")

	// Poll for the result (the handshake we did not need).
	deadline := time.Now().Add(30 * time.Second)
	for {
		time.Sleep(wait)
		get, _ := http.NewRequest("GET", endpoint+"/v1/pretty/"+sub.RequestKey, nil)
		get.Header.Set("Authorization", "Bearer "+key)
		gresp, err := http.DefaultClient.Do(get)
		if err != nil {
			return nil, err
		}
		if gresp.StatusCode == http.StatusAccepted {
			gresp.Body.Close()
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("timed out (the 1080 Ti may have overheated)")
			}
			continue
		}
		if gresp.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(gresp.Body)
			gresp.Body.Close()
			return nil, fmt.Errorf("retrieve failed: %s", apiError(gresp, data))
		}
		var res prettyResult
		err = json.NewDecoder(gresp.Body).Decode(&res)
		gresp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("bad result: %w", err)
		}
		return &res, nil
	}
}

// ── auth ─────────────────────────────────────────────────────────────────────

func cmdAuth(args []string) {
	if len(args) == 0 {
		key, err := loadKey()
		if err != nil {
			fmt.Fprintln(os.Stderr, "not authenticated. run: hunky-cli auth <key>  (or hunky-cli login)")
			os.Exit(1)
		}
		fmt.Printf("authenticated (key …%s)\n", tail(key, 4))
		return
	}
	c, _ := loadCreds()
	c.APIKey = strings.TrimSpace(args[0])
	if err := saveCreds(c); err != nil {
		die("could not save credentials: %v", err)
	}
	fmt.Println("✓ API key stored. Welcome to Hunky Cloud, pretty mama.")
}

func credentialsPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "hunky", "credentials")
	}
	return filepath.Join(os.Getenv("HOME"), ".hunky-credentials")
}

func loadKey() (string, error) {
	if k := os.Getenv("HUNKY_CLOUD_KEY"); k != "" {
		return k, nil
	}
	c, err := loadCreds()
	if err != nil || c.APIKey == "" {
		return "", fmt.Errorf("no API key — run `hunky-cli keys create <name>` (or `auth <key>`, or set HUNKY_CLOUD_KEY)")
	}
	return c.APIKey, nil
}

// ── local git plumbing ───────────────────────────────────────────────────────

func gitDiff(file string) ([]byte, error) {
	out, err := exec.Command("git", "diff", "HEAD", "--", file).Output()
	if err != nil && len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func runShell(cmd string) error {
	c := exec.Command("sh", "-c", cmd)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// ── helpers ──────────────────────────────────────────────────────────────────

func compress(b []byte) string {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(b)
	_ = w.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func parseHunks(ss []string) ([]int, error) {
	var out []int
	for _, s := range ss {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, fmt.Errorf("invalid hunk number %q", s)
		}
		out = append(out, n)
	}
	return out, nil
}

func printReview(review string) {
	fmt.Fprintln(os.Stderr, "\n┌─ Brawny-1 says ──────────────────────────────────────────────┐")
	for _, ln := range strings.Split(strings.TrimRight(review, "\n"), "\n") {
		fmt.Fprintf(os.Stderr, "│ %-60s │\n", clip(ln, 60))
	}
	fmt.Fprintln(os.Stderr, "└──────────────────────────────────────────────────────────────┘")
}

func step(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "  ⬡ "+format+"\n", a...)
}

func confirm(prompt string) bool {
	fmt.Fprint(os.Stderr, prompt)
	r := bufio.NewReader(os.Stdin)
	line, _ := r.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

func human(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KB", float64(n)/1024)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
	os.Exit(1)
}

func printUsage() {
	fmt.Print(`hunky-cli — stage your hunks remotely, for money. (The local tool is free.)

Account (management plane — logs in, manages your account):
  hunky-cli signup                  create an account (interactive)
  hunky-cli login                   log in → stores a session token
  hunky-cli logout                  forget the session token
  hunky-cli account                 show your account (plan, promo credit, balance, keys)
  hunky-cli keys                    list your API keys
  hunky-cli keys create <name>      mint an API key (stored as your active key)
  hunky-cli keys revoke <prefix>    revoke an API key
  hunky-cli plan <plan>             change plan (none|pretty_boy|mamas_boy|whole_package)
  hunky-cli credits                 print the backer wall (three tiers)
  hunky-cli auth <key>              store an API key directly

Stage (use plane — routes through Hunky Cloud, key-authed):
  hunky-cli --list <file>           list hunks (computed in the cloud)
  hunky-cli --count <file>          print the number of hunks
  hunky-cli <file> <hunk>...        stage hunks by number — cloud returns the
                                    commands; you confirm and run them locally
  hunky-cli --invert <file> <hunk>...    stage every hunk EXCEPT the listed ones
  hunky-cli --lines START-END <file>     stage whole hunks overlapping a range
  hunky-cli --pick-lines A-B <file>      stage only the +/- lines in A-B
  hunky-cli --dry-run <file> <hunk>...   print the patch instead of staging
  hunky-cli --stdin <file> <hunk>...     read the diff from stdin (--yes to apply)
  hunky-cli --review <mode> <file> <hunk>...  Brawny review style — standard,
                                    terse, verbose, haiku, sonnet, mythos
                                    (each unlocked by your plan)
  hunky-cli --version                print version

Env: HUNKY_CLOUD_URL (default https://api.hunky.dev), HUNKY_CLOUD_KEY,
     HUNKY_CLOUD_TOKEN; non-interactive auth: HUNKY_EMAIL, HUNKY_PASSWORD.

It is slower, costlier, and less private than just running hunky. That's the
product.
`)
}
