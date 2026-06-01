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

// ── Nuki (smart lock) API Tools ─────────────────────────────────────────
//
// Thin REST client for a small slice of the Nuki Web API
// (https://api.nuki.io/static/swagger/swagger.json):
//
// AccountUser CRUD (sub-accounts on the Nuki account):
//   - nuki_create_account_user
//   - nuki_get_account_user       (single by id, or list / filter by email)
//   - nuki_update_account_user
//   - nuki_delete_account_user
//
// SmartLock keypad code management:
//   - nuki_list_smartlocks        (discover the Web API smartlockId for a given lock)
//   - nuki_list_keypad_codes      (needed to discover the auth id before delete)
//   - nuki_set_keypad_code        (create a new keypad authorization)
//   - nuki_delete_keypad_code     (remove an existing keypad authorization)
// Note: The Web API `smartlockId` is NOT the same as the hex device ID printed on
//       the lock or shown in the Nuki mobile app. Use nuki_list_smartlocks (or
//       agent Knowledge) to discover the correct integer id, or use this bash command:
//
// TOKEN=$(grep -E '^NUKI_API_TOKEN=' configs/secrets-private.env | cut -d= -f2-) \
// curl -s https://api.nuki.io/smartlock \
//  -H "Authorization: Bearer $TOKEN" \
//  | jq '.[] | {smartlockId, name, nukiId}'
//
// Auth: NUKI_API_TOKEN sent as `Authorization: Bearer …`.
//
// The HTTP transport lives in services.NukiRequest so the private `nuki_native`
// tool can share it.

func Nuki() []ToolDefinition {
	if strings.TrimSpace(config.NukiApiToken) == "" {
		log.Println("[nuki] NUKI_API_TOKEN not set — Nuki tools disabled")
		return nil
	}
	log.Println("[nuki] REST client initialized")

	return []ToolDefinition{
		nukiCreateAccountUserTool(),
		nukiGetAccountUserTool(),
		nukiUpdateAccountUserTool(),
		nukiDeleteAccountUserTool(),
		nukiListSmartlocksTool(),
		nukiListKeypadCodesTool(),
		nukiSetKeypadCodeTool(),
		nukiDeleteKeypadCodeTool(),
	}
}

// ── SmartLock discovery ─────────────────────────────────────────────────

func nukiListSmartlocksTool() ToolDefinition {
	return ToolDefinition{
		Name: "nuki_list_smartlocks",
		Description: "List all smartlocks visible on this Nuki account. Use this to discover the " +
			"Web API `smartlockId` (a large integer) needed by the keypad-code tools. " +
			"The hex device ID printed on the lock or shown in the Nuki mobile app is NOT the " +
			"same value — match the user's lock by `name` (or `nukiId` if they provided the " +
			"hex/serial) and use the returned `smartlockId` field.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {}
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			body, err := services.NukiRequest(ctx, http.MethodGet, "/smartlock", nil)
			if err != nil {
				return "", err
			}
			return string(body), nil
		},
		StaticSystem: "- The Nuki Web API `smartlock_id` (used by nuki_set_keypad_code, nuki_list_keypad_codes, nuki_delete_keypad_code) is NOT the hex device ID printed on the lock or shown in the Nuki mobile app. It is a separate large integer issued by Nuki's backend. If the user gives you a hex ID (e.g. `2E29080D`), a serial, or a friendly name, call nuki_list_smartlocks first and match by `name` or `nukiId` to find the correct numeric `smartlockId`. NEVER convert a hex device ID to decimal and pass it as smartlock_id — that will fail.\n",
	}
}

// ── AccountUser ─────────────────────────────────────────────────────────

func nukiCreateAccountUserTool() ToolDefinition {
	return ToolDefinition{
		Name: "nuki_create_account_user",
		Description: "Create a new Nuki account user (sub-account). Returns the created user " +
			"including its numeric accountUserId, which is required by the keypad-code and " +
			"update/delete tools.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"name":     {"type": "string", "description": "Display name of the sub-account."},
				"email":    {"type": "string", "description": "Email address of the sub-account."},
				"language": {"type": "string", "enum": ["en","de","es","fr","it","nl","cs","sk"], "description": "Optional UI language code."},
				"type":     {"type": "integer", "enum": [0,1], "description": "Optional account type: 0=user (default), 1=company."}
			},
			"required": ["name","email"]
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				Name     string `json:"name"`
				Email    string `json:"email"`
				Language string `json:"language,omitempty"`
				Type     *int   `json:"type,omitempty"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}
			if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Email) == "" {
				return "", fmt.Errorf("name and email are required")
			}
			body, err := services.NukiRequest(ctx, http.MethodPut, "/account/user", p)
			if err != nil {
				return "", err
			}
			return string(body), nil
		},
	}
}

func nukiGetAccountUserTool() ToolDefinition {
	return ToolDefinition{
		Name: "nuki_get_account_user",
		Description: "Look up Nuki account users. With account_user_id, returns that single user. " +
			"Otherwise lists users, optionally filtered by email; supports paging via offset/limit.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"account_user_id": {"type": "integer", "description": "If set, fetches a single user by id."},
				"email":           {"type": "string",  "description": "Optional email filter when listing."},
				"offset":          {"type": "integer", "description": "Optional list offset."},
				"limit":           {"type": "integer", "description": "Optional list limit."}
			}
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				AccountUserID int    `json:"account_user_id"`
				Email         string `json:"email"`
				Offset        int    `json:"offset"`
				Limit         int    `json:"limit"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}
			if p.AccountUserID > 0 {
				path := fmt.Sprintf("/account/user/%d", p.AccountUserID)
				body, err := services.NukiRequest(ctx, http.MethodGet, path, nil)
				if err != nil {
					return "", err
				}
				return string(body), nil
			}
			q := url.Values{}
			if p.Email != "" {
				q.Set("email", p.Email)
			}
			if p.Offset > 0 {
				q.Set("offset", fmt.Sprintf("%d", p.Offset))
			}
			if p.Limit > 0 {
				q.Set("limit", fmt.Sprintf("%d", p.Limit))
			}
			path := "/account/user"
			if enc := q.Encode(); enc != "" {
				path += "?" + enc
			}
			body, err := services.NukiRequest(ctx, http.MethodGet, path, nil)
			if err != nil {
				return "", err
			}
			return string(body), nil
		},
	}
}

func nukiUpdateAccountUserTool() ToolDefinition {
	return ToolDefinition{
		Name:        "nuki_update_account_user",
		Description: "Update name, email, and/or language on an existing Nuki account user. Returns 204 No Content on success.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"account_user_id": {"type": "integer", "description": "Id of the user to update."},
				"name":            {"type": "string",  "description": "New display name."},
				"email":           {"type": "string",  "description": "New email address."},
				"language":        {"type": "string",  "enum": ["en","de","es","fr","it","nl","cs","sk"], "description": "New UI language code."}
			},
			"required": ["account_user_id"]
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				AccountUserID int    `json:"account_user_id"`
				Name          string `json:"name,omitempty"`
				Email         string `json:"email,omitempty"`
				Language      string `json:"language,omitempty"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}
			if p.AccountUserID <= 0 {
				return "", fmt.Errorf("account_user_id is required")
			}
			body := map[string]any{}
			if p.Name != "" {
				body["name"] = p.Name
			}
			if p.Email != "" {
				body["email"] = p.Email
			}
			if p.Language != "" {
				body["language"] = p.Language
			}
			if len(body) == 0 {
				return "", fmt.Errorf("at least one of name, email, or language must be provided")
			}
			path := fmt.Sprintf("/account/user/%d", p.AccountUserID)
			respBody, err := services.NukiRequest(ctx, http.MethodPost, path, body)
			if err != nil {
				return "", err
			}
			if len(respBody) == 0 {
				return `{"status":"updated"}`, nil
			}
			return string(respBody), nil
		},
	}
}

func nukiDeleteAccountUserTool() ToolDefinition {
	return ToolDefinition{
		Name:        "nuki_delete_account_user",
		Description: "Delete a Nuki account user by id. Returns 204 No Content on success.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"account_user_id": {"type": "integer", "description": "Id of the user to delete."}
			},
			"required": ["account_user_id"]
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				AccountUserID int `json:"account_user_id"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}
			if p.AccountUserID <= 0 {
				return "", fmt.Errorf("account_user_id is required")
			}
			path := fmt.Sprintf("/account/user/%d", p.AccountUserID)
			respBody, err := services.NukiRequest(ctx, http.MethodDelete, path, nil)
			if err != nil {
				return "", err
			}
			if len(respBody) == 0 {
				return `{"status":"deleted"}`, nil
			}
			return string(respBody), nil
		},
	}
}

// ── SmartLock keypad codes ──────────────────────────────────────────────

func nukiListKeypadCodesTool() ToolDefinition {
	return ToolDefinition{
		Name: "nuki_list_keypad_codes",
		Description: "List smartlock authorizations for a given smartlock, filtered to keypad codes " +
			"(type=13). Use this to discover the `id` of an existing keypad authorization before " +
			"deleting it.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"smartlock_id": {"type": "integer", "description": "The smartlock id (int64)."}
			},
			"required": ["smartlock_id"]
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				SmartlockID int64 `json:"smartlock_id"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}
			if p.SmartlockID <= 0 {
				return "", fmt.Errorf("smartlock_id is required")
			}
			q := url.Values{}
			q.Set("types", fmt.Sprintf("%d", services.NukiAuthTypeKeypad))
			path := fmt.Sprintf("/smartlock/%d/auth?%s", p.SmartlockID, q.Encode())
			body, err := services.NukiRequest(ctx, http.MethodGet, path, nil)
			if err != nil {
				return "", err
			}
			return string(body), nil
		},
	}
}

func nukiSetKeypadCodeTool() ToolDefinition {
	return ToolDefinition{
		Name: "nuki_set_keypad_code",
		Description: "Create a new keypad-code authorization on a smartlock. `code` is the numeric " +
			"PIN (typically 6 digits, no leading zero, no repeating digits per Nuki rules). " +
			"Optional time/weekday restrictions limit when the code is valid. Returns the async " +
			"operation acknowledgement from Nuki.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"smartlock_id":       {"type": "integer", "description": "The smartlock id (int64)."},
				"name":               {"type": "string",  "description": "Label for the authorization (max 32 chars)."},
				"code":               {"type": "integer", "description": "Numeric keypad PIN (e.g. 6 digits)."},
				"account_user_id":    {"type": "integer", "description": "Optional id of a linked account user."},
				"remote_allowed":     {"type": "boolean", "description": "Whether remote access is permitted. Defaults to false."},
				"allowed_from_date":  {"type": "string",  "description": "Optional RFC3339 start datetime (e.g. 2026-06-01T09:00:00Z)."},
				"allowed_until_date": {"type": "string",  "description": "Optional RFC3339 end datetime."},
				"allowed_week_days":  {"type": "integer", "minimum": 0, "maximum": 127, "description": "Optional weekday bitmask: 64=Mon, 32=Tue, 16=Wed, 8=Thu, 4=Fri, 2=Sat, 1=Sun. Combine with bitwise OR (e.g. 124 = Mon-Fri)."},
				"allowed_from_time":  {"type": "integer", "description": "Optional daily start, minutes from midnight (0-1439)."},
				"allowed_until_time": {"type": "integer", "description": "Optional daily end, minutes from midnight (0-1439)."}
			},
			"required": ["smartlock_id","name","code"]
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				SmartlockID      int64  `json:"smartlock_id"`
				Name             string `json:"name"`
				Code             int    `json:"code"`
				AccountUserID    int    `json:"account_user_id,omitempty"`
				RemoteAllowed    *bool  `json:"remote_allowed,omitempty"`
				AllowedFromDate  string `json:"allowed_from_date,omitempty"`
				AllowedUntilDate string `json:"allowed_until_date,omitempty"`
				AllowedWeekDays  int    `json:"allowed_week_days,omitempty"`
				AllowedFromTime  int    `json:"allowed_from_time,omitempty"`
				AllowedUntilTime int    `json:"allowed_until_time,omitempty"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}
			if p.SmartlockID <= 0 {
				return "", fmt.Errorf("smartlock_id is required")
			}
			if strings.TrimSpace(p.Name) == "" {
				return "", fmt.Errorf("name is required")
			}
			if p.Code <= 0 {
				return "", fmt.Errorf("code is required and must be a positive integer PIN")
			}

			body := map[string]any{
				"name": p.Name,
				"type": services.NukiAuthTypeKeypad,
				"code": p.Code,
			}
			// remoteAllowed is marked required by the swagger; default to false when unset.
			if p.RemoteAllowed != nil {
				body["remoteAllowed"] = *p.RemoteAllowed
			} else {
				body["remoteAllowed"] = false
			}
			if p.AccountUserID > 0 {
				body["accountUserId"] = p.AccountUserID
			}
			if p.AllowedFromDate != "" {
				body["allowedFromDate"] = p.AllowedFromDate
			}
			if p.AllowedUntilDate != "" {
				body["allowedUntilDate"] = p.AllowedUntilDate
			}
			if p.AllowedWeekDays > 0 {
				body["allowedWeekDays"] = p.AllowedWeekDays
			}
			if p.AllowedFromTime > 0 {
				body["allowedFromTime"] = p.AllowedFromTime
			}
			if p.AllowedUntilTime > 0 {
				body["allowedUntilTime"] = p.AllowedUntilTime
			}

			path := fmt.Sprintf("/smartlock/%d/auth", p.SmartlockID)
			respBody, err := services.NukiRequest(ctx, http.MethodPut, path, body)
			if err != nil {
				return "", err
			}
			if len(respBody) == 0 {
				return `{"status":"accepted","note":"Nuki processes auth creation asynchronously; use nuki_list_keypad_codes to confirm."}`, nil
			}
			return string(respBody), nil
		},
		StaticSystem: "- Nuki keypad PINs are typically 6 digits, must not start with 0, and cannot use repeating or sequential digits. The Nuki API will reject codes that violate these rules.\n" +
			"- Creating or deleting a smartlock authorization is asynchronous; an empty 2xx response means the operation was accepted, not yet applied. Re-query with `nuki_list_keypad_codes` if confirmation is needed.\n",
	}
}

func nukiDeleteKeypadCodeTool() ToolDefinition {
	return ToolDefinition{
		Name: "nuki_delete_keypad_code",
		Description: "Delete a keypad-code authorization from a smartlock by its authorization id. " +
			"Use nuki_list_keypad_codes first if you only know the user or PIN.",
		InputSchema: []byte(`{
			"type": "object",
			"properties": {
				"smartlock_id": {"type": "integer", "description": "The smartlock id (int64)."},
				"auth_id":      {"type": "string",  "description": "The smartlock authorization id (the 'id' field returned by nuki_list_keypad_codes)."}
			},
			"required": ["smartlock_id","auth_id"]
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				SmartlockID int64  `json:"smartlock_id"`
				AuthID      string `json:"auth_id"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parsing input: %w", err)
			}
			if p.SmartlockID <= 0 {
				return "", fmt.Errorf("smartlock_id is required")
			}
			if strings.TrimSpace(p.AuthID) == "" {
				return "", fmt.Errorf("auth_id is required")
			}
			path := fmt.Sprintf("/smartlock/%d/auth/%s", p.SmartlockID, url.PathEscape(p.AuthID))
			respBody, err := services.NukiRequest(ctx, http.MethodDelete, path, nil)
			if err != nil {
				return "", err
			}
			if len(respBody) == 0 {
				return `{"status":"accepted","note":"Nuki processes auth deletion asynchronously."}`, nil
			}
			return string(respBody), nil
		},
	}
}
