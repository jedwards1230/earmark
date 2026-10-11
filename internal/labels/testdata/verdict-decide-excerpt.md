# Earmark DECIDE: context-labelled threshold pass

Date: 2026-10-10. Inputs: `decide-jev.jsonl` and `decide-clef.jsonl` (the same 2,000 held findings), earlier text-only labels (`verdict.md` Appendix B/C, `qw/results/labels.json`), and the chunk text, book title and series read from earmark's Postgres. All of this was read-only. Every DB session set `default_transaction_read_only = on` and `statement_timeout = '10s'`. Each query was capped at 25 rows and batched by `WHERE f.id = ANY($ids)` (18 batches). No model API was called and nothing was written to any repo, DB or service. The working files are in `bake/ctx/`: `blind.jsonl`, `labels.txt`, `analyze.py`, `auc.py`, `split.py`, `curves.json` and `appendix.json`.

## Appendix: machine-readable labels

```json
[
 {
  "finding_id": "00143cba-9b28-43eb-9440-a379ba7f59df",
  "label": "unsure",
  "reason": "name Bushel/Bushell can't confirm; adds punctuation",
  "context": "e central ticketing area and encountered a man named bushel an executive tech from sensenet's los angeles complex bushel was pale beneath an l a tan his seersucker suit limp with sweat he was carrying",
  "book": "Count Zero",
  "original": "an executive tech from sensenet's los angeles complex bushel was pale beneath an l a tan",
  "replacement": "an executive tech from sensenet’s Los Angeles complex. Bushell was pale beneath an L.A. tan",
  "evidence": "none",
  "strata": [
   "S5_mid"
  ],
  "jev_p": 0.42,
  "clef_p": 0.6736078968239173
 },
 {
  "finding_id": "02191ca9-6730-4dac-9790-b14c81296968",
  "label": "junk",
  "reason": "intentional chant",
  "context": "ol he wouldn't you say no i wouldn't his followers shout it joe joe joe joe over and over it's hypnotic well said selden making a move to return to his tri computer and adjust the multidimensional sim",
  "book": "Forward the Foundation",
  "original": "joe joe joe joe",
  "replacement": "joe",
  "evidence": "exact_repeat",
  "strata": [
   "S2_080_085"
  ],
  "jev_p": 0.69,
  "clef_p": 0.8256756763833758
 }
]
```
