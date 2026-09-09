package tools

import (
	"context"
	"encoding/json"
	"testing"

	"kikubot/internal/config"
	"kikubot/internal/services"
)

// TestSendEmail_ReplyAndForward_AppendsAttachment locks in the fix for the
// coordinator-loop bug: a message_tool call that sets BOTH In-Reply-To (stay
// in-thread) and X-Forwarded (deliver a file) must reply in-thread AND carry
// the forwarded email's attachments. The old if/else-if routing let In-Reply-To
// win and silently dropped the forward, so the recipient never got the file.
func TestSendEmail_ReplyAndForward_AppendsAttachment(t *testing.T) {
	const (
		replyID = "<reply@agents.mxhero.com>"
		fwdID   = "<draft@agents.mxhero.com>"
	)

	restore := stubMailIO(t)
	defer restore()

	// Return a distinct email per requested Message-Id: the reply target has no
	// attachment; the forwarded draft carries the file we expect to be delivered.
	services.GetEmails = func(_ context.Context, ids []string) ([]services.Email, error) {
		switch ids[0] {
		case replyID:
			return []services.Email{{
				MessageId: replyID,
				Subject:   "Re: Newsletter draft",
			}}, nil
		case fwdID:
			return []services.Email{{
				MessageId: fwdID,
				Subject:   "Re: Newsletter draft",
				Attachments: []services.Attachment{
					{Name: "mxhero-newsletter-draft.txt", Data: []byte("draft body")},
				},
			}}, nil
		default:
			return nil, nil
		}
	}

	var sent services.Email
	services.SendEmail = func(_ context.Context, msg services.Email) error {
		sent = msg
		return nil
	}

	input := mustJSON(t, map[string]any{
		"To":          "beta@agents.mxhero.com",
		"In-Reply-To": replyID,
		"X-Forwarded": fwdID,
		"Message":     "Please merge in the mxMCP section.",
	})

	if _, err := sendEmail(context.Background(), input); err != nil {
		t.Fatalf("sendEmail returned error: %v", err)
	}

	// The forwarded attachment must be on the wire.
	if len(sent.Attachments) != 1 {
		t.Fatalf("expected 1 forwarded attachment, got %d: %+v", len(sent.Attachments), sent.Attachments)
	}
	if got := sent.Attachments[0].Name; got != "mxhero-newsletter-draft.txt" {
		t.Errorf("attachment name = %q, want mxhero-newsletter-draft.txt", got)
	}
	if got := string(sent.Attachments[0].Data); got != "draft body" {
		t.Errorf("attachment data = %q, want %q", got, "draft body")
	}

	// Reply threading must be preserved — subject stays "Re:" (not "Fwd:") and
	// In-Reply-To is set. This is what distinguishes attachment-only compose
	// from the forward-only branch.
	if sent.Subject != "Re: Newsletter draft" {
		t.Errorf("subject = %q, want %q (reply threading, not Fwd)", sent.Subject, "Re: Newsletter draft")
	}
	if sent.InReplyTo != replyID {
		t.Errorf("InReplyTo = %q, want %q", sent.InReplyTo, replyID)
	}
}

// TestSendEmail_ForwardOnly_StillForwards guards the untouched forward-only
// branch: with no In-Reply-To, the message is a "Fwd:" and still carries the
// forwarded attachment.
func TestSendEmail_ForwardOnly_StillForwards(t *testing.T) {
	const fwdID = "<draft@agents.mxhero.com>"

	restore := stubMailIO(t)
	defer restore()

	services.GetEmails = func(_ context.Context, _ []string) ([]services.Email, error) {
		return []services.Email{{
			MessageId: fwdID,
			Subject:   "Newsletter draft",
			Attachments: []services.Attachment{
				{Name: "mxhero-newsletter-draft.txt", Data: []byte("draft body")},
			},
		}}, nil
	}

	var sent services.Email
	services.SendEmail = func(_ context.Context, msg services.Email) error {
		sent = msg
		return nil
	}

	input := mustJSON(t, map[string]any{
		"To":          "beta@agents.mxhero.com",
		"X-Forwarded": fwdID,
		"Message":     "Here's the draft.",
	})

	if _, err := sendEmail(context.Background(), input); err != nil {
		t.Fatalf("sendEmail returned error: %v", err)
	}

	if len(sent.Attachments) != 1 {
		t.Fatalf("expected 1 forwarded attachment, got %d", len(sent.Attachments))
	}
	if sent.Subject != "Fwd: Newsletter draft" {
		t.Errorf("subject = %q, want %q", sent.Subject, "Fwd: Newsletter draft")
	}
}

// TestSendEmail_ReplyOnly_NoAttachment confirms a plain in-thread reply (no
// X-Forwarded) carries no attachment and does not touch GetEmails a second time.
func TestSendEmail_ReplyOnly_NoAttachment(t *testing.T) {
	const replyID = "<reply@agents.mxhero.com>"

	restore := stubMailIO(t)
	defer restore()

	services.GetEmails = func(_ context.Context, _ []string) ([]services.Email, error) {
		return []services.Email{{MessageId: replyID, Subject: "Re: Newsletter draft"}}, nil
	}

	var sent services.Email
	services.SendEmail = func(_ context.Context, msg services.Email) error {
		sent = msg
		return nil
	}

	input := mustJSON(t, map[string]any{
		"To":          "beta@agents.mxhero.com",
		"In-Reply-To": replyID,
		"Message":     "Thanks!",
	})

	if _, err := sendEmail(context.Background(), input); err != nil {
		t.Fatalf("sendEmail returned error: %v", err)
	}
	if len(sent.Attachments) != 0 {
		t.Fatalf("expected no attachments on a plain reply, got %d", len(sent.Attachments))
	}
}

// stubMailIO sets a known agent identity and returns a restore func for the
// mail-IO seams the caller overrides. It saves/restores GetEmails and SendEmail
// so tests don't leak stubs into one another.
func stubMailIO(t *testing.T) func() {
	t.Helper()
	origGet := services.GetEmails
	origSend := services.SendEmail
	origEmail := config.AgentEmail
	origName := config.AgentName
	config.AgentEmail = "kiku@agents.mxhero.com"
	config.AgentName = "Kiku"
	return func() {
		services.GetEmails = origGet
		services.SendEmail = origSend
		config.AgentEmail = origEmail
		config.AgentName = origName
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal test input: %v", err)
	}
	return b
}

// TestSendEmail_NeverEmptySubject covers every route by which an outbound
// could previously leave with a blank Subject header. The reported failure
// was a report_tool reply: report_tool and report_strict_tool have no Subject
// field in their schemas at all, so when the model omits In-Reply-To (which
// the schema marks required but nothing enforces), subject resolution fell
// through to params.Subject — a field the tool can never populate — and the
// recipient got a subject-less email.
func TestSendEmail_NeverEmptySubject(t *testing.T) {
	const parentID = "<parent@agents.mxhero.com>"

	cases := []struct {
		name string
		// parent is the email GetEmails returns for In-Reply-To lookups.
		parent *services.Email
		// src is the trusted inbound stashed on ctx by HandleMessage.
		src   *services.Email
		input map[string]any
		want  string
	}{
		{
			name:  "new message, no subject, no inbound context",
			input: map[string]any{"To": "beta@agents.mxhero.com", "Message": "hi"},
			want:  "Message from Kiku",
		},
		{
			name:  "new message, no subject, inherits inbound thread subject",
			src:   &services.Email{MessageId: parentID, Subject: "Quarterly numbers"},
			input: map[string]any{"To": "beta@agents.mxhero.com", "Message": "hi"},
			want:  "Re: Quarterly numbers",
		},
		{
			name:  "new message, explicit empty subject string",
			src:   &services.Email{MessageId: parentID, Subject: "Re: Quarterly numbers"},
			input: map[string]any{"To": "beta@agents.mxhero.com", "Subject": "", "Message": "hi"},
			want:  "Re: Quarterly numbers",
		},
		{
			name:   "reply to a parent that itself had no subject",
			parent: &services.Email{MessageId: parentID},
			src:    &services.Email{MessageId: parentID},
			input: map[string]any{
				"To": "beta@agents.mxhero.com", "In-Reply-To": parentID, "Message": "hi",
			},
			want: "Message from Kiku",
		},
		{
			name:   "reply to a subject-less parent, thread topic survives",
			parent: &services.Email{MessageId: parentID},
			src:    &services.Email{MessageId: parentID, ThreadTopic: "Budget review"},
			input: map[string]any{
				"To": "beta@agents.mxhero.com", "In-Reply-To": parentID, "Message": "hi",
			},
			want: "Re: Budget review",
		},
		{
			name:   "reply keeps the parent subject without double-prefixing",
			parent: &services.Email{MessageId: parentID, Subject: "RE: Newsletter draft"},
			input: map[string]any{
				"To": "beta@agents.mxhero.com", "In-Reply-To": parentID, "Message": "hi",
			},
			want: "RE: Newsletter draft",
		},
		{
			name:   "forward of a subject-less parent",
			parent: &services.Email{MessageId: parentID},
			input: map[string]any{
				"To": "beta@agents.mxhero.com", "X-Forwarded": parentID, "Message": "hi",
			},
			want: "Message from Kiku",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := stubMailIO(t)
			defer restore()

			parent := tc.parent
			services.GetEmails = func(_ context.Context, _ []string) ([]services.Email, error) {
				if parent == nil {
					return nil, nil
				}
				return []services.Email{*parent}, nil
			}

			var sent services.Email
			services.SendEmail = func(_ context.Context, msg services.Email) error {
				sent = msg
				return nil
			}

			ctx := context.Background()
			if tc.src != nil {
				ctx = services.WithSourceEmail(ctx, tc.src)
			}

			if _, err := sendEmail(ctx, mustJSON(t, tc.input)); err != nil {
				t.Fatalf("sendEmail returned error: %v", err)
			}
			if sent.Subject != tc.want {
				t.Errorf("subject = %q, want %q", sent.Subject, tc.want)
			}
		})
	}
}

// TestAllRecipientsAllowed_WhitelistedNonParticipant locks in the fix for
// the silently-redirected daily report: a recipient configured in the
// agent's whitelist (and named by the knowledge base) is a legitimate
// report target even though it has never sent into the thread. Before the
// fix, heal treated it as a typo and substituted the last human to speak,
// so the report went to the CC'd requester instead of the seller.
func TestAllRecipientsAllowed_WhitelistedNonParticipant(t *testing.T) {
	origWhitelist := config.Whitelist
	defer func() { config.Whitelist = origWhitelist }()
	config.Whitelist = []string{"agents.blacklich.com", "apanagides@gmail.com", "oliverpanagides@gmail.com"}

	// Only the requester has spoken in this thread.
	humans := map[string]bool{"apanagides@gmail.com": true}

	cases := []struct {
		name string
		to   []string
		want bool
	}{
		{"whitelisted non-participant", []string{"oliverpanagides@gmail.com"}, true},
		{"thread participant", []string{"apanagides@gmail.com"}, true},
		{"both", []string{"oliverpanagides@gmail.com", "apanagides@gmail.com"}, true},
		{"display name form", []string{`"Oliver" <oliverpanagides@gmail.com>`}, true},
		{"case insensitive", []string{"OliverPanagides@Gmail.com"}, true},
		{"unknown address still healed", []string{"stranger@gmail.com"}, false},
		{"one bad recipient fails the set", []string{"oliverpanagides@gmail.com", "stranger@gmail.com"}, false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := allRecipientsAllowed(tc.to, humans); got != tc.want {
				t.Fatalf("allRecipientsAllowed(%v) = %v, want %v", tc.to, got, tc.want)
			}
		})
	}
}

// TestWhitelistedAddress_IgnoresBareDomains ensures a domain-only whitelist
// entry (a coarse inbound-ACL grant) does not become an outbound recipient
// allowlist — otherwise heal would stop catching local-part typos at every
// whitelisted domain, which is most of what it exists for.
func TestWhitelistedAddress_IgnoresBareDomains(t *testing.T) {
	origWhitelist := config.Whitelist
	defer func() { config.Whitelist = origWhitelist }()
	config.Whitelist = []string{"agents.blacklich.com", "oliverpanagides@gmail.com"}

	if whitelistedAddress("typo@agents.blacklich.com") {
		t.Fatal("bare-domain whitelist entry must not authorize an arbitrary address at that domain")
	}
	if !whitelistedAddress("oliverpanagides@gmail.com") {
		t.Fatal("explicit address entry should be authorized")
	}
	if whitelistedAddress("") {
		t.Fatal("empty address should never be authorized")
	}
}
