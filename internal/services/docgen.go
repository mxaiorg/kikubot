package services

// docgen.go — stubs for producing document attachments from Claude's text output.
//
// Claude returns plain text / Markdown; these helpers convert that content into
// binary document formats that can be attached to an outbound Email and sent
// via SendEmail().
//
// Usage pattern:
//
//	att, err := GenerateDocx("Report.docx", markdownText)
//	if err == nil {
//	    replyEmail.Attachments = append(replyEmail.Attachments, att)
//	}
//
// Each function is a TODO stub that returns ErrNotImplemented until a real
// back-end (library call, external service, Tika, etc.) is wired in.

import "errors"

// ErrNotImplemented is returned by document generation stubs that have not
// yet been implemented.
var ErrNotImplemented = errors.New("docgen: not yet implemented")

// GenerateTxt wraps plain text from Claude as a named .txt attachment.
// This is the only format that needs no conversion and is fully functional.
func GenerateTxt(filename, text string) (Attachment, error) {
	return Attachment{
		Name: filename,
		Data: []byte(text),
	}, nil
}

// GenerateDocx converts Markdown/plain-text output from Claude into a Word
// document (.docx — Office Open XML).
//
// TODO: implement using a library such as github.com/fumiama/go-docx, or by
// POST-ing the text to a conversion service (e.g. LibreOffice / Gotenberg).
func GenerateDocx(filename, markdownText string) (Attachment, error) {
	_, _ = filename, markdownText
	return Attachment{}, ErrNotImplemented
}

// GenerateXlsx converts tabular data (e.g. a JSON array of objects or a CSV
// string produced by Claude) into an Excel workbook (.xlsx).
//
// TODO: implement using github.com/xuri/excelize/v2 or similar.
func GenerateXlsx(filename, csvOrJSON string) (Attachment, error) {
	_, _ = filename, csvOrJSON
	return Attachment{}, ErrNotImplemented
}

// GeneratePptx converts a structured outline or Markdown produced by Claude
// into a PowerPoint presentation (.pptx).
//
// TODO: implement using a library or a conversion service.
func GeneratePptx(filename, markdownOutline string) (Attachment, error) {
	_, _ = filename, markdownOutline
	return Attachment{}, ErrNotImplemented
}

// GeneratePdf converts Markdown/HTML output from Claude into a PDF document.
//
// TODO: implement using a headless browser (e.g. chromedp), wkhtmltopdf, or
// a service such as Gotenberg (POST to /forms/chromium/convert/markdown).
func GeneratePdf(filename, markdownText string) (Attachment, error) {
	_, _ = filename, markdownText
	return Attachment{}, ErrNotImplemented
}
