package limitedlogsbuffer

import (
	"io"
	"sync"
)

// LimitedLogsBuffer wraps an io.Writer and limits the number of bytes written to it.
// It discards any data written beyond the specified limit.
type LimitedLogsBuffer struct {
	mu     sync.Mutex
	writer io.Writer
	limit  int64
	n      int64
}

// NewLimitedLogsBuffer creates a new LimitedLogsBuffer.
func NewLimitedLogsBuffer(w io.Writer, limit int64) *LimitedLogsBuffer {
	return &LimitedLogsBuffer{
		writer: w,
		limit:  limit,
	}
}

// Write writes bytes to the underlying writer, up to the limit.
// It returns the number of bytes successfully written to the underlying writer.
func (lw *LimitedLogsBuffer) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()

	if lw.n >= lw.limit {
		// Limit reached, write nothing more to the underlying writer.
		return len(p), nil
	}

	remaining := lw.limit - lw.n
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}

	n, err := lw.writer.Write(p)
	lw.n += int64(n)
	return len(p), err
}
