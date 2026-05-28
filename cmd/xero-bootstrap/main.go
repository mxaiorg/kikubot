// xero-bootstrap performs the one-time Xero OAuth 2.0 authorization-code
// flow needed to seed a refresh token for the xero_api tool. Run once per
// kikubot deployment; kikubot maintains the token file on its own after that.
//
// Usage (from the kikubot repo root):
//
//	export XERO_CLIENT_ID=...                  # from developer.xero.com
//	export XERO_CLIENT_SECRET=...
//	export XERO_REDIRECT_URI=http://localhost:8080/callback   # must match portal
//	go run ./cmd/xero-bootstrap -out data/<agent>/xero/tokens.json
//
// The redirect URI must match exactly what's registered for your WebApp at
// https://developer.xero.com/app/manage. The default is
// http://localhost:8080/callback.
//
// Scopes are taken from XERO_SCOPES (space-separated) or fall back to
// services.DefaultXeroScopes — accounting read-only by default. Add
// accounting.transactions or accounting.contacts (without .read) if you need
// to write.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"kikubot/internal/services"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	clientID := strings.TrimSpace(os.Getenv("XERO_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("XERO_CLIENT_SECRET"))
	redirectURI := strings.TrimSpace(os.Getenv("XERO_REDIRECT_URI"))
	scopesEnv := strings.TrimSpace(os.Getenv("XERO_SCOPES"))

	tokenPath := flag.String("out", "xero/tokens.json", "Where to write the token file. Use data/<agent>/xero/tokens.json to drop straight into the agent's docker-compose data volume.")
	flag.Parse()

	if clientID == "" || clientSecret == "" {
		log.Fatal("XERO_CLIENT_ID and XERO_CLIENT_SECRET are required (export them or source from your secrets.env)")
	}
	if redirectURI == "" {
		redirectURI = "http://localhost:8080/callback"
	}
	parsed, err := url.Parse(redirectURI)
	if err != nil || parsed.Host == "" {
		log.Fatalf("invalid XERO_REDIRECT_URI %q: %v", redirectURI, err)
	}
	listenAddr := parsed.Host
	if !strings.Contains(listenAddr, ":") {
		listenAddr += ":80"
	}

	scopes := services.DefaultXeroScopes
	if scopesEnv != "" {
		scopes = strings.Fields(scopesEnv)
	}

	state, err := randomState()
	if err != nil {
		log.Fatalf("generating state: %v", err)
	}

	authURL := buildAuthURL(clientID, redirectURI, scopes, state)
	fmt.Println()
	fmt.Println("Open this URL in a browser, log in to Xero, and consent to the listed scopes:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	fmt.Println("Listening for the redirect on", redirectURI, "...")

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(parsed.Path, func(w http.ResponseWriter, r *http.Request) {
		gotState := r.URL.Query().Get("state")
		if gotState != state {
			errCh <- fmt.Errorf("state mismatch (got %q, want %q) — possible CSRF; aborting", gotState, state)
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			errCh <- fmt.Errorf("missing code in callback: %s", r.URL.RawQuery)
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, "Got the code. You can close this tab.")
		codeCh <- code
	})
	server := &http.Server{Addr: listenAddr, Handler: mux}
	go func() {
		if serveErr := server.ListenAndServe(); serveErr != nil && serveErr != http.ErrServerClosed {
			errCh <- serveErr
		}
	}()

	select {
	case err := <-errCh:
		log.Fatal(err)
	case code := <-codeCh:
		_ = server.Shutdown(context.Background())
		exchange(clientID, clientSecret, redirectURI, code, *tokenPath)
	case <-time.After(5 * time.Minute):
		log.Fatal("timed out waiting for Xero callback")
	}
}

func exchange(clientID, clientSecret, redirectURI, code, tokenPath string) {
	ctx := context.Background()

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://identity.xero.com/connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		log.Fatalf("building token request: %v", err)
	}
	req.SetBasicAuth(clientID, clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("token request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("Xero token exchange failed (HTTP %d): %s", resp.StatusCode, body)
	}

	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		log.Fatalf("parsing token response: %v", err)
	}

	tenantID, tenantName, err := pickTenant(ctx, parsed.AccessToken)
	if err != nil {
		log.Fatalf("picking tenant: %v", err)
	}

	tokens := services.XeroTokens{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second),
		TenantID:     tenantID,
		TenantName:   tenantName,
	}

	dir := filepath.Dir(tokenPath)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			log.Fatalf("creating %s: %v", dir, err)
		}
	}
	out, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		log.Fatalf("marshalling tokens: %v", err)
	}
	if err := os.WriteFile(tokenPath, out, 0600); err != nil {
		log.Fatalf("writing %s: %v", tokenPath, err)
	}

	fmt.Println()
	fmt.Printf("Wrote %s\n", tokenPath)
	fmt.Printf("Tenant: %s (%s)\n", tenantName, tenantID)
	fmt.Printf("Scopes granted: %s\n", parsed.Scope)
	fmt.Println()
	fmt.Println("Copy this file into your agent's data volume if you ran the bootstrap")
	fmt.Println("locally (e.g. data/<agent>/xero/tokens.json), then enable the 'xero_api'")
	fmt.Println("tool key in configs/agents.yaml and restart the agent.")
}

func pickTenant(ctx context.Context, accessToken string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.xero.com/connections", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("listing /connections failed (HTTP %d): %s", resp.StatusCode, body)
	}
	var conns []struct {
		TenantID   string `json:"tenantId"`
		TenantType string `json:"tenantType"`
		TenantName string `json:"tenantName"`
	}
	if err := json.Unmarshal(body, &conns); err != nil {
		return "", "", err
	}
	if len(conns) == 0 {
		return "", "", fmt.Errorf("no Xero tenants returned — did you consent to an organisation?")
	}
	if len(conns) == 1 {
		return conns[0].TenantID, conns[0].TenantName, nil
	}
	fmt.Println()
	fmt.Println("Multiple Xero tenants connected to this app:")
	for i, c := range conns {
		fmt.Printf("  [%d] %s (%s, %s)\n", i, c.TenantName, c.TenantType, c.TenantID)
	}
	fmt.Print("Pick one [0]: ")
	var pick int
	_, _ = fmt.Scanln(&pick)
	if pick < 0 || pick >= len(conns) {
		pick = 0
	}
	return conns[pick].TenantID, conns[pick].TenantName, nil
}

func buildAuthURL(clientID, redirectURI string, scopes []string, state string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", strings.Join(scopes, " "))
	q.Set("state", state)
	return "https://login.xero.com/identity/connect/authorize?" + q.Encode()
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
