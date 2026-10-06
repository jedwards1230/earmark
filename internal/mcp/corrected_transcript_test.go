package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jedwards1230/earmark/internal/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// correctedTrack is a two-segment track whose ASR text says "ganema"; its
// chunk projection (the text search returns) has the reviewed correction.
func correctedTrack() *db.TrackDetail {
	return &db.TrackDetail{
		ID: "job-c", FilePath: "/books/Frank Herbert/Children of Dune/01.m4b", Status: "done",
		HasTranscript: true, Language: "en", ModelName: "parakeet", DurationSeconds: 90,
		Segments: []db.Segment{
			{ID: 0, Start: 0, End: 4, Text: "ganema said", Words: []db.Word{{Word: "ganema", Start: 0, End: 1}}},
			{ID: 1, Start: 4, End: 8, Text: "nothing else"},
		},
	}
}

func correctedPage() *db.CorrectedTranscriptPage {
	return &db.CorrectedTranscriptPage{
		TotalChunks: 3, CorrectedChunks: 1,
		Chunks: []db.CorrectedChunk{
			{ID: "c0", ChunkIndex: 0, StartSec: 0, EndSec: 30, Text: "Ghanima said", Corrected: true},
			{ID: "c1", ChunkIndex: 1, StartSec: 30, EndSec: 60, Text: "nothing else", Corrected: false},
		},
	}
}

// TestGetTranscriptServesCorrectedText: a track with corrections is served as
// the corrected chunk projection — corrected=true, unit=chunk, chunk time
// ranges, no segments and no word timestamps — paged by chunk with the default
// chunk page size.
func TestGetTranscriptServesCorrectedText(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockDB.On("GetTrackDetail", mock.Anything, "job-c").Return(correctedTrack(), nil).Once()
	mockDB.On("GetCorrectedTranscriptPage", mock.Anything, "job-c", 0, defaultTranscriptChunkLimit).
		Return(correctedPage(), nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleGetTranscript(context.Background(), req("get_transcript", map[string]interface{}{
		"trackID": "job-c",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	out, ok := res.StructuredContent.(TranscriptOutput)
	require.True(t, ok)
	assert.True(t, out.Corrected)
	assert.Equal(t, "chunk", out.Unit)
	assert.Empty(t, out.Segments, "corrected mode serves chunks, not ASR segments")
	require.Len(t, out.Chunks, 2)
	assert.Equal(t, TranscriptChunk{ChunkID: "c0", ChunkIndex: 0, Start: 0, End: 30, Text: "Ghanima said", Corrected: true}, out.Chunks[0])
	assert.Equal(t, 3, out.TotalChunks)
	assert.Equal(t, 1, out.CorrectedChunks)
	require.NotNil(t, out.NextOffset)
	assert.Equal(t, 2, *out.NextOffset)
	assert.Contains(t, out.Note, "CORRECTED")

	raw, err := json.Marshal(out)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"words"`, "corrected mode has no word timestamps")
	assert.Contains(t, string(raw), `"corrected":true`)

	text := res.Content[0].(*mcp.TextContent).Text
	assert.Contains(t, text, "Text: CORRECTED")
	assert.Contains(t, text, "[00:00 → 00:30] (corrected) Ghanima said")
	assert.NotContains(t, text, "ganema", "the ASR misrecognition must not be served")
	assert.Contains(t, text, "Showing chunks 1–2 of 3. Next page: offset=2.")
	mockDB.AssertExpectations(t)
}

// TestGetTranscriptUncorrectedServesSegments: with no corrections the ASR
// segments are served, explicitly labelled corrected=false / unit=segment.
func TestGetTranscriptUncorrectedServesSegments(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockDB.On("GetTrackDetail", mock.Anything, "job-c").Return(correctedTrack(), nil).Once()
	mockDB.On("GetCorrectedTranscriptPage", mock.Anything, "job-c", 0, defaultTranscriptChunkLimit).
		Return(&db.CorrectedTranscriptPage{TotalChunks: 3}, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleGetTranscript(context.Background(), req("get_transcript", map[string]interface{}{
		"trackID": "job-c",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)
	out := res.StructuredContent.(TranscriptOutput)
	assert.False(t, out.Corrected)
	assert.Equal(t, "segment", out.Unit)
	assert.Len(t, out.Segments, 2)
	assert.Empty(t, out.Chunks)
	assert.Empty(t, out.Note)
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"corrected":false`, "the field is always present")
	assert.Contains(t, res.Content[0].(*mcp.TextContent).Text, "Text: ASR segments (no corrections on this track).")
	mockDB.AssertExpectations(t)
}

// TestGetTranscriptWordsWithCorrectionsFallsBackToASR: word timestamps exist
// only for the ASR record, so includeWordTimestamps=true serves the segments —
// but only asks for counts (limit 0) and says, in note, that corrections exist.
func TestGetTranscriptWordsWithCorrectionsFallsBackToASR(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockDB.On("GetTrackDetail", mock.Anything, "job-c").Return(correctedTrack(), nil).Once()
	mockDB.On("GetCorrectedTranscriptPage", mock.Anything, "job-c", 0, 0).
		Return(&db.CorrectedTranscriptPage{TotalChunks: 3, CorrectedChunks: 1}, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleGetTranscript(context.Background(), req("get_transcript", map[string]interface{}{
		"trackID": "job-c", "includeWordTimestamps": true,
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)
	out := res.StructuredContent.(TranscriptOutput)
	assert.False(t, out.Corrected)
	assert.Equal(t, "segment", out.Unit)
	require.Len(t, out.Segments, 2)
	assert.NotEmpty(t, out.Segments[0].Words)
	assert.Equal(t, 1, out.CorrectedChunks)
	assert.Contains(t, out.Note, "UNCORRECTED")
	assert.Contains(t, res.Content[0].(*mcp.TextContent).Text, "UNCORRECTED: 1 of 3 chunks")
	mockDB.AssertExpectations(t)
}

// TestGetTranscriptCorrectedPaging: offset/limit page chunks in corrected
// mode, and a large limit is capped to the chunk maximum.
func TestGetTranscriptCorrectedPaging(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockDB.On("GetTrackDetail", mock.Anything, "job-c").Return(correctedTrack(), nil).Once()
	mockDB.On("GetCorrectedTranscriptPage", mock.Anything, "job-c", 2, maxTranscriptChunkLimit).
		Return(&db.CorrectedTranscriptPage{TotalChunks: 3, CorrectedChunks: 1, Chunks: []db.CorrectedChunk{
			{ID: "c2", ChunkIndex: 2, StartSec: 60, EndSec: 90, Text: "the end"},
		}}, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleGetTranscript(context.Background(), req("get_transcript", map[string]interface{}{
		"trackID": "job-c", "offset": 2.0, "limit": 500.0,
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)
	out := res.StructuredContent.(TranscriptOutput)
	assert.True(t, out.Corrected)
	assert.Nil(t, out.NextOffset)
	assert.Contains(t, res.Content[0].(*mcp.TextContent).Text, "Showing chunks 3–3 of 3 (end of transcript).")
	mockDB.AssertExpectations(t)
}

// TestGetTranscriptCorrectedReadFailureFailsClosed: if the corrected text
// cannot be read the call errors instead of serving ASR text as though the
// track had no corrections.
func TestGetTranscriptCorrectedReadFailureFailsClosed(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockDB.On("GetTrackDetail", mock.Anything, "job-c").Return(correctedTrack(), nil).Once()
	mockDB.On("GetCorrectedTranscriptPage", mock.Anything, "job-c", 0, defaultTranscriptChunkLimit).
		Return(nil, errors.New("db down")).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleGetTranscript(context.Background(), req("get_transcript", map[string]interface{}{
		"trackID": "job-c",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content[0].(*mcp.TextContent).Text, "corrected text")
	mockDB.AssertExpectations(t)
}

func TestClampChunkLimit(t *testing.T) {
	for in, want := range map[int]int{0: 10, -3: 10, 1: 1, 25: 25, 26: 25, 1000: 25} {
		assert.Equal(t, want, clampChunkLimit(in), "clampChunkLimit(%d)", in)
	}
}
