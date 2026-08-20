package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
)

// Correction review tools (CONTRACT §2.17).
//
// The overlay closed the loop in code — a finding can be accepted, replayed
// onto the regenerated projection, and reverted — but nothing could REACH that
// loop: db.SetPatchState had no caller. These three tools are that surface.
//
//	list_transcript_corrections   — the worklist, with the context to judge one
//	decide_transcript_correction  — accept / reject / revert / reconsider
//	create_transcript_correction  — an edit no model proposed
//
// They are the first MCP tools in this server that write. What they may write
// is deliberately narrow: transcript_findings (the decisions layer) and
// transcript_chunks.embedding_stale (the rebuild flag). They never write
// transcripts.segments or transcripts.raw_text — the immutable ASR record — and
// they never write chunk TEXT, because the projection is regenerated on every
// embed and text written there is destroyed by the next rebuild.
//
// Every write is reversible by construction: a state flip has an inverse, a
// direct edit is a row that can be reverted, and no finding is ever deleted.

// Review actions, as an agent names them, mapped to the patch state they move
// a finding to. Kept as a map so the legality of each is answered by
// patch.CanTransition rather than a second copy of the state machine.
var reviewActions = map[string]string{
	"accept":     patch.StateAccepted,
	"reject":     patch.StateRejected,
	"revert":     patch.StateReverted,
	"reconsider": patch.StateProposed,
}

// reviewActionOrder is the stable order allowed actions are reported in — map
// iteration order must never leak into a tool result.
var reviewActionOrder = []string{"accept", "reject", "revert", "reconsider"}

// anchorStatusOK is the anchor status of a correction that resolves cleanly
// against the chunk's current pristine text. Every other status is one of the
// patch.StaleReason* values, plus anchorStatusNoChunk — one vocabulary, so a
// reviewer reads the same word the rebuild will write into stale_reason.
const (
	anchorStatusOK      = "ok"
	anchorStatusNoChunk = "no_chunk"
)

// correctionContextRunes is how much pristine text is returned around a located
// span. Enough to judge a homophone in its sentence, bounded so a worklist page
// stays readable in an agent's context.
const correctionContextRunes = 220

// correctionExcerptRunes bounds the fallback excerpt shown when the anchor does
// not resolve and there is no span to centre on.
const correctionExcerptRunes = 600

// maxDecidedByRunes bounds the attribution string a caller can write into
// decided_by.
const maxDecidedByRunes = 200

// CorrectionContext is the pristine text around a correction's span.
//
// PRISTINE, never the corrected surface: an anchor resolved against corrected
// text records a fingerprint the projection's input can never match, so a
// correction authored from it would be born stale. Before/Span/After are the
// windowed split; ChunkExcerpt is populated instead when the anchor does not
// resolve, so a reviewer can still see what the chunk actually says.
type CorrectionContext struct {
	Before       string `json:"before,omitempty"`
	Span         string `json:"span,omitempty"`
	After        string `json:"after,omitempty"`
	ChunkExcerpt string `json:"chunkExcerpt,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`
}

// CorrectionAnchor reports where a correction actually resolves in the chunk's
// CURRENT pristine text — not what the recording model claimed.
//
// Status is the whole point of the field: a `proposed` finding whose status is
// anchor_ambiguous will never be applicable, so accepting it only produces a
// stale row at the next rebuild. A reviewer should see that before deciding.
type CorrectionAnchor struct {
	Status           string `json:"status"`
	RecordedOffset   *int   `json:"recordedOffset,omitempty"`
	RecordedOccurred *int   `json:"recordedOccurrence,omitempty"`
	ResolvedStart    *int   `json:"resolvedStart,omitempty"`
	ResolvedEnd      *int   `json:"resolvedEnd,omitempty"`
}

// CorrectionEntry is one correction as an agent needs to see it to decide.
type CorrectionEntry struct {
	ID                  string            `json:"id"`
	State               string            `json:"state"`
	Origin              string            `json:"origin"`
	IssueType           string            `json:"issueType"`
	Confidence          float64           `json:"confidence"`
	Model               string            `json:"model"`
	OriginalText        string            `json:"originalText"`
	SuggestedCorrection string            `json:"suggestedCorrection,omitempty"`
	FilePath            string            `json:"filePath"`
	BookDir             string            `json:"bookDir"`
	TranscriptID        string            `json:"transcriptId"`
	ChunkID             string            `json:"chunkId,omitempty"`
	ChunkIndex          *int              `json:"chunkIndex,omitempty"`
	StartSec            float64           `json:"startSec"`
	EndSec              float64           `json:"endSec"`
	StaleReason         string            `json:"staleReason,omitempty"`
	DecidedBy           string            `json:"decidedBy,omitempty"`
	DecidedAt           string            `json:"decidedAt,omitempty"`
	AppliedAt           string            `json:"appliedAt,omitempty"`
	CreatedAt           string            `json:"createdAt"`
	AllowedActions      []string          `json:"allowedActions"`
	Anchor              CorrectionAnchor  `json:"anchor"`
	Context             CorrectionContext `json:"context"`
}

// ListCorrectionsOutput is the structured payload for list_transcript_corrections.
type ListCorrectionsOutput struct {
	Count       int               `json:"count"`
	States      []string          `json:"states,omitempty"`
	Path        string            `json:"path,omitempty"`
	Offset      int               `json:"offset"`
	NextOffset  *int              `json:"nextOffset,omitempty"`
	Corrections []CorrectionEntry `json:"corrections"`
}

// DecideCorrectionOutput is the structured payload for decide_transcript_correction.
type DecideCorrectionOutput struct {
	ID           string           `json:"id"`
	Action       string           `json:"action"`
	From         string           `json:"from"`
	To           string           `json:"to"`
	DecidedBy    string           `json:"decidedBy"`
	ChunkFlagged bool             `json:"chunkFlagged"`
	Correction   *CorrectionEntry `json:"correction,omitempty"`
}

// CreateCorrectionOutput is the structured payload for create_transcript_correction.
//
// Preview is what the chunk's text will become once the rebuild replays the
// overlay — the same patch.Replay the embed worker runs, not an approximation.
type CreateCorrectionOutput struct {
	ID           string   `json:"id,omitempty"`
	DryRun       bool     `json:"dryRun"`
	State        string   `json:"state"`
	Origin       string   `json:"origin"`
	DecidedBy    string   `json:"decidedBy"`
	ChunkID      string   `json:"chunkId"`
	TranscriptID string   `json:"transcriptId"`
	FilePath     string   `json:"filePath"`
	ChunkIndex   int      `json:"chunkIndex"`
	IssueType    string   `json:"issueType"`
	OriginalText string   `json:"originalText"`
	Correction   string   `json:"correction"`
	AnchorOffset int      `json:"anchorOffset"`
	Occurrence   int      `json:"anchorOccurrence"`
	ChunkSHA256  string   `json:"chunkSha256"`
	Preview      string   `json:"preview"`
	AlreadyStale []string `json:"alreadyStale,omitempty"`
	ChunkFlagged bool     `json:"chunkFlagged"`
}

// handleListCorrections serves list_transcript_corrections: the review worklist.
func (h *ToolHandlers) handleListCorrections(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, err := parseArgs(req)
	if err != nil {
		return errorResult(fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	states, err := parseStates(args.getString("state", patch.StateProposed))
	if err != nil {
		return errorResult(err.Error()), nil
	}

	path := strings.TrimSpace(args.getString("path", ""))
	if book := strings.TrimSpace(args.getString("book", "")); book != "" && path == "" {
		dir, errResult := h.resolveBookDir(ctx, book)
		if errResult != nil {
			return errResult, nil
		}
		path = dir
	}

	id := strings.TrimSpace(args.getString("id", ""))
	// Looking up ONE finding by id must not be filtered by the default state.
	// A caller asking for a specific correction wants that correction, and
	// silently returning nothing because it has since been accepted would read
	// as "no such finding".
	if id != "" && strings.TrimSpace(args.getString("state", "")) == "" {
		states = nil
	}

	filter := db.CorrectionFilter{
		ID:            id,
		Path:          path,
		States:        states,
		MinConfidence: clampThreshold(args.getFloat("min_confidence", 0)),
		Limit:         clampLimit(args.getInt("limit", 20), 20),
		Offset:        clampOffset(args.getInt("offset", 0)),
	}

	rows, err := h.db.ListCorrections(ctx, filter)
	if err != nil {
		h.logger.Error("list corrections failed", "error", err)
		return errorResult(fmt.Sprintf("Failed to list corrections: %v", err)), nil
	}

	entries := make([]CorrectionEntry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, buildCorrectionEntry(r))
	}

	out := ListCorrectionsOutput{
		Count:       len(entries),
		States:      states,
		Path:        filter.Path,
		Offset:      filter.Offset,
		Corrections: entries,
	}
	// A full page means there is probably another one. Same convention as
	// list_books: nextOffset is advisory, not a promise of more rows.
	if len(entries) == filter.Limit {
		next := filter.Offset + filter.Limit
		out.NextOffset = &next
	}

	return structuredResult(out, formatCorrectionList(out)), nil
}

// handleDecideCorrection serves decide_transcript_correction: the human gate.
//
// It drives db.SetPatchState, which validates the move against
// patch.CanTransition BEFORE any SQL runs and guards the UPDATE on the expected
// current state. Neither check is duplicated or bypassed here — this handler's
// job is to resolve the current state, name the target, and report what
// happened.
func (h *ToolHandlers) handleDecideCorrection(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, err := parseArgs(req)
	if err != nil {
		return errorResult(fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	id, err := args.requireString("id")
	if err != nil {
		return errorResult(fmt.Sprintf("Missing or invalid id parameter: %v", err)), nil
	}
	id = strings.TrimSpace(id)

	action := strings.ToLower(strings.TrimSpace(args.getString("action", "")))
	to, ok := reviewActions[action]
	if !ok {
		return errorResult(fmt.Sprintf("Unknown action %q — expected one of: %s",
			action, strings.Join(reviewActionOrder, ", "))), nil
	}

	rows, err := h.db.ListCorrections(ctx, db.CorrectionFilter{ID: id, Limit: 1})
	if err != nil {
		h.logger.Error("read correction for decision failed", "id", id, "error", err)
		return errorResult(fmt.Sprintf("Failed to read correction %s: %v", id, err)), nil
	}
	if len(rows) == 0 {
		return errorResult(fmt.Sprintf("No correction with id %s", id)), nil
	}
	current := rows[0]

	// An explicit expected_state makes the compare-and-swap the CALLER's: it
	// fails rather than deciding a finding that moved since the caller read it.
	from := current.PatchState
	if want := strings.TrimSpace(args.getString("expected_state", "")); want != "" {
		if want != current.PatchState {
			return errorResult(fmt.Sprintf(
				"Correction %s is in state %q, not the expected %q — re-read it before deciding",
				id, current.PatchState, want)), nil
		}
		from = want
	}

	if !patch.CanTransition(from, to) {
		return errorResult(fmt.Sprintf(
			"Cannot %s correction %s from state %q. Legal actions here: %s",
			action, id, from, describeActions(allowedActions(from)))), nil
	}

	by := attributeDecision(args.getString("decided_by", ""))
	if err := h.db.SetPatchState(ctx, id, from, to, by); err != nil {
		switch {
		case errors.Is(err, db.ErrPatchStateConflict):
			return errorResult(fmt.Sprintf(
				"Correction %s is no longer in state %q — someone decided it first. Re-read it and retry.",
				id, from)), nil
		case errors.Is(err, db.ErrIllegalTransition):
			return errorResult(fmt.Sprintf("Illegal transition for correction %s: %v", id, err)), nil
		}
		h.logger.Error("set patch state failed", "id", id, "from", from, "to", to, "error", err)
		return errorResult(fmt.Sprintf("Failed to %s correction %s: %v", action, id, err)), nil
	}

	// Accepting adds a correction to the overlay and reverting removes one, so
	// either leaves the chunk's projection out of date; SetPatchState flags it
	// embedding_stale in the SAME transaction and the worker's rebuild pass
	// re-embeds it. Rejecting changes no text, so nothing is flagged.
	flagged := to == patch.StateAccepted || to == patch.StateReverted

	out := DecideCorrectionOutput{
		ID: id, Action: action, From: from, To: to,
		ChunkFlagged: flagged,
	}
	// Only the states the DB treats as human decisions stamp decided_at/decided_by
	// (accept, reject, revert). Reporting an attribution for `reconsider`, which
	// stamps nothing, would describe an audit trail that does not exist.
	if stampsDecision(to) {
		out.DecidedBy = by
	}
	// Re-read so the caller sees the decided row, not its pre-decision self.
	if after, err := h.db.ListCorrections(ctx, db.CorrectionFilter{ID: id, Limit: 1}); err == nil && len(after) == 1 {
		e := buildCorrectionEntry(after[0])
		out.Correction = &e
	}

	return structuredResult(out, formatDecision(out)), nil
}

// stampsDecision reports whether reaching a state records decided_at/decided_by.
// Mirrors the DB layer's isHumanDecision: accept, reject and revert are
// decisions; returning a finding to `proposed` for reconsideration is not, and
// must not overwrite the record of who decided it last time.
func stampsDecision(to string) bool {
	switch to {
	case patch.StateAccepted, patch.StateRejected, patch.StateReverted:
		return true
	default:
		return false
	}
}

// handleCreateCorrection serves create_transcript_correction: an edit no model
// proposed.
//
// This is the escape hatch, and it is deliberately NOT a shortcut. A hand edit
// takes the identical path an accepted judge finding takes — anchor resolution,
// chunk-hash verification, overlap refusal, replay onto regenerated text — for
// one reason: an edit that skips them either desyncs the embedding from the
// text or is silently wiped by the next rebuild. What differs is provenance
// (origin = human), not rigor.
func (h *ToolHandlers) handleCreateCorrection(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, err := parseArgs(req)
	if err != nil {
		return errorResult(fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	chunkID, err := args.requireString("chunk_id")
	if err != nil {
		return errorResult(fmt.Sprintf("Missing or invalid chunk_id parameter: %v", err)), nil
	}
	chunkID = strings.TrimSpace(chunkID)

	original, err := args.requireString("original_text")
	if err != nil {
		return errorResult(fmt.Sprintf("Missing or invalid original_text parameter: %v", err)), nil
	}
	correction, err := args.requireString("correction")
	if err != nil {
		return errorResult(fmt.Sprintf("Missing or invalid correction parameter: %v", err)), nil
	}
	if strings.TrimSpace(correction) == "" {
		return errorResult("correction is empty — an empty correction would delete text without saying so"), nil
	}

	issueType := normalizeIssueType(args.getString("issue_type", ""))
	dryRun := args.getBool("dry_run", false)

	target, err := h.db.GetChunkForEdit(ctx, chunkID)
	if err != nil {
		if errors.Is(err, db.ErrChunkNotFound) {
			return errorResult(fmt.Sprintf("No chunk with id %s — search first, then edit the chunk a result names", chunkID)), nil
		}
		h.logger.Error("read chunk for edit failed", "chunk_id", chunkID, "error", err)
		return errorResult(fmt.Sprintf("Failed to read chunk %s: %v", chunkID, err)), nil
	}

	// The chunk's own overlay, so the edit can be refused if it collides with a
	// correction already accepted on the same characters. Read via the same
	// query the rebuild uses — there is only one definition of "the overlay".
	rows, _, err := h.db.GetCorrectionOverlay(ctx, target.TranscriptID)
	if err != nil {
		h.logger.Error("read correction overlay for edit failed", "transcript_id", target.TranscriptID, "error", err)
		return errorResult(fmt.Sprintf("Failed to read the correction overlay for transcript %s: %v",
			target.TranscriptID, err)), nil
	}
	overlay, _ := db.BuildOverlay(rows)

	edit := patch.Patch{
		ID: "(new)",
		Anchor: patch.Anchor{
			OriginalText: original,
			// A caller-reported offset is a HINT into Locate's ladder, never a
			// coordinate to trust: a real model once reported 27 for a span at 29.
			// What gets persisted is the offset Locate actually resolved.
			Offset:     args.getInt("offset", -1),
			Occurrence: args.getInt("occurrence", -1),
		},
		Correction: correction,
		ChunkHash:  strings.TrimSpace(args.getString("expected_chunk_sha256", "")),
	}

	plan, err := patch.PlanDirectEdit(target.Pristine, overlay[target.ChunkIndex], edit)
	if err != nil {
		return errorResult(explainEditRefusal(err, target, original)), nil
	}

	by := attributeDecision(args.getString("decided_by", ""))
	out := CreateCorrectionOutput{
		DryRun:       dryRun,
		State:        patch.StateAccepted,
		Origin:       db.OriginHuman,
		DecidedBy:    by,
		ChunkID:      target.ID,
		TranscriptID: target.TranscriptID,
		FilePath:     target.FilePath,
		ChunkIndex:   target.ChunkIndex,
		IssueType:    issueType,
		OriginalText: original,
		Correction:   correction,
		AnchorOffset: plan.Span.Start,
		Occurrence:   plan.Occurrence,
		ChunkSHA256:  plan.ChunkHash,
		Preview:      plan.Preview,
		AlreadyStale: staleIDs(plan.PreviewStale),
	}

	if dryRun {
		return structuredResult(out, formatCreate(out)), nil
	}

	id, err := h.db.InsertManualCorrection(ctx, db.ManualCorrection{
		ChunkID:          target.ID,
		OriginalText:     original,
		Correction:       correction,
		AnchorOffset:     plan.Span.Start,
		AnchorOccurrence: plan.Occurrence,
		ChunkSHA256:      plan.ChunkHash,
		IssueType:        issueType,
		DecidedBy:        by,
	})
	if err != nil {
		if errors.Is(err, patch.ErrChunkChanged) {
			return errorResult(fmt.Sprintf(
				"Chunk %s changed while the edit was being verified — re-read it and retry", target.ID)), nil
		}
		h.logger.Error("insert manual correction failed", "chunk_id", target.ID, "error", err)
		return errorResult(fmt.Sprintf("Failed to record the correction: %v", err)), nil
	}

	out.ID = id
	out.ChunkFlagged = true
	h.logger.Info("manual correction recorded",
		"id", id, "chunk_id", target.ID, "file", target.FilePath, "decided_by", by)

	return structuredResult(out, formatCreate(out)), nil
}

// buildCorrectionEntry projects a stored correction into the review view,
// resolving its anchor against the chunk's CURRENT pristine text.
//
// The resolution runs through patch.Replay — the very function the embed worker
// replays overlays with — so "will this apply?" is answered by the code that
// will actually answer it at rebuild time, never by a second implementation of
// the ladder that could drift.
func buildCorrectionEntry(c db.CorrectionDetail) CorrectionEntry {
	e := CorrectionEntry{
		ID:                  c.ID,
		State:               c.PatchState,
		Origin:              c.Origin,
		IssueType:           c.IssueType,
		Confidence:          c.Confidence,
		Model:               c.Model,
		OriginalText:        c.OriginalText,
		SuggestedCorrection: derefString(c.SuggestedCorrection),
		FilePath:            c.FilePath,
		BookDir:             c.BookDir,
		TranscriptID:        c.TranscriptID,
		ChunkID:             derefString(c.ChunkID),
		ChunkIndex:          c.ChunkIndex,
		StartSec:            c.StartSec,
		EndSec:              c.EndSec,
		StaleReason:         derefString(c.StaleReason),
		DecidedBy:           derefString(c.DecidedBy),
		DecidedAt:           formatTime(c.DecidedAt),
		AppliedAt:           formatTime(c.AppliedAt),
		CreatedAt:           c.CreatedAt.Format(time.RFC3339),
		AllowedActions:      allowedActions(c.PatchState),
		Anchor: CorrectionAnchor{
			Status:           anchorStatusNoChunk,
			RecordedOffset:   c.AnchorOffset,
			RecordedOccurred: c.AnchorOccurrence,
		},
	}

	if c.ChunkText == "" {
		// No chunk to resolve against: the transcript was requeued, or the
		// finding recorded no chunk at all.
		return e
	}

	res := patch.Replay(c.ChunkText, []patch.Patch{{
		ID: c.ID,
		Anchor: patch.Anchor{
			OriginalText: c.OriginalText,
			Offset:       intOrUnknown(c.AnchorOffset),
			Occurrence:   intOrUnknown(c.AnchorOccurrence),
		},
		Correction: derefString(c.SuggestedCorrection),
		ChunkHash:  derefString(c.ChunkTextSHA256),
	}})

	switch {
	case len(res.Applied) == 1:
		span := res.Applied[0].Span
		start, end := span.Start, span.End
		e.Anchor.Status = anchorStatusOK
		e.Anchor.ResolvedStart = &start
		e.Anchor.ResolvedEnd = &end
		e.Context = windowContext(c.ChunkText, span)
	case len(res.Stale) == 1:
		e.Anchor.Status = res.Stale[0].Reason
		e.Context = excerptContext(c.ChunkText)
	default:
		// Unreachable: Replay partitions one input patch into exactly one bucket.
		e.Context = excerptContext(c.ChunkText)
	}
	return e
}

// windowContext splits the pristine chunk text around a located span, bounded
// so a page of corrections stays readable.
func windowContext(text string, span patch.Span) CorrectionContext {
	runes := []rune(text)
	if span.Start < 0 || span.End > len(runes) || span.Start > span.End {
		return excerptContext(text)
	}
	from := span.Start - correctionContextRunes
	if from < 0 {
		from = 0
	}
	to := span.End + correctionContextRunes
	if to > len(runes) {
		to = len(runes)
	}
	return CorrectionContext{
		Before:    string(runes[from:span.Start]),
		Span:      string(runes[span.Start:span.End]),
		After:     string(runes[span.End:to]),
		Truncated: from > 0 || to < len(runes),
	}
}

// excerptContext is the fallback when there is no span to centre on.
func excerptContext(text string) CorrectionContext {
	runes := []rune(text)
	if len(runes) <= correctionExcerptRunes {
		return CorrectionContext{ChunkExcerpt: text}
	}
	return CorrectionContext{ChunkExcerpt: string(runes[:correctionExcerptRunes]), Truncated: true}
}

// allowedActions lists the review actions legal from a state, derived from
// patch.CanTransition so the tool surface and the DB boundary cannot disagree
// about what a reviewer may do.
func allowedActions(from string) []string {
	out := make([]string, 0, len(reviewActionOrder))
	for _, a := range reviewActionOrder {
		if patch.CanTransition(from, reviewActions[a]) {
			out = append(out, a)
		}
	}
	return out
}

func describeActions(actions []string) string {
	if len(actions) == 0 {
		return "none (terminal state)"
	}
	return strings.Join(actions, ", ")
}

// parseStates parses the `state` filter: a comma-separated list of patch states,
// or "all"/"any" for no filter. Unknown values are rejected rather than silently
// matching nothing, which would read as "this book has no findings".
func parseStates(raw string) ([]string, error) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" || raw == "all" || raw == "any" {
		return nil, nil
	}
	valid := make(map[string]bool, len(patch.AllStates()))
	for _, s := range patch.AllStates() {
		valid[s] = true
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		s := strings.TrimSpace(part)
		if s == "" {
			continue
		}
		if !valid[s] {
			return nil, fmt.Errorf("unknown state %q — expected one of: %s, or \"all\"",
				s, strings.Join(patch.AllStates(), ", "))
		}
		out = append(out, s)
	}
	return out, nil
}

// attributeDecision normalizes the decided_by attribution.
//
// transcript_findings.decided_by is the audit answer to "who approved this", so
// a bare name is not enough: it cannot distinguish a person clicking the
// dashboard from an agent calling a tool. Every decision made through this
// surface is therefore prefixed, and an unattributed call still records that it
// came from an agent rather than recording nothing.
func attributeDecision(raw string) string {
	who := strings.TrimSpace(raw)
	if who == "" {
		who = "agent"
	}
	if !strings.HasPrefix(who, "mcp:") {
		who = "mcp:" + who
	}
	if r := []rune(who); len(r) > maxDecidedByRunes {
		who = string(r[:maxDecidedByRunes])
	}
	return who
}

// normalizeIssueType maps a caller's classification onto the closed vocabulary
// the judge uses (CONTRACT §2.15), coercing anything unknown to "other" exactly
// as the judge's parser does. A human edit is still a finding, so it must stay
// enumerable alongside the judge's — provenance is carried by origin, not by
// smuggling a new issue_type value into the vocabulary.
func normalizeIssueType(raw string) string {
	v := strings.TrimSpace(strings.ToLower(raw))
	switch v {
	case "misheard_proper_noun", "misheard_word", "repeated_text",
		"number_artifact", "homophone", "dropped_word", "other":
		return v
	default:
		return "other"
	}
}

// explainEditRefusal turns a plan failure into an instruction the caller can
// act on. A refusal that only says "not found" invites a retry that fails the
// same way; naming the number of candidate spans tells the caller to pass an
// occurrence instead.
func explainEditRefusal(err error, target *db.ChunkTarget, original string) string {
	switch {
	case errors.Is(err, patch.ErrEmptyCorrection):
		return "The correction is empty — an empty correction would delete text without saying so"
	case errors.Is(err, patch.ErrChunkChanged):
		return fmt.Sprintf("Chunk %s no longer matches expected_chunk_sha256 (now %s) — re-read it and retry",
			target.ID, patch.ChunkHash(target.Pristine))
	case errors.Is(err, patch.ErrAnchorAmbiguous):
		n := len(patch.Occurrences(target.Pristine, original))
		return fmt.Sprintf("%q occurs %d times in chunk %s — pass `occurrence` (0-based) to say which one",
			original, n, target.ID)
	case errors.Is(err, patch.ErrAnchorNotFound):
		return fmt.Sprintf("%q does not occur in chunk %s's pristine text. "+
			"Copy the span verbatim from this chunk's original (uncorrected) text — "+
			"an anchor is never resolved against corrected text", original, target.ID)
	case errors.Is(err, patch.ErrOverlappingPatches):
		return fmt.Sprintf("The edit overlaps a correction already accepted on chunk %s: %v. "+
			"Revert that one first, or edit a different span", target.ID, err)
	default:
		return fmt.Sprintf("The edit could not be verified against chunk %s: %v", target.ID, err)
	}
}

func staleIDs(refs []patch.StaleRef) []string {
	if len(refs) == 0 {
		return nil
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, fmt.Sprintf("%s (%s)", r.ID, r.Reason))
	}
	return out
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func formatTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

// intOrUnknown maps a NULL anchor column to patch's "unknown" sentinel (-1), so
// Locate falls back down its resolution ladder instead of trusting a zero it was
// never given. Mirrors the db-side helper of the same name.
func intOrUnknown(v *int) int {
	if v == nil {
		return -1
	}
	return *v
}

// formatCorrectionList renders the text fallback for list_transcript_corrections.
func formatCorrectionList(out ListCorrectionsOutput) string {
	if out.Count == 0 {
		scope := "the library"
		if out.Path != "" {
			scope = out.Path
		}
		states := "any state"
		if len(out.States) > 0 {
			states = strings.Join(out.States, "/")
		}
		return fmt.Sprintf("No corrections in %s matching %s.", scope, states)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Found %d correction(s):\n", out.Count)
	for i, c := range out.Corrections {
		fmt.Fprintf(&b, "\n%d. %s · %s · %s · confidence %.2f · %s\n",
			i+1, c.ID, c.State, c.IssueType, c.Confidence, c.Origin)
		fmt.Fprintf(&b, "   %s @ %.1fs–%.1fs\n", c.FilePath, c.StartSec, c.EndSec)
		fmt.Fprintf(&b, "   %q → %q\n", c.OriginalText, c.SuggestedCorrection)
		fmt.Fprintf(&b, "   anchor: %s", c.Anchor.Status)
		if c.Anchor.ResolvedStart != nil {
			fmt.Fprintf(&b, " (runes %d–%d)", *c.Anchor.ResolvedStart, *c.Anchor.ResolvedEnd)
		}
		if c.StaleReason != "" {
			fmt.Fprintf(&b, " · stale: %s", c.StaleReason)
		}
		b.WriteString("\n")
		if c.Context.Span != "" {
			fmt.Fprintf(&b, "   context: …%s⟦%s⟧%s…\n", c.Context.Before, c.Context.Span, c.Context.After)
		} else if c.Context.ChunkExcerpt != "" {
			fmt.Fprintf(&b, "   chunk: %s\n", c.Context.ChunkExcerpt)
		}
		fmt.Fprintf(&b, "   actions: %s\n", describeActions(c.AllowedActions))
	}
	if out.NextOffset != nil {
		fmt.Fprintf(&b, "\nMore may follow — call again with offset=%d.\n", *out.NextOffset)
	}
	return b.String()
}

// formatDecision renders the text fallback for decide_transcript_correction.
func formatDecision(out DecideCorrectionOutput) string {
	var b strings.Builder
	if out.DecidedBy != "" {
		fmt.Fprintf(&b, "Correction %s: %s (%s → %s), recorded as %s.\n",
			out.ID, out.Action, out.From, out.To, out.DecidedBy)
	} else {
		fmt.Fprintf(&b, "Correction %s: %s (%s → %s). Returning a finding to the queue is not "+
			"itself a decision, so no decided_by is stamped.\n",
			out.ID, out.Action, out.From, out.To)
	}
	if out.ChunkFlagged {
		b.WriteString("The chunk is flagged for re-embed; the worker's rebuild pass replays the overlay and updates the searchable text.\n")
	} else {
		b.WriteString("No text changes — nothing was flagged for re-embed.\n")
	}
	if out.Correction != nil {
		fmt.Fprintf(&b, "Now in state %q; next actions: %s.\n",
			out.Correction.State, describeActions(out.Correction.AllowedActions))
	}
	return b.String()
}

// formatCreate renders the text fallback for create_transcript_correction.
func formatCreate(out CreateCorrectionOutput) string {
	var b strings.Builder
	if out.DryRun {
		b.WriteString("Dry run — nothing was written.\n")
	} else {
		fmt.Fprintf(&b, "Recorded correction %s (%s, state %s, by %s).\n",
			out.ID, out.Origin, out.State, out.DecidedBy)
	}
	fmt.Fprintf(&b, "%s chunk %d (%s)\n", out.FilePath, out.ChunkIndex, out.ChunkID)
	fmt.Fprintf(&b, "  %q → %q at rune %d (occurrence %d)\n",
		out.OriginalText, out.Correction, out.AnchorOffset, out.Occurrence)
	fmt.Fprintf(&b, "  chunk fingerprint: %s\n", out.ChunkSHA256)
	fmt.Fprintf(&b, "  preview: %s\n", out.Preview)
	if len(out.AlreadyStale) > 0 {
		fmt.Fprintf(&b, "  note: %d existing correction(s) on this chunk cannot replay: %s\n",
			len(out.AlreadyStale), strings.Join(out.AlreadyStale, ", "))
	}
	if out.ChunkFlagged {
		b.WriteString("  chunk flagged for re-embed.\n")
	}
	return b.String()
}
