package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"kikubot/internal/config"
	"log"
	"strings"
	"sync"

	postgrest "github.com/supabase-community/postgrest-go"
	supabase "github.com/supabase-community/supabase-go"
)

// ── Supabase CRUD Tool ──────────────────────────────────────────────────
//
// One unified tool over the supabase-go / PostgREST client. The model picks
// an operation (select | insert | update | upsert | delete) and supplies a
// table, optional filters, optional payload, and optional paging/order.
//
// Filters are a map of column → "operator.value" (PostgREST syntax), e.g.
// {"id":"eq.5","name":"ilike.%alex%","status":"in.(a,b,c)","deleted_at":"is.null"}.
// Supported operators: eq, neq, gt, gte, lt, lte, like, ilike, is, in, cs,
// cd, sl, sr, nxl, nxr, adj, ov, fts, plfts, phfts, wfts.
//
// Auth: SUPABASE_URL + SUPABASE_API_KEY (anon or service-role). No raw SQL
// is exposed — PostgREST already covers CRUD with fewer tokens than the
// model would spend writing SQL, and Supabase doesn't accept arbitrary SQL
// over HTTP anyway (only stored RPC functions).

var (
	supabaseClientOnce sync.Once
	supabaseClient     *supabase.Client
	supabaseClientErr  error
)

func getSupabaseClient() (*supabase.Client, error) {
	supabaseClientOnce.Do(func() {
		supabaseClient, supabaseClientErr = supabase.NewClient(
			strings.TrimRight(config.SupabaseUrl, "/"),
			config.SupabaseApiKey,
			nil,
		)
	})
	return supabaseClient, supabaseClientErr
}

func Supabase() []ToolDefinition {
	if strings.TrimSpace(config.SupabaseUrl) == "" || strings.TrimSpace(config.SupabaseApiKey) == "" {
		log.Println("[supabase] SUPABASE_URL or SUPABASE_API_KEY not set — Supabase tool disabled")
		return nil
	}
	log.Println("[supabase] client initialized")
	return []ToolDefinition{supabaseDbTool()}
}

func supabaseDbTool() ToolDefinition {
	return ToolDefinition{
		Name: "supabase_db",
		Description: "CRUD over a Supabase/PostgREST table. Choose `operation` " +
			"(select|insert|update|upsert|delete), supply `table`, and the matching fields. " +
			"`filters` is a map of column → \"operator.value\" (e.g. \"eq.5\", " +
			"\"ilike.%alex%\", \"in.(1,2,3)\", \"is.null\"). update/delete REQUIRE filters " +
			"to avoid touching the whole table. Returns the JSON result body.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"operation":   {"type": "string", "enum": ["select","insert","update","upsert","delete"], "description": "CRUD operation."},
				"table":       {"type": "string", "description": "Table name in the public schema."},
				"columns":     {"type": "string", "description": "select-only: comma-separated column list. Defaults to \"*\"."},
				"filters":     {"type": "object", "additionalProperties": {"type": "string"}, "description": "Column → \"operator.value\", e.g. {\"id\":\"eq.5\",\"name\":\"ilike.%alex%\"}. Required for update/delete."},
				"data":        {"description": "insert/update/upsert payload: object for one row or array of objects for many. Keys are column names."},
				"on_conflict": {"type": "string", "description": "upsert-only: comma-separated unique columns used for conflict resolution."},
				"order":       {"type": "string", "description": "select-only: \"column.asc\" or \"column.desc\" (default desc)."},
				"limit":       {"type": "integer", "description": "select-only: row limit."},
				"offset":      {"type": "integer", "description": "select-only: row offset (requires limit)."},
				"single":      {"type": "boolean", "description": "select-only: return a single object instead of an array (errors if result is not exactly one row)."}
			},
			"required": ["operation","table"]
		}`),
		Execute: supabaseExecute,
		StaticSystem: "- supabase_db filter values use the PostgREST operator-then-value form: " +
			"\"eq.\", \"neq.\", \"gt.\", \"gte.\", \"lt.\", \"lte.\", \"like.\", \"ilike.\", \"is.\", \"in.(a,b,c)\". " +
			"Example filters: {\"status\":\"eq.active\",\"created_at\":\"gte.2026-01-01\"}.\n" +
			"- update and delete WITHOUT filters would affect every row; the tool refuses those calls. Always pass at least one filter.\n",
	}
}

func supabaseExecute(ctx context.Context, input json.RawMessage) (string, error) {
	var p struct {
		Operation  string            `json:"operation"`
		Table      string            `json:"table"`
		Columns    string            `json:"columns"`
		Filters    map[string]string `json:"filters"`
		Data       json.RawMessage   `json:"data"`
		OnConflict string            `json:"on_conflict"`
		Order      string            `json:"order"`
		Limit      int               `json:"limit"`
		Offset     int               `json:"offset"`
		Single     bool              `json:"single"`
	}
	if err := json.Unmarshal(input, &p); err != nil {
		return "", fmt.Errorf("parsing input: %w", err)
	}
	if strings.TrimSpace(p.Table) == "" {
		return "", fmt.Errorf("table is required")
	}

	client, err := getSupabaseClient()
	if err != nil {
		return "", fmt.Errorf("supabase client: %w", err)
	}

	switch strings.ToLower(strings.TrimSpace(p.Operation)) {
	case "select":
		fb := client.From(p.Table).Select(p.Columns, "", false)
		if err := applyFilters(fb, p.Filters); err != nil {
			return "", err
		}
		if p.Order != "" {
			applyOrder(fb, p.Order)
		}
		if p.Limit > 0 {
			fb = fb.Limit(p.Limit, "")
		}
		if p.Offset > 0 {
			span := p.Limit
			if span <= 0 {
				span = 1
			}
			fb = fb.Range(p.Offset, p.Offset+span-1, "")
		}
		if p.Single {
			fb = fb.Single()
		}
		body, _, err := fb.ExecuteString()
		if err != nil {
			return "", fmt.Errorf("supabase select: %w", err)
		}
		return body, nil

	case "insert":
		payload, err := decodeData(p.Data, "insert")
		if err != nil {
			return "", err
		}
		body, _, err := client.From(p.Table).
			Insert(payload, false, "", "representation", "").
			ExecuteString()
		if err != nil {
			return "", fmt.Errorf("supabase insert: %w", err)
		}
		return body, nil

	case "upsert":
		payload, err := decodeData(p.Data, "upsert")
		if err != nil {
			return "", err
		}
		body, _, err := client.From(p.Table).
			Upsert(payload, p.OnConflict, "representation", "").
			ExecuteString()
		if err != nil {
			return "", fmt.Errorf("supabase upsert: %w", err)
		}
		return body, nil

	case "update":
		if len(p.Filters) == 0 {
			return "", fmt.Errorf("filters are required for update (refusing to update every row)")
		}
		payload, err := decodeData(p.Data, "update")
		if err != nil {
			return "", err
		}
		fb := client.From(p.Table).Update(payload, "representation", "")
		if err := applyFilters(fb, p.Filters); err != nil {
			return "", err
		}
		body, _, err := fb.ExecuteString()
		if err != nil {
			return "", fmt.Errorf("supabase update: %w", err)
		}
		return body, nil

	case "delete":
		if len(p.Filters) == 0 {
			return "", fmt.Errorf("filters are required for delete (refusing to delete every row)")
		}
		fb := client.From(p.Table).Delete("representation", "")
		if err := applyFilters(fb, p.Filters); err != nil {
			return "", err
		}
		body, _, err := fb.ExecuteString()
		if err != nil {
			return "", fmt.Errorf("supabase delete: %w", err)
		}
		return body, nil

	default:
		return "", fmt.Errorf("unknown operation %q (want select|insert|update|upsert|delete)", p.Operation)
	}
}

func decodeData(raw json.RawMessage, op string) (any, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("data is required for %s", op)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("parsing data: %w", err)
	}
	return v, nil
}

func applyFilters(fb *postgrest.FilterBuilder, filters map[string]string) error {
	for col, opVal := range filters {
		dot := strings.IndexByte(opVal, '.')
		if dot <= 0 || dot == len(opVal)-1 {
			return fmt.Errorf("filter for %q must be \"operator.value\" (got %q)", col, opVal)
		}
		fb.Filter(col, opVal[:dot], opVal[dot+1:])
	}
	return nil
}

func applyOrder(fb *postgrest.FilterBuilder, spec string) {
	col, dir, ok := strings.Cut(spec, ".")
	if !ok {
		col, dir = spec, "desc"
	}
	fb.Order(col, &postgrest.OrderOpts{Ascending: strings.EqualFold(dir, "asc")})
}
