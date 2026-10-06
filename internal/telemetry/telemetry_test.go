package telemetry_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/metrics"
	"github.com/jedwards1230/earmark/internal/predict"
	"github.com/jedwards1230/earmark/internal/telemetry"
	"github.com/jedwards1230/earmark/internal/version"
)

// clearOTelEnv unsets every OTEL_* variable Setup reads, so a developer's
// environment cannot leak into a test.
func clearOTelEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"OTEL_SDK_DISABLED", "OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES",
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_PROTOCOL",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
		"OTEL_METRICS_EXPORTER", "OTEL_TRACES_EXPORTER",
	} {
		t.Setenv(k, "")
	}
}

func shutdown(t *testing.T, tel *telemetry.Telemetry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tel.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// TestSetup_Disabled: OTEL_SDK_DISABLED=true installs nothing — no OTel
// series on /metrics, no SDK providers behind the globals.
func TestSetup_Disabled(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_SDK_DISABLED", "true")
	// Even with an endpoint configured, nothing is exported.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	// Start from no-op globals so a previous test's SDK providers cannot hide
	// an install.
	otel.SetMeterProvider(metricnoop.NewMeterProvider())
	otel.SetTracerProvider(tracenoop.NewTracerProvider())
	tel, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer shutdown(t, tel)
	if tel.Gatherer() != nil || tel.OTLPMetrics() || tel.OTLPTraces() {
		t.Errorf("disabled: gatherer=%v otlpMetrics=%v otlpTraces=%v, want none",
			tel.Gatherer(), tel.OTLPMetrics(), tel.OTLPTraces())
	}
	if _, ok := otel.GetMeterProvider().(*sdkmetric.MeterProvider); ok {
		t.Error("disabled: an SDK MeterProvider was installed")
	}
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); ok {
		t.Error("disabled: an SDK TracerProvider was installed")
	}
}

// TestSetup_NoEndpoint: with no OTLP endpoint, OTLP is off (no exporter, no
// network) and only the Prometheus pull exporter runs.
func TestSetup_NoEndpoint(t *testing.T) {
	clearOTelEnv(t)
	tel, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer shutdown(t, tel)
	if tel.Gatherer() == nil {
		t.Error("prometheus exporter missing with default OTEL_METRICS_EXPORTER")
	}
	if tel.OTLPMetrics() || tel.OTLPTraces() {
		t.Errorf("OTLP on without an endpoint: metrics=%v traces=%v", tel.OTLPMetrics(), tel.OTLPTraces())
	}
}

// TestSetup_ExplicitOTLPWithoutEndpointStaysOff: asking for otlp without an
// endpoint must not fall back to the SDK default (localhost:4317/4318).
func TestSetup_ExplicitOTLPWithoutEndpointStaysOff(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	tel, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer shutdown(t, tel)
	if tel.OTLPMetrics() || tel.OTLPTraces() || tel.Gatherer() != nil {
		t.Errorf("otlp-only without endpoint: metrics=%v traces=%v prom=%v; want all off",
			tel.OTLPMetrics(), tel.OTLPTraces(), tel.Gatherer() != nil)
	}
}

// TestSetup_UnreachableEndpointNeverBlocks: an endpoint that refuses
// connections must not block Setup, and Shutdown's flush is bounded by its
// context — for both protocols.
func TestSetup_UnreachableEndpointNeverBlocks(t *testing.T) {
	// A port nothing listens on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	for _, proto := range []string{"", "http/protobuf"} {
		t.Run(proto, func(t *testing.T) {
			clearOTelEnv(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+addr)
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", proto)
			t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "200")

			start := time.Now()
			tel, err := telemetry.Setup(context.Background())
			if err != nil {
				t.Fatalf("Setup: %v", err)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("Setup took %v with an unreachable endpoint", d)
			}
			if !tel.OTLPMetrics() || !tel.OTLPTraces() {
				t.Fatalf("OTLP not enabled with an endpoint: metrics=%v traces=%v", tel.OTLPMetrics(), tel.OTLPTraces())
			}
			_, span := otel.Tracer("test").Start(context.Background(), "s")
			span.End()

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			start = time.Now()
			_ = tel.Shutdown(ctx) // an export error is expected; a hang is not
			if d := time.Since(start); d > 3*time.Second {
				t.Errorf("Shutdown took %v", d)
			}
		})
	}
}

type fakeStats struct{}

func (fakeStats) GetServiceStatus(context.Context) (*db.QueueStats, error) {
	return &db.QueueStats{Pending: 2, Done: 3}, nil
}
func (fakeStats) GetPredictInputs(context.Context) (predict.Inputs, error) {
	return predict.Inputs{}, nil
}

// TestMetricsEndpointServesOTelAndLegacy: the OTel instruments, read through
// the Prometheus exporter, appear on the existing /metrics next to the legacy
// client_golang metrics, whose names are unchanged.
func TestMetricsEndpointServesOTelAndLegacy(t *testing.T) {
	clearOTelEnv(t)
	tel, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer shutdown(t, tel)

	reg := metrics.New(fakeStats{}, time.Second)
	reg.AddGatherer(tel.Gatherer())
	tel.SetLegacyGatherer(reg.Gatherer())
	tel.SetRecipes([]telemetry.RecipeInfo{{
		Step: "propose", RecipeID: "abc", Model: "anthropic/claude-haiku-4-5-20251001",
		Revision: "20251001", PromptVersion: "judge@v1",
	}})
	tel.StartStaleRefresh(time.Hour, time.Second, func(context.Context) (map[string]int64, error) {
		return map[string]int64{"embed": 7}, nil
	})

	want := []string{
		`earmark_build_info{commit="` + version.Commit + `",version="` + version.Version + `"} 1`,
		`earmark_recipe_info{model="anthropic/claude-haiku-4-5-20251001",prompt_version="judge@v1",recipe="abc",revision="20251001",step="propose"} 1`,
		`earmark_stale_items{step="embed"} 7`,
		// legacy names unchanged
		`earmark_jobs{status="pending"} 2`,
		`earmark_jobs_failed_total 0`,
	}
	deadline := time.Now().Add(2 * time.Second)
	var body string
	for {
		body = scrape(t, reg)
		if strings.Contains(body, `earmark_stale_items{step="embed"} 7`) || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond) // the stale refresh runs asynchronously
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("/metrics missing %q", w)
		}
	}
	if strings.Contains(body, "target_info") {
		t.Error("/metrics carries target_info; the exporter must run WithoutTargetInfo")
	}
	if strings.Contains(body, "otel_scope_") {
		t.Error("/metrics carries otel_scope_* labels; the exporter must run WithoutScopeInfo")
	}
}

// TestMetricsEndpointWithoutTelemetry: a disabled SDK leaves /metrics exactly
// the legacy surface (no OTel series, no error).
func TestMetricsEndpointWithoutTelemetry(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_SDK_DISABLED", "true")
	tel, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reg := metrics.New(fakeStats{}, time.Second)
	reg.AddGatherer(tel.Gatherer())
	body := scrape(t, reg)
	if strings.Contains(body, "earmark_build_info") {
		t.Error("earmark_build_info served with the SDK disabled")
	}
	if !strings.Contains(body, `earmark_jobs{status="pending"} 2`) {
		t.Error("legacy metrics missing")
	}
}

func scrape(t *testing.T, r *metrics.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestShutdownHonorsCtx (review M3): a stale-items refresh stuck on a hung DB
// must not hold Shutdown past its context. A count that honors its context is
// cancelled at once; one that ignores it is abandoned when ctx expires.
func TestShutdownHonorsCtx(t *testing.T) {
	for _, honors := range []bool{true, false} {
		clearOTelEnv(t)
		tel, err := telemetry.Setup(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		started := make(chan struct{})
		release := make(chan struct{})
		defer close(release)
		tel.StartStaleRefresh(time.Hour, time.Minute, func(ctx context.Context) (map[string]int64, error) {
			close(started)
			if honors {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			<-release // a driver that ignores cancellation
			return nil, nil
		})
		<-started
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		start := time.Now()
		err = tel.Shutdown(ctx)
		cancel()
		d := time.Since(start)
		if d > 2*time.Second {
			t.Errorf("honors=%v: Shutdown took %v with a stuck refresh", honors, d)
		}
		if honors && err != nil {
			t.Errorf("honors=%v: Shutdown = %v, want nil (refresh cancelled)", honors, err)
		}
		if !honors && err == nil {
			t.Error("abandoned refresh: want a context error from Shutdown")
		}
	}
}

// TestSetup_GRPCProtocolTurnsOTLPOff (review M7): only OTLP/HTTP ships. A
// grpc protocol setting is refused per signal rather than sending HTTP to a
// gRPC port on every export.
func TestSetup_GRPCProtocolTurnsOTLPOff(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "http/protobuf") // per-signal override wins
	tel, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(t, tel)
	if tel.OTLPMetrics() {
		t.Error("OTLP metrics on with protocol grpc")
	}
	if !tel.OTLPTraces() {
		t.Error("OTLP traces off despite OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/protobuf")
	}
}
