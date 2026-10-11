# Earmark judge: GPT-6 Luna vs Haiku 5.5 on fresh seed qw2, context-backed labels

Date: 2026-10-11. Follows `verdict-judges.md` (seed qw1, text-only labels). Read-only: chunk text was pulled from earmark Postgres via `ctx/run.sh` + `ctx/q.py` (read-only session, 10 s statement timeout, 10 queries of at most 25 chunk ids by primary key, 233 chunks total). No model API calls, no writes to repos, DB or services. Working files are in `ctx2/` next to this report.

## Appendix: machine-readable labels

```json
{
 "legend": {
  "label": "G good fix; P right target but replacement still misspelt or paraphrased (counted as junk); B junk (wrong fix, or corrupts authored text); U unsure even with chunk context",
  "seed": "qw2 = all unique (chunk, original, replacement) triples from judge2-haiku/judge2-luna; qw1 = the 53 unique triples (61 arm-items) from verdict-judges.md Appendix A that were U, re-labelled with context",
  "context": "<=200 chars of the stored chunk text around the original span"
 },
 "labels": [
  {
   "id": "Q000",
   "seed": "qw2",
   "chunk_id": "858e2694-9a67-538d-933a-0430b675dafd",
   "track": "Abaddon's Gate: The Expanse, Book 3 [1549142194] - 26 - Chapter Twenty-Five: Holden",
   "original": "do you holden",
   "replacement": "do you know holden",
   "arms": [
    "haiku"
   ],
   "rows": {
    "haiku": 1
   },
   "confidence": {
    "haiku": [
     0.6
    ]
   },
   "issue_type": {
    "haiku": [
     "dropped_word"
    ]
   },
   "label": "B",
   "reason": "\"How much do you?\" Holden replied — sentence already complete; inserting \"know\" is wrong",
   "context": "teeth itched what will happen he asked how much do you know about quantum mechanics how much do you holden replied a lot turns out miller said with a lopsided grin do now anyway i'm not going to burs"
  },
  {
   "id": "Q001",
   "seed": "qw2",
   "chunk_id": "bd852987-58c8-5504-a68a-ffa44ffe9d2f",
   "track": "The Dark Forest [B010PKSKBA] - 2 - Part I: Year 3, Crisis Era",
   "original": "shu tiang",
   "replacement": "shu qiang",
   "arms": [
    "haiku"
   ],
   "rows": {
    "haiku": 2
   },
   "confidence": {
    "haiku": [
     0.6,
     0.6
    ]
   },
   "issue_type": {
    "haiku": [
     "misheard_proper_noun"
    ]
   },
   "label": "P",
   "reason": "Shi Qiang intended; \"shu qiang\" still misspelt",
   "context": " sleeping pills take them if you can't sleep i asked for alcohol but they said there isn't any shu tiang reminded luo ji that he should notify the captain before getting out of bed then turned to lea"
  },
  {
   "id": "J009",
   "seed": "qw1",
   "chunk_id": "0065eadf-021a-5782-a98f-94e347e5a417",
   "track": "A World Appears: A Journey into Consciousness [B0FQT9VX7K] - 12 - Chapter 4: Self",
   "original": "epiphenomenon phenomenon",
   "replacement": "epiphenomenon",
   "arms": [
    "haiku"
   ],
   "label_before": "U",
   "label": "G",
   "reason": "\"not as an epiphenomenon of brain activity\"",
   "context": "or ridiculed by scientists i'm thinking of theories that regard consciousness not as an epiphenomenon phenomenon of brain activity but is something more fundamental as fundamental as gravity or electr"
  }
 ]
}
```
