package metaprovider

import "testing"

// TestParseSeries covers the shapes the book_metadata.series column actually
// takes: the multi-series comma-joined form, a single series with and without a
// position, decimal (novella) positions, and the degenerate/empty inputs.
func TestParseSeries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want []SeriesRef
	}{
		{
			name: "multi-series comma joined",
			raw:  "Dune #2, The Dune Sequence #13",
			want: []SeriesRef{
				{Name: "Dune", Sequence: "2"},
				{Name: "The Dune Sequence", Sequence: "13"},
			},
		},
		{
			name: "single series with number",
			raw:  "Project Hail Mary #1",
			want: []SeriesRef{{Name: "Project Hail Mary", Sequence: "1"}},
		},
		{
			name: "series without number",
			raw:  "The Culture",
			want: []SeriesRef{{Name: "The Culture"}},
		},
		{
			name: "decimal sequence (novella)",
			raw:  "The Expanse #1.5",
			want: []SeriesRef{{Name: "The Expanse", Sequence: "1.5"}},
		},
		{
			name: "no space before hash",
			raw:  "Foundation#3",
			want: []SeriesRef{{Name: "Foundation", Sequence: "3"}},
		},
		{
			name: "extra spaces around name, hash and sequence",
			raw:  "  Wheel of Time   #  4  ,   Mistborn #2 ",
			want: []SeriesRef{
				{Name: "Wheel of Time", Sequence: "4"},
				{Name: "Mistborn", Sequence: "2"},
			},
		},
		{
			name: "non-numeric sequence is preserved verbatim",
			raw:  "Discworld #1-3",
			want: []SeriesRef{{Name: "Discworld", Sequence: "1-3"}},
		},
		{
			name: "trailing comma yields no empty entry",
			raw:  "Dune #2,",
			want: []SeriesRef{{Name: "Dune", Sequence: "2"}},
		},
		{
			name: "empty string",
			raw:  "",
			want: nil,
		},
		{
			name: "whitespace only",
			raw:  "   \t ",
			want: nil,
		},
		{
			name: "separators only",
			raw:  " , , ",
			want: nil,
		},
		{
			name: "sequence with no name is dropped",
			raw:  "#7",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := ParseSeries(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("ParseSeries(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("ParseSeries(%q)[%d] = %+v, want %+v", tc.raw, i, got[i], tc.want[i])
				}
			}
		})
	}
}
