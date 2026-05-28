package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kikubot/internal/config"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Xero OAuth 2.0 token store for the standard authorization-code flow.
//
// kikubot uses a WebApp-type Xero app in regions where Custom Connections
// aren't available (Cyprus / most of the EU). The standard flow yields an
// access_token (~30 min lifetime) and a refresh_token (~60 days, rotated on
// every grant). A single JSON file on disk holds the current pair plus the
// chosen tenant id; refreshes happen on demand inside XeroAccessToken before
// the access token expires, serialised through a package-level mutex so
// concurrent tool calls don't race.
//
// Bootstrap once via cmd/xero-bootstrap to seed the token file; afterwards
// kikubot maintains it on its own. The refresh token is rotated on every
// grant — we MUST persist atomically after a successful refresh, or the next
// refresh will fail with invalid_grant.

// xeroDir defaults to "xero" (working-directory relative for dev). Overridden
// to "data/xero" by InitDataPaths when running in a container so the token
// file lands on the persistent volume.
var xeroDir = "xero"

const (
	xeroTokenURL = "https://identity.xero.com/connect/token"
	xeroAPIBase  = "https://api.xero.com"
	// xeroRefreshSkew is the buffer before expiry at which we proactively
	// refresh. 2 min covers clock drift and request latency.
	xeroRefreshSkew = 2 * time.Minute
)

// DefaultXeroScopes is the scope set requested by the bootstrap flow when
// XERO_SCOPES is unset. offline_access is mandatory for receiving a refresh
// token. Read-only by default — extend as needed for writes.
//
// Note: Xero does NOT have an umbrella "accounting.transactions" scope.
// Transaction-shaped data is split across accounting.invoices,
// accounting.banktransactions, accounting.payments, and
// accounting.manualjournals (each with a matching .read variant).
var DefaultXeroScopes = []string{
	"offline_access",
	"accounting.contacts.read",
	"accounting.settings.read",
	"accounting.invoices.read",
	"accounting.banktransactions.read",
}

type XeroTokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	TenantID     string    `json:"tenant_id"`
	TenantName   string    `json:"tenant_name,omitempty"`
}

var (
	xeroMu     sync.Mutex
	xeroCached *XeroTokens
)

func xeroTokenFilePath() string {
	return filepath.Join(xeroDir, "tokens.json")
}

// LoadXeroTokens reads the token file from disk. Returns os.ErrNotExist if
// the file is missing — that signals the bootstrap hasn't been run yet.
func LoadXeroTokens() (*XeroTokens, error) {
	data, err := os.ReadFile(xeroTokenFilePath())
	if err != nil {
		return nil, err
	}
	var t XeroTokens
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parsing xero token file: %w", err)
	}
	return &t, nil
}

// SaveXeroTokens persists tokens with a tmp-and-rename to avoid leaving the
// file half-written if the process dies mid-save.
func SaveXeroTokens(t *XeroTokens) error {
	if err := os.MkdirAll(xeroDir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	path := xeroTokenFilePath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// XeroAccessToken returns a non-expired access token plus the tenant id,
// refreshing on demand. The first call after process start loads from disk;
// subsequent calls hit the in-memory cache. All refreshes are serialised
// through xeroMu.
func XeroAccessToken(ctx context.Context) (token, tenantID string, err error) {
	xeroMu.Lock()
	defer xeroMu.Unlock()

	if xeroCached == nil {
		t, loadErr := LoadXeroTokens()
		if loadErr != nil {
			return "", "", fmt.Errorf("xero not bootstrapped (run cmd/xero-bootstrap): %w", loadErr)
		}
		xeroCached = t
	}
	if time.Until(xeroCached.ExpiresAt) > xeroRefreshSkew {
		return xeroCached.AccessToken, xeroCached.TenantID, nil
	}
	if refreshErr := refreshXeroLocked(ctx); refreshErr != nil {
		return "", "", refreshErr
	}
	return xeroCached.AccessToken, xeroCached.TenantID, nil
}

// ForceXeroRefresh refreshes regardless of expiry. Used by XeroRequest after
// a 401, covering the case where Xero invalidated a still-cached token.
func ForceXeroRefresh(ctx context.Context) error {
	xeroMu.Lock()
	defer xeroMu.Unlock()
	if xeroCached == nil {
		t, err := LoadXeroTokens()
		if err != nil {
			return err
		}
		xeroCached = t
	}
	return refreshXeroLocked(ctx)
}

// refreshXeroLocked exchanges the cached refresh_token for a new access +
// refresh pair, then persists. MUST be called with xeroMu held.
func refreshXeroLocked(ctx context.Context) error {
	if strings.TrimSpace(config.XeroClientId) == "" || strings.TrimSpace(config.XeroClientSecret) == "" {
		return errors.New("XERO_CLIENT_ID or XERO_CLIENT_SECRET not set")
	}
	if xeroCached == nil || xeroCached.RefreshToken == "" {
		return errors.New("no xero refresh token — run cmd/xero-bootstrap")
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", xeroCached.RefreshToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, xeroTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("building refresh request: %w", err)
	}
	req.SetBasicAuth(config.XeroClientId, config.XeroClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("xero refresh request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("xero token refresh failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("parsing xero token response: %w", err)
	}
	if parsed.AccessToken == "" || parsed.RefreshToken == "" {
		return errors.New("xero token response missing access_token or refresh_token")
	}

	xeroCached.AccessToken = parsed.AccessToken
	xeroCached.RefreshToken = parsed.RefreshToken
	xeroCached.ExpiresAt = time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	if err := SaveXeroTokens(xeroCached); err != nil {
		// Loud warning: the in-memory token is fresh but the on-disk one is
		// stale. If the process dies before the next refresh, the bootstrap
		// will need to be re-run because Xero already rotated the refresh
		// token we still have on disk.
		log.Printf("[xero] WARNING: refreshed token but failed to persist to %s: %v", xeroTokenFilePath(), err)
	}
	log.Printf("[xero] access token refreshed; expires in %ds", parsed.ExpiresIn)
	return nil
}

// XeroRequest performs an authenticated request against the Xero REST API.
// Caller supplies path under the API base (e.g. "/api.xro/2.0/Invoices") and
// optional query/body. On a 401 we force a refresh and retry once; other
// non-2xx responses surface as errors with the response body included.
func XeroRequest(ctx context.Context, method, path string, query url.Values, body any) ([]byte, error) {
	doRequest := func() (int, []byte, error) {
		token, tenant, err := XeroAccessToken(ctx)
		if err != nil {
			return 0, nil, err
		}
		full := xeroAPIBase + path
		if len(query) > 0 {
			full += "?" + query.Encode()
		}
		var reader io.Reader
		if body != nil {
			buf, marshalErr := json.Marshal(body)
			if marshalErr != nil {
				return 0, nil, fmt.Errorf("encoding xero request body: %w", marshalErr)
			}
			reader = strings.NewReader(string(buf))
		}
		req, err := http.NewRequestWithContext(ctx, method, full, reader)
		if err != nil {
			return 0, nil, fmt.Errorf("building xero request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Xero-tenant-id", tenant)
		req.Header.Set("Accept", "application/json")
		if reader != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil, fmt.Errorf("xero request failed: %w", err)
		}
		defer resp.Body.Close()
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return 0, nil, fmt.Errorf("reading xero response: %w", err)
		}
		return resp.StatusCode, respBody, nil
	}

	status, respBody, err := doRequest()
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized {
		if refreshErr := ForceXeroRefresh(ctx); refreshErr != nil {
			return nil, fmt.Errorf("xero 401 + refresh failed: %w", refreshErr)
		}
		status, respBody, err = doRequest()
		if err != nil {
			return nil, err
		}
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("xero API error (HTTP %d) on %s %s: %s", status, method, path, strings.TrimSpace(string(respBody)))
	}
	return respBody, nil
}
