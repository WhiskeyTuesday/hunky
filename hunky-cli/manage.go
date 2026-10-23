package main

// Management plane: account auth + key/plan management. These talk to the
// token-authed `/v1/{signup,login,account,keys,plan}` endpoints — distinct from
// the key-authed `/v1/pretty` use plane. A short-lived session token is stored
// alongside the API key in the credentials file.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"
)

// ── credentials (JSON: API key + session token) ──────────────────────────────

type creds struct {
	APIKey         string `json:"api_key,omitempty"`
	Token          string `json:"token,omitempty"`
	TokenExpiresAt int64  `json:"token_expires_at,omitempty"`
}

func loadCreds() (creds, error) {
	var c creds
	b, err := os.ReadFile(credentialsPath())
	if err != nil {
		return c, err
	}
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, "{") {
		_ = json.Unmarshal([]byte(s), &c)
		return c, nil
	}
	c.APIKey = s // legacy plaintext file = bare API key
	return c, nil
}

func saveCreds(c creds) error {
	p := credentialsPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(p, append(b, '\n'), 0o600)
}

func storeToken(token string, expiresIn int) {
	c, _ := loadCreds()
	c.Token = token
	if expiresIn > 0 {
		c.TokenExpiresAt = time.Now().Unix() + int64(expiresIn)
	}
	if err := saveCreds(c); err != nil {
		die("could not save credentials: %v", err)
	}
}

func loadToken() (string, error) {
	if t := os.Getenv("HUNKY_CLOUD_TOKEN"); t != "" {
		return t, nil
	}
	c, err := loadCreds()
	if err != nil || c.Token == "" {
		return "", fmt.Errorf("not logged in — run `hunky-cli login`")
	}
	if c.TokenExpiresAt > 0 && time.Now().Unix() > c.TokenExpiresAt {
		return "", fmt.Errorf("session expired — run `hunky-cli login` again")
	}
	return c.Token, nil
}

// ── HTTP ──────────────────────────────────────────────────────────────────────

func apiBase() string { return strings.TrimRight(getEnv("HUNKY_CLOUD_URL", defaultEndpoint), "/") }

func mgmtDo(method, path string, body any, token string) (*http.Response, []byte, error) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, apiBase()+path, r)
	if err != nil {
		return nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := cloudHTTPClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("could not reach Hunky Cloud at %s: %w", apiBase(), err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data, nil
}

func apiError(resp *http.Response, data []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error != "" {
		return e.Error
	}
	return resp.Status
}

// ── commands ──────────────────────────────────────────────────────────────────

func cmdSignup(args []string) {
	email, password, name := authInputs(args, true)
	body := map[string]string{"email": email, "password": password}
	if name != "" {
		body["name"] = name
	}
	if code := flagOrEnv(args, "--code", "HUNKY_SIGNUP_CODE"); code != "" {
		body["code"] = code
	}

	resp, data, err := mgmtDo("POST", "/v1/signup", body, "")
	if err != nil {
		die("%v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		die("signup failed: %s", apiError(resp, data))
	}
	var out struct {
		Token     string `json:"token"`
		AccountID string `json:"account_id"`
		ExpiresIn int    `json:"expires_in"`
	}
	_ = json.Unmarshal(data, &out)
	storeToken(out.Token, out.ExpiresIn)
	fmt.Printf("✓ account created (%s). You're logged in, pretty mama.\n", out.AccountID)
	fmt.Println("Next: hunky-cli keys create laptop")
}

func cmdLogin(args []string) {
	email, password, _ := authInputs(args, false)
	resp, data, err := mgmtDo("POST", "/v1/login", map[string]string{"email": email, "password": password}, "")
	if err != nil {
		die("%v", err)
	}
	if resp.StatusCode != http.StatusOK {
		die("login failed: %s", apiError(resp, data))
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expires_in"`
	}
	_ = json.Unmarshal(data, &out)
	storeToken(out.Token, out.ExpiresIn)
	fmt.Printf("✓ logged in as %s. Session good for %d min.\n", email, out.ExpiresIn/60)
}

func cmdLogout() {
	c, _ := loadCreds()
	c.Token, c.TokenExpiresAt = "", 0
	_ = saveCreds(c)
	fmt.Println("✓ logged out (session token forgotten). Your API key is untouched.")
}

type keyInfo struct {
	Prefix string `json:"prefix"`
	Name   string `json:"name"`
	Env    string `json:"env"`
}

func cmdAccount(_ []string) {
	token, err := loadToken()
	if err != nil {
		die("%v", err)
	}
	resp, data, err := mgmtDo("GET", "/v1/account", nil, token)
	if err != nil {
		die("%v", err)
	}
	if resp.StatusCode != http.StatusOK {
		die("%s", apiError(resp, data))
	}
	var a struct {
		Name         string    `json:"name"`
		Plan         string    `json:"plan"`
		CreditCents  int       `json:"credit_cents"`
		BalanceCents int       `json:"balance_cents"`
		RunsCount    int       `json:"runs_count"`
		Keys         []keyInfo `json:"keys"`
	}
	_ = json.Unmarshal(data, &a)
	plan := a.Plan
	if plan == "" {
		plan = "none (free / PAYG)"
	}
	fmt.Printf("Account:  %s\nPlan:     %s\nPromo:    $%.2f\nBalance:  $%.2f\nRuns:     %d\nKeys:     %d\n",
		a.Name, plan, float64(a.CreditCents)/100, float64(a.BalanceCents)/100, a.RunsCount, len(a.Keys))
	printKeys(a.Keys)
}

func cmdKeys(args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "create", "new":
			cmdKeysCreate(args[1:])
			return
		case "revoke", "delete", "rm":
			cmdKeysRevoke(args[1:])
			return
		}
	}

	token, err := loadToken()
	if err != nil {
		die("%v", err)
	}
	resp, data, err := mgmtDo("GET", "/v1/account", nil, token)
	if err != nil {
		die("%v", err)
	}
	if resp.StatusCode != http.StatusOK {
		die("%s", apiError(resp, data))
	}
	var a struct {
		Keys []keyInfo `json:"keys"`
	}
	_ = json.Unmarshal(data, &a)
	if len(a.Keys) == 0 {
		fmt.Println("No keys yet. Create one: hunky-cli keys create <name>")
		return
	}
	printKeys(a.Keys)
}

func cmdKeysCreate(args []string) {
	token, err := loadToken()
	if err != nil {
		die("%v", err)
	}

	var env, name string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--env" && i+1 < len(args):
			env = args[i+1]
			i++
		default:
			rest = append(rest, args[i])
		}
	}
	if len(rest) > 0 {
		name = rest[0]
	}
	if name == "" {
		name = "untitled"
	}

	body := map[string]string{"name": name}
	if env != "" {
		body["env"] = env
	}
	resp, data, err := mgmtDo("POST", "/v1/keys", body, token)
	if err != nil {
		die("%v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		die("could not create key: %s", apiError(resp, data))
	}
	var out struct {
		APIKey string `json:"api_key"`
		Prefix string `json:"prefix"`
		Name   string `json:"name"`
	}
	_ = json.Unmarshal(data, &out)

	c, _ := loadCreds()
	c.APIKey = out.APIKey
	_ = saveCreds(c)

	fmt.Printf("✓ created key %q:\n\n  %s\n\n", out.Name, out.APIKey)
	fmt.Println("Stored as your active key — copy it now, it won't be shown again.")
	fmt.Println("You can now: hunky-cli <file> <hunk>")
}

func cmdKeysRevoke(args []string) {
	if len(args) == 0 {
		die("usage: hunky-cli keys revoke <prefix>")
	}
	token, err := loadToken()
	if err != nil {
		die("%v", err)
	}
	resp, data, err := mgmtDo("DELETE", "/v1/keys/"+args[0], nil, token)
	if err != nil {
		die("%v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		die("could not revoke: %s", apiError(resp, data))
	}
	fmt.Printf("✓ revoked %s.\n", args[0])
}

func cmdPlan(args []string) {
	if len(args) == 0 {
		die("usage: hunky-cli plan <none|pretty_boy|mamas_boy|whole_package>")
	}
	token, err := loadToken()
	if err != nil {
		die("%v", err)
	}
	resp, data, err := mgmtDo("POST", "/v1/plan", map[string]string{"plan": args[0]}, token)
	if err != nil {
		die("%v", err)
	}
	if resp.StatusCode != http.StatusOK {
		die("could not change plan: %s", apiError(resp, data))
	}

	var r struct {
		Plan        string `json:"plan"`
		Status      string `json:"status"`
		CheckoutURL string `json:"checkout_url"`
	}
	_ = json.Unmarshal(data, &r)

	if r.Status == "checkout_required" {
		fmt.Printf("Starting your %s subscription needs a card.\n", args[0])
		fmt.Printf("Open this link to finish in your browser:\n\n  %s\n\n", r.CheckoutURL)
		fmt.Println("After that, every plan change is instant from the CLI.")
		return
	}
	fmt.Printf("✓ plan changed to %s.\n", args[0])
}

// cmdCredits prints the public backer wall in three descending tiers — top tier
// loud and spaced, bottom tier a small lowercase huddle.
func cmdCredits(_ []string) {
	resp, data, err := mgmtDo("GET", "/v1/credits", nil, "") // public, no token
	if err != nil {
		die("%v", err)
	}
	if resp.StatusCode != http.StatusOK {
		die("%s", apiError(resp, data))
	}
	var c struct {
		WholePackage []string `json:"whole_package"`
		MamasBoy     []string `json:"mamas_boy"`
		PrettyBoy    []string `json:"pretty_boy"`
	}
	_ = json.Unmarshal(data, &c)

	fmt.Println()
	fmt.Println("        ✦  HUNKY CLOUD BACKERS  ✦")
	fmt.Println()

	if len(c.WholePackage) > 0 {
		fmt.Println("  ── THE WHOLE PACKAGE ──")
		for _, n := range c.WholePackage {
			fmt.Printf("      %s\n", strings.ToUpper(n))
		}
		fmt.Println()
	}
	if len(c.MamasBoy) > 0 {
		fmt.Println("  ── Mama's Boys ──")
		for _, n := range c.MamasBoy {
			fmt.Printf("    %s\n", n)
		}
		fmt.Println()
	}
	if len(c.PrettyBoy) > 0 {
		fmt.Println("  ── pretty boys ──")
		lc := make([]string, len(c.PrettyBoy))
		for i, n := range c.PrettyBoy {
			lc[i] = strings.ToLower(n)
		}
		fmt.Printf("    %s\n\n", strings.Join(lc, " · "))
	}
	if len(c.WholePackage)+len(c.MamasBoy)+len(c.PrettyBoy) == 0 {
		fmt.Println("  (no backers yet — be the first)")
		fmt.Println()
	}
	fmt.Println("  Your name here: hunky-cli plan <pretty_boy|mamas_boy|whole_package>")
	fmt.Println()
}

func printKeys(keys []keyInfo) {
	for _, k := range keys {
		name := k.Name
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Printf("  %s…  %-12s %s\n", k.Prefix, name, k.Env)
	}
}

// ── interactive input (flags / env / prompt) ──────────────────────────────────

// authInputs resolves email/password (and optionally name) from flags, then env,
// then an interactive prompt — so it's scriptable (--email/--password or
// HUNKY_EMAIL/HUNKY_PASSWORD) and human-friendly.
func authInputs(args []string, wantName bool) (email, password, name string) {
	email = flagOrEnv(args, "--email", "HUNKY_EMAIL")
	password = flagOrEnv(args, "--password", "HUNKY_PASSWORD")
	if wantName {
		name = flagOrEnv(args, "--name", "HUNKY_NAME")
	}

	if email == "" {
		email = readLine("Email: ")
	}
	if password == "" {
		password = readSecret("Password: ")
	}
	if wantName && name == "" && term.IsTerminal(int(os.Stdin.Fd())) {
		name = readLine("Name (optional): ")
	}
	if email == "" || password == "" {
		die("email and password are required (pass --email/--password or set HUNKY_EMAIL/HUNKY_PASSWORD)")
	}
	return
}

func flagOrEnv(args []string, flag, env string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, flag+"=") {
			return strings.TrimPrefix(a, flag+"=")
		}
	}
	return os.Getenv(env)
}

func readLine(prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line)
}

func readSecret(prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line)
}
