package db

import (
	"strings"
	"testing"
)

// The batch lock must be a NON-waiting, session-scoped lock on its own key: a
// waiting lock would queue a second CronJob run for hours; a transaction lock
// would drop at the first COMMIT; the schema key would stall pod migrations.
func TestBatchLockSQL(t *testing.T) {
	if !strings.Contains(batchTryLockSQL, "pg_try_advisory_lock($1)") {
		t.Errorf("batch lock must be pg_try_advisory_lock on the key, got: %s", batchTryLockSQL)
	}
	for _, banned := range []string{"xact", "shared"} {
		if strings.Contains(batchTryLockSQL, banned) || strings.Contains(batchUnlockSQL, banned) {
			t.Errorf("batch lock must be an exclusive session lock, found %q", banned)
		}
	}
	if !strings.Contains(batchUnlockSQL, "pg_advisory_unlock($1)") {
		t.Errorf("batch unlock must release the same session lock, got: %s", batchUnlockSQL)
	}
	if batchLockKey == schemaInitLockKey {
		t.Error("batch lock key must differ from the schema migration key")
	}
	if batchLockKey != 0x4541524D_42544348 {
		t.Errorf("batch lock key changed to %#x: every coordinator must agree on it, so a change lets an old and a new one run together", batchLockKey)
	}
}
