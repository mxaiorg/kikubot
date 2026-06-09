package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"kikubot/internal/config"
	"kikubot/internal/services"
	"log"
	"net/http"
	"net/url"
	"strings"
)

// ── Xero direct REST tools (xero_api) ───────────────────────────────────
//
// xero_api is a thin set of direct REST wrappers over the Xero Accounting
// API. Unlike xero_mcp (the upstream MCP server), this path:
//
//   - Uses the standard authorization-code OAuth 2.0 flow (WebApp app type),
//     required in regions where Custom Connections aren't available
//     (Cyprus / most of the EU).
//   - Refreshes the access token on demand inside services.XeroRequest, so
//     the integration survives multi-day uptime without operator action.
//   - Exposes only the endpoints we actually use, keeping the model's
//     visible tool surface small.
//
// One-time bootstrap: `go run scripts/xero-bootstrap/*.go` to seed data/<agent>/xero/
// tokens.json before enabling the "xero_api" tool key in agents.yaml.

func XeroAPI() []ToolDefinition {
	if strings.TrimSpace(config.XeroClientId) == "" || strings.TrimSpace(config.XeroClientSecret) == "" {
		log.Println("[xero_api] XERO_CLIENT_ID/XERO_CLIENT_SECRET not set — Xero REST tools disabled")
		return nil
	}
	if _, err := services.LoadXeroTokens(); err != nil {
		log.Printf("[xero_api] token file not found (%v) — run cmd/xero-bootstrap, then enable xero_api", err)
		return nil
	}
	log.Println("[xero_api] direct REST client initialized")
	return []ToolDefinition{
		xeroListInvoicesTool(),
		xeroGetInvoiceTool(),
		xeroListContactsTool(),
		xeroGetContactTool(),
	}
}

func xeroListInvoicesTool() ToolDefinition {
	return ToolDefinition{
		Name: "xero_list_invoices",
		Description: "List Xero invoices (max 100 per page; paginate with `page`). " +
			"Filter by statuses, ContactIDs, or invoice numbers. " +
			"To answer outstanding-balance questions, filter Statuses=AUTHORISED and sum the `AmountDue` field on each returned invoice. " +
			"Combine multiple filters in one call; do NOT call this tool in a loop.",
		InputSchema: []byte(`{
			"type":"object",
			"properties":{
				"statuses":{"type":"string","description":"Comma-separated invoice statuses. Common values: DRAFT, SUBMITTED, AUTHORISED, PAID, VOIDED."},
				"contact_ids":{"type":"string","description":"Optional comma-separated Xero ContactIDs to restrict the query."},
				"invoice_numbers":{"type":"string","description":"Optional comma-separated invoice numbers (e.g. \"INV-0123,INV-0124\")."},
				"where":{"type":"string","description":"Optional raw Xero 'where' clause for advanced filters (e.g. \"Date >= DateTime(2026,01,01)\")."},
				"page":{"type":"integer","minimum":1,"description":"1-indexed page number. Defaults to 1."}
			},
			"additionalProperties":false
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				Statuses       string `json:"statuses,omitempty"`
				ContactIDs     string `json:"contact_ids,omitempty"`
				InvoiceNumbers string `json:"invoice_numbers,omitempty"`
				Where          string `json:"where,omitempty"`
				Page           int    `json:"page,omitempty"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}
			q := url.Values{}
			if p.Statuses != "" {
				q.Set("Statuses", p.Statuses)
			}
			if p.ContactIDs != "" {
				q.Set("ContactIDs", p.ContactIDs)
			}
			if p.InvoiceNumbers != "" {
				q.Set("InvoiceNumbers", p.InvoiceNumbers)
			}
			if p.Where != "" {
				q.Set("where", p.Where)
			}
			if p.Page > 0 {
				q.Set("page", fmt.Sprintf("%d", p.Page))
			}
			body, err := services.XeroRequest(ctx, http.MethodGet, "/api.xro/2.0/Invoices", q, nil)
			if err != nil {
				return "", err
			}
			return string(body), nil
		},
		StaticSystem: "- Xero invoice statuses: DRAFT (not yet submitted), SUBMITTED (awaiting approval), AUTHORISED (open / outstanding), PAID, VOIDED. To answer \"how much does X owe?\", first call xero_list_contacts to resolve X to a ContactID, then call xero_list_invoices with statuses=AUTHORISED and contact_ids=<that id>, then sum AmountDue across the results. For a quick balance without line-item detail, xero_get_contact returns the Balances block directly.\n",
	}
}

func xeroGetInvoiceTool() ToolDefinition {
	return ToolDefinition{
		Name:        "xero_get_invoice",
		Description: "Get a single Xero invoice by InvoiceID (uuid) or InvoiceNumber (e.g. \"INV-0123\"). Returns the full invoice including line items.",
		InputSchema: []byte(`{
			"type":"object",
			"properties":{
				"identifier":{"type":"string","description":"Either the InvoiceID (uuid) or the InvoiceNumber (e.g. \"INV-0123\")."}
			},
			"required":["identifier"],
			"additionalProperties":false
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				Identifier string `json:"identifier"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}
			if strings.TrimSpace(p.Identifier) == "" {
				return "", fmt.Errorf("identifier is required")
			}
			body, err := services.XeroRequest(ctx, http.MethodGet, "/api.xro/2.0/Invoices/"+url.PathEscape(p.Identifier), nil, nil)
			if err != nil {
				return "", err
			}
			return string(body), nil
		},
	}
}

func xeroListContactsTool() ToolDefinition {
	return ToolDefinition{
		Name:        "xero_list_contacts",
		Description: "List Xero contacts (customers and suppliers). Use `where` for name lookups, e.g. where=\"Name.Contains(\\\"Acme\\\")\". Returns up to 100 per page.",
		InputSchema: []byte(`{
			"type":"object",
			"properties":{
				"where":{"type":"string","description":"Optional Xero 'where' clause, e.g. \"Name.Contains(\\\"Acme\\\")\" or \"EmailAddress.Contains(\\\"@example.com\\\")\"."},
				"ids":{"type":"string","description":"Optional comma-separated ContactIDs."},
				"include_archived":{"type":"boolean","description":"Include archived contacts. Defaults to false."},
				"page":{"type":"integer","minimum":1,"description":"1-indexed page number. Defaults to 1."}
			},
			"additionalProperties":false
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				Where           string `json:"where,omitempty"`
				IDs             string `json:"ids,omitempty"`
				IncludeArchived bool   `json:"include_archived,omitempty"`
				Page            int    `json:"page,omitempty"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}
			q := url.Values{}
			if p.Where != "" {
				q.Set("where", p.Where)
			}
			if p.IDs != "" {
				q.Set("IDs", p.IDs)
			}
			if p.IncludeArchived {
				q.Set("includeArchived", "true")
			}
			if p.Page > 0 {
				q.Set("page", fmt.Sprintf("%d", p.Page))
			}
			body, err := services.XeroRequest(ctx, http.MethodGet, "/api.xro/2.0/Contacts", q, nil)
			if err != nil {
				return "", err
			}
			return string(body), nil
		},
	}
}

func xeroGetContactTool() ToolDefinition {
	return ToolDefinition{
		Name:        "xero_get_contact",
		Description: "Get a single Xero contact by ContactID (uuid) or ContactNumber. The response includes a `Balances` block with the contact's outstanding receivable and payable amounts — use this for a quick balance lookup once you have the ContactID.",
		InputSchema: []byte(`{
			"type":"object",
			"properties":{
				"identifier":{"type":"string","description":"Either the ContactID (uuid) or the ContactNumber."}
			},
			"required":["identifier"],
			"additionalProperties":false
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				Identifier string `json:"identifier"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}
			if strings.TrimSpace(p.Identifier) == "" {
				return "", fmt.Errorf("identifier is required")
			}
			body, err := services.XeroRequest(ctx, http.MethodGet, "/api.xro/2.0/Contacts/"+url.PathEscape(p.Identifier), nil, nil)
			if err != nil {
				return "", err
			}
			return string(body), nil
		},
	}
}
