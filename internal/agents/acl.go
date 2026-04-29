package agents

import (
	"context"
	"fmt"
	"kikubot/internal/config"
	"kikubot/internal/services"
	"log"
	"slices"
	"strings"
)

// Access Control List

// extractDomain returns the domain part of an email address (after '@').
// If no '@' is present, returns empty string.
func extractDomain(email string) string {
	parts := strings.Split(email, "@")
	if len(parts) == 2 {
		return parts[1]
	}
	return ""
}

// AccessControl checks if the inbound email is allowed to talk to the agent.
// Whitelist takes priority over Blacklist.
//
// Whitelist mode (strict): every immediate identity — email.From and the
// ctx-stamped X-Senders chain — must match the whitelist. We do NOT walk
// the References thread here. Rationale:
//
//   - X-Senders is built from services.SourceEmail(ctx), not LLM input, so
//     each hop carries a trustworthy chain of who has handled the thread.
//   - Every agent applies its own ACL on receipt, so if an upstream agent
//     is on our whitelist we trust it already filtered its own inbound.
//   - A thread walk can only ADD identities, never remove one; it cannot
//     rescue a non-whitelisted immediate identity. Its only value would be
//     catching a non-whitelisted identity hiding in References but absent
//     from X-Senders — which requires a compromised whitelisted agent. If
//     we don't trust that agent, it shouldn't be on the whitelist.
//   - The thread walk causes false positives in multi-hop delegation: a
//     downstream agent has no access to upstream agents' INBOX / Sent, so
//     References legitimately cannot be resolved.
//
// Blacklist mode (lenient): we walk the thread so we can catch blacklisted
// identities hidden in References. Unresolvable References are skipped.
func AccessControl(ctx context.Context, email services.Email) error {
	immediate := services.AddToSenders(append([]string{}, email.Senders...), email.From)
	if len(immediate) == 0 {
		log.Println("no sender found")
		return fmt.Errorf("no sender found")
	}

	if len(config.Whitelist) > 0 {
		for _, sender := range immediate {
			if !matchesList(sender, config.Whitelist) {
				log.Printf("sender %s not in whitelist", sender)
				return fmt.Errorf("sender %s not in whitelist", sender)
			}
		}
		return nil
	}

	if len(config.Blacklist) > 0 {
		identities, err := collectThreadIdentities(ctx, email)
		if err != nil {
			return err
		}
		for _, sender := range identities {
			if matchesList(sender, config.Blacklist) {
				log.Printf("sender %s in blacklist", sender)
				return fmt.Errorf("sender %s in blacklist", sender)
			}
		}
	}

	return nil
}

// matchesList reports whether sender matches any entry in list. Entries
// containing '@' are compared as full email addresses; entries without '@'
// are compared against the sender's domain. All comparisons are case-
// insensitive.
func matchesList(sender string, list []string) bool {
	return slices.ContainsFunc(list, func(w string) bool {
		if strings.Contains(w, "@") {
			return strings.EqualFold(w, sender)
		}
		senderDomain := extractDomain(sender)
		return senderDomain != "" && strings.EqualFold(w, senderDomain)
	})
}

// collectThreadIdentities returns every sender identity that has touched
// the thread of the inbound email — the immediate senders plus the From
// and X-Senders of the thread root and every message referenced along the
// chain. Lookup failures and unresolvable References are skipped (lenient);
// we only report identities we actually observe. Used by blacklist mode.
func collectThreadIdentities(ctx context.Context, email services.Email) ([]string, error) {
	identities := services.AddToSenders(append([]string{}, email.Senders...), email.From)

	// Build the set of thread message IDs to resolve.
	threadIds := make([]string, 0, len(email.References)+1)
	seen := map[string]struct{}{}
	add := func(id string) {
		if id == "" || id == email.MessageId {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		threadIds = append(threadIds, id)
	}
	add(email.GetThreadRoot())
	for _, ref := range email.References {
		add(ref)
	}

	if len(threadIds) == 0 {
		return identities, nil
	}

	threadMsgs, err := services.GetEmails(ctx, threadIds)
	if err != nil {
		log.Printf("acl: error resolving thread messages (lenient, continuing): %v", err)
		return identities, nil
	}

	for _, m := range threadMsgs {
		identities = services.AddToSenders(identities, m.From)
		for _, s := range m.Senders {
			identities = services.AddToSenders(identities, s)
		}
	}
	return identities, nil
}

func EmailAclFailure() {
	// TODO email response back to sender that ACL check failed
}
