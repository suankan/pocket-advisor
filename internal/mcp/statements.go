package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/suankan/pocket-advisor/internal/retrieval"
	"github.com/suankan/pocket-advisor/internal/statements"
)

const (
	maxStatementFilterRunes = 320
	maxStatementDateRunes   = 64
	maxStatementLimit       = statements.MaxLimit
)

// StatementBrowser is the transport-independent boundary for deterministic
// bank statement document browsing. statements.Service satisfies it.
type StatementBrowser interface {
	List(context.Context, statements.Filters) (*retrieval.Result, error)
}

// StatementsTool exposes deterministic bank statement document lookup by
// owner, account, and period, alongside retrieval. It never ranks: a filter
// either resolves a registry bank-transactions collection or it does not
// (internal/statements' package doc). Its output reuses Query's evidence
// packet, citation, and continuation machinery — a matched statement's full
// text is evidence exactly like a search hit's — so results share the same
// snapshot store and the same read_evidence continuation as ordinary search.
type StatementsTool struct {
	Browser   StatementBrowser
	Query     *QueryTool
	Workspace string
	Title     string
}

func (t *StatementsTool) Name() string { return "list_bank_statements" }

func (t *StatementsTool) Describe() ToolDefinition {
	title := t.Title
	if title == "" {
		title = t.Workspace
	}
	return ToolDefinition{
		Name:  t.Name(),
		Title: "List " + title + " bank statement documents",
		Description: "List exact bank statement documents in this fixed workspace, filtered by owner, " +
			"account (bsb and/or account_number, or an account name matching a registry title), and period. " +
			"This is deterministic document selection, not semantic search or transaction-line parsing: statement " +
			"text is a layout-preserving PDF extraction, not CSV, so returned evidence is each whole matching " +
			"statement's full text — read it and compute totals, categories, or any other analysis yourself. " +
			"A matched document's period in its snippet is a best-effort hint from scanning its own text, not a " +
			"guaranteed-exact statement period; read the full text for the authoritative period, and if a period " +
			"filter is given a document whose own text carries no confidently detectable date is excluded rather " +
			"than guessed at. Matching uses AND across every filter given; an empty call lists every bank " +
			"statement document in this workspace. Cite the complete references shown, such as " +
			"[R0123456789ab:E1]. When complete=false, call the named continuation_tool with exactly next_cursor " +
			"and continue until complete=true. Never invent a cursor, byte range, document identifier, or workspace.",
		InputSchema: statementsInputSchema(), OutputSchema: evidencePageOutputSchema(),
		Annotations: readOnlyAnnotations(),
	}
}

type statementsArguments struct {
	Owner         string  `json:"owner,omitempty"`
	BSB           string  `json:"bsb,omitempty"`
	AccountNumber string  `json:"account_number,omitempty"`
	AccountName   string  `json:"account_name,omitempty"`
	Since         *string `json:"since,omitempty"`
	Until         *string `json:"until,omitempty"`
	Limit         int     `json:"limit,omitempty"`
}

// Call validates and dispatches list_bank_statements. QueryTool calls it
// before its own search dispatcher, so an unrecognized name still falls
// through to the ordinary unknown-tool error there.
func (t *StatementsTool) Call(ctx context.Context, raw json.RawMessage) (CallToolResult, error) {
	if t == nil || t.Browser == nil || t.Query == nil {
		return CallToolResult{}, fmt.Errorf("statements service is unavailable")
	}
	var params rawCallParams
	if err := decodeStrict(raw, &params); err != nil {
		return CallToolResult{}, &argumentError{message: "tools/call params must be a valid object"}
	}
	if params.Name != t.Name() {
		return CallToolResult{}, &unknownToolError{}
	}
	if len(params.Task) > 0 && string(params.Task) != "null" {
		return CallToolResult{}, &argumentError{message: "task-augmented execution is not supported"}
	}
	var args statementsArguments
	if err := decodeStrict(params.Arguments, &args); err != nil {
		return CallToolResult{}, &argumentError{message: "arguments must match the advertised list_bank_statements input schema"}
	}
	if err := validateStatementFilter(args.Owner, "owner"); err != nil {
		return CallToolResult{}, err
	}
	if err := validateStatementFilter(args.BSB, "bsb"); err != nil {
		return CallToolResult{}, err
	}
	if err := validateStatementFilter(args.AccountNumber, "account_number"); err != nil {
		return CallToolResult{}, err
	}
	if err := validateStatementFilter(args.AccountName, "account_name"); err != nil {
		return CallToolResult{}, err
	}
	if args.Limit < 0 || args.Limit > maxStatementLimit {
		return CallToolResult{}, &argumentError{message: fmt.Sprintf("limit must be omitted, zero, or between 1 and %d", maxStatementLimit)}
	}
	since, err := parseStatementDate(args.Since)
	if err != nil {
		return CallToolResult{}, err
	}
	until, err := parseStatementDate(args.Until)
	if err != nil {
		return CallToolResult{}, err
	}
	if since != nil && until != nil && since.After(*until) {
		return CallToolResult{}, &argumentError{message: "since must not be after until"}
	}

	res, err := t.Browser.List(ctx, statements.Filters{
		Owner: args.Owner, BSB: args.BSB, AccountNumber: args.AccountNumber, AccountName: args.AccountName,
		Since: since, Until: until, Limit: args.Limit,
	})
	if err != nil {
		return CallToolResult{}, err
	}
	resultID, err := t.Query.newResultID()
	if err != nil {
		return CallToolResult{}, err
	}
	evidence, err := newEvidenceResult(res, resultID)
	if err != nil {
		t.Query.releaseResultID(resultID)
		return CallToolResult{}, fmt.Errorf("build evidence result: %w", err)
	}
	return t.Query.storeSearchResult(evidence)
}

func validateStatementFilter(value, name string) error {
	if len(value) > maxStatementFilterRunes {
		return &argumentError{message: name + " exceeds the statement filter bound"}
	}
	return nil
}

func parseStatementDate(value *string) (*time.Time, error) {
	if value == nil {
		return nil, nil
	}
	if len(*value) > maxStatementDateRunes {
		return nil, &argumentError{message: "date must be a bounded RFC 3339 timestamp or YYYY-MM-DD date"}
	}
	trimmed := strings.TrimSpace(*value)
	if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
		u := parsed.UTC()
		return &u, nil
	}
	if parsed, err := time.Parse("2006-01-02", trimmed); err == nil {
		u := parsed.UTC()
		return &u, nil
	}
	return nil, &argumentError{message: "date must be an RFC 3339 timestamp or YYYY-MM-DD date"}
}

func statementsInputSchema() map[string]any {
	return map[string]any{
		"$schema": jsonSchema202012, "type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"owner":          map[string]any{"type": "string", "maxLength": maxStatementFilterRunes, "description": "Exact registry owner name, case-insensitive."},
			"bsb":            map[string]any{"type": "string", "maxLength": maxStatementFilterRunes, "description": "Bank BSB. Dashes and spaces are ignored."},
			"account_number": map[string]any{"type": "string", "maxLength": maxStatementFilterRunes, "description": "Bank account number. Dashes and spaces are ignored."},
			"account_name":   map[string]any{"type": "string", "maxLength": maxStatementFilterRunes, "description": "Exact registry account id, or a case-insensitive substring of its title."},
			"since":          nullableStringSchema(),
			"until":          nullableStringSchema(),
			"limit":          map[string]any{"type": "integer", "minimum": 1, "maximum": maxStatementLimit, "default": statements.DefaultLimit},
		},
	}
}
