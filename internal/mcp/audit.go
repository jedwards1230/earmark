package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
)

const (
	// auditQueryTimeout is the request deadline of an audit read: a margin over
	// the database's own statement timeout, so a slow query ends as the server's
	// cancellation (503 below), not as a torn connection.
	auditQueryTimeout = db.AuditStatementTimeout + time.Second

	// auditWriteTimeout is how long the response may take to write, counted from
	// the start of the request, so a client that reads slowly cannot hold the
	// connection (and a database slot's worth of attention) open. It covers the
	// query itself.
	auditWriteTimeout = 10 * time.Second

	// auditMaxInflight caps the audit reads running at once in this process.
	auditMaxInflight = 3
)

// handleAPIAudit serves GET /api/v1/audit/{source}[?after=ID | order=desc
// [&before=ID]][&limit=N][&<filter>=V] (CONTRACT §2.12.2): the audit trail as
// stored, ascending by id after a cursor or, with order=desc, newest first
// before one. The source, its columns and its filters all come from the
// db.Audit* registry; this handler only validates the query string against it.
//
//	404 unknown source
//	400 malformed query string; unknown / repeated / empty parameter; bad
//	    after, before, order or limit; order=desc with after; before without
//	    order=desc; bad filter value
//	429 auditMaxInflight reads are already running (Retry-After: 1)
//	503 the read hit its statement timeout
//	500 anything else (logged, never echoed)
//
// A client that goes away mid-query gets no response and no error log.
func (s *MCPServer) handleAPIAudit(w http.ResponseWriter, r *http.Request) {
	src, err := db.LookupAuditSource(r.PathValue("source"))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	// r.URL.Query() drops a pair it cannot parse, which would read as "no
	// filter" and answer with unfiltered rows; refuse the request instead.
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "malformed query string")
		return
	}
	cur, limit, filters, err := parseAuditQuery(src, q)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Taken only now, so a malformed request is a 400 however busy the server is.
	if s.auditInflight.Add(1) > auditMaxInflight {
		s.auditInflight.Add(-1)
		w.Header().Set("Retry-After", "1")
		writeJSONError(w, http.StatusTooManyRequests, "too many concurrent audit reads; retry shortly")
		return
	}
	defer s.auditInflight.Add(-1)

	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(auditWriteTimeout)); err != nil &&
		!errors.Is(err, http.ErrNotSupported) {
		s.logger.Debug("api audit: set write deadline", "error", err)
	}

	ctx, cancel := context.WithTimeout(r.Context(), auditQueryTimeout)
	defer cancel()
	page, err := s.db.AuditRows(ctx, src.Name, cur, limit, filters)
	switch {
	case errors.Is(err, context.Canceled):
		s.logger.Debug("api audit: client went away", "source", src.Name)
		return
	case errors.Is(err, db.ErrAuditTimeout):
		s.logger.Warn("api audit timed out", "source", src.Name, "cursor", cur.ID, "desc", cur.Desc, "limit", limit, "filters", len(filters))
		writeJSONError(w, http.StatusServiceUnavailable, "audit query timed out; add a filter or lower limit")
		return
	case err != nil:
		s.logger.Error("api audit error", "source", src.Name, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if len(page.Rows) == 0 {
		page.Rows = json.RawMessage("[]")
	}
	writeJSON(w, http.StatusOK, page)
}

// parseAuditQuery validates the query string against the source: order (asc,
// the default, or desc), after and before (plain decimal digits; after for an
// ascending read, before for a descending one), limit (plain decimal digits,
// 1..AuditMaxLimit, default AuditDefaultLimit) and the source's equality
// filters. Any other parameter, a repeated one, or an empty value is an error,
// and so is mixing the directions: order=desc with after, or before without
// order=desc. Without order=desc and after the read is ascending from 0; with
// order=desc and no before it starts at the newest row. Parameters are checked
// in name order so the first error reported is stable. No message echoes a
// value.
func parseAuditQuery(src db.AuditSource, q url.Values) (cur db.AuditCursor, limit int, filters map[string]string, err error) {
	var (
		after, before       int64
		hasAfter, hasBefore bool
		desc                bool
	)
	limit = db.AuditDefaultLimit
	names := make([]string, 0, len(q))
	for name := range q {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		vals := q[name]
		if len(vals) != 1 {
			return db.AuditCursor{}, 0, nil, fmt.Errorf("parameter %q must be given once", name)
		}
		v := vals[0]
		if v == "" {
			return db.AuditCursor{}, 0, nil, fmt.Errorf("parameter %q is empty", name)
		}
		switch name {
		case "after", "before":
			n, ok := parseDigits(v)
			if !ok {
				return db.AuditCursor{}, 0, nil, fmt.Errorf("%s must be an integer >= 0 in plain digits", name)
			}
			if name == "after" {
				after, hasAfter = n, true
			} else {
				before, hasBefore = n, true
			}
		case "order":
			if v != "asc" && v != "desc" {
				return db.AuditCursor{}, 0, nil, errors.New("order must be asc or desc")
			}
			desc = v == "desc"
		case "limit":
			n, ok := parseDigits(v)
			if !ok || n < 1 || n > db.AuditMaxLimit {
				return db.AuditCursor{}, 0, nil, fmt.Errorf("limit must be 1..%d in plain digits", db.AuditMaxLimit)
			}
			limit = int(n)
		default:
			if err = src.CheckFilter(name, v); err != nil {
				return db.AuditCursor{}, 0, nil, err
			}
			if filters == nil {
				filters = map[string]string{}
			}
			filters[name] = v
		}
	}
	switch {
	case desc && hasAfter:
		return db.AuditCursor{}, 0, nil, errors.New("order=desc pages with before, not after")
	case hasBefore && !desc:
		return db.AuditCursor{}, 0, nil, errors.New("before needs order=desc")
	case desc && hasBefore:
		cur = db.AuditBefore(before)
	case desc:
		cur = db.AuditNewest()
	default:
		cur = db.AuditAfter(after)
	}
	return cur, limit, filters, nil
}

// parseDigits reads a canonical non-negative decimal: ASCII digits only, no sign,
// no leading zero (except "0" itself), and within int64. strconv alone would
// also take "+5" and "007".
func parseDigits(v string) (int64, bool) {
	if v == "" || (len(v) > 1 && v[0] == '0') {
		return 0, false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil
}
