package db

// Postgres integration tests for provenance recipes (CONTRACT §1.9): the
// 00002 legacy backfill, stamping on write, and the stale_work view. Skipped
// unless EARMARK_TEST_DATABASE_URL is set (see migrate_integration_test.go).

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvector "github.com/pgvector/pgvector-go/pgx"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/openai"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// seedLegacyRows writes v0.40-shaped data into a legacy (pre-goose) database:
// two transcripts, chunks for both (only the first job has an embed model in
// run_metrics), and findings from three judge eras plus one human correction.
func seedLegacyRows(t *testing.T, dbURL string) {
	t.Helper()
	_, err := connect(t, dbURL).Exec(context.Background(), `
		INSERT INTO transcription_jobs (id, file_path, checksum, status) VALUES
		  ('00000000-0000-0000-0000-00000000000a', '/b/A/01.m4b', 'c1', 'done'),
		  ('00000000-0000-0000-0000-00000000000b', '/b/B/01.m4b', 'c2', 'done');
		INSERT INTO run_metrics (job_id, embed_model) VALUES
		  ('00000000-0000-0000-0000-00000000000a', 'nomic-embed-text');
		INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds,
		                         segments, raw_text, model_name) VALUES
		  ('00000000-0000-0000-0000-0000000000a1', '00000000-0000-0000-0000-00000000000a',
		   '/b/A/01.m4b', 'c1', 'en', 60, '[]', 'ganema said', 'nvidia/parakeet-tdt-1.1b'),
		  ('00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-00000000000b',
		   '/b/B/01.m4b', 'c2', 'en', 60, '[]', 'the the cat', 'nvidia/parakeet-tdt-1.1b');
		INSERT INTO transcript_chunks (transcript_id, file_path, chunk_index, start_sec, end_sec, text, embedding) VALUES
		  ('00000000-0000-0000-0000-0000000000a1', '/b/A/01.m4b', 0, 0, 30, 'ganema', array_fill(0.1, ARRAY[768])::vector),
		  ('00000000-0000-0000-0000-0000000000a1', '/b/A/01.m4b', 1, 30, 60, 'said',  array_fill(0.2, ARRAY[768])::vector),
		  ('00000000-0000-0000-0000-0000000000b1', '/b/B/01.m4b', 0, 0, 60, 'the the cat', array_fill(0.3, ARRAY[768])::vector);
		INSERT INTO transcript_findings (transcript_id, file_path, start_sec, end_sec, original_text,
		                                 issue_type, confidence, model, resolved_model, origin) VALUES
		  ('00000000-0000-0000-0000-0000000000a1', '/b/A/01.m4b', 0, 30, 'ganema', 'misheard_proper_noun', 0.9, 'gemma3:12b', NULL, 'judge'),
		  ('00000000-0000-0000-0000-0000000000a1', '/b/A/01.m4b', 0, 30, 'ganema', 'misheard_proper_noun', 0.9, 'gemma3:12b', NULL, 'judge'),
		  ('00000000-0000-0000-0000-0000000000a1', '/b/A/01.m4b', 0, 30, 'ganema', 'misheard_proper_noun', 0.8, 'qwen3.8', NULL, 'judge'),
		  ('00000000-0000-0000-0000-0000000000b1', '/b/B/01.m4b', 0, 60, 'the the', 'repeated_text', 0.9,
		   'anthropic/claude-haiku-4-5-20251001', 'anthropic/claude-haiku-4-5-20251001', 'judge'),
		  ('00000000-0000-0000-0000-0000000000b1', '/b/B/01.m4b', 0, 60, 'cat', 'misheard_word', 1, 'human', NULL, 'human');
	`)
	if err != nil {
		t.Fatalf("seed legacy rows: %v", err)
	}
}

func legacyID(t *testing.T, step, alias, resolved string) string {
	t.Helper()
	id, err := recipe.Recipe{
		Step: step, CodeVersion: recipe.LegacyCodeVersion, ModelAlias: alias, ModelResolved: resolved,
	}.ID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func queryStrings(t *testing.T, conn *pgx.Conn, sql string, args ...any) []string {
	t.Helper()
	rows, err := conn.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestIntegrationLegacyRecipeBackfill: existing rows get one legacy recipe per
// (step, model), with IDs the Go canonical form reproduces exactly; human
// corrections get none.
func TestIntegrationLegacyRecipeBackfill(t *testing.T) {
	dbURL := newTestDatabase(t)
	applyLegacyInitialize(t, dbURL)
	seedLegacyRows(t, dbURL)
	if err := migrate(itCtx(t), dbURL, testLog()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	conn := connect(t, dbURL)

	asr := legacyID(t, recipe.StepASR, "nvidia/parakeet-tdt-1.1b", "")
	gemma := legacyID(t, recipe.StepPropose, "gemma3:12b", "")
	qwen := legacyID(t, recipe.StepPropose, "qwen3.8", "")
	haiku := legacyID(t, recipe.StepPropose, "anthropic/claude-haiku-4-5-20251001", "anthropic/claude-haiku-4-5-20251001")
	nomic := legacyID(t, recipe.StepEmbed, "nomic-embed-text", "")
	unknownEmbed := legacyID(t, recipe.StepEmbed, "", "")

	got := queryStrings(t, conn, `
		SELECT step || ' ' || recipe_id || ' ' || coalesce(model_alias, '-') || ' ' || coalesce(model_resolved, '-')
		  FROM recipes
		 WHERE step_version = 0 AND code_version = 'legacy-unknown' AND params = '{}'::jsonb
		   AND model_revision IS NULL AND prompt_version IS NULL AND prompt_sha256 IS NULL
		 ORDER BY 1`)
	want := []string{
		"asr " + asr + " nvidia/parakeet-tdt-1.1b -",
		"embed " + nomic + " nomic-embed-text -",
		"embed " + unknownEmbed + " - -",
		"propose " + haiku + " anthropic/claude-haiku-4-5-20251001 anthropic/claude-haiku-4-5-20251001",
		"propose " + gemma + " gemma3:12b -",
		"propose " + qwen + " qwen3.8 -",
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("legacy recipes:\n got %v\nwant %v", got, want)
	}
	var total int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM recipes`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != len(want) {
		t.Errorf("%d recipes, want exactly the %d legacy ones", total, len(want))
	}

	for _, c := range []struct{ sql, want string }{
		{`SELECT string_agg(DISTINCT recipe_id, ',') FROM transcripts`, asr},
		{`SELECT string_agg(DISTINCT recipe_id, ',') FROM transcript_findings WHERE model = 'gemma3:12b'`, gemma},
		{`SELECT string_agg(DISTINCT recipe_id, ',') FROM transcript_findings WHERE model = 'qwen3.8'`, qwen},
		{`SELECT string_agg(DISTINCT recipe_id, ',') FROM transcript_findings WHERE model LIKE 'anthropic/%'`, haiku},
		{`SELECT coalesce(string_agg(DISTINCT recipe_id, ','), 'NULL') FROM transcript_findings WHERE origin = 'human'`, "NULL"},
		{`SELECT string_agg(DISTINCT recipe_id, ',') FROM transcript_chunks WHERE file_path LIKE '/b/A/%'`, nomic},
		{`SELECT string_agg(DISTINCT recipe_id, ',') FROM transcript_chunks WHERE file_path LIKE '/b/B/%'`, unknownEmbed},
		{`SELECT count(*)::text FROM transcript_chunks WHERE recipe_id IS NULL`, "0"},
	} {
		var g string
		if err := conn.QueryRow(context.Background(), c.sql).Scan(&g); err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		if g != c.want {
			t.Errorf("%s = %s, want %s", c.sql, g, c.want)
		}
	}
}

// TestIntegrationLegacyBackfillWithoutRewrite pins the fast path production
// takes: when every row of a table maps to ONE legacy recipe (one ASR model,
// one embedding model), the column is stamped through a constant default that
// Postgres stores as the missing value — no table rewrite, no UPDATE, so no
// HNSW re-insertion of every vector. Measured at production size (4,276
// transcripts / 39,644 chunks / 32,337 findings): 0.9 s, versus 91 s for the
// UPDATE path. And the default is gone afterwards: new rows are NULL until a
// writer stamps them.
func TestIntegrationLegacyBackfillWithoutRewrite(t *testing.T) {
	dbURL := newTestDatabase(t)
	applyLegacyInitialize(t, dbURL)
	seedLegacyRows(t, dbURL)
	conn := connect(t, dbURL)
	ctx := context.Background()
	// Give job b an embed model too, so every chunk maps to one embed recipe.
	if _, err := conn.Exec(ctx, `INSERT INTO run_metrics (job_id, embed_model)
		VALUES ('00000000-0000-0000-0000-00000000000b', 'nomic-embed-text')`); err != nil {
		t.Fatal(err)
	}
	// relfilenode changes if the table is rewritten; a row's xmin changes if it
	// is UPDATEd (a new tuple version). The fast path does neither.
	physical := func() []string {
		return queryStrings(t, conn, `
			SELECT 'file ' || relname || ' ' || relfilenode FROM pg_class
			 WHERE relname IN ('transcripts', 'transcript_chunks')
			UNION ALL SELECT 'chunk ' || id || ' ' || xmin FROM transcript_chunks
			UNION ALL SELECT 'transcript ' || id || ' ' || xmin FROM transcripts
			ORDER BY 1`)
	}
	before := physical()

	if err := migrate(itCtx(t), dbURL, testLog()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if after := physical(); !slices.Equal(before, after) {
		t.Errorf("the backfill rewrote or updated rows it should only have defaulted:\nbefore %v\nafter  %v", before, after)
	}

	nomic := legacyID(t, recipe.StepEmbed, "nomic-embed-text", "")
	asr := legacyID(t, recipe.StepASR, "nvidia/parakeet-tdt-1.1b", "")
	var chunks, transcripts int
	if err := conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM transcript_chunks WHERE recipe_id = $1),
	                                     (SELECT count(*) FROM transcripts WHERE recipe_id = $2)`,
		nomic, asr).Scan(&chunks, &transcripts); err != nil {
		t.Fatal(err)
	}
	if chunks != 3 || transcripts != 2 {
		t.Errorf("stamped %d/3 chunks and %d/2 transcripts with their legacy recipe", chunks, transcripts)
	}

	// No lingering default: a row written now, by an unstamped writer, is NULL.
	var hasDefault bool
	if err := conn.QueryRow(ctx, `SELECT bool_or(column_default IS NOT NULL) FROM information_schema.columns
		WHERE column_name = 'recipe_id' AND table_name IN ('transcripts','transcript_chunks','transcript_findings')`).Scan(&hasDefault); err != nil {
		t.Fatal(err)
	}
	if hasDefault {
		t.Error("a recipe_id column kept the legacy default — new rows would be mis-stamped as legacy")
	}
	var newRecipe *string
	if err := conn.QueryRow(ctx, `
		INSERT INTO transcript_chunks (transcript_id, file_path, chunk_index, start_sec, end_sec, text, embedding)
		VALUES ('00000000-0000-0000-0000-0000000000b1', '/b/B/01.m4b', 7, 0, 1, 'new', array_fill(0.5, ARRAY[768])::vector)
		RETURNING recipe_id`).Scan(&newRecipe); err != nil {
		t.Fatal(err)
	}
	if newRecipe != nil {
		t.Errorf("a new chunk got recipe_id %s, want NULL", *newRecipe)
	}
}

// newIntegrationDB opens a *DB over a migrated database the way New does
// (pgvector types registered), with an embeddings role bound to nomic.
func newIntegrationDB(t *testing.T, dbURL string) *DB {
	t.Helper()
	if err := migrate(itCtx(t), dbURL, testLog()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	poolCfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvector.RegisterTypes(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	cfg := &config.Config{
		DatabaseURL: dbURL,
		ChunkSize:   512,
		AIEndpoints: []config.AIEndpoint{{ID: "e", Type: config.AIEndpointTypeEmbeddings, Model: "nomic-embed-text"}},
		AIRoles:     &config.AIRoles{Embeddings: "e"},
	}
	d := &DB{pool: pool, cfg: cfg, log: testLog(), e: openai.NewEmbeddings(cfg)}
	d.embedRecipe = d.EmbedRecipe()
	return d
}

func staleRows(t *testing.T, d *DB) []string {
	t.Helper()
	rows, err := d.pool.Query(context.Background(),
		`SELECT step || ':' || source_table || ':' || coalesce(recipe_id, 'NULL') FROM stale_work ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func vec(v float32) []float32 {
	out := make([]float32, openai.EmbeddingDimension)
	for i := range out {
		out[i] = v
	}
	return out
}

// TestIntegrationStampingAndStaleWork drives the write paths and the view:
// chunks and findings are stamped with registered recipes (a fallback model
// registered on the fly); stale_work lists exactly the rows whose recipe is
// not equivalent to the current one — ignoring code_version, never listing
// human corrections, and silent for steps with no current recipe.
func TestIntegrationStampingAndStaleWork(t *testing.T) {
	ctx := context.Background()
	d := newIntegrationDB(t, newTestDatabase(t))
	if _, err := d.pool.Exec(ctx, `
		INSERT INTO transcription_jobs (id, file_path, checksum, status)
		VALUES ('00000000-0000-0000-0000-00000000000a', '/b/A/01.m4b', 'c1', 'done');
		INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds,
		                         segments, raw_text, model_name)
		VALUES ('00000000-0000-0000-0000-0000000000a1', '00000000-0000-0000-0000-00000000000a',
		        '/b/A/01.m4b', 'c1', 'en', 60, '[]', 'x', 'nvidia/parakeet-tdt-1.1b');
	`); err != nil {
		t.Fatal(err)
	}
	const tid = "00000000-0000-0000-0000-0000000000a1"

	// Chunks are stamped with the embed recipe, registered in the same tx.
	if err := d.InsertChunks(ctx, []Chunk{
		{TranscriptID: tid, FilePath: "/b/A/01.m4b", ChunkIndex: 0, EndSec: 30, Text: "a", Embedding: vec(0.1)},
		{TranscriptID: tid, FilePath: "/b/A/01.m4b", ChunkIndex: 1, StartSec: 30, EndSec: 60, Text: "b", Embedding: vec(0.2)},
	}); err != nil {
		t.Fatalf("InsertChunks: %v", err)
	}
	embedID := mustID(t, d.EmbedRecipe())
	var stamped int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM transcript_chunks WHERE recipe_id = $1`, embedID).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if stamped != 2 {
		t.Errorf("%d chunks stamped with the embed recipe, want 2", stamped)
	}

	// Findings: one from the current judge recipe, one a fallback answered,
	// one human correction (no recipe).
	judge := recipe.Recipe{
		Step: recipe.StepPropose, StepVersion: 1, CodeVersion: "v1+a", ModelAlias: "earmark-judge",
		ModelResolved: "anthropic/claude-haiku-4-5-20251001", PromptVersion: "judge@v1", PromptSHA256: "abc",
	}
	fallback := judge
	fallback.ModelResolved = "gemini/gemini-2.5-flash"
	f := func(r *recipe.Recipe, text string) Finding {
		return Finding{TranscriptID: tid, FilePath: "/b/A/01.m4b", EndSec: 30, OriginalText: text,
			IssueType: "misheard_word", Confidence: 0.9, Model: "earmark-judge", Recipe: r}
	}
	if err := d.InsertFindings(ctx, []Finding{f(&judge, "current"), f(&fallback, "fallback")}); err != nil {
		t.Fatalf("InsertFindings: %v", err)
	}
	if _, err := d.pool.Exec(ctx, `
		INSERT INTO transcript_findings (transcript_id, file_path, start_sec, end_sec, original_text,
		                                 issue_type, confidence, model, origin)
		VALUES ($1, '/b/A/01.m4b', 0, 30, 'human', 'misheard_word', 1, 'human', 'human')`, tid); err != nil {
		t.Fatal(err)
	}
	var registered int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM recipes WHERE recipe_id = ANY($1)`,
		[]string{mustID(t, judge), mustID(t, fallback)}).Scan(&registered); err != nil {
		t.Fatal(err)
	}
	if registered != 2 {
		t.Errorf("%d of 2 judge recipes registered (the fallback must be registered on the fly)", registered)
	}

	// No current recipes yet: nothing is stale.
	if got := staleRows(t, d); len(got) != 0 {
		t.Errorf("stale_work with no current recipes = %v, want none", got)
	}

	// Current = what wrote the rows: only the fallback finding is stale.
	if err := d.SetCurrentRecipes(ctx, d.EmbedRecipe(), judge); err != nil {
		t.Fatal(err)
	}
	if got, want := staleRows(t, d), []string{"propose:transcript_findings:" + mustID(t, fallback)}; !slices.Equal(got, want) {
		t.Errorf("stale_work = %v, want %v", got, want)
	}

	// A new release that changes nothing else is not "stale".
	rebuilt := judge
	rebuilt.CodeVersion = "v2+b"
	if err := d.SetCurrentRecipes(ctx, rebuilt); err != nil {
		t.Fatal(err)
	}
	if got := staleRows(t, d); len(got) != 1 {
		t.Errorf("a code_version-only change marked rows stale: %v", got)
	}

	// A new embedding configuration makes every chunk stale, nothing else.
	newEmbed := d.EmbedRecipe()
	newEmbed.Params = map[string]any{"chunk_size": 1024}
	if err := d.SetCurrentRecipes(ctx, newEmbed); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"embed:transcript_chunks:" + embedID,
		"embed:transcript_chunks:" + embedID,
		"propose:transcript_findings:" + mustID(t, fallback),
	}
	if got := staleRows(t, d); !slices.Equal(got, want) {
		t.Errorf("stale_work after an embed change = %v, want %v", got, want)
	}

	// Re-embedding restamps: ON CONFLICT updates recipe_id with the embedding.
	d.embedRecipe = newEmbed
	if err := d.InsertChunks(ctx, []Chunk{
		{TranscriptID: tid, FilePath: "/b/A/01.m4b", ChunkIndex: 0, EndSec: 30, Text: "a", Embedding: vec(0.4)},
	}); err != nil {
		t.Fatal(err)
	}
	if got := staleRows(t, d); len(got) != 2 {
		t.Errorf("after re-embedding one chunk, stale_work = %v, want 2 rows", got)
	}

	// The current pointer moves only when the recipe does.
	var updates int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM current_recipes`).Scan(&updates); err != nil {
		t.Fatal(err)
	}
	if updates != 2 {
		t.Errorf("%d current recipes, want 2 (embed, propose)", updates)
	}
}

// TestIntegrationRecipesAreImmutable: re-registering an ID never rewrites it.
func TestIntegrationRecipesAreImmutable(t *testing.T) {
	ctx := context.Background()
	d := newIntegrationDB(t, newTestDatabase(t))
	r := d.EmbedRecipe()
	id, err := d.RegisterRecipe(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE recipes SET created_at = '2000-01-01' WHERE recipe_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RegisterRecipe(ctx, r); err != nil {
		t.Fatal(err)
	}
	var year int
	if err := d.pool.QueryRow(ctx, `SELECT extract(year FROM created_at)::int FROM recipes WHERE recipe_id = $1`, id).Scan(&year); err != nil {
		t.Fatal(err)
	}
	if year != 2000 {
		t.Error("re-registering a recipe rewrote it")
	}
}

func mustID(t *testing.T, r recipe.Recipe) string {
	t.Helper()
	id, err := r.ID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
