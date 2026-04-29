package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"kikubot/internal/config"
	"kikubot/internal/services"
	"log"
	netmail "net/mail"
	"regexp"
	"slices"
	"strings"
	"time"
)

type sendMailMsg struct {
	To          string            `json:"To"`
	Cc          string            `json:"Cc"`
	InReplyTo   string            `json:"In-Reply-To"`
	Forward     string            `json:"X-Forwarded"`
	Subject     string            `json:"Subject"`
	Message     string            `json:"message"`
	Attachments []attachmentParam `json:"attachments"`
}

// sendEmailInternal is the same as sendEmail, but it validates the To and
// Cc addresses to ensure it's the same domain as the agent's email.
//
// Thread reference and origination chain (X-Senders) are recovered from ctx
// via services.SourceEmail() inside sendEmail — the LLM has no input into
// either, which prevents it from editing the ACL chain.
func sendEmailInternal(ctx context.Context, input json.RawMessage) (string, error) {
	var params sendMailMsg

	if err := json.Unmarshal(input, &params); err != nil {
		log.Printf("error parsing sendEmailInternal input: %s", err)
		return "", fmt.Errorf("invalid input: %w", err)
	}

	if strings.TrimSpace(params.Message) == "" {
		return "", fmt.Errorf("message body is empty — cannot send an email without content")
	}

	// get the domain of the agent's email
	splitAgentEmail := strings.Split(config.AgentEmail, "@")
	if len(splitAgentEmail) != 2 {
		return "", fmt.Errorf("invalid agent email format: %s", config.AgentEmail)
	}
	agentEmailDomain := strings.ToLower(splitAgentEmail[1])

	// 'To' email must be the same domain as the agent's email
	splitTo := strings.Split(params.To, "@")
	if len(splitTo) != 2 {
		return "", fmt.Errorf("invalid email format: %s", params.To)
	}
	toEmailDomain := strings.ToLower(splitTo[1])

	if agentEmailDomain != toEmailDomain {
		return "", fmt.Errorf("this tool can only send internal messages to the same email domain as the agent (%s)",
			agentEmailDomain)
	}

	// Verify To is a known coworker. The domain check above catches
	// wrong-domain addresses (e.g. "mail.agents.mxhero.com") but not
	// local-part typos or hallucinated agents (e.g. "gama@agents.mxhero.com"
	// when "gamma" was intended, or "echo@..." when no such agent exists).
	// Looking To up in global.AgentEmails closes that gap. Skipped when
	// AgentEmails hasn't been populated (no agents.yaml) — we fall back
	// to trusting the domain check alone in that case.
	if len(config.AgentEmails) > 0 {
		toAddr := bareAddressFromEmail(params.To)
		if toAddr == "" {
			toAddr = strings.ToLower(strings.TrimSpace(params.To))
		}
		if !config.AgentEmails[toAddr] {
			peers := knownPeerEmails()
			return "", fmt.Errorf(
				"unknown coworker %q — known coworkers: %s. Check spelling (local part) and retry",
				params.To, strings.Join(peers, ", "),
			)
		}
	}

	// Filter Cc addresses to only include those with the same domain as the agent
	if params.Cc != "" {
		ccAddresses := commaSplitReg.Split(params.Cc, -1)
		var validCc []string
		for _, cc := range ccAddresses {
			cc = strings.TrimSpace(cc)
			if cc == "" {
				continue
			}
			splitCc := strings.Split(cc, "@")
			if len(splitCc) != 2 {
				continue
			}
			ccDomain := strings.ToLower(splitCc[1])
			if ccDomain == agentEmailDomain {
				validCc = append(validCc, cc)
			}
		}
		// Update params.Cc with filtered addresses
		if len(validCc) > 0 {
			params.Cc = strings.Join(validCc, ", ")
		} else {
			params.Cc = ""
		}
		// Re-marshal params back to input
		updatedInput, err := json.Marshal(params)
		if err != nil {
			return "", fmt.Errorf("failed to update Cc addresses: %w", err)
		}
		input = updatedInput
	}

	// passes validation, send the email
	return sendEmail(ctx, input)
}

func sendEmail(ctx context.Context, input json.RawMessage) (string, error) {
	var params sendMailMsg

	if err := json.Unmarshal(input, &params); err != nil {
		log.Printf("error parsing sendEmail input: %s", err)
		return "", fmt.Errorf("invalid input: %w", err)
	}

	if strings.TrimSpace(params.Message) == "" {
		return "", fmt.Errorf("message body is empty — cannot send an email without content")
	}

	// Soft cap on the inline body. The model has finite output tokens; a
	// 50KB+ "message" payload will routinely truncate the surrounding tool
	// JSON and burn an entire turn budget on retries. If the agent has
	// something that big to send, it should be an attachment.
	if config.MaxMessageBodyChars > 0 && len(params.Message) > config.MaxMessageBodyChars {
		return "", fmt.Errorf(
			"message body is %d chars, exceeds the inline cap of %d. "+
				"Move the bulk of this content into an attachment (use the "+
				"`attachments` field with a descriptive filename) and keep "+
				"the `message` body to a short summary or cover note",
			len(params.Message), config.MaxMessageBodyChars,
		)
	}

	var references []string
	var subject string
	var content string
	var attachments []services.Attachment

	lcAgentEmail := strings.TrimSpace(strings.ToLower(config.AgentEmail))
	// To Recipients
	if params.To == "" {
		return "", fmt.Errorf("no To address specified")
	}
	toRecipients := commaSplitReg.Split(params.To, -1)
	// CAN'T SEND EMAIL TO ITSELF
	toRecipients = slices.DeleteFunc(toRecipients, func(s string) bool {
		recipient := strings.TrimSpace(strings.ToLower(s))
		return recipient == "" || recipient == lcAgentEmail
	})
	if len(toRecipients) == 0 {
		return "", fmt.Errorf("no valid recipients specified")
	}

	// Cc Recipients
	var ccRecipients []string
	if params.Cc != "" {
		ccRecipients = commaSplitReg.Split(params.Cc, -1)
		// CAN'T SEND EMAIL TO ITSELF
		ccRecipients = slices.DeleteFunc(ccRecipients, func(s string) bool {
			recipient := strings.TrimSpace(strings.ToLower(s))
			return recipient == "" || recipient == lcAgentEmail
		})
		if len(ccRecipients) == 0 {
			// not required, let it pass
			log.Printf("no valid CC recipients specified")
		}
	}

	inReplyTo := services.EnsureAngleBrackets(params.InReplyTo)
	forward := services.EnsureAngleBrackets(params.Forward)

	if ctx == nil {
		ctx = context.Background()
	}

	// Trusted thread reference and sender chain come from the inbound email
	// stashed on ctx by HandleMessage — not from LLM input. This is the ACL
	// origination chain: stripping an identity here is impossible for the LLM.
	srcEmail := services.SourceEmail(ctx)
	var senders []string
	if srcEmail != nil {
		senders = services.AddToSenders(srcEmail.Senders, strings.ToLower(srcEmail.From))
	}

	if inReplyTo != "" {
		replyEmails, replyErr := services.GetEmails(ctx, []string{inReplyTo})
		if replyErr != nil {
			log.Printf("error getting reply email: %s", replyErr)
			return "", fmt.Errorf("error getting reply email: %w", replyErr)
		}
		if len(replyEmails) == 0 {
			log.Printf("no reply email found")
			return "", fmt.Errorf("no reply email found")
		}
		replyEmail := replyEmails[0]
		references = append(replyEmail.References, replyEmail.MessageId)
		if strings.HasPrefix(replyEmail.Subject, "Re: ") {
			subject = replyEmail.Subject
		} else {
			subject = "Re: " + replyEmail.Subject
		}
		content = services.ReplyBody(&replyEmail, params.Message)
	} else if forward != "" {
		fwdEmails, fwdErr := services.GetEmails(ctx, []string{forward})
		if fwdErr != nil {
			return "", fmt.Errorf("error getting forward email: %w", fwdErr)
		}
		if len(fwdEmails) == 0 {
			return "", fmt.Errorf("no forward email found")
		}
		fwdEmail := fwdEmails[0]
		// Adding references to the forwarded email is unusual - but useful for this system's architecture
		references = append(fwdEmail.References, fwdEmail.MessageId)
		if strings.HasPrefix(fwdEmail.Subject, "Fwd: ") {
			subject = fwdEmail.Subject
		} else {
			subject = "Fwd: " + fwdEmail.Subject
		}
		content = services.ForwardBody(&fwdEmail, params.Message)
		// Include original attachments when forwarding
		attachments = append(attachments, fwdEmail.Attachments...)
	} else if srcEmail != nil && srcEmail.MessageId != "" {
		// New email (e.g., delegation). Thread it onto the inbound that
		// started this turn so downstream ACL can walk References back to
		// the original user.
		references = append(srcEmail.References, srcEmail.MessageId)
		content = params.Message
	}

	// Fall back to the subject provided by the caller (e.g., new emails)
	if subject == "" {
		subject = params.Subject
	}
	if content == "" {
		content = params.Message
	}

	// Decode LLM-provided attachments
	for _, ap := range params.Attachments {
		att, err := ap.toAttachment()
		if err != nil {
			log.Printf("warning: skipping attachment %s: %v", ap.Name, err)
			continue
		}
		attachments = append(attachments, att)
	}

	fromAddr := config.AgentEmail
	if config.AgentName != "" {
		fromAddr = fmt.Sprintf("\"%s\" <%s>", config.AgentName, config.AgentEmail)
	}

	msg := services.Email{
		To:          toRecipients,
		From:        fromAddr,
		Cc:          ccRecipients,
		Date:        time.Now(),
		References:  references,
		InReplyTo:   inReplyTo, // see note above
		Senders:     senders,
		Subject:     subject,
		Content:     content,
		Attachments: attachments,
	}

	err := services.SendEmail(ctx, msg)
	if err != nil {
		log.Printf("error sending email: %s", err)
		return "", err
	}
	return "Success", nil
}

var commaSplitReg = regexp.MustCompile(`\s*,\s*`)

// sendReportEmail is report_tool's Execute function. It wraps sendEmail
// with a user-reply auto-heal: when the LLM appears to have conflated a
// thread Message-Id with the recipient address, we walk References
// newest→oldest to find the most recent human sender and substitute their
// address. Scoped to report_tool (not send_message) so legitimate
// peer-to-peer replies are untouched.
func sendReportEmail(ctx context.Context, input json.RawMessage) (string, error) {
	healed, err := healReportRecipient(ctx, input)
	if err != nil {
		return "", err
	}
	return sendEmail(ctx, healed)
}

// healReportRecipient returns tool input with a corrected "To" for
// report_tool replies. When the call carries an In-Reply-To, we always
// walk the thread for the newest human sender and — if the LLM's To list
// doesn't already include that human — substitute it. This catches three
// common LLM failures with one mechanism: (a) Message-Id pasted into To,
// (b) hallucinated / typoed domain (e.g. alex@mail.mxhero.com), and
// (c) stale address from training data instead of the actual sender.
//
// When no human can be resolved from the thread AND the supplied To is
// clearly wrong (matches a thread Message-Id), we surface an actionable
// error so the LLM retries. Otherwise the LLM's value is trusted.
//
// New emails (no In-Reply-To) are untouched — the LLM may legitimately be
// sending to a third party. Peer-to-peer delegation goes through
// sendEmailInternal and does not reach this function.
func healReportRecipient(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	var params sendMailMsg
	if err := json.Unmarshal(input, &params); err != nil {
		return input, nil // let sendEmail emit its own parse error
	}
	if params.InReplyTo == "" {
		return input, nil
	}

	inReplyTo := services.EnsureAngleBrackets(params.InReplyTo)
	replyEmails, err := services.GetEmails(ctx, []string{inReplyTo})
	if err != nil || len(replyEmails) == 0 {
		return input, nil // let sendEmail raise the real error
	}
	replyEmail := replyEmails[0]

	toRecipients := commaSplitReg.Split(params.To, -1)
	human, foundHuman := findHumanInThread(ctx, &replyEmail)

	// Happy path: the LLM's To already contains the thread's human
	// sender. Trust the full To list (preserves legitimate multi-recipient
	// replies where the LLM CCs additional mxhero.com folks into To).
	if foundHuman && toContainsAddress(toRecipients, human) {
		return input, nil
	}

	// Detect the specific "Message-Id pasted as To" pathology for error
	// messaging. Useful as a crisp signal when we also can't resolve a
	// human from the thread.
	threadIds := make([]string, 0, len(replyEmail.References)+2)
	threadIds = append(threadIds, replyEmail.MessageId, inReplyTo)
	threadIds = append(threadIds, replyEmail.References...)
	conflation := toMatchesMessageIds(toRecipients, threadIds)

	if !foundHuman {
		// No human resolvable from the thread. If the To also looks like
		// a Message-Id, there's no safe answer — force the LLM to retry.
		if conflation {
			return nil, fmt.Errorf(
				"report_tool: To %v matches a thread Message-Id (likely confused with In-Reply-To); no human sender resolvable from References. Re-issue with the user's email address in To",
				toRecipients,
			)
		}
		// Otherwise trust the LLM — thread may be all-agent and the model
		// knows the right external address.
		return input, nil
	}

	// Human found, and it isn't in the LLM's To. Substitute the thread's
	// human as the sole recipient. Multi-recipient user replies are rare
	// and the LLM can include the human explicitly when it wants one.
	switch {
	case params.To == "":
		log.Printf("report_tool: To empty; setting to thread-derived recipient %q", human)
	case conflation:
		log.Printf("report_tool: To (%v) matches a thread Message-Id; substituting thread-derived recipient %q", toRecipients, human)
	default:
		log.Printf("report_tool: To (%v) does not match thread-derived recipient %q (likely wrong domain); substituting", toRecipients, human)
	}
	params.To = human
	out, marshalErr := json.Marshal(params)
	if marshalErr != nil {
		return input, nil
	}
	return out, nil
}

// toContainsAddress reports whether `to` includes `target` (bare, lowercased)
// either as a raw string or after RFC 5322 parsing. Used to decide whether
// the LLM's To already includes the thread's human sender.
func toContainsAddress(to []string, target string) bool {
	if target == "" {
		return false
	}
	target = strings.ToLower(target)
	for _, r := range to {
		raw := strings.ToLower(strings.TrimSpace(r))
		if raw == target {
			return true
		}
		if addr := bareAddressFromEmail(r); addr == target {
			return true
		}
	}
	return false
}

// knownPeerEmails returns a sorted list of agent emails in
// global.AgentEmails excluding the current agent's own address. Used to
// render actionable "unknown coworker" errors that show the LLM the exact
// set of valid recipients.
func knownPeerEmails() []string {
	self := strings.ToLower(strings.TrimSpace(config.AgentEmail))
	out := make([]string, 0, len(config.AgentEmails))
	for email := range config.AgentEmails {
		if email != self {
			out = append(out, email)
		}
	}
	slices.Sort(out)
	return out
}

// bareAddressFromEmail returns the bare mailbox (no display name) from an
// RFC 5322 address string, lowercased. Returns "" if unparseable.
func bareAddressFromEmail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if addr, err := netmail.ParseAddress(s); err == nil {
		return strings.ToLower(addr.Address)
	}
	return ""
}

// toMatchesMessageIds reports whether every entry in `to` equals some
// Message-Id in `ids`, ignoring angle brackets and case. Used to detect
// the LLM having pasted a thread Message-Id into the recipient field.
// Empty inputs return false.
func toMatchesMessageIds(to []string, ids []string) bool {
	if len(to) == 0 || len(ids) == 0 {
		return false
	}
	norm := func(s string) string {
		s = strings.TrimSpace(strings.ToLower(s))
		s = strings.TrimPrefix(s, "<")
		s = strings.TrimSuffix(s, ">")
		return s
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		if n := norm(id); n != "" {
			set[n] = true
		}
	}
	for _, r := range to {
		if !set[norm(r)] {
			return false
		}
	}
	return true
}

// findHumanInThread walks a thread newest→oldest looking for the first
// sender whose address is NOT in global.AgentEmails. Starts with the
// replyEmail itself (the most recent ancestor) and then walks its
// References backwards. Returns the bare lowercased address and true on
// hit; "" and false if no human is found or AgentEmails is uninitialized.
// Message lookups rely on services.GetEmails' cache, so repeat calls in
// the same thread are cheap.
func findHumanInThread(ctx context.Context, replyEmail *services.Email) (string, bool) {
	if replyEmail == nil || len(config.AgentEmails) == 0 {
		return "", false
	}
	// Newest: the reply target itself.
	if addr := bareAddressFromEmail(replyEmail.From); addr != "" && !config.AgentEmails[addr] {
		return addr, true
	}
	// Older: walk References newest→oldest. References is chronological,
	// oldest first (RFC 5322), so iterate from the back.
	for i := len(replyEmail.References) - 1; i >= 0; i-- {
		id := services.EnsureAngleBrackets(replyEmail.References[i])
		if id == "" {
			continue
		}
		emails, err := services.GetEmails(ctx, []string{id})
		if err != nil || len(emails) == 0 {
			continue
		}
		addr := bareAddressFromEmail(emails[0].From)
		if addr == "" || config.AgentEmails[addr] {
			continue
		}
		return addr, true
	}
	return "", false
}

// attachmentParam represents an attachment provided by the LLM in a tool call.
type attachmentParam struct {
	Name     string `json:"name"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"` // "base64" or "text"
}

func (ap attachmentParam) toAttachment() (services.Attachment, error) {
	var data []byte
	switch ap.Encoding {
	case "base64":
		var err error
		data, err = base64.StdEncoding.DecodeString(ap.Content)
		if err != nil {
			return services.Attachment{}, fmt.Errorf("base64 decode: %w", err)
		}
	case "text", "":
		data = []byte(ap.Content)
	default:
		return services.Attachment{}, fmt.Errorf("unsupported encoding: %s", ap.Encoding)
	}
	return services.Attachment{Name: ap.Name, Data: data}, nil
}
