package store

import (
	"fmt"
	"strings"
)

// whereClause accumulates SQL WHERE conditions and their parameter values.
type whereClause struct {
	conds  []string
	params []any
}

// add appends a condition with one bound parameter.
func (w *whereClause) add(expr string, val any) {
	w.conds = append(w.conds, expr)
	w.params = append(w.params, val)
}

// sql returns the full " WHERE …" fragment (including leading space) or an
// empty string if no conditions were added.
func (w *whereClause) sql() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

// buildConnectionFilter turns a ConnectionFilter into a WHERE clause.
func buildConnectionFilter(f ConnectionFilter) whereClause {
	var w whereClause
	if f.Provider != "" {
		w.add("provider = ?", f.Provider)
	}
	if f.State != "" {
		w.add("state = ?", f.State)
	}
	for _, id := range f.ExcludeIDs {
		w.add("id != ?", id)
	}
	return w
}

// buildUsageFilter turns a UsageFilter into a WHERE clause.
func buildUsageFilter(f UsageFilter) whereClause {
	var w whereClause
	if f.Provider != "" {
		w.add("provider = ?", f.Provider)
	}
	if f.Model != "" {
		w.add("model = ?", f.Model)
	}
	if !f.From.IsZero() {
		w.add("created_at >= ?", f.From.UTC().Format("2006-01-02T15:04:05Z"))
	}
	if !f.To.IsZero() {
		w.add("created_at <= ?", f.To.UTC().Format("2006-01-02T15:04:05Z"))
	}
	if f.APIKeyID != "" {
		w.add("api_key_id = ?", f.APIKeyID)
	}
	return w
}

// buildUsageQuery returns a full SELECT … FROM usage_log query with optional
// WHERE and LIMIT clauses applied from the filter.
func buildUsageQuery(f UsageFilter) (string, []any) {
	w := buildUsageFilter(f)
	q := "SELECT id, request_id, provider, model, connection_id, api_key_id, input_tokens, output_tokens, total_tokens, cost, latency_ms, status, created_at FROM usage_log" + w.sql() + " ORDER BY created_at DESC"
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	return q, w.params
}

// placeholders returns a string like "?,?,?" for n parameters.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}
