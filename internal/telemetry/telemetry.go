// Package telemetry wires earmark's OpenTelemetry SDK (CONTRACT §2.16): one
// MeterProvider whose instruments are read both by a Prometheus pull exporter
// (served on the existing /metrics endpoint) and, when an OTLP endpoint is
// configured, by an OTLP push exporter; and a TracerProvider exporting spans
// over OTLP.
//
// Configuration is the standard OpenTelemetry environment only — nothing about
// any collector is hard-coded:
//
//	OTEL_SDK_DISABLED=true          everything off (no-op providers, no OTel series)
//	OTEL_SERVICE_NAME               resource service.name (default "earmark")
//	OTEL_RESOURCE_ATTRIBUTES        extra resource attributes
//	OTEL_EXPORTER_OTLP_ENDPOINT     OTLP endpoint; unset → OTLP is off
//	  (or OTEL_EXPORTER_OTLP_{TRACES,METRICS}_ENDPOINT per signal)
//	OTEL_EXPORTER_OTLP_PROTOCOL     http/protobuf only (the default); anything
//	                                else turns that signal's OTLP export off
//	OTEL_METRICS_EXPORTER           comma list of otlp, prometheus, none
//	                                (default: prometheus, plus otlp when an endpoint is set)
//	OTEL_TRACES_EXPORTER            otlp | none (default otlp when an endpoint is set)
//
// The exporters themselves read the rest (OTEL_EXPORTER_OTLP_HEADERS,
// _INSECURE, _TIMEOUT, OTEL_METRIC_EXPORT_INTERVAL, OTEL_BSP_*). Exporter
// construction never dials, so an unreachable collector never blocks startup;
// Shutdown flushes what is buffered, bounded by its context.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/otlptranslator"
	prombridge "go.opentelemetry.io/contrib/bridges/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"

	"github.com/jedwards1230/earmark/internal/log"
	"github.com/jedwards1230/earmark/internal/version"
)

// ScopeName is the instrumentation scope of earmark's own instruments.
const ScopeName = "github.com/jedwards1230/earmark"

// Exporter names accepted in OTEL_METRICS_EXPORTER / OTEL_TRACES_EXPORTER.
const (
	exporterOTLP       = "otlp"
	exporterPrometheus = "prometheus"
	exporterNone       = "none"
)

// RecipeInfo is one earmark_recipe_info series: the current recipe of a step.
type RecipeInfo struct {
	Step          string
	RecipeID      string
	Model         string
	Revision      string
	PromptVersion string
}

// Telemetry owns the SDK providers. The zero value is not usable; build it
// with Setup. All methods are safe on a disabled Telemetry.
type Telemetry struct {
	disabled    bool
	otlpMetrics bool
	otlpTraces  bool

	mp      *sdkmetric.MeterProvider
	tp      *sdktrace.TracerProvider
	promReg *prometheus.Registry

	legacy  atomic.Pointer[prometheus.Gatherer]
	recipes atomic.Pointer[[]RecipeInfo]
	stale   atomic.Pointer[map[string]int64]

	stopOnce sync.Once
	stop     chan struct{}
	// runCtx parents every background refresh; Shutdown cancels it so an
	// in-flight DB count is abandoned rather than waited out.
	runCtx    context.Context
	runCancel context.CancelFunc
	wg        sync.WaitGroup
}

var logger = log.NewLogger("telemetry")

// Setup builds the providers from the OTEL_* environment and installs them as
// the OpenTelemetry globals (so instruments created through otel.Meter /
// otel.Tracer anywhere in earmark use them). With OTEL_SDK_DISABLED=true it
// returns a disabled Telemetry and installs nothing.
func Setup(ctx context.Context) (*Telemetry, error) {
	t := &Telemetry{stop: make(chan struct{})}
	t.runCtx, t.runCancel = context.WithCancel(context.Background())
	if envBool("OTEL_SDK_DISABLED") {
		t.disabled = true
		logger.Info("OpenTelemetry SDK disabled (OTEL_SDK_DISABLED)")
		return t, nil
	}

	res, err := newResource(ctx)
	if err != nil {
		// A partial resource is still usable (e.g. one bad
		// OTEL_RESOURCE_ATTRIBUTES entry); log and carry on.
		logger.Warn("OpenTelemetry resource incomplete", "error", err)
	}

	metricExporters := exporterSet("OTEL_METRICS_EXPORTER", exporterPrometheus, exporterOTLP)
	traceExporters := exporterSet("OTEL_TRACES_EXPORTER", exporterOTLP)

	var readers []sdkmetric.Option
	if metricExporters[exporterPrometheus] {
		t.promReg = prometheus.NewRegistry()
		pe, err := otelprom.New(
			otelprom.WithRegisterer(t.promReg),
			// Prometheus-style names: counters get _total, and no
			// otel_scope_* labels on every series (cardinality rule, §2.16).
			otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithSuffixes),
			otelprom.WithoutScopeInfo(),
			// No target_info series: the resource labels duplicate the
			// scrape's job/instance and are not part of the §2.16 surface.
			otelprom.WithoutTargetInfo(),
		)
		if err != nil {
			return nil, fmt.Errorf("prometheus exporter: %w", err)
		}
		readers = append(readers, sdkmetric.WithReader(pe))
	}
	if metricExporters[exporterOTLP] {
		if !endpointSet("METRICS") {
			logger.Info("OTLP metrics not exported: no OTEL_EXPORTER_OTLP_ENDPOINT")
		} else if protocolSupported("METRICS") {
			exp, err := newMetricExporter(ctx)
			if err != nil {
				return nil, err
			}
			// The legacy client_golang registry (SetLegacyGatherer) rides
			// along in the push under its existing names. It is not added to
			// the Prometheus exporter: /metrics already serves it directly.
			bridge := prombridge.NewMetricProducer(prombridge.WithGatherer(gathererFunc(t.gatherLegacy)))
			readers = append(readers, sdkmetric.WithReader(
				sdkmetric.NewPeriodicReader(exp, sdkmetric.WithProducer(bridge))))
			t.otlpMetrics = true
		}
	}
	t.mp = sdkmetric.NewMeterProvider(append(readers, sdkmetric.WithResource(res))...)
	otel.SetMeterProvider(t.mp)

	if traceExporters[exporterOTLP] {
		if !endpointSet("TRACES") {
			logger.Info("OTLP traces not exported: no OTEL_EXPORTER_OTLP_ENDPOINT")
		} else if protocolSupported("TRACES") {
			exp, err := newTraceExporter(ctx)
			if err != nil {
				_ = t.mp.Shutdown(ctx)
				return nil, err
			}
			t.tp = sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
			otel.SetTracerProvider(t.tp)
			t.otlpTraces = true
		}
	}
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	if err := t.registerInstruments(); err != nil {
		_ = t.Shutdown(ctx)
		return nil, err
	}
	logger.Info("OpenTelemetry configured",
		"prometheus", t.promReg != nil, "otlp_metrics", t.otlpMetrics, "otlp_traces", t.otlpTraces)
	return t, nil
}

// registerInstruments creates earmark's info and state gauges. Their values
// are read at collection time from atomics the owner updates.
func (t *Telemetry) registerInstruments() error {
	m := t.mp.Meter(ScopeName)
	// The ldflags-stamped build identity (not version.GetInfo, which shells
	// out to git for unstamped builds).
	buildAttrs := metric.WithAttributes(
		attribute.String("version", version.Version), attribute.String("commit", version.Commit))
	if _, err := m.Int64ObservableGauge("earmark_build_info",
		metric.WithDescription("Always 1; labels carry the running earmark build (version, commit)."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(1, buildAttrs)
			return nil
		})); err != nil {
		return fmt.Errorf("earmark_build_info: %w", err)
	}
	if _, err := m.Int64ObservableGauge("earmark_recipe_info",
		metric.WithDescription("Always 1 per step with a current recipe (CONTRACT §1.9); labels: step, recipe, model, revision, prompt_version."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			if rs := t.recipes.Load(); rs != nil {
				for _, r := range *rs {
					o.Observe(1, metric.WithAttributes(
						attribute.String("step", r.Step),
						attribute.String("recipe", r.RecipeID),
						attribute.String("model", r.Model),
						attribute.String("revision", r.Revision),
						attribute.String("prompt_version", r.PromptVersion)))
				}
			}
			return nil
		})); err != nil {
		return fmt.Errorf("earmark_recipe_info: %w", err)
	}
	if _, err := m.Int64ObservableGauge("earmark_stale_items",
		metric.WithDescription("Output rows whose recipe is not equivalent to their step's current recipe (the stale_work view), per step with a current recipe. Refreshed on a timer."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			if s := t.stale.Load(); s != nil {
				for step, n := range *s {
					o.Observe(n, metric.WithAttributes(attribute.String("step", step)))
				}
			}
			return nil
		})); err != nil {
		return fmt.Errorf("earmark_stale_items: %w", err)
	}
	return nil
}

// Gatherer returns the Prometheus registry the OTel instruments are exported
// through, to be served alongside the legacy registry on /metrics. nil when
// the SDK is disabled or the prometheus exporter is not selected.
func (t *Telemetry) Gatherer() prometheus.Gatherer {
	if t == nil || t.promReg == nil {
		return nil
	}
	return t.promReg
}

// OTLPMetrics / OTLPTraces report whether an OTLP exporter is active.
func (t *Telemetry) OTLPMetrics() bool { return t != nil && t.otlpMetrics }
func (t *Telemetry) OTLPTraces() bool  { return t != nil && t.otlpTraces }

// SetLegacyGatherer sets the pre-existing client_golang registry (earmark_jobs,
// earmark_stage_duration_seconds, …) that the OTLP metric push bridges, so a
// push-only collector sees the same metrics /metrics serves. No-op when OTLP
// metrics are off.
func (t *Telemetry) SetLegacyGatherer(g prometheus.Gatherer) {
	if t == nil || g == nil {
		return
	}
	t.legacy.Store(&g)
}

func (t *Telemetry) gatherLegacy() ([]*dto.MetricFamily, error) {
	if g := t.legacy.Load(); g != nil {
		return (*g).Gather()
	}
	return nil, nil
}

// gathererFunc adapts a function to prometheus.Gatherer.
type gathererFunc func() ([]*dto.MetricFamily, error)

func (f gathererFunc) Gather() ([]*dto.MetricFamily, error) { return f() }

// SetRecipes replaces the earmark_recipe_info series.
func (t *Telemetry) SetRecipes(rs []RecipeInfo) {
	if t == nil {
		return
	}
	cp := append([]RecipeInfo(nil), rs...)
	t.recipes.Store(&cp)
}

// StartStaleRefresh refreshes earmark_stale_items from count every interval
// (and once immediately) until Shutdown. Each refresh is bounded by timeout; a
// failed refresh keeps the previous values and is logged.
func (t *Telemetry) StartStaleRefresh(interval, timeout time.Duration, count func(context.Context) (map[string]int64, error)) {
	if t == nil || t.disabled || count == nil {
		return
	}
	refresh := func() {
		ctx, cancel := context.WithTimeout(t.runCtx, timeout)
		defer cancel()
		n, err := count(ctx)
		if err != nil {
			logger.Warn("earmark_stale_items refresh failed (keeping previous values)", "error", err)
			return
		}
		t.stale.Store(&n)
	}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		refresh()
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-t.stop:
				return
			case <-tick.C:
				refresh()
			}
		}
	}()
}

// Shutdown stops the refresh loop and flushes and shuts down the providers,
// bounded by ctx. Safe to call more than once and on a nil or disabled
// Telemetry.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}
	var errs []error
	t.stopOnce.Do(func() {
		// Stop the refresh loop and abandon an in-flight count first, then
		// flush, so a hung DB never eats the flush budget (the k8s grace
		// period).
		close(t.stop)
		t.runCancel()
		if t.tp != nil {
			if err := t.tp.Shutdown(ctx); err != nil {
				errs = append(errs, fmt.Errorf("tracer provider: %w", err))
			}
		}
		if t.mp != nil {
			if err := t.mp.Shutdown(ctx); err != nil {
				errs = append(errs, fmt.Errorf("meter provider: %w", err))
			}
		}
		// Wait for the refresh goroutine, but never past ctx: a count that
		// ignores cancellation must not block exit.
		done := make(chan struct{})
		go func() { t.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
			errs = append(errs, fmt.Errorf("stale refresh still running: %w", ctx.Err()))
		}
	})
	return errors.Join(errs...)
}

// newResource describes this process: service.name "earmark" and its version,
// overridden by OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES (WithFromEnv is
// applied last, so the environment wins).
func newResource(ctx context.Context) (*resource.Resource, error) {
	return resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("earmark"),
			semconv.ServiceVersion(version.Version),
		),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
}

// exporterSet parses an OTEL_*_EXPORTER list. Unset → defaults. "none" anywhere
// → empty. Unknown names are logged and ignored.
func exporterSet(env string, defaults ...string) map[string]bool {
	out := map[string]bool{}
	raw := strings.TrimSpace(os.Getenv(env))
	if raw == "" {
		for _, d := range defaults {
			out[d] = true
		}
		return out
	}
	for _, name := range strings.Split(raw, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		switch name {
		case exporterNone:
			return map[string]bool{}
		case exporterOTLP, exporterPrometheus:
			out[name] = true
		case "":
		default:
			logger.Warn("unsupported exporter ignored", "env", env, "exporter", name)
		}
	}
	return out
}

// endpointSet reports whether an OTLP endpoint is configured for signal
// ("TRACES" or "METRICS"), generally or per signal.
func endpointSet(signal string) bool {
	return strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) != "" ||
		strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_"+signal+"_ENDPOINT")) != ""
}

// protocolSupported reports whether the configured OTLP protocol for signal
// (the per-signal variable, then OTEL_EXPORTER_OTLP_PROTOCOL) is http/protobuf,
// the spec default and the only one earmark ships (OTLP/gRPC would add ~8 MB
// of grpc to the binary; collectors such as Alloy take OTLP/HTTP on :4318).
// Anything else is logged and that signal's OTLP export stays off — sending
// HTTP to a gRPC port would fail on every export instead.
func protocolSupported(signal string) bool {
	p := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_" + signal + "_PROTOCOL"))
	if p == "" {
		p = strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"))
	}
	if p == "" || p == "http/protobuf" {
		return true
	}
	logger.Warn("unsupported OTLP protocol; only http/protobuf is supported (use the collector's OTLP/HTTP port, e.g. :4318) — OTLP export off for this signal",
		"signal", signal, "protocol", p)
	return false
}

func newMetricExporter(ctx context.Context) (sdkmetric.Exporter, error) {
	exp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp metric exporter: %w", err)
	}
	return exp, nil
}

func newTraceExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}
	return exp, nil
}

func envBool(name string) bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(name)), "true")
}
