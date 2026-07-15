package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kikubot/internal/config"
)

// wpTestServer stands in for a WordPress site's /wp-json/wp/v2/users/me
// endpoint. WordPress does not reject an invalid application password
// outright — it downgrades the request to anonymous and answers 401
// rest_not_logged_in — which is exactly the behaviour the preflight has to
// distinguish from a real login.
func wpTestServer(t *testing.T, wantUser, wantPass string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wp-json/wp/v2/users/me" {
			http.NotFound(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if ok && user == wantUser && pass == wantPass {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "name": wantUser, "slug": wantUser})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"code": "rest_not_logged_in", "message": "You are not currently logged in.",
			"data": map[string]any{"status": 401},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWordPressPreflight_ValidCredentialsPass(t *testing.T) {
	srv := wpTestServer(t, "admin", "app pass word")
	if msg := wordpressPreflight(context.Background(), srv.URL, "admin", "app pass word"); msg != "" {
		t.Fatalf("valid credentials rejected: %q", msg)
	}
}

func TestWordPressPreflight_RevokedPasswordIsTerminal(t *testing.T) {
	srv := wpTestServer(t, "admin", "current password")
	msg := wordpressPreflight(context.Background(), srv.URL, "admin", "revoked password")
	if msg == "" {
		t.Fatal("revoked credentials passed preflight")
	}
	for _, want := range []string{"invalid or has been revoked", "Do NOT retry", "report"} {
		if !strings.Contains(msg, want) {
			t.Errorf("terminal message missing %q:\n%s", want, msg)
		}
	}
}

// A 200 whose body has no real user id (id null / missing) must not pass:
// it is the anonymous-downgrade signature seen through some proxies.
func TestWordPressPreflight_AnonymousBodyIsTerminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":null,"name":null}`))
	}))
	t.Cleanup(srv.Close)
	if msg := wordpressPreflight(context.Background(), srv.URL, "admin", "x"); msg == "" {
		t.Fatal("200 with null user id passed preflight")
	}
}

func TestWordPressPreflight_UnreachableSiteIsTerminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // guaranteed-refused address
	msg := wordpressPreflight(context.Background(), srv.URL, "admin", "x")
	if msg == "" {
		t.Fatal("unreachable site passed preflight")
	}
	if !strings.Contains(msg, "unreachable") || !strings.Contains(msg, "Do NOT retry") {
		t.Errorf("unreachable message lacks diagnosis/stop directive:\n%s", msg)
	}
}

// The workflow instructions must reference credentials only via environment
// expansion — the literal password (or its base64) fossilising in thread
// memory is what made a rotated application password unrecoverable in-thread.
func TestExecuteWordPress_NeverInlinesCredentials(t *testing.T) {
	const user, pass = "admin", "6cIP test only pass"
	srv := wpTestServer(t, user, pass)

	prevURL, prevUser, prevPass := config.WebSiteUrl, config.WordPressUser, config.WordPressPassword
	config.WebSiteUrl, config.WordPressUser, config.WordPressPassword = srv.URL, user, pass
	t.Cleanup(func() {
		config.WebSiteUrl, config.WordPressUser, config.WordPressPassword = prevURL, prevUser, prevPass
	})

	out, err := executeWordPress(context.Background(), json.RawMessage(`{"task":"test"}`))
	if err != nil {
		t.Fatalf("executeWordPress: %v", err)
	}
	if !strings.Contains(out, `AUTH=$(printf '%s:%s' "$WORDPRESS_USER" "$WORDPRESS_PASSWORD" | base64 | tr -d '\n')`) {
		t.Error("instructions missing env-expansion AUTH line")
	}
	inlined := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	for _, leak := range []string{pass, inlined} {
		if strings.Contains(out, leak) {
			t.Errorf("credentials leaked into instructions: found %q", leak)
		}
	}
}

func TestExecuteWordPress_UnconfiguredIsTerminal(t *testing.T) {
	prevURL, prevUser, prevPass := config.WebSiteUrl, config.WordPressUser, config.WordPressPassword
	config.WebSiteUrl, config.WordPressUser, config.WordPressPassword = "", "", ""
	t.Cleanup(func() {
		config.WebSiteUrl, config.WordPressUser, config.WordPressPassword = prevURL, prevUser, prevPass
	})

	out, err := executeWordPress(context.Background(), json.RawMessage(`{"task":"test"}`))
	if err != nil {
		t.Fatalf("executeWordPress: %v", err)
	}
	if !strings.Contains(out, "not configured") {
		t.Errorf("expected not-configured guidance, got:\n%s", out)
	}
}
