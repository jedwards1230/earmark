package mcp

import (
	"bytes"
	"strings"
	"testing"

	elog "github.com/jedwards1230/earmark/internal/log"
)

// TestConfigureLogOutput (review M1): in stdio mode — explicit or the default
// empty transport — every internal/log line, including from loggers created
// earlier, goes to the stderr writer, never stdout; http leaves output alone.
func TestConfigureLogOutput(t *testing.T) {
	early := elog.NewLogger("early") // package-level loggers exist before main runs
	for _, tc := range []struct {
		transport string
		redirect  bool
	}{
		{"", true},
		{"stdio", true},
		{"http", false},
	} {
		t.Run("transport="+tc.transport, func(t *testing.T) {
			defer elog.SetOutput(nil)
			elog.SetOutput(nil)
			var stderr bytes.Buffer
			configureLogOutput(tc.transport, &stderr)
			early.Info("probe-line")
			if got := strings.Contains(stderr.String(), "probe-line"); got != tc.redirect {
				t.Errorf("log line on stderr writer = %v, want %v", got, tc.redirect)
			}
		})
	}
}
