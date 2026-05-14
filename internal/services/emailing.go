package services

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"kikubot/internal/config"
	"log"
	"net"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/mail"
	"github.com/gomarkdown/markdown"
	mdhtml "github.com/gomarkdown/markdown/html"
	"github.com/gomarkdown/markdown/parser"
	"github.com/patrickmn/go-cache"
	"golang.org/x/net/html"
	gomail "gopkg.in/gomail.v2"
)

var (
	// Matches patterns like: "On Tue, Mar 10, 2026 at 1:28 AM Donald Hammons <don@mxhero.com> wrote:"
	replyMarkerRegex = regexp.MustCompile(`(?i)On\s+[A-Za-z]+,\s+[A-Za-z]+\s+\d+,\s+\d{4}\s+at\s+\d+:\d+\s+[AP]M\s+.*?wrote:`)
)

var msgCache = cache.New(10*time.Minute, 30*time.Minute)

type Attachment struct {
	Name string `json:"name,omitempty"`
	Data []byte `json:"data,omitempty"`
}
type Email struct {
	To            []string     `json:"to,omitempty"`
	From          string       `json:"from,omitempty"`
	Cc            []string     `json:"cc,omitempty"`
	Date          time.Time    `json:"date"`
	InReplyTo     string       `json:"inReplyTo,omitempty"`
	References    []string     `json:"references,omitempty"`
	MessageId     string       `json:"message-id,omitempty"`
	Senders       []string     `json:"senders,omitempty"`
	Subject       string       `json:"subject,omitempty"`
	Content       string       `json:"content,omitempty"`
	Attachments   []Attachment `json:"attachments,omitempty"`
	AutoSubmitted string       `json:"autoSubmitted,omitempty"` // RFC 3834; "auto-replied" on bounces

	// Outlook/Exchange threading (empty for non-MS clients). ConvID is the
	// 22-byte conversation prefix of Thread-Index, hex-encoded — stable
	// across replies even when Exchange rewrites or drops References.
	// ThreadTopic is the normalised subject set by Outlook on conversation
	// start. We use these as a fallback thread key, gated on a normalised-
	// subject match, when References lookup misses.
	ThreadIndexConvID string `json:"threadIndexConvID,omitempty"`
	ThreadTopic       string `json:"threadTopic,omitempty"`
}

// sourceEmailKey is the unexported context key under which the inbound email
// (the one that kicked off the current HandleMessage turn) is stashed. Tools
// that need to trust the originating message — e.g. for ACL enforcement on
// X-Senders or authoritative Message-Id — read it via SourceEmail.
type sourceEmailKey struct{}

// WithSourceEmail returns a derived context carrying the inbound email as the
// trusted origin for the current agent turn. Call once at the top of
// HandleMessage; downstream tool invocations can recover the email with
// SourceEmail(ctx) instead of trusting LLM-provided headers.
func WithSourceEmail(ctx context.Context, email *Email) context.Context {
	if email == nil {
		return ctx
	}
	return context.WithValue(ctx, sourceEmailKey{}, email)
}

// SourceEmail returns the inbound email stashed on ctx by WithSourceEmail,
// or nil if the context was not decorated (e.g. tool invoked outside a
// HandleMessage turn).
func SourceEmail(ctx context.Context) *Email {
	if ctx == nil {
		return nil
	}
	e, _ := ctx.Value(sourceEmailKey{}).(*Email)
	return e
}

func (e *Email) GetThreadRoot() string {
	if len(e.References) == 0 {
		return e.MessageId
	}
	return e.References[0]
}

func (e *Email) MarshalJSON() ([]byte, error) {
	//log.Println("email:", e.Subject)
	type EmailAlias Email
	jsonObj, err := json.Marshal((*EmailAlias)(e))
	if err != nil {
		//return nil, err
	}
	return jsonObj, nil
}

func (e *Email) UserMessage() (*anthropic.MessageParam, error) {
	// Build a lightweight copy without binary attachment data for the text block.
	type AttachmentSummary struct {
		Name string `json:"name"`
		Size int    `json:"size_bytes"`
	}
	type EmailSummary struct {
		To          []string            `json:"to,omitempty"`
		From        string              `json:"from,omitempty"`
		Cc          []string            `json:"cc,omitempty"`
		Date        time.Time           `json:"date"`
		InReplyTo   string              `json:"inReplyTo,omitempty"`
		References  []string            `json:"references,omitempty"`
		MessageId   string              `json:"message-id,omitempty"`
		Subject     string              `json:"subject,omitempty"`
		Content     string              `json:"content,omitempty"`
		Attachments []AttachmentSummary `json:"attachments,omitempty"`
	}

	summary := EmailSummary{
		To: e.To, From: e.From, Cc: e.Cc, Date: e.Date,
		InReplyTo: e.InReplyTo, References: e.References,
		MessageId: e.MessageId, Subject: e.Subject, Content: e.Content,
	}
	for _, att := range e.Attachments {
		summary.Attachments = append(summary.Attachments, AttachmentSummary{
			Name: att.Name, Size: len(att.Data),
		})
	}

	jsonBytes, err := json.Marshal(summary)
	if err != nil {
		return nil, err
	}

	blocks := []anthropic.ContentBlockParamUnion{
		anthropic.NewTextBlock(string(jsonBytes)),
	}

	// Convert each attachment to a native content block when possible.
	for _, att := range e.Attachments {
		block, ok := attachmentToContentBlock(att)
		if ok {
			blocks = append(blocks, block)
		}
	}

	msg := anthropic.NewUserMessage(blocks...)
	return &msg, nil
}

// attachmentToContentBlock converts an email attachment to a native Anthropic
// content block (image, PDF, or plain-text document). Returns false if the
// attachment type is unsupported or exceeds size limits.
func attachmentToContentBlock(att Attachment) (anthropic.ContentBlockParamUnion, bool) {
	ext := strings.ToLower(filepath.Ext(att.Name))

	// Images (max 20 MB)
	if mediaType, ok := imageMediaType(ext); ok {
		if len(att.Data) > 20*1024*1024 {
			log.Printf("skipping image attachment %s: %d bytes exceeds 20MB limit", att.Name, len(att.Data))
			return anthropic.ContentBlockParamUnion{}, false
		}
		encoded := base64.StdEncoding.EncodeToString(att.Data)
		return anthropic.NewImageBlockBase64(mediaType, encoded), true
	}

	// PDFs (max 32 MB)
	if ext == ".pdf" {
		if len(att.Data) > 32*1024*1024 {
			log.Printf("skipping PDF attachment %s: %d bytes exceeds 32MB limit", att.Name, len(att.Data))
			return anthropic.ContentBlockParamUnion{}, false
		}
		encoded := base64.StdEncoding.EncodeToString(att.Data)
		block := anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{
			Data: encoded,
		})
		if block.OfDocument != nil {
			block.OfDocument.Title = param.NewOpt(att.Name)
		}
		return block, true
	}

	// Text-based files (max 5 MB)
	if isTextExtension(ext) {
		if len(att.Data) > 5*1024*1024 {
			log.Printf("skipping text attachment %s: %d bytes exceeds 5MB limit", att.Name, len(att.Data))
			return anthropic.ContentBlockParamUnion{}, false
		}
		block := anthropic.NewDocumentBlock(anthropic.PlainTextSourceParam{
			Data: string(att.Data),
		})
		if block.OfDocument != nil {
			block.OfDocument.Title = param.NewOpt(att.Name)
		}
		return block, true
	}

	// Office Open XML formats (.docx, .xlsx, .pptx) — extract text, send as plain text.
	// The Anthropic API does not natively support these formats; per the docs the
	// recommended approach is to convert them to plain text before sending.
	if text, ok := extractOfficeText(att.Data, ext); ok {
		if len(text) > 5*1024*1024 {
			log.Printf("skipping office attachment %s: extracted text %d bytes exceeds 5MB limit", att.Name, len(text))
			return anthropic.ContentBlockParamUnion{}, false
		}
		block := anthropic.NewDocumentBlock(anthropic.PlainTextSourceParam{
			Data: text,
		})
		if block.OfDocument != nil {
			block.OfDocument.Title = param.NewOpt(att.Name)
		}
		return block, true
	}

	// Last resort: ask Tika to convert the file to plain text.
	if text, ok := tikaExtractText(att.Name, att.Data); ok {
		if len(text) > 5*1024*1024 {
			log.Printf("skipping tika attachment %s: extracted text %d bytes exceeds 5MB limit", att.Name, len(text))
			return anthropic.ContentBlockParamUnion{}, false
		}
		block := anthropic.NewDocumentBlock(anthropic.PlainTextSourceParam{
			Data: text,
		})
		if block.OfDocument != nil {
			block.OfDocument.Title = param.NewOpt(att.Name)
		}
		return block, true
	}

	return anthropic.ContentBlockParamUnion{}, false
}

// extractOfficeText extracts plain text from Office Open XML files (.docx, .xlsx, .pptx).
// These formats are ZIP archives containing XML — no external dependency is needed.
func extractOfficeText(data []byte, ext string) (string, bool) {
	switch ext {
	case ".docx", ".xlsx", ".pptx":
	default:
		return "", false
	}

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", false
	}

	// Determine which XML entry paths to read for each format.
	var xmlTexts []string
	for _, f := range zr.File {
		name := f.Name
		var want bool
		switch ext {
		case ".docx":
			// Primary body text lives in word/document.xml; headers, footers, footnotes
			// are in other parts — include them all for completeness.
			want = strings.HasPrefix(name, "word/") && strings.HasSuffix(name, ".xml") &&
				!strings.Contains(name, "theme") && !strings.Contains(name, "setting") &&
				!strings.Contains(name, "style") && !strings.Contains(name, "fontTable") &&
				!strings.Contains(name, "webSettings") && !strings.Contains(name, "numbering")
		case ".xlsx":
			// Shared strings hold cell text; sheet XMLs hold cell references.
			want = name == "xl/sharedStrings.xml" ||
				(strings.HasPrefix(name, "xl/worksheets/sheet") && strings.HasSuffix(name, ".xml"))
		case ".pptx":
			want = strings.HasPrefix(name, "ppt/slides/slide") && strings.HasSuffix(name, ".xml")
		}
		if !want {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			continue
		}
		xmlData, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			continue
		}
		xmlTexts = append(xmlTexts, string(xmlData))
	}

	if len(xmlTexts) == 0 {
		return "", false
	}

	// Strip all XML tags, keeping only the text content.
	var sb strings.Builder
	for _, xmlStr := range xmlTexts {
		dec := xml.NewDecoder(strings.NewReader(xmlStr))
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			if cd, ok := tok.(xml.CharData); ok {
				t := strings.TrimSpace(string(cd))
				if t != "" {
					sb.WriteString(t)
					sb.WriteByte('\n')
				}
			}
		}
	}

	result := strings.TrimSpace(sb.String())
	if result == "" {
		return "", false
	}
	return result, true
}

// tikaExtractText converts an arbitrary file to plain text via Apache Tika.
// It is the catch-all converter for formats not handled natively (RTF, ODF,
// MSG, etc.) and is invoked from attachmentToContentBlock after the format-
// specific paths have all declined. Returns ("", false) on any failure so
// the caller falls through to skipping the attachment.
func tikaExtractText(name string, data []byte) (string, bool) {
	if len(data) == 0 {
		return "", false
	}
	text, err := ExtractTextFromBytes(data, name)
	if err != nil {
		log.Printf("tika: failed to convert %s (%d bytes): %v", name, len(data), err)
		return "", false
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false
	}
	return text, true
}

func imageMediaType(ext string) (string, bool) {
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg", true
	case ".png":
		return "image/png", true
	case ".gif":
		return "image/gif", true
	case ".webp":
		return "image/webp", true
	}
	return "", false
}

func isTextExtension(ext string) bool {
	switch ext {
	case ".txt", ".csv", ".md", ".json", ".xml", ".html", ".htm",
		".log", ".yml", ".yaml", ".toml", ".ini", ".cfg", ".conf",
		".go", ".py", ".js", ".ts", ".java", ".c", ".cpp", ".h",
		".css", ".sql", ".sh", ".bat", ".ps1", ".rb", ".rs", ".swift":
		return true
	}
	return false
}

func GetNewEmails(ctx context.Context) ([]Email, error) {
	//goland:noinspection GoResourceLeak
	c, err := dialServer(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Logout()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	_, err = c.Select(config.InboxFolder, false)
	if err != nil {
		return nil, fmt.Errorf("selecting INBOX: %w", err)
	}

	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{imap.SeenFlag}
	return fetchAndParse(c, criteria)
}

// MarkSeen explicitly sets the \Seen flag on the given Message-Ids so they
// won't be returned by GetNewEmails again. Call this only after the email has
// been successfully processed.
func MarkSeen(ctx context.Context, messageIds []string) error {
	if len(messageIds) == 0 {
		return nil
	}
	c, err := dialServer(ctx)
	if err != nil {
		return err
	}
	defer c.Logout()

	_, err = c.Select(config.InboxFolder, false)
	if err != nil {
		return fmt.Errorf("selecting INBOX: %w", err)
	}

	for _, msgId := range messageIds {
		criteria := imap.NewSearchCriteria()
		criteria.Header.Set("Message-Id", msgId)
		ids, searchErr := c.Search(criteria)
		if searchErr != nil {
			log.Printf("warning: could not search for Message-Id %s: %v", msgId, searchErr)
			continue
		}
		if len(ids) == 0 {
			continue
		}
		seqset := new(imap.SeqSet)
		seqset.AddNum(ids...)
		storeItem := imap.FormatFlagsOp(imap.AddFlags, true)
		if storeErr := c.Store(seqset, storeItem, []interface{}{imap.SeenFlag}, nil); storeErr != nil {
			log.Printf("warning: could not mark Message-Id %s as seen: %v", msgId, storeErr)
		}
	}
	return nil
}

func GetThreadRootId(ctx context.Context, messageId string) (string, error) {
	emails, emailErr := GetEmails(ctx, []string{messageId})
	if emailErr != nil {
		return "", fmt.Errorf("error getting email: %w", emailErr)
	}
	if len(emails) == 0 {
		return "", fmt.Errorf("no email found with Message-Id: %s", messageId)
	}
	return emails[0].GetThreadRoot(), nil
}

func parseIMAPMessage(msg *imap.Message, section *imap.BodySectionName) (*Email, error) {
	body := msg.GetBody(section)
	if body == nil {
		return nil, fmt.Errorf("no message body")
	}

	// Buffer the entire raw message so we can retry if MIME parsing fails
	rawBytes, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("reading raw message body: %w", err)
	}
	//log.Printf("raw message size: %d bytes", len(rawBytes))

	mr, err := mail.CreateReader(bytes.NewReader(rawBytes))
	if err != nil {
		return nil, fmt.Errorf("creating mail reader: %w", err)
	}

	header := mr.Header
	email := &Email{}

	email.Subject, _ = header.Subject()
	email.Date, _ = header.Date()
	email.MessageId = header.Get("Message-Id")
	email.References = parseReferences(header.Get("References"))
	email.InReplyTo = header.Get("In-Reply-To")
	email.AutoSubmitted = strings.TrimSpace(header.Get("Auto-Submitted"))
	email.ThreadIndexConvID = decodeThreadIndexConvID(header.Get("Thread-Index"))
	email.ThreadTopic = strings.TrimSpace(header.Get("Thread-Topic"))

	var fromAddress string
	if fromAddrs, err := header.AddressList("From"); err == nil && len(fromAddrs) > 0 {
		email.From = fromAddrs[0].String()
		fromAddress = fromAddrs[0].Address
	}

	// If all agents are in the same domain, we can identify ingress and strip X-Senders
	// as an extra ACL precaution. Otherwise, when agents are from different domains,
	// we will need to keep X-Senders. Forged X-Senders are possible, but not a risk in
	// the current architecture.
	singleDomainAgents := true

	// X-Senders carries the ACL origination chain. sendEmail builds it from
	// ctx (unforgeable by the LLM), so we trust it when the message comes
	// from another agent in our own cluster — same-domain senders. Any other
	// source's X-Senders is attacker-controlled input and must be discarded
	// before it reaches ACL, logging, or chain propagation.
	rawSenders := parseSenders(header.Get("X-Senders"))
	if singleDomainAgents {
		if sameDomain(fromAddress, config.AgentEmail) {
			email.Senders = rawSenders
		} else if len(rawSenders) > 0 {
			log.Printf("ingress: stripped untrusted X-Senders (%d entries) from %s on %s",
				len(rawSenders), email.From, email.MessageId)
		}
	} else {
		email.Senders = rawSenders
	}
	if toAddrs, err := header.AddressList("To"); err == nil {
		for _, addr := range toAddrs {
			email.To = append(email.To, addr.String())
		}
	}
	if ccAddrs, err := header.AddressList("Cc"); err == nil {
		for _, addr := range ccAddrs {
			email.Cc = append(email.Cc, addr.String())
		}
	}

	var plainText, htmlText string
	var partCount int
	for {
		p, err2 := mr.NextPart()
		if err2 == io.EOF {
			break
		}
		if err2 != nil {
			log.Printf("MIME NextPart error (after %d parts) for msg %s: %v", partCount, email.MessageId, err2)
			break
		}
		partCount++

		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			ct, _, _ := h.ContentType()
			//log.Printf("MIME part %d: inline content-type=%s for msg %s", partCount, ct, email.MessageId)
			if ct == "text/plain" && plainText == "" {
				b, err3 := io.ReadAll(p.Body)
				if err3 == nil {
					plainText = string(b)
					//log.Printf("MIME part %d: read %d bytes of text/plain for msg %s", partCount, len(plainText), email.MessageId)
				} else {
					log.Printf("MIME part %d: error reading text/plain body for msg %s: %v", partCount, email.MessageId, err3)
				}
			} else if ct == "text/html" && htmlText == "" {
				b, err2 := io.ReadAll(p.Body)
				if err2 == nil {
					htmlText = string(b)
				} else {
					log.Printf("MIME part %d: error reading text/html body for msg %s: %v", partCount, email.MessageId, err2)
				}
			}
		case *mail.AttachmentHeader:
			filename, _ := h.Filename()
			b, err3 := io.ReadAll(p.Body)
			if err3 == nil {
				email.Attachments = append(email.Attachments, Attachment{
					Name: filename,
					Data: b,
				})
			}
		default:
			log.Printf("MIME part %d: unhandled header type %T for msg %s", partCount, p.Header, email.MessageId)
		}
	}

	if plainText != "" {
		email.Content = plainText
	} else if htmlText != "" {
		email.Content = htmlToText(htmlText)
	}

	if email.Content == "" && partCount == 0 {
		log.Printf("WARNING: no MIME parts found for msg %s (subject: %s, from: %s)", email.MessageId, email.Subject, email.From)
	} else if email.Content == "" {
		log.Printf("WARNING: %d MIME parts found but no text content extracted for msg %s (subject: %s)", partCount, email.MessageId, email.Subject)
	}

	// Fallback: if MIME parsing found no content, extract text from raw body
	if email.Content == "" {
		log.Printf("FALLBACK: attempting raw body extraction for msg %s", email.MessageId)
		email.Content = extractRawBodyText(rawBytes)
		if email.Content != "" {
			log.Printf("FALLBACK: recovered %d bytes of text for msg %s", len(email.Content), email.MessageId)
		} else {
			log.Printf("FALLBACK: no text recovered from raw body for msg %s", email.MessageId)
		}
	}

	return email, nil
}

// extractRawBodyText is a fallback for when MIME parsing fails to extract content.
// It scans the raw RFC822 message bytes for the first text section after the headers.
func extractRawBodyText(raw []byte) string {
	// Find the header/body boundary (first blank line)
	bodyStart := bytes.Index(raw, []byte("\r\n\r\n"))
	if bodyStart == -1 {
		bodyStart = bytes.Index(raw, []byte("\n\n"))
		if bodyStart == -1 {
			return ""
		}
		bodyStart += 2
	} else {
		bodyStart += 4
	}

	bodyBytes := raw[bodyStart:]

	// If this is a multipart message, try to extract the first text/plain part
	headerSection := string(raw[:bodyStart])
	if idx := strings.Index(strings.ToLower(headerSection), "boundary="); idx != -1 {
		// Extract boundary string
		bStr := headerSection[idx+len("boundary="):]
		if strings.HasPrefix(bStr, "\"") {
			end := strings.Index(bStr[1:], "\"")
			if end != -1 {
				bStr = bStr[1 : end+1]
			}
		} else {
			for i, c := range bStr {
				if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ';' {
					bStr = bStr[:i]
					break
				}
			}
		}
		bStr = strings.TrimSpace(bStr)
		if bStr != "" {
			return extractTextFromMultipart(bodyBytes, bStr)
		}
	}

	// Not multipart — return body as-is, trimmed
	return strings.TrimSpace(string(bodyBytes))
}

// extractTextFromMultipart finds the first text/plain section within a multipart body.
func extractTextFromMultipart(body []byte, boundary string) string {
	delimiter := []byte("--" + boundary)
	parts := bytes.Split(body, delimiter)

	for _, part := range parts {
		partStr := string(part)
		if strings.TrimSpace(partStr) == "" || strings.HasPrefix(strings.TrimSpace(partStr), "--") {
			continue
		}

		splitIdx := strings.Index(partStr, "\r\n\r\n")
		if splitIdx == -1 {
			splitIdx = strings.Index(partStr, "\n\n")
			if splitIdx == -1 {
				continue
			}
			splitIdx += 2
		} else {
			splitIdx += 4
		}

		partHeader := strings.ToLower(partStr[:splitIdx])
		partBody := partStr[splitIdx:]

		if strings.Contains(partHeader, "text/plain") {
			return strings.TrimSpace(partBody)
		}
	}

	// No text/plain found — try text/html
	for _, part := range parts {
		partStr := string(part)
		if strings.TrimSpace(partStr) == "" || strings.HasPrefix(strings.TrimSpace(partStr), "--") {
			continue
		}
		splitIdx := strings.Index(partStr, "\r\n\r\n")
		if splitIdx == -1 {
			splitIdx = strings.Index(partStr, "\n\n")
			if splitIdx == -1 {
				continue
			}
			splitIdx += 2
		} else {
			splitIdx += 4
		}

		partHeader := strings.ToLower(partStr[:splitIdx])
		partBody := partStr[splitIdx:]

		if strings.Contains(partHeader, "text/html") {
			return htmlToText(strings.TrimSpace(partBody))
		}
	}

	return ""
}

func htmlToText(s string) string {
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return s
	}
	var buf strings.Builder
	extractHTMLText(doc, &buf)
	return strings.TrimSpace(buf.String())
}

func extractHTMLText(n *html.Node, buf *strings.Builder) {
	if n.Type == html.ElementNode {
		switch n.Data {
		case "script", "style", "head":
			return
		case "br":
			buf.WriteString("\n")
		case "p", "div", "h1", "h2", "h3", "h4", "h5", "h6", "tr", "li", "blockquote":
			buf.WriteString("\n")
		}
	}
	if n.Type == html.TextNode {
		buf.WriteString(n.Data)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		extractHTMLText(c, buf)
	}
	if n.Type == html.ElementNode {
		switch n.Data {
		case "p", "div", "h1", "h2", "h3", "h4", "h5", "h6", "tr":
			buf.WriteString("\n")
		}
	}
}

func parseReferences(refs string) []string {
	if refs == "" {
		return nil
	}
	var result []string
	for _, ref := range strings.Fields(refs) {
		ref = strings.Trim(ref, " \t\n\r")
		if ref != "" {
			result = append(result, EnsureAngleBrackets(ref))
		}
	}
	return result
}

// decodeThreadIndexConvID returns the hex-encoded 22-byte conversation
// prefix of an Outlook/Exchange Thread-Index header, or "" if the header
// is missing or malformed. The remainder of the header (5-byte child
// blocks per reply) is discarded — we key on the conversation, not the
// position within it.
func decodeThreadIndexConvID(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(h)
	if err != nil || len(raw) < 22 {
		return ""
	}
	return fmt.Sprintf("%x", raw[:22])
}

// replyPrefixRe matches a single localized reply/forward prefix at the
// start of a subject. Covers EN, DE, FR, IT, ES, PT, NL, NO/DA, SE, FI,
// PL, plus CJK languages (ZH, JA, KO), numbered variants like "Re[2]:",
// and bracketed admin tags "[EXT]"/"[EXTERNAL]".
var replyPrefixRe = regexp.MustCompile(
	`(?i)^\s*(?:` +
		`\[(?:ext|external|extern)\]|` +
		`(?:re|aw|fwd?|wg|tr|sv|vs|rif|r|rv|ant|vl|odp|pd|回复|答复|转发|回覆|轉寄|返信|転送|회신|답장|전달)` +
		`(?:\[\d+\])?\s*[:：]` +
		`)\s*`)

// NormalizeSubject strips leading reply/forward prefixes (repeatedly),
// collapses internal whitespace, and lowercases. Used as a corroborating
// signal when matching threads by Outlook Thread-Index — the conversation
// fingerprint matches *and* humans agree it's the same conversation.
func NormalizeSubject(s string) string {
	s = strings.TrimSpace(s)
	for {
		next := replyPrefixRe.ReplaceAllString(s, "")
		if next == s {
			break
		}
		s = next
	}
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func parseSenders(senders string) []string {
	if senders == "" {
		return nil
	}
	var result []string
	for _, sender := range strings.Split(senders, ",") {
		sender = strings.TrimSpace(sender)
		if sender != "" {
			result = append(result, sender)
		}
	}
	return result
}

// EnsureAngleBrackets wraps a message ID in angle brackets if missing.
// LLMs tend to strip angle brackets from message IDs since they look like XML/HTML tags.
// They also sometimes HTML-encode them as &lt; / &gt;.
func EnsureAngleBrackets(id string) string {
	if id == "" {
		return id
	}
	// Unescape HTML entities that LLMs sometimes produce
	id = strings.ReplaceAll(id, "&lt;", "<")
	id = strings.ReplaceAll(id, "&gt;", ">")
	id = strings.ReplaceAll(id, "&amp;", "&")
	if !strings.HasPrefix(id, "<") {
		id = "<" + id
	}
	if !strings.HasSuffix(id, ">") {
		id = id + ">"
	}
	return id
}

func SendEmail(ctx context.Context, msg Email) error {
	m := gomail.NewMessage()

	from := msg.From
	if from == "" {
		from = config.AgentEmail
	}
	m.SetHeader("From", from)
	m.SetHeader("To", msg.To...)
	if len(msg.Senders) > 0 {
		m.SetHeader("X-Senders", msg.Senders...)
	}
	if len(msg.Cc) > 0 {
		m.SetHeader("Cc", msg.Cc...)
	}
	m.SetHeader("Subject", msg.Subject)

	// Generate a valid Message-ID (RFC 5322). Without this, some gomail/Postfix
	// combinations produce an empty Message-ID (<>) which Google rejects.
	domain := config.AgentEmail
	if parts := strings.SplitN(domain, "@", 2); len(parts) == 2 {
		domain = parts[1]
	}
	messageId := fmt.Sprintf("<%s@%s>", uuid.New().String(), domain)
	m.SetHeader("Message-ID", messageId)

	if msg.InReplyTo != "" {
		m.SetHeader("In-Reply-To", msg.InReplyTo)
		// Ensure InReplyTo is in References
		if !slices.Contains(msg.References, msg.InReplyTo) {
			msg.References = append(msg.References, msg.InReplyTo)
		}
	}

	if len(msg.References) > 0 {
		m.SetHeader("References", strings.Join(msg.References, " "))
	}

	if msg.AutoSubmitted != "" {
		m.SetHeader("Auto-Submitted", msg.AutoSubmitted)
	}

	// Echo Outlook conversation headers so MS clients in the audience keep
	// the message stitched into the existing thread view. We only repeat
	// the 22-byte conversation prefix; appending a child block would
	// require carrying byte offsets per reply, and Outlook is happy to
	// thread on the prefix alone.
	if msg.ThreadIndexConvID != "" {
		if raw, err := hex.DecodeString(msg.ThreadIndexConvID); err == nil && len(raw) == 22 {
			m.SetHeader("Thread-Index", base64.StdEncoding.EncodeToString(raw))
		}
	}
	if msg.ThreadTopic != "" {
		m.SetHeader("Thread-Topic", msg.ThreadTopic)
	}

	m.SetBody("text/plain", msg.Content)
	htmlContent := mdToHTML(msg.Content)
	m.AddAlternative("text/html", htmlContent, gomail.SetPartEncoding(gomail.QuotedPrintable))

	for _, att := range msg.Attachments {
		data := att.Data
		m.Attach(att.Name, gomail.SetCopyFunc(func(w io.Writer) error {
			_, err := w.Write(data)
			return err
		}))
	}

	d := gomail.NewDialer(config.SmtpServer, config.SmtpPort, config.AgentEmail, config.EmailPassword)
	d.LocalName = config.SmtpHelo
	d.TLSConfig = &tls.Config{InsecureSkipVerify: true}

	if err := d.DialAndSend(m); err != nil {
		log.Println("error sending email:", err)
		return fmt.Errorf("sending email: %w", err)
	}

	// Copy the sent message to the IMAP Sent folder.
	// Use a dedicated short timeout so a slow IMAP server cannot eat
	// into the agent's context and cause the whole turn to fail.
	if err := appendToSent(m); err != nil {
		log.Println("warning: email sent but failed to save to Sent folder:", err)
	}

	return nil
}

func SendBounce(ctx context.Context, rcvdEmail Email, msg string) error {
	subject := rcvdEmail.Subject
	if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}

	reply := Email{
		To:            []string{rcvdEmail.From},
		Subject:       subject,
		Content:       msg,
		InReplyTo:     rcvdEmail.MessageId,
		References:    rcvdEmail.References,
		Date:          time.Now(),
		AutoSubmitted: "auto-replied",
	}

	if err := SendEmail(ctx, reply); err != nil {
		return err
	}

	if rcvdEmail.MessageId != "" {
		if err := MarkSeen(ctx, []string{rcvdEmail.MessageId}); err != nil {
			log.Println("warning: bounce sent but failed to mark received email as seen:", err)
		}
	}

	return nil
}

// appendToSent saves a copy of the 'Sent' message to the IMAP Sent folder.
// It uses its own 15-second timeout, independent of the caller's context,
// because the email has already been sent — this is best-effort.
func appendToSent(m *gomail.Message) error {
	var buf bytes.Buffer
	if _, err := m.WriteTo(&buf); err != nil {
		return fmt.Errorf("serializing message: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	//goland:noinspection GoResourceLeak
	c, err := dialServer(ctx)
	if err != nil {
		return fmt.Errorf("connecting to IMAP server: %w", err)
	}
	defer c.Logout()

	return c.Append(config.SentFolder, []string{imap.SeenFlag}, time.Now(), &buf)
}

func GetEmails(ctx context.Context, messageIds []string) ([]Email, error) {
	var emails []Email
	var uncached []string
	//log.Println("GetEmails:", messageIds)
	for _, id := range messageIds {
		if cached, ok := msgCache.Get(id); ok {
			if email, ok2 := cached.(*Email); ok2 {
				emails = append(emails, *email)
				continue
			}
		}
		uncached = append(uncached, id)
	}
	if len(uncached) == 0 {
		return emails, nil
	}

	//goland:noinspection GoResourceLeak
	c, err := dialServer(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Logout()

	_, err = c.Select(config.InboxFolder, true)
	if err != nil {
		return nil, fmt.Errorf("selecting INBOX: %w", err)
	}

	// Search for each message-id on the single connection, collect all sequence numbers
	seqset := new(imap.SeqSet)
	var found int
	for _, id := range uncached {
		criteria := imap.NewSearchCriteria()
		criteria.Header.Set("Message-Id", id)
		ids, err := c.Search(criteria)
		if err != nil || len(ids) == 0 {
			continue
		}
		seqset.AddNum(ids[0])
		found++
	}
	if found == 0 {
		return emails, nil
	}

	// Single batch fetch for all matched messages
	section := &imap.BodySectionName{}
	messages := make(chan *imap.Message, found)
	done := make(chan error, 1)
	go func() {
		done <- c.Fetch(seqset, []imap.FetchItem{section.FetchItem()}, messages)
	}()

	for msg := range messages {
		email, err2 := parseIMAPMessage(msg, section)
		if err2 != nil {
			continue
		}
		msgCache.Set(email.MessageId, email, cache.DefaultExpiration)
		emails = append(emails, *email)
	}

	if err3 := <-done; err3 != nil {
		return nil, fmt.Errorf("fetching messages: %w", err3)
	}

	sort.Slice(emails, func(i, j int) bool {
		return emails[i].Date.Before(emails[j].Date)
	})

	return emails, nil
}

// GetSentEmail looks up a single message by Message-Id in the Sent folder.
// Intended for diagnosing DSNs / bounces that reference our own outbound —
// those referenced messages live in Sent, not INBOX, so GetEmails misses
// them. Returns (nil, nil) if the message is not present in Sent. Results
// are NOT cached (the msgCache is keyed by Message-Id alone and an INBOX
// lookup elsewhere would collide), and this path is rare enough that the
// extra IMAP round-trip per call is fine.
func GetSentEmail(ctx context.Context, messageId string) (*Email, error) {
	messageId = strings.TrimSpace(messageId)
	if messageId == "" {
		return nil, nil
	}

	//goland:noinspection GoResourceLeak
	c, err := dialServer(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Logout()

	_, err = c.Select(config.SentFolder, true)
	if err != nil {
		return nil, fmt.Errorf("selecting Sent folder: %w", err)
	}

	criteria := imap.NewSearchCriteria()
	criteria.Header.Set("Message-Id", messageId)
	ids, err := c.Search(criteria)
	if err != nil {
		return nil, fmt.Errorf("searching Sent: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(ids[0])

	section := &imap.BodySectionName{}
	messages := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.Fetch(seqset, []imap.FetchItem{section.FetchItem()}, messages)
	}()

	var result *Email
	for msg := range messages {
		parsed, parseErr := parseIMAPMessage(msg, section)
		if parseErr != nil {
			continue
		}
		result = parsed
	}
	if err3 := <-done; err3 != nil {
		return nil, fmt.Errorf("fetching from Sent: %w", err3)
	}

	return result, nil
}

func dialServer(ctx context.Context) (*client.Client, error) {
	// DialContext respects context cancellation for the TCP connect.
	dialer := &net.Dialer{}
	rawConn, err := dialer.DialContext(ctx, "tcp", config.EmailServer)
	if err != nil {
		return nil, fmt.Errorf("connecting to IMAP server: %w", err)
	}

	// Extract hostname for TLS ServerName verification.
	host, _, _ := net.SplitHostPort(config.EmailServer)
	if host == "" {
		host = config.EmailServer
	}

	// Wrap the raw connection with TLS. HandshakeContext respects
	// context cancellation so Ctrl-C can interrupt the handshake.
	tlsConn := tls.Client(rawConn, &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: config.EmailInsecureTLS,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}

	c, err := client.New(tlsConn)
	if err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("creating IMAP client: %w", err)
	}

	if err2 := c.Login(config.AgentEmail, config.EmailPassword); err2 != nil {
		c.Logout()
		return nil, fmt.Errorf("login: %w", err2)
	}

	return c, nil
}

func fetchAndParse(c *client.Client, criteria *imap.SearchCriteria) ([]Email, error) {
	ids, err := c.Search(criteria)
	if err != nil {
		return nil, fmt.Errorf("searching messages: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(ids...)

	section := &imap.BodySectionName{Peek: true}
	messages := make(chan *imap.Message, len(ids))
	done := make(chan error, 1)
	go func() {
		done <- c.Fetch(seqset, []imap.FetchItem{section.FetchItem()}, messages)
	}()

	var emails []Email
	for msg := range messages {
		email, err := parseIMAPMessage(msg, section)
		if err != nil {
			continue
		}
		emails = append(emails, *email)
	}

	if err := <-done; err != nil {
		return nil, fmt.Errorf("fetching messages: %w", err)
	}

	sort.Slice(emails, func(i, j int) bool {
		return emails[i].Date.Before(emails[j].Date)
	})

	return emails, nil
}

type MailSearch struct {
	From           string
	To             string
	Subject        string
	DateFrom       time.Time
	DateTo         time.Time
	Unread         bool
	Starred        bool
	HasAttachments bool
}

func MailBoxSearch(ctx context.Context, search MailSearch) ([]Email, error) {
	//goland:noinspection GoResourceLeak
	c, err := dialServer(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Logout()

	_, err = c.Select(config.InboxFolder, true)
	if err != nil {
		return nil, fmt.Errorf("selecting INBOX: %w", err)
	}

	criteria := imap.NewSearchCriteria()

	if search.From != "" {
		criteria.Header.Set("From", search.From)
	}
	if search.To != "" {
		criteria.Header.Set("To", search.To)
	}
	if search.Subject != "" {
		criteria.Header.Set("Subject", search.Subject)
	}
	if !search.DateFrom.IsZero() {
		criteria.Since = search.DateFrom
	}
	if !search.DateTo.IsZero() {
		criteria.Before = search.DateTo
	}
	if search.Unread {
		criteria.WithoutFlags = []string{imap.SeenFlag}
	}
	if search.Starred {
		criteria.WithFlags = []string{imap.FlaggedFlag}
	}

	results, resultsErr := fetchAndParse(c, criteria)
	if resultsErr != nil {
		return nil, resultsErr
	}

	// Need to loop through all messages and check for attachments
	if search.HasAttachments {
		finalResults := make([]Email, 0, len(results))
		for _, email := range results {
			if len(email.Attachments) > 0 {
				finalResults = append(finalResults, email)
			}
		}
		results = finalResults
	}

	return results, nil
}

// ReplyBody returns the body of a reply email. It adds the msg followed by a
// reply string then the original message contents. If the original message contents
// contain a reply string, it is truncated at the first occurrence of the reply string.
func ReplyBody(email *Email, msg string) string {
	originalContent := email.Content

	// Truncate at first reply marker if present
	if loc := replyMarkerRegex.FindStringIndex(originalContent); loc != nil {
		originalContent = strings.TrimSpace(originalContent[:loc[0]])
	}

	// Format the reply
	var replyBuilder strings.Builder
	replyBuilder.WriteString(msg)
	replyBuilder.WriteString("\n\n")

	// Add reply marker
	fromAddr := email.From
	if fromAddr == "" {
		fromAddr = "Unknown"
	}
	dateStr := email.Date.Format("Mon, Jan 2, 2006 at 3:04 PM")
	replyBuilder.WriteString(fmt.Sprintf("On %s %s wrote:\n\n", dateStr, fromAddr))
	replyBuilder.WriteString(originalContent)

	return replyBuilder.String()
}

func ForwardBody(email *Email, msg string) string {
	var forwardBuilder strings.Builder

	forwardBuilder.WriteString(msg)
	forwardBuilder.WriteString("\n\n")
	forwardBuilder.WriteString("---------- Forwarded message ---------\n")

	fromAddr := email.From
	if fromAddr == "" {
		fromAddr = "Unknown"
	}
	forwardBuilder.WriteString(fmt.Sprintf("From: %s\n", fromAddr))

	dateStr := email.Date.Format("Mon, Jan 2, 2006 at 3:04 PM")
	forwardBuilder.WriteString(fmt.Sprintf("Date: %s\n", dateStr))

	if email.Subject != "" {
		forwardBuilder.WriteString(fmt.Sprintf("Subject: %s\n", email.Subject))
	}
	if len(email.To) > 0 {
		forwardBuilder.WriteString(fmt.Sprintf("To: %s\n", strings.Join(email.To, ", ")))
	}
	if len(email.Cc) > 0 {
		forwardBuilder.WriteString(fmt.Sprintf("Cc: %s\n", strings.Join(email.Cc, ", ")))
	}

	forwardBuilder.WriteString("\n\n")
	forwardBuilder.WriteString(email.Content)

	return forwardBuilder.String()
}

func mdToHTML(md string) string {
	// create markdown parser with extensions (removed MathJax)
	common := parser.NoIntraEmphasis | parser.Tables | parser.FencedCode |
		parser.Autolink | parser.Strikethrough | parser.SpaceHeadings | parser.HeadingIDs |
		parser.BackslashLineBreak | parser.DefinitionLists
	extensions := common | parser.AutoHeadingIDs | parser.NoEmptyLineBeforeBlock
	p := parser.NewWithExtensions(extensions)

	doc := p.Parse([]byte(md))

	// create HTML renderer with extensions
	htmlFlags := mdhtml.CommonFlags | mdhtml.HrefTargetBlank
	opts := mdhtml.RendererOptions{Flags: htmlFlags}
	renderer := mdhtml.NewRenderer(opts)

	return string(markdown.Render(doc, renderer))
}

// sameDomain reports whether two addresses share the same mail domain
// (case-insensitive). Either argument may be a bare mailbox ("x@y") or a
// display-name form ("Name" <x@y>); only the part after the last '@' in
// each is compared. Empty inputs or a missing '@' → false.
func sameDomain(a, b string) bool {
	domain := func(s string) string {
		s = strings.TrimSpace(s)
		if s == "" {
			return ""
		}
		if addr, err := mail.ParseAddress(s); err == nil {
			s = addr.Address
		}
		at := strings.LastIndex(s, "@")
		if at < 0 || at == len(s)-1 {
			return ""
		}
		return strings.ToLower(s[at+1:])
	}
	da, db := domain(a), domain(b)
	return da != "" && da == db
}

// AddToSenders returns senders with the From mailbox appended if not
// already present. Callers must use the returned slice — the input
// slice's header is not mutated.
func AddToSenders(senders []string, from string) []string {
	addr, err := mail.ParseAddress(from)
	if err != nil {
		log.Printf("failed to parse From header '%s': %v", from, err)
		return senders
	}
	mailbox := addr.Address

	for _, existing := range senders {
		if strings.EqualFold(existing, mailbox) {
			return senders
		}
	}
	return append(senders, mailbox)
}
