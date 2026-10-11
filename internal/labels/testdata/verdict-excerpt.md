# Earmark local-vs-cloud bake-off: verdict

Date: 2026-10-10. Inputs: `/home/justin/.claude/jobs/ed7a5e0d/tmp/bake/` (judge-haiku/qwen, decide-jev/clef, score.py) and the 130-entry label file `qw/results/labels.json`. Everything here is read-only analysis. No model APIs were called, and no repo, DB or service was touched.

## Appendix: machine-readable labels

```json
{
 "judge": [
  {
   "id": "J000",
   "chunk_id": "0065eadf-021a-5782-a98f-94e347e5a417",
   "path": "A World Appears: A Journey into Consciousness [B0FQT9VX7K] - 12 - Chapter 4: Self",
   "original": "castrop",
   "correction": "castro",
   "by": {
    "Q": 4
   },
   "issue_type": {
    "Q": "misheard_proper_noun"
   },
   "confidence": {
    "Q": [
     0.95,
     0.95,
     0.95,
     0.95
    ]
   },
   "label": "B",
   "reason": "Kastrup (philosopher) intended; 'castro' is a different wrong name"
  },
  {
   "id": "J001",
   "chunk_id": "96ac19ac-9c0a-5b8a-9fa6-1f8c04da9181",
   "path": "The Gutenberg Parenthesis: The Age of Print and Its Lessons for the Age of the Internet [B0CQ5RFM9D] - 16 - Chapter 16",
   "original": "simon garfield",
   "correction": "simon schaffer",
   "by": {
    "Q": 1
   },
   "issue_type": {
    "Q": "misheard_proper_noun"
   },
   "confidence": {
    "Q": [
     0.95
    ]
   },
   "label": "B",
   "reason": "Simon Garfield is a real author, plausible as written; not a sound-alike"
  },
  {
   "id": "J002",
   "chunk_id": "c0573b7a-5b62-524b-9b82-546af4936739",
   "path": "Speaker for the Dead [B002V8N9VG] - 06 - Chapter 4: Ender",
   "original": "pechaninos",
   "correction": "pechani",
   "by": {
    "Q": 2
   },
   "issue_type": {
    "Q": "misheard_proper_noun"
   },
   "confidence": {
    "Q": [
     0.95,
     0.95
    ]
   },
   "label": "B",
   "reason": "pequeninos intended; 'pechani' is invented"
  }
 ],
 "decide": {
  "decide_disagreements_top60": [
   {
    "finding_id": "22498907-495b-400f-98ef-2208bc832aa3",
    "label": "G",
    "reason": "Eragon (asin evidence)",
    "jev_p": 0.24,
    "clef_p": 0.9437222715985669,
    "original": "erugon had felt himself changing during the trip",
    "replacement": "eragon had felt himself changing during the trip",
    "prior_label": null
   }
  ],
  "clef_applies": [
   {
    "finding_id": "0508278b-732f-4f83-bce7-4f2e4ff5f9c4",
    "label": "G",
    "reason": "repeated-word ASR duplication; removal matches print (small risk it was an intended stammer)",
    "jev_p": 0.84,
    "clef_p": 0.9589279585386297,
    "original": "essentially essentially",
    "replacement": "essentially"
   }
  ]
 }
}
```
