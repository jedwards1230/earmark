package db

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jedwards1230/earmark/internal/metaprovider"
)

// captureExecer records the SQL and bound arguments of each Exec call.
type captureExecer struct {
	sql  []string
	args [][]any
}

func (c *captureExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c.sql = append(c.sql, sql)
	c.args = append(c.args, args)
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

// TestUpsertBookMetadataSQL_RefreshesEnrichment pins the refresh semantics for
// the ABS enrichment columns: a non-NULL value on a re-lookup overwrites the
// stored one (EXCLUDED wins), while NULL keeps it (COALESCE) so a PathProvider
// call never wipes ABS data.
func TestUpsertBookMetadataSQL_RefreshesEnrichment(t *testing.T) {
	sql := strings.Join(strings.Fields(upsertBookMetadataSQL), " ")
	for _, c := range []string{"description", "genres", "isbn"} {
		want := c + " = CASE WHEN EXCLUDED.identity_status = 'conflict' THEN NULL ELSE COALESCE(EXCLUDED." + c + ", book_metadata." + c + ") END"
		if !strings.Contains(sql, want) {
			t.Errorf("upsertBookMetadataSQL must refresh %s via %q:\n%s", c, want, sql)
		}
	}
	if !strings.Contains(sql, "$10, $11, $12") {
		t.Errorf("upsertBookMetadataSQL must bind description/genres/isbn as $10-$12:\n%s", sql)
	}
}

// TestUpsertBookMetadata_BindsEnrichment drives the upsert through a capturing
// execer and asserts description/genres/isbn are bound at $10-$12, with empty
// values bound as NULL so a later sparse lookup cannot clobber them.
func TestUpsertBookMetadata_BindsEnrichment(t *testing.T) {
	t.Run("abs lookup with enrichment", func(t *testing.T) {
		ex := &captureExecer{}
		meta := metaprovider.BookMeta{
			Title: "Project Hail Mary", Author: "Andy Weir", Source: "abs",
			Description: "A lone astronaut.", Genres: []string{"Science Fiction", "Thriller"},
			ISBN: "9780593135204",
		}
		if err := upsertBookMetadata(context.Background(), ex, "/books/a/b", meta); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		args := ex.args[0]
		if len(args) != 14 {
			t.Fatalf("bound %d args, want 14", len(args))
		}
		if d, ok := args[9].(*string); !ok || d == nil || *d != "A lone astronaut." {
			t.Errorf("$10 description = %#v, want *string \"A lone astronaut.\"", args[9])
		}
		g, ok := args[10].([]string)
		if !ok || len(g) != 2 || g[0] != "Science Fiction" || g[1] != "Thriller" {
			t.Errorf("$11 genres = %#v, want []string{Science Fiction, Thriller}", args[10])
		}
		if i, ok := args[11].(*string); !ok || i == nil || *i != "9780593135204" {
			t.Errorf("$12 isbn = %#v, want *string 9780593135204", args[11])
		}
	})

	t.Run("path lookup binds NULLs", func(t *testing.T) {
		ex := &captureExecer{}
		meta := metaprovider.BookMeta{Title: "Book", Author: "Author", Source: "path"}
		if err := upsertBookMetadata(context.Background(), ex, "/books/a/b", meta); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		args := ex.args[0]
		if d, _ := args[9].(*string); d != nil {
			t.Errorf("$10 description = %v, want NULL", *d)
		}
		if args[10] != nil {
			t.Errorf("$11 genres = %#v, want untyped nil (NULL)", args[10])
		}
		if i, _ := args[11].(*string); i != nil {
			t.Errorf("$12 isbn = %v, want NULL", *i)
		}
	})
}

// TestUpsertBookMetadata_BindsIdentity pins the ASIN identity columns
// (migration 00006): asin_source and identity_status bind at $13/$14, are
// written as a pair when identity was resolved and kept otherwise, and bind
// NULL when unset. Behavior is proven against Postgres in
// TestIntegrationIdentityTransitions.
func TestUpsertBookMetadata_BindsIdentity(t *testing.T) {
	sql := strings.Join(strings.Fields(upsertBookMetadataSQL), " ")
	for _, want := range []string{
		"identity_status = COALESCE(EXCLUDED.identity_status, book_metadata.identity_status)",
		"asin_source = CASE WHEN EXCLUDED.identity_status IS NOT NULL THEN EXCLUDED.asin_source ELSE book_metadata.asin_source END",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("upsertBookMetadataSQL missing %q:\n%s", want, sql)
		}
	}

	ex := &captureExecer{}
	meta := metaprovider.BookMeta{Title: "Book", Author: "A", Source: "path",
		ASINSource: "embedded_tag", IdentityStatus: metaprovider.IdentityConflict}
	if err := upsertBookMetadata(context.Background(), ex, "/books/a/b", meta); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	args := ex.args[0]
	if v, ok := args[12].(*string); !ok || v == nil || *v != "embedded_tag" {
		t.Errorf("$13 asin_source = %#v, want embedded_tag", args[12])
	}
	if v, ok := args[13].(*string); !ok || v == nil || *v != "conflict" {
		t.Errorf("$14 identity_status = %#v, want conflict", args[13])
	}

	ex = &captureExecer{}
	if err := upsertBookMetadata(context.Background(), ex, "/books/a/b",
		metaprovider.BookMeta{Title: "Book", Source: "path"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if v, _ := ex.args[0][12].(*string); v != nil {
		t.Errorf("$13 asin_source = %v, want NULL", *v)
	}
	if v, _ := ex.args[0][13].(*string); v != nil {
		t.Errorf("$14 identity_status = %v, want NULL", *v)
	}
}
