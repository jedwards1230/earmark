package db

import (
	"context"
	"encoding/json"
	"fmt"
)

// Pipeline event stage values (CONTRACT §1.7). These mirror the pipeline_events
// stage CHECK constraint. Only a subset is Go-emitted today; claim/transcribe/
// done are runner-side (deferred — see CONTRACT §1.7).
const (
	StageDiscover           = "discover"
	StageEnqueue            = "enqueue"
	StageClaim              = "claim"
	StageTranscribe         = "transcribe"
	StageChunk              = "chunk"
	StageEmbed              = "embed"
	StageEval               = "eval"
	StageDone               = "done"
	StageFail               = "fail"
	StageRequeue            = "requeue"
	StageHeartbeat          = "heartbeat"
	StageRunnerAvailability = "runner_availability"
)

// Pipeline event verbs (CONTRACT §1.7), mirroring the event CHECK constraint.
const (
	EventStart  = "start"
	EventFinish = "finish"
	EventError  = "error"
	EventSkip   = "skip"
	EventRetry  = "retry"
	EventState  = "state"
)

// Conventional runner_host identities for Go-emitted events (the runner uses its
// own claimed_by identity; these label the in-cluster Go emitters).
const (
	HostGoMonitor = "go-monitor"
	HostGoWorker  = "go-worker"
)

// PipelineEvent is one append-only row in pipeline_events (CONTRACT §1.7). All
// fields except Stage and Event are optional (nil/zero-friendly): pointers are
// written as NULL when nil, and Detail is marshaled to JSONB (nil → NULL).
type PipelineEvent struct {
	JobID        string // "" → NULL (e.g. runner_availability/heartbeat events)
	FilePath     string // "" → NULL; denormalized so the timeline survives a requeue
	Stage        string // one of the Stage* constants (required)
	Event        string // one of the Event* constants (required)
	RunnerHost   string // who: a runner id, HostGoWorker, or HostGoMonitor; "" → NULL
	Model        string // stage model id; "" → NULL
	ModelVersion string // family+runtime or chart/image version; "" → NULL
	DurationMS   *int64 // set on finish/error; nil → NULL
	ItemCount    *int   // chunks/windows/findings (stage-dependent); nil → NULL
	TokenCount   *int64 // prompt+total where applicable; nil → NULL
	Attempt      *int   // transcription_jobs.attempts at the time; nil → NULL
	Reason       string // failure/skip reason; "" → NULL
	// Detail is stage-specific extras serialized to JSONB. nil/empty → NULL.
	Detail map[string]any
}

// appendEventSQL is the single INSERT behind AppendEvent. A package var (not a
// const) so tests can assert its shape (append-only: INSERT only, no UPDATE/
// DELETE). Parameter order matches the AppendEvent argument list below.
var appendEventSQL = `
	INSERT INTO pipeline_events
	       (job_id, file_path, stage, event, runner_host, model, model_version,
	        duration_ms, item_count, token_count, attempt, reason, detail)
	VALUES (NULLIF($1,'')::uuid, NULLIF($2,''), $3, $4, NULLIF($5,''),
	        NULLIF($6,''), NULLIF($7,''), $8, $9, $10, $11, NULLIF($12,''), $13::jsonb)
`

// AppendEvent records one pipeline_events row (CONTRACT §1.7). It is
// **best-effort**: a marshal or insert failure is returned for the caller to log,
// but the caller MUST log-and-continue — appending an event NEVER fails the
// pipeline stage that produced it. Empty string fields become NULL; a nil/empty
// Detail map becomes a NULL JSONB.
func (db *DB) AppendEvent(ctx context.Context, e PipelineEvent) error {
	var detailJSON []byte
	if len(e.Detail) > 0 {
		var err error
		detailJSON, err = json.Marshal(e.Detail)
		if err != nil {
			return fmt.Errorf("marshal event detail: %w", err)
		}
	}
	_, err := db.pool.Exec(ctx, appendEventSQL,
		e.JobID, e.FilePath, e.Stage, e.Event, e.RunnerHost,
		e.Model, e.ModelVersion, e.DurationMS, e.ItemCount, e.TokenCount,
		e.Attempt, e.Reason, detailJSON)
	if err != nil {
		return fmt.Errorf("append pipeline event: %w", err)
	}
	return nil
}

// pruneEventsSQL deletes high-frequency, low-value event rows older than the
// retention window. Per-job stage events (low volume) are kept indefinitely;
// only the heartbeat / runner_availability rows are pruned. A package var so the
// shape is testable (it deletes ONLY heartbeat/runner_availability).
var pruneEventsSQL = `
	DELETE FROM pipeline_events
	WHERE created_at < now() - interval '180 days'
	  AND stage IN ('heartbeat','runner_availability')
`

// PruneEvents removes heartbeat/runner_availability events older than the
// retention window (180 days). Best-effort: callers log-and-continue. Returns the
// number of rows removed.
func (db *DB) PruneEvents(ctx context.Context) (int64, error) {
	tag, err := db.pool.Exec(ctx, pruneEventsSQL)
	if err != nil {
		return 0, fmt.Errorf("prune pipeline events: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Int64Ptr / IntPtr are small helpers so callers can populate the nilable
// PipelineEvent fields inline without scratch variables.
func Int64Ptr(v int64) *int64 { return &v }

// IntPtr returns a pointer to v.
func IntPtr(v int) *int { return &v }
