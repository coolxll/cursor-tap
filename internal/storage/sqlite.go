package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/burpheart/cursor-tap/internal/httpstream"
	_ "modernc.org/sqlite"
)

// DB wraps the local SQLite database used by the desktop app and API.
type DB struct {
	db      *sql.DB
	path    string
	writeMu sync.Mutex
}

// SessionRow is the API-facing aggregate for an intercepted RPC session.
type SessionRow struct {
	ID           string `json:"id"`
	Seq          int64  `json:"seq"`
	Host         string `json:"host"`
	RecordCount  int64  `json:"record_count"`
	FirstTS      string `json:"first_ts"`
	LastTS       string `json:"last_ts"`
	GRPCService  string `json:"grpc_service,omitempty"`
	GRPCMethod   string `json:"grpc_method,omitempty"`
	URL          string `json:"url,omitempty"`
	RequestSize  int64  `json:"request_size"`
	ResponseSize int64  `json:"response_size"`
	GRPCPreview  string `json:"grpc_preview,omitempty"`
}

// CallFilter controls API-side call aggregation filtering.
type CallFilter struct {
	Limit         int
	Query         string
	Kind          string
	Status        string
	Service       string
	Method        string
	StartedAfter  string
	StartedBefore string
}

// CallRow is a session-level RPC aggregate used by the prototype inspector UI.
type CallRow struct {
	ID              string `json:"id"`
	Seq             int64  `json:"seq"`
	Host            string `json:"host"`
	URL             string `json:"url,omitempty"`
	Service         string `json:"service,omitempty"`
	Method          string `json:"method,omitempty"`
	FullMethod      string `json:"full_method,omitempty"`
	Status          string `json:"status"`
	HTTPStatus      int    `json:"http_status,omitempty"`
	Streaming       bool   `json:"streaming"`
	StartedAt       string `json:"started_at"`
	EndedAt         string `json:"ended_at"`
	DurationMS      int64  `json:"duration_ms"`
	RecordCount     int64  `json:"record_count"`
	FrameCount      int64  `json:"frame_count"`
	RequestBytes    int64  `json:"request_bytes"`
	ResponseBytes   int64  `json:"response_bytes"`
	RequestPreview  string `json:"request_preview,omitempty"`
	ResponsePreview string `json:"response_preview,omitempty"`
	Error           string `json:"error,omitempty"`
	Origin          string `json:"origin"`
}

// CaptureRow stores a named set of records in SQLite.
type CaptureRow struct {
	ID          string              `json:"id"`
	CreatedAt   string              `json:"created_at"`
	Name        string              `json:"name"`
	Note        string              `json:"note,omitempty"`
	CallCount   int                 `json:"call_count"`
	RecordCount int                 `json:"record_count"`
	Records     []httpstream.Record `json:"records,omitempty"`
}

// ProtocolVersion stores a runtime protocol extraction result.
type ProtocolVersion struct {
	ID          string            `json:"id"`
	CreatedAt   string            `json:"created_at"`
	Path        string            `json:"path"`
	SHA256      string            `json:"sha256"`
	SourceKind  string            `json:"source_kind"`
	Messages    int               `json:"messages"`
	Enums       int               `json:"enums"`
	Services    int               `json:"services"`
	Diagnostics []string          `json:"diagnostics"`
	ProtoSource map[string]string `json:"proto_sources"`
	Active      bool              `json:"active"`
}

// Open opens or creates a SQLite database at path.
func Open(path string) (*DB, error) {
	if path == "" {
		return nil, fmt.Errorf("sqlite path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create sqlite dir: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)

	store := &DB{db: db, path: path}
	if err := store.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *DB) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *DB) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *DB) migrate(ctx context.Context) error {
	schema := []string{
		`CREATE TABLE IF NOT EXISTS records (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts TEXT NOT NULL,
			session TEXT NOT NULL,
			seq INTEGER NOT NULL,
			idx INTEGER NOT NULL,
			type TEXT NOT NULL,
			method TEXT,
			url TEXT,
			host TEXT,
			status INTEGER,
			status_text TEXT,
			event_type TEXT,
			event_id TEXT,
			event_data TEXT,
			headers_json TEXT,
			direction TEXT,
			size INTEGER,
			body TEXT,
			body_base64 TEXT,
			body_encoding TEXT,
			content_type TEXT,
			grpc_service TEXT,
			grpc_method TEXT,
			grpc_data TEXT,
			grpc_streaming INTEGER,
			grpc_frame_index INTEGER,
			grpc_compressed INTEGER,
			grpc_raw TEXT,
			error TEXT,
			protocol_id TEXT,
			raw_json TEXT NOT NULL,
			UNIQUE(session, idx)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_records_session ON records(session, idx)`,
		`CREATE INDEX IF NOT EXISTS idx_records_method ON records(grpc_service, grpc_method)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			seq INTEGER NOT NULL,
			host TEXT,
			record_count INTEGER NOT NULL DEFAULT 0,
			first_ts TEXT,
			last_ts TEXT,
			grpc_service TEXT,
			grpc_method TEXT,
			url TEXT,
			request_size INTEGER NOT NULL DEFAULT 0,
			response_size INTEGER NOT NULL DEFAULT 0,
			grpc_preview TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_seq ON sessions(seq DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_last_ts ON sessions(last_ts DESC)`,
		`CREATE TABLE IF NOT EXISTS protocol_versions (
			id TEXT PRIMARY KEY,
			created_at TEXT NOT NULL,
			path TEXT NOT NULL,
			sha256 TEXT NOT NULL,
			source_kind TEXT NOT NULL,
			messages INTEGER NOT NULL DEFAULT 0,
			enums INTEGER NOT NULL DEFAULT 0,
			services INTEGER NOT NULL DEFAULT 0,
			diagnostics_json TEXT,
			proto_sources_json TEXT,
			active INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS protocol_files (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			protocol_id TEXT NOT NULL,
			path TEXT NOT NULL,
			source TEXT NOT NULL,
			FOREIGN KEY(protocol_id) REFERENCES protocol_versions(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_protocol_files_version ON protocol_files(protocol_id)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS captures (
			id TEXT PRIMARY KEY,
			created_at TEXT NOT NULL,
			name TEXT NOT NULL,
			note TEXT,
			records_json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS capture_items (
			capture_id TEXT NOT NULL,
			session TEXT NOT NULL,
			PRIMARY KEY(capture_id, session),
			FOREIGN KEY(capture_id) REFERENCES captures(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_captures_created ON captures(created_at DESC)`,
	}
	for _, stmt := range schema {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate sqlite: %w", err)
		}
	}
	return nil
}

// SaveRecord persists a parsed HTTP/gRPC record and updates its session aggregate.
func (s *DB) SaveRecord(rec httpstream.Record) error {
	if s == nil {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	headersJSON := mustJSON(rec.Headers)
	rawJSON := mustJSON(rec)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	insert := `INSERT OR REPLACE INTO records (
		ts, session, seq, idx, type, method, url, host, status, status_text,
		event_type, event_id, event_data, headers_json, direction, size, body,
		body_base64, body_encoding, content_type, grpc_service, grpc_method,
		grpc_data, grpc_streaming, grpc_frame_index, grpc_compressed, grpc_raw,
		error, raw_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := tx.ExecContext(ctx, insert,
		rec.Timestamp, rec.SessionID, rec.SessionSeq, rec.RecordIndex, rec.Type,
		rec.Method, rec.URL, rec.Host, rec.Status, rec.StatusText,
		rec.EventType, rec.EventID, rec.EventData, headersJSON, rec.Direction,
		rec.Size, rec.Body, rec.BodyBase64, rec.BodyEncoding, rec.ContentType,
		rec.GRPCService, rec.GRPCMethod, rec.GRPCData, boolToInt(rec.GRPCStreaming),
		rec.GRPCFrameIndex, boolToInt(rec.GRPCCompressed), rec.GRPCRawData,
		rec.Error, rawJSON,
	); err != nil {
		return fmt.Errorf("insert record: %w", err)
	}

	sessionURL := ""
	if rec.Type == "request" {
		sessionURL = rec.URL
	}
	requestSize := int64(0)
	responseSize := int64(0)
	if rec.Direction == "C2S" {
		requestSize = int64(rec.Size)
	}
	if rec.Direction == "S2C" {
		responseSize = int64(rec.Size)
	}
	preview := ""
	if rec.Type == "grpc" && rec.Direction == "C2S" {
		preview = grpcRequestPreview(rec.GRPCData)
	}

	upsert := `INSERT INTO sessions (
		id, seq, host, record_count, first_ts, last_ts, grpc_service, grpc_method,
		url, request_size, response_size, grpc_preview
	) VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		record_count = sessions.record_count + 1,
		last_ts = CASE WHEN excluded.last_ts > sessions.last_ts THEN excluded.last_ts ELSE sessions.last_ts END,
		first_ts = CASE WHEN sessions.first_ts = '' OR excluded.first_ts < sessions.first_ts THEN excluded.first_ts ELSE sessions.first_ts END,
		grpc_service = COALESCE(NULLIF(sessions.grpc_service, ''), excluded.grpc_service),
		grpc_method = COALESCE(NULLIF(sessions.grpc_method, ''), excluded.grpc_method),
		url = COALESCE(NULLIF(sessions.url, ''), excluded.url),
		request_size = sessions.request_size + excluded.request_size,
		response_size = sessions.response_size + excluded.response_size,
		grpc_preview = COALESCE(NULLIF(sessions.grpc_preview, ''), excluded.grpc_preview)`
	if _, err := tx.ExecContext(ctx, upsert,
		rec.SessionID, rec.SessionSeq, rec.Host, rec.Timestamp, rec.Timestamp,
		rec.GRPCService, rec.GRPCMethod, sessionURL, requestSize, responseSize, preview,
	); err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}

	return tx.Commit()
}

// RecentRecords returns the newest records in chronological order.
func (s *DB) RecentRecords(limit int) ([]httpstream.Record, error) {
	if s == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 5000 {
		limit = 100
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, `SELECT raw_json FROM (
		SELECT id, raw_json FROM records ORDER BY id DESC LIMIT ?
	) ORDER BY id ASC`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []httpstream.Record
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var rec httpstream.Record
		if err := json.Unmarshal([]byte(raw), &rec); err == nil {
			records = append(records, rec)
		}
	}
	return records, rows.Err()
}

func (s *DB) ListSessions(limit int) ([]SessionRow, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, `SELECT id, seq, host, record_count, first_ts, last_ts,
		COALESCE(grpc_service, ''), COALESCE(grpc_method, ''), COALESCE(url, ''),
		request_size, response_size, COALESCE(grpc_preview, '')
		FROM sessions ORDER BY last_ts DESC, seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []SessionRow
	for rows.Next() {
		var row SessionRow
		if err := rows.Scan(&row.ID, &row.Seq, &row.Host, &row.RecordCount, &row.FirstTS, &row.LastTS,
			&row.GRPCService, &row.GRPCMethod, &row.URL, &row.RequestSize, &row.ResponseSize, &row.GRPCPreview); err != nil {
			return nil, err
		}
		sessions = append(sessions, row)
	}
	return sessions, rows.Err()
}

// SessionRecords returns every persisted record for a session in frame order.
func (s *DB) SessionRecords(sessionID string) ([]httpstream.Record, error) {
	if s == nil || sessionID == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, `SELECT raw_json FROM records WHERE session = ? ORDER BY idx ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []httpstream.Record
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var rec httpstream.Record
		if err := json.Unmarshal([]byte(raw), &rec); err == nil {
			records = append(records, rec)
		}
	}
	return records, rows.Err()
}

// ClearTraffic removes live traffic records and session aggregates.
func (s *DB) ClearTraffic() error {
	if s == nil {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM records`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions`); err != nil {
		return err
	}
	return tx.Commit()
}

// ListCalls returns session aggregates enriched with frame-level data.
func (s *DB) ListCalls(filter CallFilter) ([]CallRow, error) {
	if filter.Limit <= 0 || filter.Limit > 2000 {
		filter.Limit = 500
	}

	sourceLimit := filter.Limit
	if hasActiveCallFilter(filter) {
		sourceLimit = 2000
	}
	sessions, err := s.ListSessions(sourceLimit)
	if err != nil {
		return nil, err
	}

	callMap, err := s.loadCallRows(sessions)
	if err != nil {
		return nil, err
	}

	calls := make([]CallRow, 0, len(sessions))
	for _, session := range sessions {
		call := callMap[session.ID]
		if callMatchesFilter(call, filter) {
			calls = append(calls, call)
			if len(calls) >= filter.Limit {
				break
			}
		}
	}
	return calls, nil
}

func (s *DB) loadCallRows(sessions []SessionRow) (map[string]CallRow, error) {
	calls := make(map[string]CallRow, len(sessions))
	if len(sessions) == 0 {
		return calls, nil
	}

	args := make([]interface{}, 0, len(sessions))
	placeholders := make([]string, 0, len(sessions))
	for _, session := range sessions {
		placeholders = append(placeholders, "?")
		args = append(args, session.ID)
		calls[session.ID] = newCallRow(session)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, `SELECT session, ts, type, COALESCE(url, ''), COALESCE(host, ''),
		COALESCE(status, 0), COALESCE(direction, ''), COALESCE(size, 0),
		COALESCE(grpc_service, ''), COALESCE(grpc_method, ''), COALESCE(grpc_data, ''),
		COALESCE(grpc_streaming, 0), COALESCE(error, '')
		FROM records WHERE session IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY seq DESC, idx ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	computedRequestBytes := make(map[string]int64, len(sessions))
	computedResponseBytes := make(map[string]int64, len(sessions))
	for rows.Next() {
		var sessionID, ts, typ, url, host, direction, grpcService, grpcMethod, grpcData, recErr string
		var status, size, grpcStreaming int
		if err := rows.Scan(&sessionID, &ts, &typ, &url, &host, &status, &direction, &size,
			&grpcService, &grpcMethod, &grpcData, &grpcStreaming, &recErr); err != nil {
			return nil, err
		}
		call, ok := calls[sessionID]
		if !ok {
			continue
		}
		if call.Host == "" {
			call.Host = host
		}
		if call.StartedAt == "" || (ts != "" && ts < call.StartedAt) {
			call.StartedAt = ts
		}
		if ts > call.EndedAt {
			call.EndedAt = ts
		}
		if typ == "request" && call.URL == "" {
			call.URL = url
		}
		if typ == "response" {
			call.HTTPStatus = status
			if status >= 400 {
				call.Status = "error"
			} else if status > 0 && call.Status != "error" {
				call.Status = "ok"
			}
		}
		if typ == "error" || recErr != "" {
			call.Status = "error"
			if call.Error == "" {
				call.Error = recErr
			}
		}
		if direction == "C2S" {
			computedRequestBytes[sessionID] += int64(size)
		}
		if direction == "S2C" {
			computedResponseBytes[sessionID] += int64(size)
		}
		if typ == "grpc" {
			call.FrameCount++
			if grpcStreaming != 0 || call.FrameCount > 1 {
				call.Streaming = true
			}
			if call.Service == "" {
				call.Service = grpcService
			}
			if call.Method == "" {
				call.Method = grpcMethod
			}
			if direction == "C2S" {
				nextPreview := grpcRequestPreview(grpcData)
				if call.RequestPreview == "" || isPreferredPreview(nextPreview, call.RequestPreview) {
					call.RequestPreview = nextPreview
				}
			}
			if direction == "S2C" {
				if delta := grpcTextDelta(grpcData); delta != "" {
					call.ResponsePreview = appendTextPreview(call.ResponsePreview, delta)
				} else if call.ResponsePreview == "" {
					call.ResponsePreview = grpcData
				}
			}
		}
		calls[sessionID] = finalizeCallRow(call, computedRequestBytes[sessionID], computedResponseBytes[sessionID])
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for id, call := range calls {
		calls[id] = finalizeCallRow(call, computedRequestBytes[id], computedResponseBytes[id])
	}
	return calls, nil
}

func newCallRow(session SessionRow) CallRow {
	call := CallRow{
		ID:             session.ID,
		Seq:            session.Seq,
		Host:           session.Host,
		URL:            session.URL,
		Service:        session.GRPCService,
		Method:         session.GRPCMethod,
		Status:         "pending",
		StartedAt:      session.FirstTS,
		EndedAt:        session.LastTS,
		RecordCount:    session.RecordCount,
		RequestBytes:   session.RequestSize,
		ResponseBytes:  session.ResponseSize,
		RequestPreview: session.GRPCPreview,
		Origin:         "live",
	}
	if call.Service != "" && call.Method != "" {
		call.FullMethod = "/" + call.Service + "/" + call.Method
	}
	return call
}

func finalizeCallRow(call CallRow, computedRequestBytes, computedResponseBytes int64) CallRow {
	if call.FullMethod == "" && call.Service != "" && call.Method != "" {
		call.FullMethod = "/" + call.Service + "/" + call.Method
	}
	if call.Status == "pending" && call.FrameCount > 0 {
		call.Status = "ok"
	}
	if call.RequestBytes == 0 {
		call.RequestBytes = computedRequestBytes
	}
	if call.ResponseBytes == 0 {
		call.ResponseBytes = computedResponseBytes
	}
	if start, err := time.Parse(time.RFC3339Nano, call.StartedAt); err == nil {
		if end, err := time.Parse(time.RFC3339Nano, call.EndedAt); err == nil && !end.Before(start) {
			call.DurationMS = end.Sub(start).Milliseconds()
		}
	}
	return call
}

func buildCallRow(session SessionRow, records []httpstream.Record) CallRow {
	call := CallRow{
		ID:             session.ID,
		Seq:            session.Seq,
		Host:           session.Host,
		URL:            session.URL,
		Service:        session.GRPCService,
		Method:         session.GRPCMethod,
		Status:         "pending",
		StartedAt:      session.FirstTS,
		EndedAt:        session.LastTS,
		RecordCount:    session.RecordCount,
		RequestBytes:   session.RequestSize,
		ResponseBytes:  session.ResponseSize,
		RequestPreview: session.GRPCPreview,
		Origin:         "live",
	}
	if call.Service != "" && call.Method != "" {
		call.FullMethod = "/" + call.Service + "/" + call.Method
	}

	var computedRequestBytes int64
	var computedResponseBytes int64
	for _, rec := range records {
		if call.Host == "" {
			call.Host = rec.Host
		}
		if call.StartedAt == "" || (rec.Timestamp != "" && rec.Timestamp < call.StartedAt) {
			call.StartedAt = rec.Timestamp
		}
		if rec.Timestamp > call.EndedAt {
			call.EndedAt = rec.Timestamp
		}
		if rec.Type == "request" && call.URL == "" {
			call.URL = rec.URL
		}
		if rec.Type == "response" {
			call.HTTPStatus = rec.Status
			if rec.Status >= 400 {
				call.Status = "error"
			} else if rec.Status > 0 && call.Status != "error" {
				call.Status = "ok"
			}
		}
		if rec.Type == "error" {
			call.Status = "error"
			if call.Error == "" {
				call.Error = rec.Error
			}
		}
		if rec.Direction == "C2S" {
			computedRequestBytes += int64(rec.Size)
		}
		if rec.Direction == "S2C" {
			computedResponseBytes += int64(rec.Size)
		}
		if rec.Type != "grpc" {
			continue
		}
		call.FrameCount++
		if rec.GRPCStreaming || call.FrameCount > 1 {
			call.Streaming = true
		}
		if call.Service == "" {
			call.Service = rec.GRPCService
		}
		if call.Method == "" {
			call.Method = rec.GRPCMethod
		}
		if rec.Direction == "C2S" {
			nextPreview := grpcRequestPreview(rec.GRPCData)
			if call.RequestPreview == "" || isPreferredPreview(nextPreview, call.RequestPreview) {
				call.RequestPreview = nextPreview
			}
		}
		if rec.Direction == "S2C" {
			if delta := grpcTextDelta(rec.GRPCData); delta != "" {
				call.ResponsePreview = appendTextPreview(call.ResponsePreview, delta)
			} else if call.ResponsePreview == "" {
				call.ResponsePreview = rec.GRPCData
			}
		}
		if rec.Error != "" {
			call.Status = "error"
			if call.Error == "" {
				call.Error = rec.Error
			}
		}
	}
	if call.FullMethod == "" && call.Service != "" && call.Method != "" {
		call.FullMethod = "/" + call.Service + "/" + call.Method
	}
	if call.Status == "pending" && call.FrameCount > 0 {
		call.Status = "ok"
	}
	if call.RequestBytes == 0 {
		call.RequestBytes = computedRequestBytes
	}
	if call.ResponseBytes == 0 {
		call.ResponseBytes = computedResponseBytes
	}
	if start, err := time.Parse(time.RFC3339Nano, call.StartedAt); err == nil {
		if end, err := time.Parse(time.RFC3339Nano, call.EndedAt); err == nil && !end.Before(start) {
			call.DurationMS = end.Sub(start).Milliseconds()
		}
	}
	return call
}

func hasActiveCallFilter(filter CallFilter) bool {
	kind := strings.ToLower(strings.TrimSpace(filter.Kind))
	return strings.TrimSpace(filter.Query) != "" ||
		kind != "" && kind != "all" ||
		strings.TrimSpace(filter.Status) != "" ||
		strings.TrimSpace(filter.Service) != "" ||
		strings.TrimSpace(filter.Method) != "" ||
		strings.TrimSpace(filter.StartedAfter) != "" ||
		strings.TrimSpace(filter.StartedBefore) != ""
}

func callMatchesFilter(call CallRow, filter CallFilter) bool {
	if filter.Service != "" && call.Service != filter.Service {
		return false
	}
	if filter.Method != "" && call.Method != filter.Method {
		return false
	}
	if filter.Status != "" && call.Status != filter.Status {
		return false
	}
	if !callMatchesTimeRange(call, filter) {
		return false
	}
	switch strings.ToLower(filter.Kind) {
	case "streaming":
		if !call.Streaming {
			return false
		}
	case "unary":
		if call.Streaming {
			return false
		}
	case "errors":
		if call.Status != "error" {
			return false
		}
	}
	if filter.Query == "" {
		return true
	}
	q := strings.ToLower(filter.Query)
	haystack := strings.ToLower(strings.Join([]string{
		call.ID,
		call.Host,
		call.URL,
		call.Service,
		call.Method,
		call.FullMethod,
		call.RequestPreview,
		call.ResponsePreview,
		call.Error,
	}, " "))
	return strings.Contains(haystack, q)
}

func callMatchesTimeRange(call CallRow, filter CallFilter) bool {
	startedAt, ok := parseFilterTime(call.StartedAt)
	if !ok {
		return filter.StartedAfter == "" && filter.StartedBefore == ""
	}
	if after, ok := parseFilterTime(filter.StartedAfter); ok && startedAt.Before(after) {
		return false
	}
	if before, ok := parseFilterTime(filter.StartedBefore); ok && startedAt.After(before) {
		return false
	}
	return true
}

func grpcRequestPreview(data string) string {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		return ""
	}
	var msg struct {
		RunRequest *struct {
			Action *struct {
				UserMessageAction *struct {
					UserMessage *struct {
						Text      string `json:"text"`
						MessageID string `json:"messageId"`
						Mode      string `json:"mode"`
					} `json:"userMessage"`
				} `json:"userMessageAction"`
			} `json:"action"`
			ModelDetails any    `json:"modelDetails"`
			Conversation string `json:"conversationId"`
		} `json:"runRequest"`
	}
	if err := json.Unmarshal([]byte(trimmed), &msg); err == nil && msg.RunRequest != nil {
		userMessage := map[string]any{}
		if msg.RunRequest.Action != nil &&
			msg.RunRequest.Action.UserMessageAction != nil &&
			msg.RunRequest.Action.UserMessageAction.UserMessage != nil {
			userMessage = map[string]any{
				"text":      msg.RunRequest.Action.UserMessageAction.UserMessage.Text,
				"messageId": msg.RunRequest.Action.UserMessageAction.UserMessage.MessageID,
				"mode":      msg.RunRequest.Action.UserMessageAction.UserMessage.Mode,
			}
		}
		value := map[string]any{
			"runRequest": map[string]any{
				"userMessage":    userMessage,
				"modelDetails":   msg.RunRequest.ModelDetails,
				"conversationId": msg.RunRequest.Conversation,
			},
		}
		if encoded, err := json.Marshal(value); err == nil {
			return string(encoded)
		}
	}
	return trimmed
}

func grpcTextDelta(data string) string {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		return ""
	}
	var msg struct {
		InteractionUpdate *struct {
			TextDelta *struct {
				Text string `json:"text"`
			} `json:"textDelta"`
		} `json:"interactionUpdate"`
	}
	if err := json.Unmarshal([]byte(trimmed), &msg); err != nil || msg.InteractionUpdate == nil || msg.InteractionUpdate.TextDelta == nil {
		return ""
	}
	return msg.InteractionUpdate.TextDelta.Text
}

func appendTextPreview(current, delta string) string {
	const maxPreview = 4096
	if delta == "" {
		return current
	}
	if strings.TrimSpace(current) == "" || strings.HasPrefix(strings.TrimSpace(current), "{") {
		current = ""
	}
	next := current + delta
	if len(next) > maxPreview {
		return next[:maxPreview] + "..."
	}
	return next
}

func isPreferredPreview(next, current string) bool {
	if next == "" {
		return false
	}
	if current == "" {
		return true
	}
	if strings.Contains(next, `"runRequest"`) && len(next) < len(current) {
		return true
	}
	return false
}

func parseFilterTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// SaveCapture persists a named capture. If sessions is empty, recent records are captured.
func (s *DB) SaveCapture(name, note string, sessions []string) (CaptureRow, error) {
	if s == nil {
		return CaptureRow{}, fmt.Errorf("database is not available")
	}
	if strings.TrimSpace(name) == "" {
		name = "Untitled Capture"
	}

	var records []httpstream.Record
	seen := make(map[string]bool)
	for _, session := range sessions {
		session = strings.TrimSpace(session)
		if session == "" || seen[session] {
			continue
		}
		seen[session] = true
		sessionRecords, err := s.SessionRecords(session)
		if err != nil {
			return CaptureRow{}, err
		}
		records = append(records, sessionRecords...)
	}
	if len(records) == 0 {
		var err error
		records, err = s.RecentRecords(500)
		if err != nil {
			return CaptureRow{}, err
		}
	}

	created := time.Now().Format(time.RFC3339Nano)
	sum := sha256.Sum256([]byte(name + created))
	id := hex.EncodeToString(sum[:])[:16]
	recordsJSON := mustJSON(records)

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CaptureRow{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO captures(id, created_at, name, note, records_json) VALUES(?, ?, ?, ?, ?)`,
		id, created, name, note, recordsJSON); err != nil {
		return CaptureRow{}, err
	}
	for session := range seen {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO capture_items(capture_id, session) VALUES(?, ?)`, id, session); err != nil {
			return CaptureRow{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return CaptureRow{}, err
	}

	return CaptureRow{
		ID:          id,
		CreatedAt:   created,
		Name:        name,
		Note:        note,
		CallCount:   countSessions(records),
		RecordCount: len(records),
		Records:     records,
	}, nil
}

func (s *DB) ListCaptures(limit int) ([]CaptureRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, `SELECT id, created_at, name, COALESCE(note, ''), records_json FROM captures ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	captures := make([]CaptureRow, 0)
	for rows.Next() {
		capture, err := scanCapture(rows)
		if err != nil {
			return nil, err
		}
		capture.Records = nil
		captures = append(captures, capture)
	}
	return captures, rows.Err()
}

func (s *DB) GetCapture(id string) (CaptureRow, error) {
	if id == "" {
		return CaptureRow{}, sql.ErrNoRows
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	row := s.db.QueryRowContext(ctx, `SELECT id, created_at, name, COALESCE(note, ''), records_json FROM captures WHERE id = ?`, id)
	return scanCapture(row)
}

func (s *DB) DeleteCapture(id string) error {
	if id == "" {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := s.db.ExecContext(ctx, `DELETE FROM captures WHERE id = ?`, id)
	return err
}

type captureScanner interface {
	Scan(dest ...interface{}) error
}

func scanCapture(row captureScanner) (CaptureRow, error) {
	var capture CaptureRow
	var recordsJSON string
	if err := row.Scan(&capture.ID, &capture.CreatedAt, &capture.Name, &capture.Note, &recordsJSON); err != nil {
		return CaptureRow{}, err
	}
	json.Unmarshal([]byte(recordsJSON), &capture.Records)
	capture.RecordCount = len(capture.Records)
	capture.CallCount = countSessions(capture.Records)
	return capture, nil
}

func countSessions(records []httpstream.Record) int {
	seen := make(map[string]bool)
	for _, rec := range records {
		if rec.SessionID != "" {
			seen[rec.SessionID] = true
		}
	}
	return len(seen)
}

func (s *DB) SaveProtocol(p ProtocolVersion) error {
	if s == nil {
		return nil
	}
	if p.ID == "" {
		sum := sha256.Sum256([]byte(p.Path + p.SHA256 + p.SourceKind))
		p.ID = hex.EncodeToString(sum[:])[:16]
	}
	if p.CreatedAt == "" {
		p.CreatedAt = time.Now().Format(time.RFC3339Nano)
	}
	diagJSON := mustJSON(p.Diagnostics)
	sourceJSON := mustJSON(p.ProtoSource)

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if p.Active {
		if _, err := tx.ExecContext(ctx, `UPDATE protocol_versions SET active = 0`); err != nil {
			return err
		}
	}

	_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO protocol_versions (
		id, created_at, path, sha256, source_kind, messages, enums, services,
		diagnostics_json, proto_sources_json, active
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.CreatedAt, p.Path, p.SHA256, p.SourceKind, p.Messages, p.Enums, p.Services,
		diagJSON, sourceJSON, boolToInt(p.Active),
	)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM protocol_files WHERE protocol_id = ?`, p.ID); err != nil {
		return err
	}
	for name, source := range p.ProtoSource {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO protocol_files (protocol_id, path, source) VALUES (?, ?, ?)`,
			p.ID, name, source,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *DB) ListProtocols() ([]ProtocolVersion, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, `SELECT id, created_at, path, sha256, source_kind, messages,
		enums, services, COALESCE(diagnostics_json, '[]'), COALESCE(proto_sources_json, '{}'), active
		FROM protocol_versions ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var protocols []ProtocolVersion
	for rows.Next() {
		var p ProtocolVersion
		var diagJSON, sourceJSON string
		var active int
		if err := rows.Scan(&p.ID, &p.CreatedAt, &p.Path, &p.SHA256, &p.SourceKind, &p.Messages,
			&p.Enums, &p.Services, &diagJSON, &sourceJSON, &active); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(diagJSON), &p.Diagnostics)
		json.Unmarshal([]byte(sourceJSON), &p.ProtoSource)
		p.Active = active != 0
		protocols = append(protocols, p)
	}
	return protocols, rows.Err()
}

func (s *DB) SetSetting(key, value string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO settings(key, value) VALUES(?, ?)`, key, value)
	return err
}

func (s *DB) GetSetting(key string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

func mustJSON(v interface{}) string {
	if v == nil {
		return ""
	}
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(data)
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
