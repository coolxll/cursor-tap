package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/burpheart/cursor-tap/internal/httpstream"
)

func TestClearTrafficRemovesRecordsSessionsAndCalls(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "cursor-tap.sqlite"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	ts := time.Now().UTC().Format(time.RFC3339Nano)
	records := []httpstream.Record{
		{
			Timestamp:      ts,
			SessionID:      "session-1",
			SessionSeq:     1,
			RecordIndex:    0,
			Type:           "request",
			Method:         "POST",
			URL:            "https://api2.cursor.sh/aiserver.v1.AiService/Chat",
			Host:           "api2.cursor.sh",
			Headers:        map[string][]string{"authorization": {"Bearer test-token"}},
			Direction:      "C2S",
			Size:           42,
			ContentType:    "application/connect+proto",
			GRPCService:    "aiserver.v1.AiService",
			GRPCMethod:     "Chat",
			GRPCData:       `{"prompt":"hello"}`,
			GRPCFrameIndex: 0,
		},
		{
			Timestamp:      ts,
			SessionID:      "session-1",
			SessionSeq:     1,
			RecordIndex:    1,
			Type:           "grpc",
			Host:           "api2.cursor.sh",
			Direction:      "S2C",
			Size:           64,
			ContentType:    "application/connect+proto",
			GRPCService:    "aiserver.v1.AiService",
			GRPCMethod:     "Chat",
			GRPCData:       `{"text":"world"}`,
			GRPCStreaming:  true,
			GRPCFrameIndex: 0,
		},
	}
	for _, rec := range records {
		if err := db.SaveRecord(rec); err != nil {
			t.Fatalf("save record: %v", err)
		}
	}

	assertCount(t, "recent records before clear", lenRecentRecords(t, db), 2)
	assertCount(t, "sessions before clear", lenSessions(t, db), 1)
	assertCount(t, "calls before clear", lenCalls(t, db), 1)
	assertCount(t, "frames before clear", lenFrames(t, db, "session-1"), 2)

	if err := db.ClearTraffic(); err != nil {
		t.Fatalf("clear traffic: %v", err)
	}

	assertCount(t, "recent records after clear", lenRecentRecords(t, db), 0)
	assertCount(t, "sessions after clear", lenSessions(t, db), 0)
	assertCount(t, "calls after clear", lenCalls(t, db), 0)
	assertCount(t, "frames after clear", lenFrames(t, db, "session-1"), 0)
}

func lenRecentRecords(t *testing.T, db *DB) int {
	t.Helper()
	records, err := db.RecentRecords(100)
	if err != nil {
		t.Fatalf("recent records: %v", err)
	}
	return len(records)
}

func lenSessions(t *testing.T, db *DB) int {
	t.Helper()
	sessions, err := db.ListSessions(100)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	return len(sessions)
}

func lenCalls(t *testing.T, db *DB) int {
	t.Helper()
	calls, err := db.ListCalls(CallFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list calls: %v", err)
	}
	return len(calls)
}

func lenFrames(t *testing.T, db *DB, sessionID string) int {
	t.Helper()
	frames, err := db.SessionRecords(sessionID)
	if err != nil {
		t.Fatalf("session records: %v", err)
	}
	return len(frames)
}

func assertCount(t *testing.T, label string, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %d, want %d", label, got, want)
	}
}
