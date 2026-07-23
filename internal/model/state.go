package model

import (
	"sync"
	"time"
)

// PlaybackSession holds per-user playback state.
// Uses sync.Map internally for lock-free concurrent access.
type PlaybackSession struct {
	DeviceID   string
	UserID     string
	MediaID    string
	Title      string
	Position   int32
	Duration   int32
	Volume     float32
	Speed      float32
	SeasonNum  int32
	EpisodeNum int32
	State      string // idle | loading | playing | paused | buffering | ended | error | transferring
	LastSeen   time.Time
}

// StateStore manages playback sessions with lock-free concurrent access.
// Separated from image LRU to prevent lock contention between
// high-frequency playback updates and image cache operations.
type StateStore struct {
	sessions sync.Map // userID → *PlaybackSession
}

func NewStateStore() *StateStore {
	return &StateStore{}
}

func (s *StateStore) Get(userID string) *PlaybackSession {
	v, ok := s.sessions.Load(userID)
	if !ok {
		return nil
	}
	return v.(*PlaybackSession)
}

func (s *StateStore) Set(session *PlaybackSession) {
	session.LastSeen = time.Now()
	s.sessions.Store(session.UserID, session)
}

func (s *StateStore) Delete(userID string) {
	s.sessions.Delete(userID)
}

func (s *StateStore) Count() int {
	n := 0
	s.sessions.Range(func(key, value interface{}) bool {
		n++
		return true
	})
	return n
}

// ── Transfer State Machine ──

type TransferStatus string

const (
	TransferPending     TransferStatus = "pending"
	TransferDownloading TransferStatus = "downloading"
	TransferCompleted   TransferStatus = "completed"
	TransferFailed      TransferStatus = "failed"
)

type TransferTask struct {
	ID        string
	MediaID   string
	SourceURL string
	SourcePan string
	Status    TransferStatus
	Progress  int32  // 0-100
	ResultURL string // 115 direct link on completion
	Error     string
	CreatedAt time.Time
}

func NewTransferTask(mediaID, sourceURL, sourcePan string) *TransferTask {
	return &TransferTask{
		ID:        generateID(),
		MediaID:   mediaID,
		SourceURL: sourceURL,
		SourcePan: sourcePan,
		Status:    TransferPending,
		CreatedAt: time.Now(),
	}
}

func generateID() string {
	return time.Now().Format("20060102_150405") + "_" + randomString(6)
}

func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[time.Now().UnixNano()%int64(len(letters))]
	}
	return string(b)
}
