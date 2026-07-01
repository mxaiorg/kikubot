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
