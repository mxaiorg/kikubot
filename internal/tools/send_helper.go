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
		if strings.HasPrefix(replyEmail.Subject, "Re: ") {
			subject = replyEmail.Subject
		} else {
			subject = "Re: " + replyEmail.Subject
		}
		content = services.ReplyBody(&replyEmail, params.Message)

		// Threading: when the inbound that triggered this turn (srcEmail)
		// shares a thread with the LLM's chosen reply target, always
		// thread off srcEmail. The LLM sometimes picks an older ancestor
		// — e.g. the original requester's message when "reporting back"
		// — which collapses References to just the root and orphans the
		// outbound from the latest activity in the thread. The LLM still
		// controls quoted body content via its choice of replyEmail
		// above; it just doesn't get to redirect the threading parent.
		if srcEmail != nil && srcEmail.MessageId != "" && sameThread(srcEmail, &replyEmail) {
			srcId := services.EnsureAngleBrackets(srcEmail.MessageId)
			replyId := services.EnsureAngleBrackets(replyEmail.MessageId)
			if srcId != replyId {
				log.Printf("threading: overriding LLM In-Reply-To %s with trusted srcEmail %s",
					replyId, srcId)
			}
			inReplyTo = srcId
			references = append(srcEmail.References, srcId)
		} else {
			references = append(replyEmail.References, replyEmail.MessageId)
		}
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
// report_tool replies. The strategy is membership-based, not equality-
// based: we collect every non-agent participant in the thread and trust
// the LLM's To when every recipient belongs to that set. Multi-party
// threads can have several legitimate reply targets (original requester
// + follow-up correspondent), and forcing equality with the most-recent
// human would substitute one valid choice for another.
//
// Heal only intervenes when a recipient clearly doesn't belong to the
// thread, which catches three common LLM failures with one mechanism:
// (a) Message-Id pasted into To, (b) hallucinated / typoed domain
// (e.g. alex@mail.mxhero.com), and (c) stale address from training data.
// In those cases we substitute the newest human walked from the trusted
// inbound (srcEmail), or surface an actionable error if no human can be
// resolved and the To looks like a Message-Id.
//
// New emails (no In-Reply-To) are untouched — the LLM may legitimately
// be sending to a third party. Peer-to-peer delegation goes through
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

	// Prefer the trusted inbound (srcEmail) as the seed for thread walks
	// whenever it sits in the same thread as the LLM's chosen reply target.
	// The LLM may pick an older ancestor as In-Reply-To (e.g. the root,
	// when "reporting back" to the original requester); seeding from that
	// ancestor short-circuits findHumanInThread on the first non-agent it
	// sees and can substitute the conversation's original sender for the
	// LLM's correctly-chosen, more recent correspondent. srcEmail is the
	// newest known message in the chain — its From + References cover
	// everyone we need.
	seed := &replyEmail
	if src := services.SourceEmail(ctx); src != nil && sameThread(src, &replyEmail) {
		seed = src
	}

	toRecipients := commaSplitReg.Split(params.To, -1)

	// Trust the LLM's To when every recipient is a known human participant
	// in this thread. Multi-party threads have multiple legitimate reply
	// targets — e.g. the original requester *and* a follow-up correspondent
	// — and the LLM may rationally pick any of them. Heal should only step
	// in when a recipient clearly doesn't belong (typoed domain, stale
	// address, Message-Id pasted as To).
	humans := collectHumansInThread(ctx, seed)
	if len(humans) > 0 && allRecipientsInHumans(toRecipients, humans) {
		return input, nil
	}

	// Fallback: identify the newest human for substitution, and detect
	// Message-Id-as-To conflation for an actionable error.
	human, foundHuman := findHumanInThread(ctx, seed)

	threadIds := make([]string, 0, len(seed.References)+2)
	threadIds = append(threadIds, seed.MessageId, inReplyTo)
	threadIds = append(threadIds, seed.References...)
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

// collectHumansInThread returns the set of non-agent sender addresses
// (lowercased) participating in the thread reachable from `seed`. The
// seed's own From plus the From of every message reachable via its
// References are inspected; agent addresses (per global.AgentEmails)
// are excluded. Missing messages are silently skipped — we use the
// cached IMAP fetcher and best-effort coverage is fine because the
// caller falls through to a substitution path when the set is empty.
//
// Used to validate LLM-supplied recipients: any address that ever sent
// into the thread is a legitimate reply target, so heal should not
// overwrite it with the most-recent-human alone.
func collectHumansInThread(ctx context.Context, seed *services.Email) map[string]bool {
	humans := map[string]bool{}
	if seed == nil || len(config.AgentEmails) == 0 {
		return humans
	}
	if addr := bareAddressFromEmail(seed.From); addr != "" && !config.AgentEmails[addr] {
		humans[addr] = true
	}
	if len(seed.References) == 0 {
		return humans
	}
	ids := make([]string, 0, len(seed.References))
	for _, ref := range seed.References {
		if id := services.EnsureAngleBrackets(ref); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return humans
	}
	emails, err := services.GetEmails(ctx, ids)
	if err != nil {
		return humans
	}
	for _, m := range emails {
		if addr := bareAddressFromEmail(m.From); addr != "" && !config.AgentEmails[addr] {
			humans[addr] = true
		}
	}
	return humans
}

// allRecipientsInHumans reports whether every entry in `to` resolves to
// an address present in `humans`. Returns false on empty input so the
// caller falls through to the substitute path. Recipients are parsed as
// RFC 5322 addresses when possible; raw strings are lower-cased as a
// fallback so a bare "user@host" is matched.
func allRecipientsInHumans(to []string, humans map[string]bool) bool {
	if len(to) == 0 {
		return false
	}
	for _, r := range to {
		addr := bareAddressFromEmail(r)
		if addr == "" {
			addr = strings.ToLower(strings.TrimSpace(r))
		}
		if addr == "" {
			return false
		}
		if !humans[addr] {
			return false
		}
	}
	return true
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

// sameThread reports whether two emails belong to the same RFC 5322 thread,
// determined by comparing their thread roots (References[0] when present,
// otherwise the message's own Message-Id). Angle brackets are normalized so
// that a root captured from a References list ("<id>") matches one captured
// from a Message-Id header (raw "id" or "<id>"). Returns false when either
// side is nil or has no resolvable root.
func sameThread(a, b *services.Email) bool {
	if a == nil || b == nil {
		return false
	}
	rootA := services.EnsureAngleBrackets(a.GetThreadRoot())
	rootB := services.EnsureAngleBrackets(b.GetThreadRoot())
	if rootA == "" || rootB == "" {
		return false
	}
	return strings.EqualFold(rootA, rootB)
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
