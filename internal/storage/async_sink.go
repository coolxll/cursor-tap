package storage

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"github.com/burpheart/cursor-tap/internal/httpstream"
)

// AsyncSink decouples hot-path traffic parsing from SQLite writes.
type AsyncSink struct {
	db     *DB
	queue  chan httpstream.Record
	closed atomic.Bool
	mu     sync.RWMutex
	wg     sync.WaitGroup
}

func NewAsyncSink(db *DB, capacity int) *AsyncSink {
	if capacity <= 0 {
		capacity = 10000
	}
	sink := &AsyncSink{
		db:    db,
		queue: make(chan httpstream.Record, capacity),
	}
	sink.wg.Add(1)
	go sink.run()
	return sink
}

func (s *AsyncSink) SaveRecord(rec httpstream.Record) error {
	if s == nil || s.db == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed.Load() {
		return s.db.SaveRecord(rec)
	}
	select {
	case s.queue <- rec:
		return nil
	default:
		return s.db.SaveRecord(rec)
	}
}

func (s *AsyncSink) RecentRecords(limit int) ([]httpstream.Record, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	return s.db.RecentRecords(limit)
}

func (s *AsyncSink) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed.CompareAndSwap(false, true) {
		close(s.queue)
	}
	s.mu.Unlock()
	s.wg.Wait()
	return nil
}

func (s *AsyncSink) run() {
	defer s.wg.Done()
	for rec := range s.queue {
		if err := s.db.SaveRecord(rec); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to persist queued record: %v\n", err)
		}
	}
}
