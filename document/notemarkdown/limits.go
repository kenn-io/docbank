package notemarkdown

import "context"

const (
	maxInput       = 4 << 20
	maxHeadings    = 4096
	maxLinks       = 10000
	maxMetadata    = 256
	maxKeys        = 64
	maxTarget      = 4096
	maxValue       = 1024
	maxYAMLDepth   = 32
	maxYAMLNodes   = 8192
	maxDepth       = 128
	maxNodes       = 262144
	maxWork        = 64 << 20
	maxDiagnostics = 32
)

// One MaxBytes-sized heading admits clone, view, raw text, a conservative
// 3x NFC output bound and Path: about 7x input. Keep bounded headroom.
const maxDerived = 32 << 20

type budget struct {
	work, derived, nodes int
	limits               Limits
	diagnostics          int
}

func (b *budget) diagnostic(ctx context.Context, code string, span Span) ([]Diagnostic, error) {
	if err := b.step(ctx, 1); err != nil {
		return nil, err
	}
	if err := addLimit("diagnostics", &b.diagnostics, 1, b.limits.MaxDiagnostics); err != nil {
		return nil, err
	}
	return []Diagnostic{{Code: code, Span: span}}, nil
}

func addLimit(resource string, value *int, n, limit int) error {
	if n < 0 || *value < 0 || *value > limit || n > limit-*value {
		return &LimitError{Resource: resource}
	}
	*value += n
	return nil
}

func (b *budget) step(ctx context.Context, n int) error {
	old := b.work
	if err := addLimit("work_steps", &b.work, n, maxWork); err != nil {
		return err
	}
	// Large admissions check at every accounting interval, too.
	for checkpoint := old / 1024; checkpoint <= b.work/1024; checkpoint++ {
		if checkpoint == old/1024 && old%1024 != 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

func (b *budget) reserveStrings(n int) error {
	return addLimit("derived_bytes", &b.derived, n, maxDerived)
}

func (b *budget) node(ctx context.Context, depth int) error {
	if depth > maxDepth {
		return &LimitError{Resource: "ast_depth"}
	}
	if err := b.step(ctx, 1); err != nil {
		return err
	}
	return addLimit("ast_nodes", &b.nodes, 1, maxNodes)
}

func limitCount(resource string, n, limit int) error {
	value := 0
	return addLimit(resource, &value, n, limit)
}

func normalizeLimits(l Limits) (Limits, error) {
	for _, field := range []struct {
		value *int
		hard  int
	}{
		{&l.MaxBytes, maxInput}, {&l.MaxHeadings, maxHeadings}, {&l.MaxLinks, maxLinks},
		{&l.MaxFrontmatterKeys, maxKeys}, {&l.MaxValuesPerKey, maxMetadata},
		{&l.MaxValueBytes, maxValue}, {&l.MaxDiagnostics, maxDiagnostics},
	} {
		if *field.value < 0 || *field.value > field.hard {
			return Limits{}, ErrInvalidInput
		}
		if *field.value == 0 {
			*field.value = field.hard
		}
	}
	return l, nil
}
