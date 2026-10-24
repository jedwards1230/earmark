package decide

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/systemone"
)

// ColdStartTimeout is the per-call timeout while no System One call has
// returned yet (CONTRACT §2.19 "Cold start"). A decision model served locally
// (Clef on Ollama) is evicted after its keep-alive (5 min), and loading it
// takes longer than systemone.DefaultTimeout (15.4 s measured against a 15 s
// limit), so the first calls of a run would otherwise time out and be recorded
// as jev_unavailable holds. Two minutes is the limit the load benchmark used.
const ColdStartTimeout = 2 * time.Minute

// coldStartAsker asks cold until one call has returned, then warm. The first
// calls of a run are all made at once (up to --concurrency of them, before
// any has returned), so the longer timeout is theirs, not one call's.
//
// It adds no call of its own: every request is forwarded as received and every
// reply returned as received, so what reaches fn_calls, the cache key and the
// decide outcome is exactly what it was without it. Only the time a call may
// take differs, and only until the first call returns — answered or failed.
// An endpoint that never answers therefore costs one cold timeout, once, not
// one per call.
type coldStartAsker struct {
	cold, warm Asker
	// settled is set when the first call returns, whatever it returned.
	settled atomic.Bool
}

// withColdStart builds the asker over client: the first calls get coldTimeout,
// the rest the client's own timeout.
func withColdStart(client *systemone.Client, coldTimeout time.Duration) *coldStartAsker {
	return &coldStartAsker{
		cold: client.With(systemone.WithTimeout(coldTimeout)),
		warm: client,
	}
}

// WithColdStart wraps client so the first calls of a run, which may have to
// wait for the model to load, get ColdStartTimeout instead of the client's.
func WithColdStart(client *systemone.Client) Asker {
	return withColdStart(client, ColdStartTimeout)
}

// AskerFromConfig is ClientFromConfig's client as the Asker a decide run
// uses: cold-start safe (see ColdStartTimeout). ok is false when the role is
// unbound.
func AskerFromConfig(cfg *config.Config) (asker Asker, ok bool, err error) {
	client, ok, err := ClientFromConfig(cfg)
	if err != nil || !ok {
		return nil, ok, err
	}
	return WithColdStart(client), true, nil
}

// Decide implements Asker.
func (a *coldStartAsker) Decide(ctx context.Context, req systemone.Request) (*systemone.Response, error) {
	if a.settled.Load() {
		return a.warm.Decide(ctx, req)
	}
	resp, err := a.cold.Decide(ctx, req)
	a.settled.Store(true)
	return resp, err
}
