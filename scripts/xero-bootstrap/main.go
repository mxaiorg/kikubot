// xero-bootstrap performs the one-time Xero OAuth 2.0 authorization-code
// flow needed to seed a refresh token for the xero_api tool. Run once per
// kikubot deployment; kikubot maintains the token file on its own after that.
//
// Usage (from the kikubot repo root):
//
//		export XERO_CLIENT_ID=...                  # from developer.xero.com
//		export XERO_CLIENT_SECRET=...
//		export XERO_REDIRECT_URI=http://localhost:8080/callback   # must match portal
//	 # replace <agent> with the actual agent name, e.g. "alice"
//		go run ./scripts/xero-bootstrap -out data/<agent>/xero/tokens.json
//
// The redirect URI must match exactly what's registered for your WebApp at
// https://developer.xero.com/app/manage. The default is
// http://localhost:8080/callback.
//
// Scopes are taken from XERO_SCOPES (space-separated) or fall back to
// services.DefaultXeroScopes — accounting read-only by default. To write,
// drop the .read suffix on the specific scope you need (e.g.
// accounting.invoices, accounting.banktransactions, accounting.contacts).
// Xero has no umbrella "accounting.transactions" scope — pick the specific
// resource(s) you want.
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
	if bad := unknownXeroScopes(scopes); len(bad) > 0 {
		log.Fatalf("unknown Xero scope(s): %v\n"+
			"Xero's full scope list is at https://developer.xero.com/documentation/guides/oauth2/scopes/\n"+
			"Common gotcha: there is no 'accounting.transactions' scope — use accounting.invoices, "+
			"accounting.banktransactions, accounting.payments, or accounting.manualjournals instead.",
			bad)
	}

	state, err := randomState()
	if err != nil {
		log.Fatalf("generating state: %v", err)
	}

	authURL := buildAuthURL(clientID, redirectURI, scopes, state)
	fmt.Println()
	fmt.Println("Requesting scopes:")
	for _, s := range scopes {
		fmt.Println("  - " + s)
	}
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

// knownXeroScopes is the set of scopes Xero exposes in the developer portal's
// authorisation list (see https://developer.xero.com/documentation/guides/oauth2/scopes/).
// offline_access and openid/profile/email are standard OAuth scopes that don't
// appear in that list but are accepted by Web App clients.
var knownXeroScopes = map[string]bool{
	"offline_access": true,
	"openid":         true,
	"profile":        true,
	"email":          true,

	"app.connections": true,

	"accounting.settings":              true,
	"accounting.settings.read":         true,
	"accounting.contacts":              true,
	"accounting.contacts.read":         true,
	"accounting.attachments":           true,
	"accounting.attachments.read":      true,
	"accounting.budgets.read":          true,
	"accounting.payments":              true,
	"accounting.payments.read":         true,
	"accounting.invoices":              true,
	"accounting.invoices.read":         true,
	"accounting.banktransactions":      true,
	"accounting.banktransactions.read": true,
	"accounting.manualjournals":        true,
	"accounting.manualjournals.read":   true,

	"accounting.reports.aged.read":             true,
	"accounting.reports.balancesheet.read":     true,
	"accounting.reports.banksummary.read":      true,
	"accounting.reports.budgetsummary.read":    true,
	"accounting.reports.executivesummary.read": true,
	"accounting.reports.profitandloss.read":    true,
	"accounting.reports.trialbalance.read":     true,
	"accounting.reports.taxreports.read":       true,
	"accounting.reports.tenninetynine.read":    true,

	"payroll.employees":       true,
	"payroll.employees.read":  true,
	"payroll.payruns":         true,
	"payroll.payruns.read":    true,
	"payroll.payslip":         true,
	"payroll.payslip.read":    true,
	"payroll.settings":        true,
	"payroll.settings.read":   true,
	"payroll.timesheets":      true,
	"payroll.timesheets.read": true,

	"files":         true,
	"files.read":    true,
	"assets":        true,
	"assets.read":   true,
	"projects":      true,
	"projects.read": true,
}

func unknownXeroScopes(scopes []string) []string {
	var bad []string
	for _, s := range scopes {
		if !knownXeroScopes[s] {
			bad = append(bad, s)
		}
	}
	return bad
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
