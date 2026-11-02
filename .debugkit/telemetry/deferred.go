// Package telemetry provides small utilities around OpenTelemetry and anonymous
// application telemetry.
package telemetry

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type number interface {
	~int64 | ~float64
}

type sample[N number] struct {
	value N
	attrs attribute.Set
}

type sampleBuffer[N number] struct {
	base attribute.Set

	mu      sync.Mutex
	samples []sample[N]
	emitted bool
}

func newSampleBuffer[N number](attrs []attribute.KeyValue) sampleBuffer[N] {
	return sampleBuffer[N]{
		base: attribute.NewSet(attrs...),
	}
}

func (b *sampleBuffer[N]) add(value N, attrs []attribute.KeyValue) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.emitted {
		return
	}

	b.samples = append(b.samples, sample[N]{
		value: value,
		attrs: attribute.NewSet(attrs...),
	})
}

func (b *sampleBuffer[N]) snapshot() ([]sample[N], bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.emitted {
		return nil, false
	}
	b.emitted = true

	samples := make([]sample[N], len(b.samples))
	copy(samples, b.samples)
	return samples, true
}

type latestValue[N number] struct {
	base attribute.Set

	mu      sync.Mutex
	value   N
	attrs   attribute.Set
	emitted bool
}

func newLatestValue[N number](attrs []attribute.KeyValue) latestValue[N] {
	return latestValue[N]{
		base: attribute.NewSet(attrs...),
	}
}

func (v *latestValue[N]) setValue(value N, attrs []attribute.KeyValue) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.emitted {
		return
	}

	v.value = value
	v.attrs = attribute.NewSet(attrs...)
}

func (v *latestValue[N]) snapshot() (N, attribute.Set, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.emitted {
		var zero N
		return zero, *attribute.EmptySet(), false
	}
	v.emitted = true

	return v.value, v.attrs, true
}

// DeferredInt64Counter buffers counter additions until Emit is called.
//
// This is useful for function-scoped metrics that should always create a data
// point, including the success path where no error or special condition was
// counted.
type DeferredInt64Counter struct {
	counter metric.Int64Counter
	buffer  sampleBuffer[int64]
}

// NewDeferredInt64Counter creates a deferred int64 counter using counter.
//
// attrs are included on every emitted measurement. Attributes passed to Add or
// Emit override attrs with the same key.
func NewDeferredInt64Counter(counter metric.Int64Counter, attrs ...attribute.KeyValue) *DeferredInt64Counter {
	return &DeferredInt64Counter{
		counter: counter,
		buffer:  newSampleBuffer[int64](attrs),
	}
}

// Add queues value to be emitted later.
func (d *DeferredInt64Counter) Add(value int64, attrs ...attribute.KeyValue) {
	d.buffer.add(value, attrs)
}

// Emit records all queued values. If no values were queued, Emit records zero.
func (d *DeferredInt64Counter) Emit(ctx context.Context, attrs ...attribute.KeyValue) {
	emitSamples(ctx, d.buffer.base, d.buffer.snapshot, d.counter.Add, int64(0), attrs)
}

// DeferredFloat64Counter buffers counter additions until Emit is called.
type DeferredFloat64Counter struct {
	counter metric.Float64Counter
	buffer  sampleBuffer[float64]
}

// NewDeferredFloat64Counter creates a deferred float64 counter using counter.
func NewDeferredFloat64Counter(counter metric.Float64Counter, attrs ...attribute.KeyValue) *DeferredFloat64Counter {
	return &DeferredFloat64Counter{
		counter: counter,
		buffer:  newSampleBuffer[float64](attrs),
	}
}

// Add queues value to be emitted later.
func (d *DeferredFloat64Counter) Add(value float64, attrs ...attribute.KeyValue) {
	d.buffer.add(value, attrs)
}

// Emit records all queued values. If no values were queued, Emit records zero.
func (d *DeferredFloat64Counter) Emit(ctx context.Context, attrs ...attribute.KeyValue) {
	emitSamples(ctx, d.buffer.base, d.buffer.snapshot, d.counter.Add, float64(0), attrs)
}

// DeferredInt64UpDownCounter buffers up-down counter additions until Emit is called.
type DeferredInt64UpDownCounter struct {
	counter metric.Int64UpDownCounter
	buffer  sampleBuffer[int64]
}

// NewDeferredInt64UpDownCounter creates a deferred int64 up-down counter using counter.
func NewDeferredInt64UpDownCounter(counter metric.Int64UpDownCounter, attrs ...attribute.KeyValue) *DeferredInt64UpDownCounter {
	return &DeferredInt64UpDownCounter{
		counter: counter,
		buffer:  newSampleBuffer[int64](attrs),
	}
}

// Add queues value to be emitted later.
func (d *DeferredInt64UpDownCounter) Add(value int64, attrs ...attribute.KeyValue) {
	d.buffer.add(value, attrs)
}

// Emit records all queued values. If no values were queued, Emit records zero.
func (d *DeferredInt64UpDownCounter) Emit(ctx context.Context, attrs ...attribute.KeyValue) {
	emitSamples(ctx, d.buffer.base, d.buffer.snapshot, d.counter.Add, int64(0), attrs)
}

// DeferredFloat64UpDownCounter buffers up-down counter additions until Emit is called.
type DeferredFloat64UpDownCounter struct {
	counter metric.Float64UpDownCounter
	buffer  sampleBuffer[float64]
}

// NewDeferredFloat64UpDownCounter creates a deferred float64 up-down counter using counter.
func NewDeferredFloat64UpDownCounter(counter metric.Float64UpDownCounter, attrs ...attribute.KeyValue) *DeferredFloat64UpDownCounter {
	return &DeferredFloat64UpDownCounter{
		counter: counter,
		buffer:  newSampleBuffer[float64](attrs),
	}
}

// Add queues value to be emitted later.
func (d *DeferredFloat64UpDownCounter) Add(value float64, attrs ...attribute.KeyValue) {
	d.buffer.add(value, attrs)
}

// Emit records all queued values. If no values were queued, Emit records zero.
func (d *DeferredFloat64UpDownCounter) Emit(ctx context.Context, attrs ...attribute.KeyValue) {
	emitSamples(ctx, d.buffer.base, d.buffer.snapshot, d.counter.Add, float64(0), attrs)
}

// DeferredInt64Histogram buffers histogram records until Emit is called.
type DeferredInt64Histogram struct {
	histogram metric.Int64Histogram
	buffer    sampleBuffer[int64]
}

// NewDeferredInt64Histogram creates a deferred int64 histogram using histogram.
func NewDeferredInt64Histogram(histogram metric.Int64Histogram, attrs ...attribute.KeyValue) *DeferredInt64Histogram {
	return &DeferredInt64Histogram{
		histogram: histogram,
		buffer:    newSampleBuffer[int64](attrs),
	}
}

// Record queues value to be emitted later.
func (d *DeferredInt64Histogram) Record(value int64, attrs ...attribute.KeyValue) {
	d.buffer.add(value, attrs)
}

// Emit records all queued values. If no values were queued, Emit records zero.
func (d *DeferredInt64Histogram) Emit(ctx context.Context, attrs ...attribute.KeyValue) {
	emitRecords(ctx, d.buffer.base, d.buffer.snapshot, d.histogram.Record, int64(0), attrs)
}

// DeferredFloat64Histogram buffers histogram records until Emit is called.
type DeferredFloat64Histogram struct {
	histogram metric.Float64Histogram
	buffer    sampleBuffer[float64]
}

// NewDeferredFloat64Histogram creates a deferred float64 histogram using histogram.
func NewDeferredFloat64Histogram(histogram metric.Float64Histogram, attrs ...attribute.KeyValue) *DeferredFloat64Histogram {
	return &DeferredFloat64Histogram{
		histogram: histogram,
		buffer:    newSampleBuffer[float64](attrs),
	}
}

// Record queues value to be emitted later.
func (d *DeferredFloat64Histogram) Record(value float64, attrs ...attribute.KeyValue) {
	d.buffer.add(value, attrs)
}

// Emit records all queued values. If no values were queued, Emit records zero.
func (d *DeferredFloat64Histogram) Emit(ctx context.Context, attrs ...attribute.KeyValue) {
	emitRecords(ctx, d.buffer.base, d.buffer.snapshot, d.histogram.Record, float64(0), attrs)
}

// DeferredInt64Gauge defers recording an instantaneous int64 value.
type DeferredInt64Gauge struct {
	gauge metric.Int64Gauge
	value latestValue[int64]
}

// NewDeferredInt64Gauge creates a deferred int64 gauge using gauge.
func NewDeferredInt64Gauge(gauge metric.Int64Gauge, attrs ...attribute.KeyValue) *DeferredInt64Gauge {
	return &DeferredInt64Gauge{
		gauge: gauge,
		value: newLatestValue[int64](attrs),
	}
}

// Set stores the value that Emit will record. Later Set calls replace earlier ones.
func (d *DeferredInt64Gauge) Set(value int64, attrs ...attribute.KeyValue) {
	d.value.setValue(value, attrs)
}

// Emit records the latest set value. If no value was set, Emit records zero.
func (d *DeferredInt64Gauge) Emit(ctx context.Context, attrs ...attribute.KeyValue) {
	emitLatest(ctx, d.value.base, d.value.snapshot, d.gauge.Record, attrs)
}

// DeferredFloat64Gauge defers recording an instantaneous float64 value.
type DeferredFloat64Gauge struct {
	gauge metric.Float64Gauge
	value latestValue[float64]
}

// NewDeferredFloat64Gauge creates a deferred float64 gauge using gauge.
func NewDeferredFloat64Gauge(gauge metric.Float64Gauge, attrs ...attribute.KeyValue) *DeferredFloat64Gauge {
	return &DeferredFloat64Gauge{
		gauge: gauge,
		value: newLatestValue[float64](attrs),
	}
}

// Set stores the value that Emit will record. Later Set calls replace earlier ones.
func (d *DeferredFloat64Gauge) Set(value float64, attrs ...attribute.KeyValue) {
	d.value.setValue(value, attrs)
}

// Emit records the latest set value. If no value was set, Emit records zero.
func (d *DeferredFloat64Gauge) Emit(ctx context.Context, attrs ...attribute.KeyValue) {
	emitLatest(ctx, d.value.base, d.value.snapshot, d.gauge.Record, attrs)
}

// DeferredTimer records elapsed time to a float64 histogram when Emit is called.
//
// Values are recorded in seconds to match OpenTelemetry's conventional unit
// for duration instruments.
type DeferredTimer struct {
	histogram metric.Float64Histogram
	base      attribute.Set
	start     time.Time

	mu      sync.Mutex
	emitted bool
}

// NewDeferredTimer starts a timer backed by histogram.
func NewDeferredTimer(histogram metric.Float64Histogram, attrs ...attribute.KeyValue) *DeferredTimer {
	return &DeferredTimer{
		histogram: histogram,
		base:      attribute.NewSet(attrs...),
		start:     time.Now(),
	}
}

// Emit records the elapsed time since NewDeferredTimer was called.
func (d *DeferredTimer) Emit(ctx context.Context, attrs ...attribute.KeyValue) {
	d.mu.Lock()
	if d.emitted {
		d.mu.Unlock()
		return
	}
	d.emitted = true
	elapsed := time.Since(d.start).Seconds()
	d.mu.Unlock()

	d.histogram.Record(ctx, elapsed, metric.WithAttributeSet(mergeSets(d.base, attribute.NewSet(attrs...))))
}

func emitSamples[N number](
	ctx context.Context,
	base attribute.Set,
	snapshot func() ([]sample[N], bool),
	record func(context.Context, N, ...metric.AddOption),
	zero N,
	attrs []attribute.KeyValue,
) {
	samples, ok := snapshot()
	if !ok {
		return
	}

	emitAttrs := attribute.NewSet(attrs...)
	if len(samples) == 0 {
		record(ctx, zero, metric.WithAttributeSet(mergeSets(base, emitAttrs)))
		return
	}

	for _, sample := range samples {
		set := mergeSets(base, sample.attrs)
		set = mergeSets(set, emitAttrs)
		record(ctx, sample.value, metric.WithAttributeSet(set))
	}
}

func emitRecords[N number](
	ctx context.Context,
	base attribute.Set,
	snapshot func() ([]sample[N], bool),
	record func(context.Context, N, ...metric.RecordOption),
	zero N,
	attrs []attribute.KeyValue,
) {
	samples, ok := snapshot()
	if !ok {
		return
	}

	emitAttrs := attribute.NewSet(attrs...)
	if len(samples) == 0 {
		record(ctx, zero, metric.WithAttributeSet(mergeSets(base, emitAttrs)))
		return
	}

	for _, sample := range samples {
		set := mergeSets(base, sample.attrs)
		set = mergeSets(set, emitAttrs)
		record(ctx, sample.value, metric.WithAttributeSet(set))
	}
}

func emitLatest[N number](
	ctx context.Context,
	base attribute.Set,
	snapshot func() (N, attribute.Set, bool),
	record func(context.Context, N, ...metric.RecordOption),
	attrs []attribute.KeyValue,
) {
	value, valueAttrs, ok := snapshot()
	if !ok {
		return
	}

	set := mergeSets(base, valueAttrs)
	set = mergeSets(set, attribute.NewSet(attrs...))
	record(ctx, value, metric.WithAttributeSet(set))
}

func mergeSets(a, b attribute.Set) attribute.Set {
	if a.Len() == 0 {
		return b
	}
	if b.Len() == 0 {
		return a
	}

	iter := attribute.NewMergeIterator(&b, &a)
	attrs := make([]attribute.KeyValue, 0, a.Len()+b.Len())
	for iter.Next() {
		attrs = append(attrs, iter.Attribute())
	}
	return attribute.NewSet(attrs...)
}
