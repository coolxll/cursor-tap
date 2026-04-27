// Package types defines common types used across the MITM proxy.
package types

import (
	"net"
	"sync/atomic"
	"time"
)

// Direction indicates the data flow direction.
type Direction int

const (
	ClientToServer Direction = iota
	ServerToClient
)

func (d Direction) String() string {
	if d == ClientToServer {
		return "C->S"
	}
	return "S->C"
}

// Session represents a single proxied connection.
type Session struct {
	ID        string    `json:"id"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time,omitempty"`

	// Stats
	BytesSent     uint64 `json:"bytes_sent"`
	BytesReceived uint64 `json:"bytes_received"`

	// Internal
	ClientConn net.Conn `json:"-"`
	ServerConn net.Conn `json:"-"`
	closed     atomic.Bool
}

// AddBytesSent atomically adds to bytes sent counter.
func (s *Session) AddBytesSent(n uint64) {
	atomic.AddUint64(&s.BytesSent, n)
}

// AddBytesReceived atomically adds to bytes received counter.
func (s *Session) AddBytesReceived(n uint64) {
	atomic.AddUint64(&s.BytesReceived, n)
}

// IsClosed returns whether the session is closed.
func (s *Session) IsClosed() bool {
	return s.closed.Load()
}

// MarkClosed marks the session as closed.
func (s *Session) MarkClosed() {
	s.closed.Store(true)
	s.EndTime = time.Now()
}

// Duration returns the session duration.
func (s *Session) Duration() time.Duration {
	if s.EndTime.IsZero() {
		return time.Since(s.StartTime)
	}
	return s.EndTime.Sub(s.StartTime)
}

// LogLevel for HTTP stream logging.
type LogLevel int

const (
	LogLevelNone LogLevel = iota
	LogLevelBasic
	LogLevelHeaders
	LogLevelBody
	LogLevelDebug
)

// Config holds the application configuration.
type Config struct {
	HTTPPort      int    `json:"http_port"`
	SOCKS5Port    int    `json:"socks5_port"`
	APIPort       int    `json:"api_port"`
	CertDir       string `json:"cert_dir"`
	DataDir       string `json:"data_dir"`
	UpstreamProxy string `json:"upstream_proxy"` // e.g., "http://127.0.0.1:7890" or "socks5://127.0.0.1:1080"

	// HTTP parsing options
	EnableHTTPParsing bool     `json:"enable_http_parsing"` // Enable HTTP stream parsing
	HTTPLogLevel      LogLevel `json:"http_log_level"`      // HTTP logging verbosity
	HTTPRecordFile    string   `json:"http_record_file"`    // JSONL file for HTTP traffic recording
	SQLitePath        string   `json:"sqlite_path"`         // SQLite database for persistent records
	ProtocolPath      string   `json:"protocol_path"`       // Runtime .proto/.js protocol source
}

// DefaultConfig returns the default configuration.
func DefaultConfig() *Config {
	return &Config{
		HTTPPort:      8080,
		SOCKS5Port:    1080,
		APIPort:       8888,
		CertDir:       "~/.cursor-tap",
		DataDir:       "~/.cursor-tap/data",
		UpstreamProxy: "", // No upstream proxy by default
		SQLitePath:    "~/.cursor-tap/data/cursor-tap.sqlite",
	}
}
