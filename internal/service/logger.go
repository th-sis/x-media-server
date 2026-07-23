package service

import (
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// LogRingBuffer — circular buffer holding the last N log entries.
type LogRingBuffer struct {
	mu       sync.RWMutex
	buf      []LogEntry
	head     int
	size     int
	capacity int
}

type LogEntry struct {
	Timestamp string `json:"t"`
	Level     string `json:"l"`
	Message   string `json:"m"`
}

func NewLogRingBuffer(capacity int) *LogRingBuffer {
	return &LogRingBuffer{
		buf:      make([]LogEntry, capacity),
		capacity: capacity,
	}
}

func (rb *LogRingBuffer) Write(entry LogEntry) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.buf[rb.head] = entry
	rb.head = (rb.head + 1) % rb.capacity
	if rb.size < rb.capacity {
		rb.size++
	}
}

func (rb *LogRingBuffer) Snapshot() []LogEntry {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	result := make([]LogEntry, rb.size)
	for i := 0; i < rb.size; i++ {
		idx := (rb.head - rb.size + i + rb.capacity) % rb.capacity
		result[i] = rb.buf[idx]
	}
	return result
}

// ZerologHook captures zerolog events into the ring buffer
type ZerologHook struct {
	Buffer *LogRingBuffer
}

func (h ZerologHook) Run(e *zerolog.Event, level zerolog.Level, msg string) {
	entry := LogEntry{
		Timestamp: time.Now().Format("15:04:05.000"),
		Level:     level.String(),
		Message:   msg,
	}
	h.Buffer.Write(entry)
}
