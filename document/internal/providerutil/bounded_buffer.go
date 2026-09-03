package providerutil

import (
	"slices"
	"sync"
)

// BoundedBuffer retains at most limit bytes and calls onOverflow once when a
// writer supplies more. It remains safe while command cancellation and output
// collection run concurrently.
type BoundedBuffer struct {
	mu         sync.Mutex
	data       []byte
	limit      int64
	exceeded   bool
	overflow   sync.Once
	onOverflow func()
}

// NewBoundedBuffer constructs a bounded process-output sink. Callers must
// supply a positive limit.
func NewBoundedBuffer(limit int64, onOverflow func()) *BoundedBuffer {
	return &BoundedBuffer{limit: limit, onOverflow: onOverflow}
}

// Write retains the bounded prefix and consumes the full write. The overflow
// callback owns stopping a producer that would otherwise continue writing.
func (buffer *BoundedBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	remaining := buffer.limit - int64(len(buffer.data))
	if remaining > 0 {
		kept := min(int64(len(value)), remaining)
		buffer.data = append(buffer.data, value[:kept]...)
	}
	exceeded := int64(len(value)) > remaining
	if exceeded {
		buffer.exceeded = true
	}
	buffer.mu.Unlock()
	if exceeded && buffer.onOverflow != nil {
		buffer.overflow.Do(buffer.onOverflow)
	}
	return len(value), nil
}

// Bytes returns an unaliased copy of the retained prefix.
func (buffer *BoundedBuffer) Bytes() []byte {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return slices.Clone(buffer.data)
}

// Len returns the number of retained bytes.
func (buffer *BoundedBuffer) Len() int {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return len(buffer.data)
}

// Exceeded reports whether any supplied bytes exceeded the limit.
func (buffer *BoundedBuffer) Exceeded() bool {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.exceeded
}
