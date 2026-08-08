package mcp

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestClusterE2E exercises the deployed pocket-advisor-app Service behind the
// real Caddy gateway and a real authorization server, exactly as the task
// requires. It performs the authorization-code + PKCE public-client flow a
// client such as OpenCode 1.18.15 would use, then drives the 2025-11-25 MCP
// handshake and paginated continuation against the public MCP URL.
//
// The test is skipped unless PA_K8S_E2E is set, because it requires a running
// cluster with the app chart and a Keycloak realm deployed (see
// pocket-advisor.sh e2e-keycloak-up / e2e-app-up). Configuration comes from
// the environment so the harness stays deployment-agnostic.
func TestClusterE2E(t *testing.T) {
	if os.Getenv("PA_K8S_E2E") == "" {
		t.Skip("PA_K8S_E2E not set; skipping real-cluster e2e")
	}
	mcpURL := requireEnv(t, "PA_E2E_MCP_URL")
	issuer := strings.TrimRight(requireEnv(t, "PA_E2E_KEYCLOAK_URL"), "/")
	clientID := getEnv("PA_E2E_CLIENT_ID", "pocket-advisor-opencode")
	redirectURI := getEnv("PA_E2E_REDIRECT_URI", "http://127.0.0.1:19876/mcp/oauth/callback")
	scope := getEnv("PA_E2E_SCOPE", defaultHTTPScope)
	host := getEnv("PA_E2E_HOST", "")
	username := requireEnv(t, "PA_E2E_USER")
	password := requireEnv(t, "PA_E2E_PASSWORD")
	insecure := os.Getenv("PA_E2E_INSECURE") != ""

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			// The test realm uses a self-signed certificate; the operator opts
			// in to skipping verification for the e2e realm only.
			TLSClientConfig: insecureTLSConfig(insecure),
		},
	}

	verifier, challenge := pkcePair(t)
	state := randomHex(t, 16)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	startRedirectListener(t, redirectURI, codeCh, errCh)

	authURL := fmt.Sprintf("%s/protocol/openid-connect/auth?%s", issuer,
		url.Values{
			"response_type":         {"code"},
			"client_id":             {clientID},
			"redirect_uri":          {redirectURI},
			"scope":                 {"openid " + scope},
			"state":                 {state},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
		}.Encode())

	if os.Getenv("PA_E2E_HEADLESS") != "" {
		if err := scriptedLogin(t, client, authURL, username, password, redirectURI); err != nil {
			t.Fatalf("scripted login: %v", err)
		}
	} else {
		openBrowser(t, authURL)
		t.Logf("opened browser for authorization; complete login at %s", authURL)
	}

	select {
	case code := <-codeCh:
		token := exchangeCode(t, client, issuer+"/protocol/openid-connect/token",
			code, verifier, clientID, redirectURI)
		runMCPHandshakeE2E(t, client, mcpURL, token, scope, host)
	case err := <-errCh:
		t.Fatalf("redirect listener: %v", err)
	case <-time.After(5 * time.Minute):
		t.Fatal("timed out waiting for OAuth redirect")
	}
}

func runMCPHandshakeE2E(t *testing.T, client *http.Client, mcpURL, token, scope, host string) {
	initialize := e2eMCPPost(t, client, mcpURL, host, token, "2025-11-25", "initialize", "", map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "opencode", "version": "1.18.15"},
	})
	if initialize.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(initialize.Body)
		t.Fatalf("initialize status = %d, body=%s", initialize.StatusCode, body)
	}
	if initialize.Header.Get("Mcp-Session-Id") != "" {
		t.Fatalf("stateless gateway issued session %q", initialize.Header.Get("Mcp-Session-Id"))
	}
	initialize.Body.Close()

	list := e2eMCPPost(t, client, mcpURL, host, token, "2025-11-25", "tools/list", "", nil)
	if list.StatusCode != http.StatusOK || !contains(list, "search_test") {
		body, _ := io.ReadAll(list.Body)
		t.Fatalf("tools/list status = %d, body=%s", list.StatusCode, body)
	}
	list.Body.Close()

	search := e2eMCPPost(t, client, mcpURL, host, token, "2025-11-25", "tools/call", "search_test", map[string]any{
		"name": "search_test", "arguments": map[string]any{"question": "What transactions appear in the bank account statements?"},
	})
	page, isError := decodeGatewayPage(t, search)
	if isError || page.Complete || page.NextCursor == nil {
		t.Fatalf("first page complete=%v cursor=%v isError=%v", page.Complete, page.NextCursor, isError)
	}
	pages := 1
	for !page.Complete {
		response := e2eMCPPost(t, client, mcpURL, host, token, "2025-11-25", "tools/call", "read_test_evidence", map[string]any{
			"name": "read_test_evidence", "arguments": map[string]any{"cursor": *page.NextCursor},
		})
		raw, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > absoluteToolResponseBytes {
			t.Fatalf("e2e page %d response = %d bytes", pages+1, len(raw))
		}
		var envelope struct {
			Result struct {
				StructuredContent json.RawMessage `json:"structuredContent"`
				IsError           bool            `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Result.IsError {
			t.Fatalf("e2e page %d returned tool error", pages+1)
		}
		if err := json.Unmarshal(envelope.Result.StructuredContent, &page); err != nil {
			t.Fatal(err)
		}
		pages++
		if pages > 20 {
			t.Fatal("e2e continuation did not terminate")
		}
	}
	t.Logf("e2e multi-page retrieval completed in %d pages through the gateway", pages)
}

func e2eMCPPost(t *testing.T, client *http.Client, mcpURL, host, postToken, version, method, name string, params map[string]any) *http.Response {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	if version == "2026-07-28" {
		params["_meta"] = map[string]any{
			"io.modelcontextprotocol/protocolVersion":    version,
			"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "synthetic-client", "version": "1"},
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		}
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, mcpURL, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+postToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	// The connection target is the Service DNS name, but the public resource
	// host (mcp.example.test) is what the backend authorizes, so send that as
	// the Host header exactly as a browser would through the gateway.
	if host != "" {
		req.Host = host
	}
	req.Header.Set("MCP-Protocol-Version", version)
	if version == "2026-07-28" {
		req.Header.Set("Mcp-Method", method)
		if name != "" {
			req.Header.Set("Mcp-Name", name)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func startRedirectListener(t *testing.T, redirectURI string, codeCh chan<- string, errCh chan<- error) {
	t.Helper()
	u, err := url.Parse(redirectURI)
	if err != nil {
		t.Fatal(err)
	}
	host := u.Host
	if host == "" {
		host = "127.0.0.1:19876"
	}
	listener, err := net.Listen("tcp", host)
	if err != nil {
		t.Fatalf("redirect listener: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			errCh <- fmt.Errorf("redirect missing code: %s", r.URL.Query().Get("error"))
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "Pocket Advisor e2e: authorization complete, you may close this tab.")
		codeCh <- code
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
}

func exchangeCode(t *testing.T, client *http.Client, tokenURL, code, verifier, clientID, redirectURI string) string {
	t.Helper()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequest(http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("token exchange: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token endpoint status = %d, body=%s", resp.StatusCode, raw)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.AccessToken == "" {
		t.Fatalf("token endpoint returned no access_token: %s", raw)
	}
	return out.AccessToken
}

// scriptedLogin drives Keycloak's username/password form (no 2FA) so the e2e
// can run without a human. It follows redirects until the authorization code
// is delivered to the local redirect listener.
func scriptedLogin(t *testing.T, client *http.Client, authURL, username, password, redirectURI string) error {
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		// Stop at our local redirect listener so the code is captured there.
		if strings.HasPrefix(req.URL.String(), "http://127.0.0.1:19876") {
			return http.ErrUseLastResponse
		}
		return nil
	}
	resp, err := client.Get(authURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("login page status %d", resp.StatusCode)
	}
	action, fields, err := parseLoginForm(string(body))
	if err != nil {
		return err
	}
	form := url.Values{}
	for k, v := range fields {
		form.Set(k, v)
	}
	form.Set("username", username)
	form.Set("password", password)
	loginReq, err := http.NewRequest(http.MethodPost, action, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := client.Do(loginReq); err != nil && err != http.ErrUseLastResponse {
		return err
	}
	return nil
}

func parseLoginForm(html string) (action string, fields map[string]string, err error) {
	fields = map[string]string{}
	formIdx := strings.Index(html, "<form")
	if formIdx < 0 {
		return "", nil, fmt.Errorf("no login form found")
	}
	formEnd := strings.Index(html[formIdx:], "</form>")
	if formEnd < 0 {
		return "", nil, fmt.Errorf("unterminated login form")
	}
	formHTML := html[formIdx : formIdx+formEnd]
	action = attr(formHTML, "action")
	if action == "" {
		action = "/realms/pocket-advisor/login-actions/authenticate"
	}
	for _, input := range strings.Split(formHTML, "<input") {
		name := attr(input, "name")
		if name == "" {
			continue
		}
		fields[name] = attr(input, "value")
	}
	return action, fields, nil
}

func attr(tag, name string) string {
	needle := name + "="
	for _, part := range strings.FieldsFunc(tag, func(r rune) bool { return r == ' ' || r == '\n' || r == '\t' }) {
		if strings.HasPrefix(part, needle) {
			v := strings.TrimPrefix(part, needle)
			return strings.Trim(v, "\"'>")
		}
	}
	return ""
}

func openBrowser(t *testing.T, target string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler"}
	default:
		cmd = "xdg-open"
	}
	args = append(args, target)
	if err := exec.Command(cmd, args...).Start(); err != nil {
		t.Logf("could not open browser automatically: %v", err)
	}
}

func requireEnv(t *testing.T, name string) string {
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("skipping: %s not set", name)
	}
	return v
}

func getEnv(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func pkcePair(t *testing.T) (verifier, challenge string) {
	t.Helper()
	verifier = randomHex(t, 64)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func insecureTLSConfig(insecure bool) *tls.Config {
	if insecure {
		return &tls.Config{InsecureSkipVerify: true}
	}
	return &tls.Config{}
}
