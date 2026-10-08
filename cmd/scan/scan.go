// Package scan implements `earmark scan`: the chunk quality scan (CONTRACT
// §1.9 "Chunk scan"). Each chunk's pristine text is put to System One through
// the scan_chunk pure function (internal/scan); every call is logged and
// cached in fn_calls. Dry-run unless --yes: without it nothing is written to
// chunk_scan (the model calls are still made and cached, so a following --yes
// run over the same chunks is served from fn_calls and costs nothing).
package scan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/fn"
	"github.com/jedwards1230/earmark/internal/genai"
	scanpkg "github.com/jedwards1230/earmark/internal/scan"
	"github.com/jedwards1230/earmark/internal/telemetry"
)

// Store is the slice of the DB the command needs (*db.DB implements it).
type Store interface {
	fn.Store
	ScanCandidates(ctx context.Context, scope db.ScanScope) ([]db.ScanCandidate, error)
	InsertChunkScan(ctx context.Context, s db.ChunkScan) (db.ChunkScanOutcome, error)
}

type options struct {
	sample      int
	seed        string
	book        string
	concurrency int
	context     int
	limit       int
	json        bool
	yes         bool
}

var opts options

// ScanCmd is `earmark scan`.
var ScanCmd = &cobra.Command{
	Use:   "scan [flags]",
	Short: "Score chunk transcription quality with System One (dry-run unless --yes)",
	Long: `scan asks System One six questions about each chunk's pristine text —
does it need a fix, a 1..5 quality score, is it boilerplate, garbled or
dialogue, and which issue type dominates — through the scan_chunk pure function.
One to two neighbouring transcript segments on each side are shown as context
the model is told not to judge. Requires AI_ROLES.scan bound to a systemone
endpoint (CONTRACT §2.14).

Every model call is logged in fn_calls and cached there: the same chunk text
with the same context is asked once per prompt and model. Without --yes nothing
is written to chunk_scan, but the calls are still made (and paid for) and
cached, so the --yes run that follows is served from the cache. With --yes each
result is one short insert that re-checks the chunk's text hash first: a chunk
rebuilt since it was read gets no row, and an existing row is left alone.
Chunks whose current text already has a scan by this recipe are skipped.

The scan reads transcripts and writes only chunk_scan (and fn_calls). It does
not take the GPU phase gate: System One is a hosted model.

Examples:
  earmark scan --sample 50                  # preview 50 random chunks
  earmark scan --sample 50 --seed q4 --yes  # the same 50 chunks, written
  earmark scan --book "Children of Dune" --yes
  earmark scan --limit 2000 --yes --json    # first 2000 unscanned chunks`,
	Run: runScan,
}

func init() {
	f := ScanCmd.Flags()
	f.IntVar(&opts.sample, "sample", 0, "scan N chunks chosen by --seed (0 = every unscanned chunk in scope)")
	f.StringVar(&opts.seed, "seed", "earmark", "sample seed: the same seed picks the same chunks")
	f.StringVar(&opts.book, "book", "", "only chunks whose file path contains this (case-insensitive)")
	f.IntVar(&opts.concurrency, "concurrency", 8, "concurrent System One calls")
	f.IntVar(&opts.context, "context", db.DefaultScanContextSegments, "neighbouring segments shown per side (part of the recipe)")
	f.IntVar(&opts.limit, "limit", 0, "without --sample, scan at most N chunks (0 = all)")
	f.BoolVar(&opts.json, "json", false, "print the report as JSON")
	f.BoolVar(&opts.yes, "yes", false, "write results to chunk_scan (otherwise dry-run)")
}

func runScan(_ *cobra.Command, _ []string) {
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	if err := validate(opts); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}
	f, client, ok, err := scanpkg.FnFromConfig(cfg, opts.context)
	if !ok {
		fmt.Fprintln(os.Stderr, "Error: earmark scan needs AI_ROLES.scan bound to a systemone endpoint in AI_ENDPOINTS (CONTRACT §2.14)")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: scan endpoint: %v\n", err)
		os.Exit(2)
	}
	database, err := db.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer database.Close()

	tel, terr := telemetry.Setup(context.Background())
	if terr != nil {
		log.Printf("WARNING: OpenTelemetry disabled: %v", terr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err = run(ctx, os.Stdout, database, scanpkg.Scanner{Fn: f, Client: client, Store: database}, opts)
	stop()
	flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if serr := tel.Shutdown(flushCtx); serr != nil {
		log.Printf("OpenTelemetry shutdown: %v", serr)
	}
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func validate(o options) error {
	switch {
	case o.sample < 0:
		return errors.New("--sample must be >= 0")
	case o.limit < 0:
		return errors.New("--limit must be >= 0")
	case o.concurrency < 1 || o.concurrency > 64:
		return errors.New("--concurrency must be 1..64")
	case o.context < 0 || o.context > db.MaxScanContextSegments:
		return fmt.Errorf("--context must be 0..%d", db.MaxScanContextSegments)
	}
	return nil
}

// Report is what a run did (the --json output).
type Report struct {
	DryRun   bool   `json:"dry_run"`
	RecipeID string `json:"recipe_id"`
	Model    string `json:"model"`
	// Chunks counts candidates visited; Scanned those with a usable answer.
	Chunks   int `json:"chunks"`
	Scanned  int `json:"scanned"`
	Cached   int `json:"cached"`
	Fallback int `json:"fallback"`
	// Errors counts failed calls by bounded class (fn_calls.error_class).
	Errors map[string]int `json:"errors"`
	// Write outcomes (--yes only).
	Written int `json:"written"`
	Exists  int `json:"exists"`
	Changed int `json:"changed"`
	// OtherRecipe counts usable answers whose recipe is not RecipeID (a
	// cache hit served from a row an older, differently configured run wrote).
	// A route prefix or case difference on the model id is not one: it maps
	// to the expected model's recipe (fn.Recipe, genai.SameModel).
	OtherRecipe int `json:"other_recipe"`

	CostUSD     float64        `json:"cost_usd"`
	MeanQuality *float64       `json:"mean_quality,omitempty"` // 1..5 over scanned chunks
	NeedsFix    int            `json:"needs_fix"`              // p_needs_fix > 0.5
	Boilerplate int            `json:"boilerplate"`            // p_boilerplate > BoilerplateCut
	Garbled     int            `json:"garbled"`                // p_garbled > 0.5
	Dialogue    int            `json:"dialogue"`               // p_dialogue > 0.5
	QualityHist map[string]int `json:"quality_histogram"`      // rounded quality 1..5
	IssueTypes  map[string]int `json:"issue_types"`
}

type outcome struct {
	cand   db.ScanCandidate
	result scanpkg.Result
	meta   fn.Meta
	err    error
}

// run is the testable core: select, scan concurrently, optionally write,
// report. Errors on individual chunks are counted, not fatal; a store error
// (candidate query or insert) stops the run.
func run(ctx context.Context, out io.Writer, store Store, sc scanpkg.Scanner, o options) error {
	if err := validate(o); err != nil {
		return err
	}
	recipeID, err := store.RegisterRecipe(ctx, sc.Fn.Recipe(""))
	if err != nil {
		return fmt.Errorf("register scan recipe: %w", err)
	}
	rep := &Report{
		DryRun: !o.yes, RecipeID: recipeID, Model: sc.Fn.ModelAlias,
		Errors: map[string]int{}, QualityHist: map[string]int{}, IssueTypes: map[string]int{},
	}
	var qualitySum float64

	scope := db.ScanScope{
		RecipeID: recipeID, Book: o.book, Sample: o.sample, Seed: o.seed,
		ContextSegments: o.context,
	}
	remaining := o.limit
	var runErr error
	for {
		if o.sample == 0 && o.limit > 0 {
			scope.Limit = min(remaining, 500)
		}
		cands, err := store.ScanCandidates(ctx, scope)
		if err != nil {
			runErr = err
			break
		}
		if len(cands) == 0 {
			break
		}
		for _, oc := range scanAll(ctx, sc, cands, o.concurrency) {
			if err := tally(ctx, store, rep, &qualitySum, oc, o.yes); err != nil {
				runErr = err
				break
			}
		}
		if runErr != nil || ctx.Err() != nil || o.sample > 0 {
			break
		}
		scope.After = cands[len(cands)-1].Cursor()
		if o.limit > 0 {
			if remaining -= len(cands); remaining <= 0 {
				break
			}
		}
	}
	if rep.Scanned > 0 {
		m := qualitySum / float64(rep.Scanned)
		rep.MeanQuality = &m
	}
	if runErr == nil {
		runErr = ctx.Err()
	}
	if perr := printReport(out, rep, o.json); perr != nil && runErr == nil {
		runErr = perr
	}
	return runErr
}

// scanAll scans cands with up to n calls in flight, returning outcomes in
// candidate order. A cancelled context stops new calls.
func scanAll(ctx context.Context, sc scanpkg.Scanner, cands []db.ScanCandidate, n int) []outcome {
	outs := make([]outcome, len(cands))
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for i, c := range cands {
		if ctx.Err() != nil {
			outs[i] = outcome{cand: c, err: ctx.Err()}
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, c db.ScanCandidate) {
			defer wg.Done()
			defer func() { <-sem }()
			r, meta, err := sc.Scan(ctx, c)
			outs[i] = outcome{cand: c, result: r, meta: meta, err: err}
		}(i, c)
	}
	wg.Wait()
	return outs
}

// tally records one outcome and, with write, stores it. Only a store error is
// returned.
func tally(ctx context.Context, store Store, rep *Report, qualitySum *float64, oc outcome, write bool) error {
	rep.Chunks++
	if oc.err != nil {
		rep.Errors[genai.ErrorClass(oc.err)]++
		return nil
	}
	if oc.meta.Fallback {
		// Another model answered: its result carries another recipe, so it is
		// not this scan's answer. Logged in fn_calls; not counted or written.
		rep.Fallback++
		return nil
	}
	rep.Scanned++
	if oc.meta.CacheHit {
		rep.Cached++
	}
	if oc.meta.RecipeID != rep.RecipeID {
		rep.OtherRecipe++
	}
	r := oc.result
	rep.CostUSD += r.CostUSD
	*qualitySum += r.Quality
	rep.QualityHist[fmt.Sprintf("%.0f", r.Quality)]++
	rep.IssueTypes[r.IssueType]++
	if r.PNeedsFix > 0.5 {
		rep.NeedsFix++
	}
	if r.PBoilerplate > scanpkg.BoilerplateCut {
		rep.Boilerplate++
	}
	if r.PGarbled > 0.5 {
		rep.Garbled++
	}
	if r.PDialogue > 0.5 {
		rep.Dialogue++
	}
	if !write {
		return nil
	}
	res, err := store.InsertChunkScan(ctx, r.Row(oc.cand, oc.meta))
	if err != nil {
		return err
	}
	switch res {
	case db.ChunkScanInserted:
		rep.Written++
	case db.ChunkScanExists:
		rep.Exists++
	case db.ChunkScanChanged:
		rep.Changed++
	}
	return nil
}

func printReport(out io.Writer, rep *Report, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }
	if rep.Chunks == 0 {
		p("No unscanned chunks in scope (recipe %s).\n", short(rep.RecipeID))
		return nil
	}
	p("scan recipe %s · model %s\n\n", short(rep.RecipeID), rep.Model)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	_, _ = fmt.Fprintln(tw, "chunks\tscanned\tcached\tfallback\terrors\t")
	_, _ = fmt.Fprintf(tw, "%d\t%d\t%d\t%d\t%d\t\n", rep.Chunks, rep.Scanned, rep.Cached, rep.Fallback, sum(rep.Errors))
	_ = tw.Flush()
	if rep.MeanQuality != nil {
		p("\nmean quality %.2f / 5 · needs fix %d · boilerplate %d · garbled %d · dialogue %d\n",
			*rep.MeanQuality, rep.NeedsFix, rep.Boilerplate, rep.Garbled, rep.Dialogue)
		p("quality 1..5: %s\n", hist(rep.QualityHist, []string{"1", "2", "3", "4", "5"}))
		p("issue types:  %s\n", hist(rep.IssueTypes, scanpkg.IssueTypes))
	}
	p("cost:         $%.6f (cached calls are free)\n", rep.CostUSD)
	if len(rep.Errors) > 0 {
		p("errors:       %s\n", hist(rep.Errors, sortedKeys(rep.Errors)))
	}
	if rep.OtherRecipe > 0 {
		p("WARNING: %d answers came back under another recipe than %s (served from a call made under\n"+
			"         other settings); they are written under that recipe, which is not the current one.\n",
			rep.OtherRecipe, short(rep.RecipeID))
	}
	if rep.DryRun {
		p("\n(dry-run) nothing written to chunk_scan; answers are cached in fn_calls — pass --yes to write them.\n")
		return nil
	}
	p("\nWrote %d, already present %d, skipped %d (chunk text changed since it was read; re-run to rescan).\n",
		rep.Written, rep.Exists, rep.Changed)
	return nil
}

func hist(m map[string]int, keys []string) string {
	s := ""
	for _, k := range keys {
		if s != "" {
			s += " · "
		}
		s += fmt.Sprintf("%s %d", k, m[k])
	}
	return s
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
