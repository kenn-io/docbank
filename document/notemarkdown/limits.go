package notemarkdown

import "context"

const (
	maxInput       = 4 << 20
	maxFrontmatter = 64 << 10
	maxLines       = 65536
	maxHeadings    = 4096
	maxLinks       = 8192
	maxMetadata    = 256
	maxTarget      = 4096
	maxTitle       = 4096
	maxValue       = 1024
	maxYAMLDepth   = 32
	maxYAMLNodes   = 8192
	maxDepth       = 128
	maxNodes       = 262144
	maxWork        = 16777216
	maxDerived     = 8 << 20
	maxArtifact    = 16 << 20
	maxDiagnostics = 32
)

type budget struct {
	work, derived, nodes int
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
