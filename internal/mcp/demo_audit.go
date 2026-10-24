package mcp

import (
	"context"
	"encoding/json"

	"github.com/jedwards1230/earmark/internal/db"
)

// AuditRows serves the audit trail in demo mode (CONTRACT §2.12.2). The demo
// has no event log behind it, so every known source answers with an empty page
// — the same shape and the same unknown-source error as the database.
func (demoDB) AuditRows(_ context.Context, source string, _ db.AuditCursor, _ int, _ map[string]string) (db.AuditPage, error) {
	s, err := db.LookupAuditSource(source)
	if err != nil {
		return db.AuditPage{}, err
	}
	return db.AuditPage{Source: s.Name, Rows: json.RawMessage("[]")}, nil
}
