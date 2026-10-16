package main

import (
	"strings"
	"sync"
)

// lineBuffer keeps the last N log lines in memory for the web UI.
type lineBuffer struct {
	mu    sync.Mutex
	max   int
	lines []string
	rem   string
}

func newLineBuffer(max int) *lineBuffer { return &lineBuffer{max: max} }

func (b *lineBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rem += string(p)
	for {
		i := strings.IndexByte(b.rem, '\n')
		if i < 0 {
			break
		}
		b.lines = append(b.lines, b.rem[:i])
		b.rem = b.rem[i+1:]
	}
	if len(b.lines) > b.max {
		b.lines = b.lines[len(b.lines)-b.max:]
	}
	return len(p), nil
}

func (b *lineBuffer) Snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.lines))
	copy(out, b.lines)
	return out
}
