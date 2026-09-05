package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shape DMS's `setup config dkim domain agents.acme.com` actually writes.
const dmsGeneratedSigningConf = `# documentation: https://rspamd.com/doc/modules/dkim_signing.html

enabled = true;

sign_authenticated = true;
sign_local = true;

use_domain = "header";
use_redis = false; # don't change unless Redis also provides the DKIM keys
use_esld = true;

check_pubkey = true; # you want to use this in the beginning

domain {
    agents.acme.com {
        path = "/tmp/docker-mailserver/rspamd/dkim/rsa-2048-mail-agents.acme.com.private.txt";
        selector = "mail";
    }
}
`

func writeSigningConf(t *testing.T, root, body string) string {
	t.Helper()
	p := dmsDKIMSigningConfPath(root)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFixDKIMUseESLD_RewritesDMSDefault(t *testing.T) {
	root := t.TempDir()
	p := writeSigningConf(t, root, dmsGeneratedSigningConf)

	changed, err := fixDKIMUseESLD(root)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected the DMS default (use_esld = true) to be rewritten")
	}
	got, _ := os.ReadFile(p)
	s := string(got)
	if strings.Count(s, "use_esld") != 1 {
		t.Errorf("use_esld must appear exactly once (UCL would turn duplicates into an array):\n%s", s)
	}
	if !strings.Contains(s, "use_esld = false;") {
		t.Errorf("use_esld not set to false:\n%s", s)
	}
	// Nothing else moves.
	want := strings.Replace(dmsGeneratedSigningConf, "use_esld = true;", "use_esld = false;", 1)
	if s != want {
		t.Errorf("unexpected collateral edits\n got:\n%s\nwant:\n%s", s, want)
	}
}

func TestFixDKIMUseESLD_Idempotent(t *testing.T) {
	root := t.TempDir()
	writeSigningConf(t, root, dmsGeneratedSigningConf)
	if _, err := fixDKIMUseESLD(root); err != nil {
		t.Fatal(err)
	}
	changed, err := fixDKIMUseESLD(root)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("second run must be a no-op")
	}
}

func TestFixDKIMUseESLD_KeepsTrailingComment(t *testing.T) {
	root := t.TempDir()
	p := writeSigningConf(t, root, "enabled = true;\n  use_esld = true; # keep me\n")
	if _, err := fixDKIMUseESLD(root); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "enabled = true;\n  use_esld = false; # keep me\n" {
		t.Errorf("got:\n%q", got)
	}
}

func TestFixDKIMUseESLD_AppendsWhenAbsent(t *testing.T) {
	root := t.TempDir()
	p := writeSigningConf(t, root, "enabled = true;\ndomain {\n  agents.acme.com { selector = \"mail\"; }\n}")
	changed, err := fixDKIMUseESLD(root)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected an explicit use_esld = false to be added (rspamd defaults to true)")
	}
	got, _ := os.ReadFile(p)
	s := string(got)
	if strings.Count(s, "use_esld") != 1 || !strings.Contains(s, "\nuse_esld = false;\n") {
		t.Errorf("got:\n%s", s)
	}
}

func TestFixDKIMUseESLD_MissingFileIsNotAnError(t *testing.T) {
	changed, err := fixDKIMUseESLD(t.TempDir())
	if err != nil || changed {
		t.Errorf("changed=%v err=%v; want false,nil", changed, err)
	}
}

func TestLoadDKIMStatus(t *testing.T) {
	root := t.TempDir()

	st := loadDKIMStatus(root, "agents.acme.com")
	if st.Started() || st.Ready() {
		t.Errorf("empty tree should read as not started: %+v", st)
	}

	// Key generated, DMS default conf: everything present except use_esld.
	writeSigningConf(t, root, dmsGeneratedSigningConf)
	keyDir := dmsDKIMKeyDir(root)
	if err := os.MkdirAll(keyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{
		"rsa-2048-mail-agents.acme.com.private.txt",
		"rsa-2048-mail-agents.acme.com.public.txt",
		"rsa-2048-mail-agents.acme.com.public.dns.txt",
		// A key for a different domain must not count.
		"rsa-2048-mail-acme.com.private.txt",
	} {
		if err := os.WriteFile(filepath.Join(keyDir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	st = loadDKIMStatus(root, "Agents.ACME.com") // case-insensitive
	if !st.KeyExists || len(st.KeyFiles) != 1 || st.KeyFiles[0] != "rsa-2048-mail-agents.acme.com.private.txt" {
		t.Errorf("key detection: %+v", st)
	}
	if !st.ConfExists || !st.DomainInConf {
		t.Errorf("conf detection: %+v", st)
	}
	if st.UseESLDFalse || st.Ready() {
		t.Errorf("DMS default must read as not ready (use_esld = true): %+v", st)
	}

	if _, err := fixDKIMUseESLD(root); err != nil {
		t.Fatal(err)
	}
	st = loadDKIMStatus(root, "agents.acme.com")
	if !st.Ready() {
		t.Errorf("after fix, expected Ready: %+v", st)
	}
}

// Renders the real Email Service page with the DKIM checklist in each state.
// Templates are only parsed at startup, so without this a typo in the
// checklist markup would surface as a crash on `go run`, not in `go test`.
func TestEmailServiceTemplate_RendersDKIMStates(t *testing.T) {
	tmpls, err := buildTemplates()
	if err != nil {
		t.Fatal(err)
	}
	tpl := tmpls["email_service"]
	cases := map[string]struct {
		st   dkimStatus
		want []string
	}{
		"not started": {
			st:   dkimStatus{Domain: "agents.acme.com"},
			want: []string{"./dkim-setup.sh agents.acme.com"},
		},
		"not started, no domain": {
			st:   dkimStatus{},
			want: []string{"./dkim-setup.sh &lt;agent-domain&gt;"},
		},
		"dms default": {
			st: dkimStatus{Domain: "agents.acme.com", KeyExists: true,
				KeyFiles: []string{"rsa-2048-mail-agents.acme.com.private.txt"}, ConfExists: true, DomainInConf: true},
			want: []string{"rsa-2048-mail-agents.acme.com.private.txt", "silently skip signing"},
		},
		"ready": {
			st: dkimStatus{Domain: "agents.acme.com", KeyExists: true,
				KeyFiles: []string{"k"}, ConfExists: true, DomainInConf: true, UseESLDFalse: true},
			want: []string{"Config looks right", "./dkim-txt.sh agents.acme.com"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var buf strings.Builder
			view := emailServiceView{
				emailServiceConfig: &emailServiceConfig{Enabled: true, Hostname: "mail.agents.acme.com", AgentDomain: tc.st.Domain},
				DKIM:               tc.st,
			}
			if err := tpl.ExecuteTemplate(&buf, "email_service", pageData{Active: "email", Data: view}); err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(buf.String(), w) {
					t.Errorf("rendered page missing %q", w)
				}
			}
		})
	}
}
