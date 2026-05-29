package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kikubot/internal/config"
	"net/http"
)

// ── Nuki Web API transport ──────────────────────────────────────────────
//
// Thin REST client for the Nuki Web API (https://api.nuki.io). It lives in
// services so multiple tool sets can share one transport: the public `nuki`
// tool (internal/tools/nuki.go) and the private `nuki_native` tool
// (internal/tools_priv/nuki_native.go). Auth is NUKI_API_TOKEN sent as a
// bearer token.

const NukiBaseURL = "https://api.nuki.io"

// NukiAuthTypeKeypad is the SmartlockAuth.type value for a keypad code (per
// swagger: "0 .. app, 1 .. bridge, 2 .. fob, 3 .. keypad, 13 .. keypad code").
// 13 is the value used when creating a new keypad PIN authorization.
const NukiAuthTypeKeypad = 13

// NukiRequest performs an authenticated call against the Nuki Web API and
// returns the raw response body. A non-2xx status is returned as an error
// with the response body attached.
func NukiRequest(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding request body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, NukiBaseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+config.NukiApiToken)
	req.Header.Set("Accept", "application/json")
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nuki request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("nuki API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}
