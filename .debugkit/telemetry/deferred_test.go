package telemetry_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestDeferredInt64CounterEmitsZeroWhenNoAddsWereQueued(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	counter, err := provider.Meter("test").Int64Counter("requests")
	if err != nil {
		require.FailNow(t, err.Error())
	}

	instr := telemetry.NewDeferredInt64Counter(counter, attribute.String("route", "/ready"))
	instr.Emit(t.Context(), attribute.String("result", "ok"))

	points := collectInt64Counter(t, reader, "requests")
	if len(points) != 1 {
		require.FailNow(t, fmt.Sprintf("expected 1 data point, got %d", len(points)))
	}
	if points[0].Value != 0 {
		require.FailNow(t, fmt.Sprintf("expected zero value, got %d", points[0].Value))
	}
	assertAttr(t, points[0].Attributes, "route", "/ready")
	assertAttr(t, points[0].Attributes, "result", "ok")
}

func TestDeferredInt64CounterEmitsQueuedAddsWithAttributes(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	counter, err := provider.Meter("test").Int64Counter("errors")
	if err != nil {
		require.FailNow(t, err.Error())
	}

	instr := telemetry.NewDeferredInt64Counter(counter, attribute.String("component", "worker"))
	instr.Add(1, attribute.String("error.type", "timeout"))
	instr.Emit(t.Context(), attribute.String("operation", "sync"))

	points := collectInt64Counter(t, reader, "errors")
	if len(points) != 1 {
		require.FailNow(t, fmt.Sprintf("expected 1 data point, got %d", len(points)))
	}
	if points[0].Value != 1 {
		require.FailNow(t, fmt.Sprintf("expected value 1, got %d", points[0].Value))
	}
	assertAttr(t, points[0].Attributes, "component", "worker")
	assertAttr(t, points[0].Attributes, "error.type", "timeout")
	assertAttr(t, points[0].Attributes, "operation", "sync")
}

func TestDeferredInt64CounterEmitIsIdempotent(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	counter, err := provider.Meter("test").Int64Counter("once")
	if err != nil {
		require.FailNow(t, err.Error())
	}

	instr := telemetry.NewDeferredInt64Counter(counter)
	instr.Add(1)
	instr.Emit(t.Context())
	instr.Emit(t.Context())

	points := collectInt64Counter(t, reader, "once")
	if len(points) != 1 {
		require.FailNow(t, fmt.Sprintf("expected 1 data point, got %d", len(points)))
	}
	if points[0].Value != 1 {
		require.FailNow(t, fmt.Sprintf("expected value 1, got %d", points[0].Value))
	}
}

func TestDeferredInstrumentTypesEmitZeroWhenUnset(t *testing.T) {
	require := require.New(t)
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	meter := provider.Meter("test")

	floatCounter, err := meter.Float64Counter("float_counter")
	if err != nil {
		require.FailNow(err.Error())
	}
	intUpDownCounter, err := meter.Int64UpDownCounter("int_up_down_counter")
	if err != nil {
		require.FailNow(err.Error())
	}
	floatUpDownCounter, err := meter.Float64UpDownCounter("float_up_down_counter")
	if err != nil {
		require.FailNow(err.Error())
	}
	intHistogram, err := meter.Int64Histogram("int_histogram")
	if err != nil {
		require.FailNow(err.Error())
	}
	floatHistogram, err := meter.Float64Histogram("float_histogram")
	if err != nil {
		require.FailNow(err.Error())
	}
	intGauge, err := meter.Int64Gauge("int_gauge")
	if err != nil {
		require.FailNow(err.Error())
	}
	floatGauge, err := meter.Float64Gauge("float_gauge")
	if err != nil {
		require.FailNow(err.Error())
	}

	telemetry.NewDeferredFloat64Counter(floatCounter).Emit(t.Context())
	telemetry.NewDeferredInt64UpDownCounter(intUpDownCounter).Emit(t.Context())
	telemetry.NewDeferredFloat64UpDownCounter(floatUpDownCounter).Emit(t.Context())
	telemetry.NewDeferredInt64Histogram(intHistogram).Emit(t.Context())
	telemetry.NewDeferredFloat64Histogram(floatHistogram).Emit(t.Context())
	telemetry.NewDeferredInt64Gauge(intGauge).Emit(t.Context())
	telemetry.NewDeferredFloat64Gauge(floatGauge).Emit(t.Context())

	rm := collect(t, reader)
	assertSumPoint(t, findMetric(t, rm, "float_counter"), float64(0))
	assertSumPoint(t, findMetric(t, rm, "int_up_down_counter"), int64(0))
	assertSumPoint(t, findMetric(t, rm, "float_up_down_counter"), float64(0))
	assertHistogramPoint(t, findMetric(t, rm, "int_histogram"), int64(0))
	assertHistogramPoint(t, findMetric(t, rm, "float_histogram"), float64(0))
	assertGaugePoint(t, findMetric(t, rm, "int_gauge"), int64(0))
	assertGaugePoint(t, findMetric(t, rm, "float_gauge"), float64(0))
}

func TestDeferredGaugesEmitLatestSetValue(t *testing.T) {
	require := require.New(t)
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	gauge, err := provider.Meter("test").Int64Gauge("depth")
	if err != nil {
		require.FailNow(err.Error())
	}

	instr := telemetry.NewDeferredInt64Gauge(gauge, attribute.String("queue", "jobs"))
	instr.Set(3, attribute.String("ignored", "true"))
	instr.Set(7, attribute.String("ignored", "false"))
	instr.Emit(t.Context(), attribute.String("host", "local"))

	rm := collect(t, reader)
	data := findMetric(t, rm, "depth").Data
	gaugeData, ok := data.(metricdata.Gauge[int64])
	if !ok {
		require.FailNow(fmt.Sprintf("metric data type = %T, want metricdata.Gauge[int64]", data))
	}
	if len(gaugeData.DataPoints) != 1 {
		require.FailNow(fmt.Sprintf("expected 1 data point, got %d", len(gaugeData.DataPoints)))
	}
	if gaugeData.DataPoints[0].Value != 7 {
		require.FailNow(fmt.Sprintf("expected value 7, got %d", gaugeData.DataPoints[0].Value))
	}
	assertAttr(t, gaugeData.DataPoints[0].Attributes, "queue", "jobs")
	assertAttr(t, gaugeData.DataPoints[0].Attributes, "ignored", "false")
	assertAttr(t, gaugeData.DataPoints[0].Attributes, "host", "local")
}

func TestDeferredTimerRecordsElapsedSeconds(t *testing.T) {
	require := require.New(t)
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	histogram, err := provider.Meter("test").Float64Histogram("duration")
	if err != nil {
		require.FailNow(err.Error())
	}

	timer := telemetry.NewDeferredTimer(histogram, attribute.String("operation", "sync"))
	time.Sleep(time.Millisecond) //nolint:kennlint // sleeptest: the deferred timer measures the real monotonic clock
	timer.Emit(t.Context())
	timer.Emit(t.Context())

	rm := collect(t, reader)
	data := findMetric(t, rm, "duration").Data
	histogramData, ok := data.(metricdata.Histogram[float64])
	if !ok {
		require.FailNow(fmt.Sprintf("metric data type = %T, want metricdata.Histogram[float64]", data))
	}
	if len(histogramData.DataPoints) != 1 {
		require.FailNow(fmt.Sprintf("expected 1 data point, got %d", len(histogramData.DataPoints)))
	}
	point := histogramData.DataPoints[0]
	if point.Count != 1 {
		require.FailNow(fmt.Sprintf("expected count 1, got %d", point.Count))
	}
	if point.Sum <= 0 {
		require.FailNow(fmt.Sprintf("expected positive elapsed seconds, got %f", point.Sum))
	}
	assertAttr(t, point.Attributes, "operation", "sync")
}

func collectInt64Counter(t *testing.T, reader *metric.ManualReader, name string) []metricdata.DataPoint[int64] {
	t.Helper()

	rm := collect(t, reader)
	metric := findMetric(t, rm, name)
	sum, ok := metric.Data.(metricdata.Sum[int64])
	if !ok {
		require.FailNow(t, fmt.Sprintf("metric %q data type = %T, want metricdata.Sum[int64]", name, metric.Data))
	}
	return sum.DataPoints
}

func collect(t *testing.T, reader *metric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		require.FailNow(t, err.Error())
	}
	return rm
}

func findMetric(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			return m
		}
	}
	require.FailNow(t, fmt.Sprintf("metric %q not found", name))
	return metricdata.Metrics{}
}

func assertSumPoint[N int64 | float64](t *testing.T, metric metricdata.Metrics, want N) {
	t.Helper()

	sum, ok := metric.Data.(metricdata.Sum[N])
	if !ok {
		require.FailNow(t, fmt.Sprintf("metric %q data type = %T, want metricdata.Sum", metric.Name, metric.Data))
	}
	if len(sum.DataPoints) != 1 {
		require.FailNow(t, fmt.Sprintf("expected 1 data point, got %d", len(sum.DataPoints)))
	}
	if sum.DataPoints[0].Value != want {
		require.FailNow(t, fmt.Sprintf("expected value %v, got %v", want, sum.DataPoints[0].Value))
	}
}

func assertHistogramPoint[N int64 | float64](t *testing.T, metric metricdata.Metrics, want N) {
	t.Helper()
	require := require.New(t)
	t.Helper()

	histogram, ok := metric.Data.(metricdata.Histogram[N])
	if !ok {
		require.FailNow(fmt.Sprintf("metric %q data type = %T, want metricdata.Histogram", metric.Name, metric.Data))
	}
	if len(histogram.DataPoints) != 1 {
		require.FailNow(fmt.Sprintf("expected 1 data point, got %d", len(histogram.DataPoints)))
	}
	if histogram.DataPoints[0].Count != 1 {
		require.FailNow(fmt.Sprintf("expected count 1, got %d", histogram.DataPoints[0].Count))
	}
	if histogram.DataPoints[0].Sum != want {
		require.FailNow(fmt.Sprintf("expected sum %v, got %v", want, histogram.DataPoints[0].Sum))
	}
}

func assertGaugePoint[N int64 | float64](t *testing.T, metric metricdata.Metrics, want N) {
	t.Helper()

	gauge, ok := metric.Data.(metricdata.Gauge[N])
	if !ok {
		require.FailNow(t, fmt.Sprintf("metric %q data type = %T, want metricdata.Gauge", metric.Name, metric.Data))
	}
	if len(gauge.DataPoints) != 1 {
		require.FailNow(t, fmt.Sprintf("expected 1 data point, got %d", len(gauge.DataPoints)))
	}
	if gauge.DataPoints[0].Value != want {
		require.FailNow(t, fmt.Sprintf("expected value %v, got %v", want, gauge.DataPoints[0].Value))
	}
}

func assertAttr(t *testing.T, set attribute.Set, key attribute.Key, want string) {
	t.Helper()

	value, ok := set.Value(key)
	if !ok {
		require.FailNow(t, fmt.Sprintf("missing attribute %q", key))
	}
	if got := value.AsString(); got != want {
		require.FailNow(t, fmt.Sprintf("attribute %q = %q, want %q", key, got, want))
	}
}
