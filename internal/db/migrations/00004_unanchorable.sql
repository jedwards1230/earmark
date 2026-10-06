-- 00004_unanchorable.sql — the unanchorable patch state and re-anchor audit
-- columns (CONTRACT §2.17 "Re-anchoring").
--
-- Re-chunking gives every chunk a new id and shifts its boundaries, so a
-- finding recorded before it names a chunk that no longer exists. `earmark
-- reanchor` finds the span in the current pristine chunks and records a fresh
-- anchor (chunk_id, chunk_index, chunk_text_sha256, anchor_offset,
-- anchor_occurrence — the existing anchor columns). A finding it cannot place
-- moves to `unanchorable` instead of `stale`: stale is terminal, while a later
-- re-anchor (after another re-chunk) may still place it.
--
--   unanchorable_reason  why the span could not be placed: 'anchor_not_found'
--                        or 'anchor_ambiguous' (the stale_reason words). Set
--                        exactly when patch_state = 'unanchorable'.
--   reanchored_at        when `earmark reanchor` last wrote this row's anchor
--                        or state. NULL = never touched by it.
--
-- Additive: two nullable columns, and the state CHECK widened by one value.
-- Readers that don't know the new state simply never ask for it.

-- +goose Up
ALTER TABLE transcript_findings
    ADD COLUMN unanchorable_reason TEXT,
    ADD COLUMN reanchored_at       TIMESTAMPTZ;

ALTER TABLE transcript_findings
    DROP CONSTRAINT transcript_findings_patch_state_valid,
    ADD CONSTRAINT transcript_findings_patch_state_valid
        CHECK (patch_state IN
            ('proposed','accepted','rejected','applied','stale','reverted','unanchorable')),
    ADD CONSTRAINT transcript_findings_unanchorable_reason_valid
        CHECK (unanchorable_reason IS NULL
               OR unanchorable_reason IN ('anchor_not_found','anchor_ambiguous')),
    ADD CONSTRAINT transcript_findings_unanchorable_reason_iff_state
        CHECK ((patch_state = 'unanchorable') = (unanchorable_reason IS NOT NULL));

-- +goose Down
-- An unanchorable finding goes back to the review queue it came from, so the
-- narrower CHECK can be restored.
UPDATE transcript_findings
   SET patch_state = 'proposed', unanchorable_reason = NULL
 WHERE patch_state = 'unanchorable';
ALTER TABLE transcript_findings
    DROP CONSTRAINT transcript_findings_unanchorable_reason_iff_state,
    DROP CONSTRAINT transcript_findings_unanchorable_reason_valid,
    DROP CONSTRAINT transcript_findings_patch_state_valid,
    ADD CONSTRAINT transcript_findings_patch_state_valid
        CHECK (patch_state IN ('proposed','accepted','rejected','applied','stale','reverted'));
ALTER TABLE transcript_findings
    DROP COLUMN reanchored_at,
    DROP COLUMN unanchorable_reason;
