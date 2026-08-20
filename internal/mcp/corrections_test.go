package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// strp / intp mirror fp/ip from tools_test.go for the pointer fields
// CorrectionDetail carries.
func strp(v string) *string { return &v }
func intp(v int) *int       { return &v }

// baseCorrectionRow returns a minimally-valid CorrectionDetail so each test
// only needs to override the fields it cares about.
func baseCorrectionRow(id, state string) db.CorrectionDetail {
	return db.CorrectionDetail{
		ID: id, TranscriptID: "t1", FilePath: "/books/a/b/ch1.m4b", BookDir: "/books/a/b",
		ChunkID: strp("chunk-1"), ChunkIndex: intp(0),
		StartSec: 10, EndSec: 20,
		OriginalText: "auto sebo", SuggestedCorrection: strp("Arecibo"),
		IssueType: "misheard_proper_noun", Confidence: 0.9,
		Model: "qwen2.5-14b-instruct", Origin: db.OriginJudge,
		PatchState: state, CreatedAt: time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC),
	}
}

// ─── list_transcript_corrections ──────────────────────────────────────────

// TestHandleListCorrections_DefaultStateIsProposed asserts the state filter
// defaults to "proposed" when the caller supplies neither `state` nor `id`.
func TestHandleListCorrections_DefaultStateIsProposed(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockDB.On("ListCorrections", mock.Anything, db.CorrectionFilter{
		States: []string{patch.StateProposed}, Limit: 20,
	}).Return([]db.CorrectionDetail{}, nil).Once()

	h := NewToolHandlers(mockDB, nil)
	res, err := h.handleListCorrections(context.Background(), req("list_transcript_corrections", map[string]interface{}{}))
	require.NoError(t, err)
	assert.False(t, res.IsError)
	mockDB.AssertExpectations(t)
}

// TestHandleListCorrections_StateAllClearsFilter asserts state="all" passes a
// nil States filter (every state).
func TestHandleListCorrections_StateAllClearsFilter(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockDB.On("ListCorrections", mock.Anything, db.CorrectionFilter{
		States: nil, Limit: 20,
	}).Return([]db.CorrectionDetail{}, nil).Once()

	h := NewToolHandlers(mockDB, nil)
	res, err := h.handleListCorrections(context.Background(), req("list_transcript_corrections", map[string]interface{}{
		"state": "all",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)
	mockDB.AssertExpectations(t)
}

// TestHandleListCorrections_UnknownStateIsRefused asserts an unrecognized
// state value is rejected with a message naming the legal values, and never
// reaches the database.
func TestHandleListCorrections_UnknownStateIsRefused(t *testing.T) {
	mockDB := &MockDBInterface{}

	h := NewToolHandlers(mockDB, nil)
	res, err := h.handleListCorrections(context.Background(), req("list_transcript_corrections", map[string]interface{}{
		"state": "bogus",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	text := res.Content[0].(*mcp.TextContent).Text
	assert.Contains(t, text, `unknown state "bogus"`)
	for _, s := range patch.AllStates() {
		assert.Contains(t, text, s)
	}
	mockDB.AssertNotCalled(t, "ListCorrections", mock.Anything, mock.Anything)
}

// TestHandleListCorrections_IDWithoutExplicitStateClearsFilter asserts that
// looking up a single finding by id is never narrowed by the default state
// filter — a caller asking for a specific correction wants that correction
// even if it has since moved past "proposed".
func TestHandleListCorrections_IDWithoutExplicitStateClearsFilter(t *testing.T) {
	mockDB := &MockDBInterface{}
	const id = "f1"
	mockDB.On("ListCorrections", mock.Anything, db.CorrectionFilter{
		ID: id, States: nil, Limit: 20,
	}).Return([]db.CorrectionDetail{}, nil).Once()

	h := NewToolHandlers(mockDB, nil)
	res, err := h.handleListCorrections(context.Background(), req("list_transcript_corrections", map[string]interface{}{
		"id": id,
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)
	mockDB.AssertExpectations(t)
}

// TestHandleListCorrections_AnchorStatuses drives buildCorrectionEntry through
// the handler for the three anchor outcomes: a resolvable anchor ("ok", with
// resolved span + windowed context), a hash mismatch ("chunk_changed", falling
// back to a chunk excerpt), and a finding with no chunk at all ("no_chunk").
func TestHandleListCorrections_AnchorStatuses(t *testing.T) {
	const chunkText = "the auto sebo dish"
	goodHash := patch.ChunkHash(chunkText)

	okRow := baseCorrectionRow("f-ok", patch.StateProposed)
	okRow.ChunkText = chunkText
	okRow.AnchorOffset = intp(4)
	okRow.AnchorOccurrence = intp(0)
	okRow.ChunkTextSHA256 = strp(goodHash)

	staleRow := baseCorrectionRow("f-stale", patch.StateProposed)
	staleRow.ChunkText = chunkText
	staleRow.ChunkTextSHA256 = strp("0000000000000000000000000000000000000000000000000000000000000000")

	noChunkRow := baseCorrectionRow("f-nochunk", patch.StateProposed)
	noChunkRow.ChunkText = ""

	mockDB := &MockDBInterface{}
	mockDB.On("ListCorrections", mock.Anything, db.CorrectionFilter{
		States: []string{patch.StateProposed}, Limit: 20,
	}).Return([]db.CorrectionDetail{okRow, staleRow, noChunkRow}, nil).Once()

	h := NewToolHandlers(mockDB, nil)
	res, err := h.handleListCorrections(context.Background(), req("list_transcript_corrections", map[string]interface{}{}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	out, ok := res.StructuredContent.(ListCorrectionsOutput)
	require.True(t, ok, "structuredContent should be a ListCorrectionsOutput, got %T", res.StructuredContent)
	require.Len(t, out.Corrections, 3)

	ok0 := out.Corrections[0]
	assert.Equal(t, anchorStatusOK, ok0.Anchor.Status)
	require.NotNil(t, ok0.Anchor.ResolvedStart)
	require.NotNil(t, ok0.Anchor.ResolvedEnd)
	assert.Equal(t, 4, *ok0.Anchor.ResolvedStart)
	assert.Equal(t, 13, *ok0.Anchor.ResolvedEnd)
	assert.Equal(t, "auto sebo", ok0.Context.Span)
	assert.Equal(t, "the ", ok0.Context.Before)
	assert.Equal(t, " dish", ok0.Context.After)

	staleEntry := out.Corrections[1]
	assert.Equal(t, patch.StaleReasonChunkChanged, staleEntry.Anchor.Status)
	assert.Nil(t, staleEntry.Anchor.ResolvedStart)
	assert.Equal(t, chunkText, staleEntry.Context.ChunkExcerpt)

	noChunk := out.Corrections[2]
	assert.Equal(t, anchorStatusNoChunk, noChunk.Anchor.Status)
	assert.Empty(t, noChunk.Context.ChunkExcerpt)

	mockDB.AssertExpectations(t)
}

// TestAllowedActions_DerivedFromState asserts the reported actions are exactly
// what patch.CanTransition allows from each state.
func TestAllowedActions_DerivedFromState(t *testing.T) {
	assert.Equal(t, []string{"accept", "reject"}, allowedActions(patch.StateProposed))
	assert.Equal(t, []string{"revert"}, allowedActions(patch.StateApplied))
	assert.Empty(t, allowedActions(patch.StateStale))
}

// ─── decide_transcript_correction ─────────────────────────────────────────

// mockCurrentCorrection registers a single ListCorrections("id", Limit:1)
// expectation returning one row in the given state, for handleDecideCorrection's
// initial read.
func mockCurrentCorrection(mockDB *MockDBInterface, id, state string) {
	row := baseCorrectionRow(id, state)
	mockDB.On("ListCorrections", mock.Anything, db.CorrectionFilter{ID: id, Limit: 1}).
		Return([]db.CorrectionDetail{row}, nil).Once()
}

// TestHandleDecideCorrection_Accept asserts an accept on a proposed finding
// calls SetPatchState with the attributed decider and reports chunkFlagged.
func TestHandleDecideCorrection_Accept(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockCurrentCorrection(mockDB, "f1", patch.StateProposed)
	mockDB.On("SetPatchState", mock.Anything, "f1", patch.StateProposed, patch.StateAccepted, "mcp:justin").
		Return(nil).Once()
	// Re-read after the decision.
	afterRow := baseCorrectionRow("f1", patch.StateAccepted)
	mockDB.On("ListCorrections", mock.Anything, db.CorrectionFilter{ID: "f1", Limit: 1}).
		Return([]db.CorrectionDetail{afterRow}, nil).Once()

	h := NewToolHandlers(mockDB, nil)
	res, err := h.handleDecideCorrection(context.Background(), req("decide_transcript_correction", map[string]interface{}{
		"id": "f1", "action": "accept", "decided_by": "justin",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	out, ok := res.StructuredContent.(DecideCorrectionOutput)
	require.True(t, ok)
	assert.Equal(t, "mcp:justin", out.DecidedBy)
	assert.True(t, out.ChunkFlagged)
	assert.Equal(t, patch.StateAccepted, out.To)
	mockDB.AssertExpectations(t)
}

// TestHandleDecideCorrection_Reject asserts a reject reports chunkFlagged=false
// (a rejection changes no text, so nothing needs re-embedding).
func TestHandleDecideCorrection_Reject(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockCurrentCorrection(mockDB, "f1", patch.StateProposed)
	mockDB.On("SetPatchState", mock.Anything, "f1", patch.StateProposed, patch.StateRejected, "mcp:agent").
		Return(nil).Once()
	afterRow := baseCorrectionRow("f1", patch.StateRejected)
	mockDB.On("ListCorrections", mock.Anything, db.CorrectionFilter{ID: "f1", Limit: 1}).
		Return([]db.CorrectionDetail{afterRow}, nil).Once()

	h := NewToolHandlers(mockDB, nil)
	res, err := h.handleDecideCorrection(context.Background(), req("decide_transcript_correction", map[string]interface{}{
		"id": "f1", "action": "reject",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	out, ok := res.StructuredContent.(DecideCorrectionOutput)
	require.True(t, ok)
	assert.False(t, out.ChunkFlagged)
	mockDB.AssertExpectations(t)
}

// TestHandleDecideCorrection_IllegalActionRefusedBeforeWrite asserts an action
// illegal from the current state (revert on a proposed finding) is refused
// before SetPatchState is ever called, and lists the legal actions.
func TestHandleDecideCorrection_IllegalActionRefusedBeforeWrite(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockCurrentCorrection(mockDB, "f1", patch.StateProposed)

	h := NewToolHandlers(mockDB, nil)
	res, err := h.handleDecideCorrection(context.Background(), req("decide_transcript_correction", map[string]interface{}{
		"id": "f1", "action": "revert",
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	text := res.Content[0].(*mcp.TextContent).Text
	assert.Contains(t, text, "Cannot revert correction f1")
	assert.Contains(t, text, "accept, reject")
	mockDB.AssertNotCalled(t, "SetPatchState", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	mockDB.AssertExpectations(t)
}

// TestHandleDecideCorrection_ExpectedStateMismatchRefusedBeforeWrite asserts a
// caller-supplied expected_state that disagrees with the actual current state
// is refused without calling SetPatchState — the compare-and-swap is the
// caller's, so a stale read must fail rather than silently deciding.
func TestHandleDecideCorrection_ExpectedStateMismatchRefusedBeforeWrite(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockCurrentCorrection(mockDB, "f1", patch.StateProposed)

	h := NewToolHandlers(mockDB, nil)
	res, err := h.handleDecideCorrection(context.Background(), req("decide_transcript_correction", map[string]interface{}{
		"id": "f1", "action": "accept", "expected_state": patch.StateAccepted,
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	text := res.Content[0].(*mcp.TextContent).Text
	assert.Contains(t, text, `is in state "proposed"`)
	assert.Contains(t, text, `expected "accepted"`)
	mockDB.AssertNotCalled(t, "SetPatchState", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	mockDB.AssertExpectations(t)
}

// TestHandleDecideCorrection_ConflictFromDB asserts a db.ErrPatchStateConflict
// surfaced by SetPatchState (someone else decided first) produces an
// actionable error result rather than a generic failure.
func TestHandleDecideCorrection_ConflictFromDB(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockCurrentCorrection(mockDB, "f1", patch.StateProposed)
	mockDB.On("SetPatchState", mock.Anything, "f1", patch.StateProposed, patch.StateAccepted, "mcp:agent").
		Return(db.ErrPatchStateConflict).Once()

	h := NewToolHandlers(mockDB, nil)
	res, err := h.handleDecideCorrection(context.Background(), req("decide_transcript_correction", map[string]interface{}{
		"id": "f1", "action": "accept",
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	text := res.Content[0].(*mcp.TextContent).Text
	assert.Contains(t, text, "no longer in state")
	assert.Contains(t, text, "Re-read it and retry")
	mockDB.AssertExpectations(t)
}

// TestHandleDecideCorrection_UnknownAction asserts an unrecognized action
// returns an error result without touching the database.
func TestHandleDecideCorrection_UnknownAction(t *testing.T) {
	mockDB := &MockDBInterface{}

	h := NewToolHandlers(mockDB, nil)
	res, err := h.handleDecideCorrection(context.Background(), req("decide_transcript_correction", map[string]interface{}{
		"id": "f1", "action": "obliterate",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content[0].(*mcp.TextContent).Text, "Unknown action")
	mockDB.AssertNotCalled(t, "ListCorrections", mock.Anything, mock.Anything)
}

// TestHandleDecideCorrection_MissingID asserts a missing id argument is
// refused before any database access.
func TestHandleDecideCorrection_MissingID(t *testing.T) {
	mockDB := &MockDBInterface{}

	h := NewToolHandlers(mockDB, nil)
	res, err := h.handleDecideCorrection(context.Background(), req("decide_transcript_correction", map[string]interface{}{
		"action": "accept",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content[0].(*mcp.TextContent).Text, "Missing or invalid id parameter")
	mockDB.AssertNotCalled(t, "ListCorrections", mock.Anything, mock.Anything)
}

// ─── create_transcript_correction ─────────────────────────────────────────

const createTestPristine = "the signal came from auto sebo in the jungle"

func createTestTarget() *db.ChunkTarget {
	return &db.ChunkTarget{
		ID: "chunk-1", TranscriptID: "t1", FilePath: "/books/a/b/ch1.m4b",
		ChunkIndex: 2, StartSec: 0, EndSec: 10,
		Pristine: createTestPristine, Corrected: createTestPristine,
	}
}

// TestHandleCreateCorrection_HappyPath asserts the write path resolves the
// span SERVER-SIDE, calls InsertManualCorrection with the resolved offset,
// occurrence, and chunk hash (never a caller-supplied one, since none was
// given here), and reports origin human / state accepted / chunkFlagged.
func TestHandleCreateCorrection_HappyPath(t *testing.T) {
	mockDB := &MockDBInterface{}
	target := createTestTarget()
	mockDB.On("GetChunkForEdit", mock.Anything, "chunk-1").Return(target, nil).Once()
	mockDB.On("GetCorrectionOverlay", mock.Anything, "t1").
		Return([]db.CorrectionRow{}, time.Time{}, nil).Once()

	wantOffset := strings.Index(createTestPristine, "auto sebo")
	wantHash := patch.ChunkHash(createTestPristine)
	mockDB.On("InsertManualCorrection", mock.Anything, db.ManualCorrection{
		ChunkID: "chunk-1", OriginalText: "auto sebo", Correction: "Arecibo",
		AnchorOffset: wantOffset, AnchorOccurrence: 0,
		ChunkSHA256: wantHash, IssueType: "misheard_proper_noun", DecidedBy: "mcp:justin",
	}).Return("new-finding-id", nil).Once()

	h := NewToolHandlers(mockDB, nil)
	res, err := h.handleCreateCorrection(context.Background(), req("create_transcript_correction", map[string]interface{}{
		"chunk_id": "chunk-1", "original_text": "auto sebo", "correction": "Arecibo",
		"issue_type": "misheard_proper_noun", "decided_by": "justin",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	out, ok := res.StructuredContent.(CreateCorrectionOutput)
	require.True(t, ok, "structuredContent should be a CreateCorrectionOutput, got %T", res.StructuredContent)
	assert.Equal(t, "new-finding-id", out.ID)
	assert.Equal(t, db.OriginHuman, out.Origin)
	assert.Equal(t, patch.StateAccepted, out.State)
	assert.True(t, out.ChunkFlagged)
	assert.Equal(t, wantOffset, out.AnchorOffset)
	assert.Equal(t, wantHash, out.ChunkSHA256)
	assert.Contains(t, out.Preview, "Arecibo")
	mockDB.AssertExpectations(t)
}

// TestHandleCreateCorrection_DryRunDoesNotWrite asserts dry_run=true returns
// the same verified plan/preview but never calls InsertManualCorrection.
func TestHandleCreateCorrection_DryRunDoesNotWrite(t *testing.T) {
	mockDB := &MockDBInterface{}
	target := createTestTarget()
	mockDB.On("GetChunkForEdit", mock.Anything, "chunk-1").Return(target, nil).Once()
	mockDB.On("GetCorrectionOverlay", mock.Anything, "t1").
		Return([]db.CorrectionRow{}, time.Time{}, nil).Once()

	h := NewToolHandlers(mockDB, nil)
	res, err := h.handleCreateCorrection(context.Background(), req("create_transcript_correction", map[string]interface{}{
		"chunk_id": "chunk-1", "original_text": "auto sebo", "correction": "Arecibo",
		"dry_run": true,
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	out, ok := res.StructuredContent.(CreateCorrectionOutput)
	require.True(t, ok)
	assert.True(t, out.DryRun)
	assert.Empty(t, out.ID)
	assert.False(t, out.ChunkFlagged)
	assert.Contains(t, out.Preview, "Arecibo")
	mockDB.AssertNotCalled(t, "InsertManualCorrection", mock.Anything, mock.Anything)
	mockDB.AssertExpectations(t)
}

// TestHandleCreateCorrection_AmbiguousSpan asserts a span occurring more than
// once without an `occurrence` hint is refused with guidance to pass one and
// how many candidates exist, and never writes.
func TestHandleCreateCorrection_AmbiguousSpan(t *testing.T) {
	mockDB := &MockDBInterface{}
	target := &db.ChunkTarget{
		ID: "chunk-1", TranscriptID: "t1", FilePath: "/books/a/b/ch1.m4b",
		ChunkIndex: 0, Pristine: "the fox ran and the fox sat", Corrected: "the fox ran and the fox sat",
	}
	mockDB.On("GetChunkForEdit", mock.Anything, "chunk-1").Return(target, nil).Once()
	mockDB.On("GetCorrectionOverlay", mock.Anything, "t1").
		Return([]db.CorrectionRow{}, time.Time{}, nil).Once()

	h := NewToolHandlers(mockDB, nil)
	res, err := h.handleCreateCorrection(context.Background(), req("create_transcript_correction", map[string]interface{}{
		"chunk_id": "chunk-1", "original_text": "fox", "correction": "wolf",
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	text := res.Content[0].(*mcp.TextContent).Text
	assert.Contains(t, text, "occurs 2 times")
	assert.Contains(t, text, "occurrence")
	mockDB.AssertNotCalled(t, "InsertManualCorrection", mock.Anything, mock.Anything)
}

// TestHandleCreateCorrection_SpanNotPresent asserts a span that does not occur
// in the pristine text is refused and never writes.
func TestHandleCreateCorrection_SpanNotPresent(t *testing.T) {
	mockDB := &MockDBInterface{}
	target := createTestTarget()
	mockDB.On("GetChunkForEdit", mock.Anything, "chunk-1").Return(target, nil).Once()
	mockDB.On("GetCorrectionOverlay", mock.Anything, "t1").
		Return([]db.CorrectionRow{}, time.Time{}, nil).Once()

	h := NewToolHandlers(mockDB, nil)
	res, err := h.handleCreateCorrection(context.Background(), req("create_transcript_correction", map[string]interface{}{
		"chunk_id": "chunk-1", "original_text": "does not occur here", "correction": "wolf",
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	mockDB.AssertNotCalled(t, "InsertManualCorrection", mock.Anything, mock.Anything)
}

// TestHandleCreateCorrection_EmptyCorrection asserts an empty/whitespace
// correction is refused BEFORE the chunk is even read.
func TestHandleCreateCorrection_EmptyCorrection(t *testing.T) {
	mockDB := &MockDBInterface{}

	h := NewToolHandlers(mockDB, nil)
	res, err := h.handleCreateCorrection(context.Background(), req("create_transcript_correction", map[string]interface{}{
		"chunk_id": "chunk-1", "original_text": "auto sebo", "correction": "   ",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content[0].(*mcp.TextContent).Text, "empty")
	mockDB.AssertNotCalled(t, "GetChunkForEdit", mock.Anything, mock.Anything)
	mockDB.AssertNotCalled(t, "InsertManualCorrection", mock.Anything, mock.Anything)
}

// TestHandleCreateCorrection_UnknownChunk asserts db.ErrChunkNotFound produces
// a helpful error result and never writes.
func TestHandleCreateCorrection_UnknownChunk(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockDB.On("GetChunkForEdit", mock.Anything, "missing").Return(nil, db.ErrChunkNotFound).Once()

	h := NewToolHandlers(mockDB, nil)
	res, err := h.handleCreateCorrection(context.Background(), req("create_transcript_correction", map[string]interface{}{
		"chunk_id": "missing", "original_text": "auto sebo", "correction": "Arecibo",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content[0].(*mcp.TextContent).Text, "No chunk with id missing")
	mockDB.AssertNotCalled(t, "GetCorrectionOverlay", mock.Anything, mock.Anything)
	mockDB.AssertNotCalled(t, "InsertManualCorrection", mock.Anything, mock.Anything)
	mockDB.AssertExpectations(t)
}

// TestNormalizeIssueType asserts an issue_type outside the closed vocabulary
// is coerced to "other", while a recognized one is preserved verbatim.
func TestNormalizeIssueType(t *testing.T) {
	for _, valid := range []string{
		"misheard_proper_noun", "misheard_word", "repeated_text",
		"number_artifact", "homophone", "dropped_word", "other",
	} {
		assert.Equal(t, valid, normalizeIssueType(valid), "valid issue type must be preserved")
	}
	assert.Equal(t, "other", normalizeIssueType("something_made_up"))
	assert.Equal(t, "other", normalizeIssueType(""))
	// Case-insensitive.
	assert.Equal(t, "homophone", normalizeIssueType("HOMOPHONE"))
}

// ─── attributeDecision ─────────────────────────────────────────────────────

func TestAttributeDecision(t *testing.T) {
	assert.Equal(t, "mcp:agent", attributeDecision(""))
	assert.Equal(t, "mcp:agent", attributeDecision("   "))
	assert.Equal(t, "mcp:justin", attributeDecision("justin"))
	// Already-prefixed values are not double-prefixed.
	assert.Equal(t, "mcp:justin", attributeDecision("mcp:justin"))

	long := strings.Repeat("x", maxDecidedByRunes+50)
	got := attributeDecision(long)
	assert.LessOrEqual(t, len([]rune(got)), maxDecidedByRunes)
}
