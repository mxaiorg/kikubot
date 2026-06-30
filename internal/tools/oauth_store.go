package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/mark3labs/mcp-go/client/transport"
)

// oauthDir holds one OAuth token file per MCP server (named "<key>.json").
// Defaults to "oauth" (working-directory relative, for dev); InitOAuthDir flips
// it to "data/oauth" in a container so the files land on the persistent volume
// (mirrors services.InitDataPaths for memory/snooze/xero).
var oauthDir = "oauth"

// InitOAuthDir points the OAuth token directory at the persistent data volume
// when running in a container. Call once at startup, alongside
// services.InitDataPaths.
func InitOAuthDir(inContainer bool) {
	if inContainer {
		oauthDir = "data/oauth"
	}
}

// FileTokenStore is a file-backed transport.TokenStore — the single piece the
// generic OAuth MCP plumbing needs that mcp-go doesn't already provide. mcp-go's
// OAuthHandler does all the refresh work: it checks expiry, exchanges the
// refresh token, and calls SaveToken with the rotated result. We persist that to
// disk so the (rotated, single-use) refresh token survives process restarts —
// the same trap the Xero token store guards against.
//
// One file per server, "<key>.json", holding a serialised transport.Token
// (access_token, refresh_token, token_type, expires_at). Hand-seed it once from
// your OAuth result; kikubot maintains it thereafter. Set expires_at in the past
// to force a refresh on the first call (handy when you don't know the exact
// access-token expiry). Writes are atomic (tmp+rename) and the store is
// mutex-guarded so a refresh can't race a read or leave a half-written file.
type FileTokenStore struct {
	path string
	mu   sync.Mutex
}

// NewFileTokenStore returns a store backed by oauthDir/<serverKey>.json.
func NewFileTokenStore(serverKey string) *FileTokenStore {
	return &FileTokenStore{path: filepath.Join(oauthDir, serverKey+".json")}
}

// GetToken implements transport.TokenStore. Returns transport.ErrNoToken when
// the file is absent or holds no access token, which is how mcp-go's handler
// distinguishes "not seeded yet" from an operational failure.
func (s *FileTokenStore) GetToken(ctx context.Context) (*transport.Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, transport.ErrNoToken
		}
		return nil, fmt.Errorf("reading oauth token file %s: %w", s.path, err)
	}
	var tok transport.Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, fmt.Errorf("parsing oauth token file %s: %w", s.path, err)
	}
	if tok.AccessToken == "" {
		return nil, transport.ErrNoToken
	}
	return &tok, nil
}

// SaveToken implements transport.TokenStore, persisting the rotated token
// atomically. Called by mcp-go's OAuthHandler after every successful refresh.
func (s *FileTokenStore) SaveToken(ctx context.Context, token *transport.Token) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(oauthDir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
