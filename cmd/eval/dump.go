package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"

	evalpkg "github.com/jedwards1230/earmark/internal/eval"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// dumpLine is one --dump JSONL record: one judged chunk, what the call cost,
// and every finding the judge returned — kept, or dropped with its reason.
// Old and new prompts run with the same --seed produce lines for the same
// chunk ids, so the two files join on chunk_id (CONTRACT §2.15 "Measuring a
// prompt change").
type dumpLine struct {
	ChunkID       string `json:"chunk_id"`
	TranscriptID  string `json:"transcript_id"`
	FilePath      string `json:"file_path"`
	ChunkIndex    int    `json:"chunk_index"`
	TextChars     int    `json:"text_chars"`
	PromptVersion string `json:"prompt_version"`
	PromptSHA256  string `json:"prompt_sha256"`
	StepVersion   int    `json:"step_version"`
	// BookContextSource is asin_record (the prompt carried the book's
	// catalogue record), path_only, or record_error (the book's record could
	// not be used; the chunk carries the error and was not judged); BookContextSHA256 is the sha256 of the
	// rendered record block ("" for path_only). CONTRACT §2.15 "Book context".
	BookContextSource string `json:"book_context_source"`
	BookContextSHA256 string `json:"book_context_sha256,omitempty"`
	Model             string `json:"model"`
	ResolvedModel     string `json:"resolved_model,omitempty"`
	// Token counts and cost are null when the endpoint did not report them.
	PromptTokens     *int     `json:"prompt_tokens"`
	CompletionTokens *int     `json:"completion_tokens"`
	CostUSD          *float64 `json:"cost_usd"`
	LatencyMS        int64    `json:"latency_ms"`
	// Cached is true when the reply was served from fn_calls (no call, no
	// cost); FnCallID is the fn_calls row that produced it (CONTRACT §2.15
	// "Judge calls").
	Cached   bool   `json:"cached,omitempty"`
	FnCallID *int64 `json:"fn_call_id,omitempty"`
	// Error is set when the chunk was skipped (the call failed or the reply
	// was unusable); Findings is then empty.
	Error    string        `json:"error,omitempty"`
	Findings []dumpFinding `json:"findings"`
	Dropped  []dumpFinding `json:"dropped,omitempty"`
}

type dumpFinding struct {
	Original   string  `json:"original"`
	Correction string  `json:"correction"`
	IssueType  string  `json:"issue_type"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason,omitempty"`
}

// maxDumpErrorRunes bounds a dumped error message: an upstream error body can
// be long (and can echo the request).
const maxDumpErrorRunes = 300

// dumper writes one dumpLine per judged chunk. The first write error is kept
// and every later write is skipped; err reports it.
type dumper struct {
	enc   *json.Encoder
	model string
	rec   recipe.Recipe
	err   error
}

func newDumper(w io.Writer, model string, rec recipe.Recipe) *dumper {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &dumper{enc: enc, model: model, rec: rec}
}

// observe is the evalpkg.RunOptions.Observe hook.
func (d *dumper) observe(res evalpkg.Result, jerr error) {
	if d.err != nil {
		return
	}
	c := res.Chunk
	line := dumpLine{
		ChunkID:           c.ChunkID,
		TranscriptID:      c.TranscriptID,
		FilePath:          c.FilePath,
		ChunkIndex:        c.ChunkIndex,
		TextChars:         utf8.RuneCountInString(c.Text),
		PromptVersion:     d.rec.PromptVersion,
		BookContextSource: res.BookContext.Source,
		BookContextSHA256: res.BookContext.SHA256,
		PromptSHA256:      d.rec.PromptSHA256,
		StepVersion:       d.rec.StepVersion,
		Model:             d.model,
		ResolvedModel:     res.ResolvedModel,
		LatencyMS:         res.Elapsed.Milliseconds(),
		Cached:            res.Cached,
		FnCallID:          res.FnCallID,
		Findings:          []dumpFinding{},
	}
	if res.PromptVersion != "" {
		line.PromptVersion = res.PromptVersion
	}
	if line.BookContextSource == "" {
		line.BookContextSource = evalpkg.BookContextPathOnly
	}
	if u := res.Usage; u.HasUsage {
		in, out := u.InputTokens, u.OutputTokens
		line.PromptTokens, line.CompletionTokens = &in, &out
	}
	if u := res.Usage; u.HasCost {
		cost := u.CostUSD
		line.CostUSD = &cost
	}
	if jerr != nil {
		line.Error = truncate(jerr.Error(), maxDumpErrorRunes)
	}
	for _, f := range res.Findings {
		corr := ""
		if f.SuggestedCorrection != nil {
			corr = *f.SuggestedCorrection
		}
		line.Findings = append(line.Findings, dumpFinding{
			Original: f.OriginalText, Correction: corr, IssueType: f.IssueType, Confidence: f.Confidence,
		})
	}
	for _, f := range res.Dropped {
		line.Dropped = append(line.Dropped, dumpFinding{
			Original: f.OriginalText, Correction: f.SuggestedCorrection, IssueType: f.IssueType,
			Confidence: f.Confidence, Reason: f.Reason,
		})
	}
	if err := d.enc.Encode(line); err != nil {
		d.err = fmt.Errorf("write dump: %w", err)
	}
}
