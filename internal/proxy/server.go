// Package proxy provides HTTP and SOCKS5 proxy servers with TLS MITM support.
package proxy

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/burpheart/cursor-tap/internal/api"
	"github.com/burpheart/cursor-tap/internal/ca"
	"github.com/burpheart/cursor-tap/internal/httpstream"
	"github.com/burpheart/cursor-tap/internal/mitm"
	"github.com/burpheart/cursor-tap/internal/protoextract"
	"github.com/burpheart/cursor-tap/internal/storage"
	"github.com/burpheart/cursor-tap/pkg/types"
)

// Server is the main proxy server that handles both HTTP and SOCKS5.
type Server struct {
	config      types.Config
	ca          *ca.CA
	interceptor *mitm.Interceptor
	keyLog      *mitm.KeyLogWriter
	recorder    *httpstream.Recorder
	store       *storage.DB
	recordSink  *storage.AsyncSink
	protocol    *protoextract.Result
	registry    *httpstream.MessageRegistry

	httpListener   net.Listener
	socks5Listener net.Listener
	apiServer      *http.Server

	// WebSocket hub for real-time updates
	hub *api.Hub

	mu       sync.Mutex
	running  bool
	stopChan chan struct{}
}

// NewServer creates a new proxy server.
func NewServer(config types.Config) (*Server, error) {
	// Ensure directories exist
	if err := os.MkdirAll(config.CertDir, 0755); err != nil {
		return nil, fmt.Errorf("create cert dir: %w", err)
	}
	if err := os.MkdirAll(config.DataDir, 0755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	var sqliteStore *storage.DB
	if config.SQLitePath != "" {
		var err error
		sqliteStore, err = storage.Open(config.SQLitePath)
		if err != nil {
			return nil, fmt.Errorf("open sqlite: %w", err)
		}
		fmt.Printf("[INFO] SQLite enabled: %s\n", sqliteStore.Path())
	}

	// Initialize CA
	caInstance, err := ca.New(ca.Options{
		CertDir: config.CertDir,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize CA: %w", err)
	}

	// Initialize KeyLog writer for bidirectional TLS key logging
	keyLogPath := filepath.Join(config.DataDir, "sslkeys.log")
	keyLog, err := mitm.NewKeyLogWriter(keyLogPath)
	if err != nil {
		return nil, fmt.Errorf("create keylog writer: %w", err)
	}
	fmt.Printf("[INFO] TLS KeyLog enabled: %s\n", keyLogPath)

	// Create WebSocket hub for real-time updates
	hub := api.NewHub()

	// Create interceptor options
	var interceptorOpts []mitm.InterceptorOption
	var recorder *httpstream.Recorder
	var recordSink *storage.AsyncSink
	var protocol *protoextract.Result
	registry := httpstream.DefaultGRPCRegistry()

	if config.ProtocolPath != "" {
		var err error
		protocol, err = protoextract.Load(config.ProtocolPath)
		if err != nil {
			if sqliteStore != nil {
				sqliteStore.Close()
			}
			return nil, fmt.Errorf("load protocol: %w", err)
		}
		registry, err = protoextract.BuildRegistry(protocol)
		if err != nil {
			if sqliteStore != nil {
				sqliteStore.Close()
			}
			return nil, fmt.Errorf("build protocol registry: %w", err)
		}
		interceptorOpts = append(interceptorOpts, mitm.WithGRPCRegistry(registry))
		if sqliteStore != nil {
			if err := sqliteStore.SaveProtocol(protocol.StorageVersion(true)); err != nil {
				fmt.Printf("[WARN] Failed to save protocol version: %v\n", err)
			}
		}
		fmt.Printf("[INFO] Protocol loaded: %s (%d messages, %d services)\n", config.ProtocolPath, protocol.Messages, protocol.Services)
	}

	// Enable HTTP parsing if configured
	if config.EnableHTTPParsing {
		interceptorOpts = append(interceptorOpts, mitm.WithHTTPParsing(true))

		// Create HTTP logger with configured level
		httpLogLevel := httpstream.LogLevel(config.HTTPLogLevel)
		httpLogger := httpstream.NewDefaultLogger(
			httpstream.WithLevel(httpLogLevel),
			httpstream.WithColor(true),
		)
		interceptorOpts = append(interceptorOpts, mitm.WithHTTPLogger(httpLogger))

		fmt.Printf("[INFO] HTTP parsing enabled (log level: %d)\n", config.HTTPLogLevel)

		// Create recorder if file path or SQLite path is configured
		if config.HTTPRecordFile != "" || sqliteStore != nil {
			var err error
			recorderOpts := []httpstream.RecorderOption{
				httpstream.WithRecorderLogLevel(httpLogLevel),
				httpstream.WithOnRecord(func(rec httpstream.Record) {
					// Broadcast to WebSocket clients
					hub.Broadcast(rec)
				}),
				httpstream.WithCacheSize(10000),
			}
			if sqliteStore != nil {
				recordSink = storage.NewAsyncSink(sqliteStore, 50000)
				recorderOpts = append(recorderOpts, httpstream.WithRecordSink(recordSink))
			}
			recorder, err = httpstream.NewRecorder(config.HTTPRecordFile, recorderOpts...)
			if err != nil {
				if recordSink != nil {
					recordSink.Close()
				}
				if sqliteStore != nil {
					sqliteStore.Close()
				}
				return nil, fmt.Errorf("create HTTP recorder: %w", err)
			}
			interceptorOpts = append(interceptorOpts, mitm.WithRecorder(recorder))
			if config.HTTPRecordFile != "" {
				fmt.Printf("[INFO] HTTP JSONL recording enabled: %s\n", config.HTTPRecordFile)
			}
		}
	}

	// Create interceptor
	interceptor := mitm.NewInterceptor(caInstance, keyLog, config.UpstreamProxy, interceptorOpts...)

	return &Server{
		config:      config,
		ca:          caInstance,
		interceptor: interceptor,
		keyLog:      keyLog,
		recorder:    recorder,
		store:       sqliteStore,
		recordSink:  recordSink,
		protocol:    protocol,
		registry:    registry,
		hub:         hub,
		stopChan:    make(chan struct{}),
	}, nil
}

// Start starts all proxy servers.
func (s *Server) Start() error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errors.New("server already running")
	}
	s.running = true
	s.mu.Unlock()

	// Write API address file
	apiAddr := fmt.Sprintf("127.0.0.1:%d", s.config.APIPort)
	addrFile := filepath.Join(s.config.CertDir, "api.addr")
	if err := os.WriteFile(addrFile, []byte(apiAddr), 0644); err != nil {
		fmt.Printf("[WARN] Failed to write API address file: %v\n", err)
	}

	// Start WebSocket hub
	if s.hub != nil {
		go s.hub.Run()
	}

	var wg sync.WaitGroup
	errChan := make(chan error, 3)

	// Start HTTP proxy
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.startHTTPProxy(); err != nil {
			errChan <- fmt.Errorf("HTTP proxy: %w", err)
		}
	}()

	// Start SOCKS5 proxy
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.startSOCKS5Proxy(); err != nil {
			errChan <- fmt.Errorf("SOCKS5 proxy: %w", err)
		}
	}()

	// Start API server
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.startAPIServer(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- fmt.Errorf("API server: %w", err)
		}
	}()

	// Wait for stop signal or error
	select {
	case <-s.stopChan:
		// Normal shutdown
	case err := <-errChan:
		return err
	}

	wg.Wait()
	return nil
}

// Stop stops all proxy servers.
func (s *Server) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	s.mu.Unlock()

	close(s.stopChan)

	if s.httpListener != nil {
		s.httpListener.Close()
	}
	if s.socks5Listener != nil {
		s.socks5Listener.Close()
	}
	if s.apiServer != nil {
		s.apiServer.Close()
	}
	if s.keyLog != nil {
		s.keyLog.Close()
	}
	if s.recorder != nil {
		s.recorder.Close()
	}
	if s.recordSink != nil {
		s.recordSink.Close()
	}
	if s.store != nil {
		s.store.Close()
	}

	// Remove API address file
	addrFile := filepath.Join(s.config.CertDir, "api.addr")
	os.Remove(addrFile)
}

// startHTTPProxy starts the HTTP/HTTPS proxy server.
func (s *Server) startHTTPProxy() error {
	addr := fmt.Sprintf("127.0.0.1:%d", s.config.HTTPPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	s.httpListener = listener
	fmt.Printf("[INFO] HTTP proxy listening on %s\n", addr)

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-s.stopChan:
				return nil
			default:
				fmt.Printf("[ERROR] HTTP accept: %v\n", err)
				continue
			}
		}

		go s.handleHTTPConnection(conn)
	}
}

// handleHTTPConnection handles an incoming HTTP proxy connection.
func (s *Server) handleHTTPConnection(conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReader(conn)

	// Read the first line to get the request
	req, err := http.ReadRequest(reader)
	if err != nil {
		if err != io.EOF {
			fmt.Printf("[DEBUG] HTTP read request error: %v\n", err)
		}
		return
	}

	if req.Method == http.MethodConnect {
		// HTTPS CONNECT tunnel - use bufferedConn to preserve any buffered data
		buffConn := &bufferedNetConn{Conn: conn, reader: reader}
		s.handleHTTPConnect(buffConn, req)
	} else {
		// Plain HTTP request - forward it
		s.handleHTTPRequest(conn, req, reader)
	}
}

// handleHTTPConnect handles HTTP CONNECT method (HTTPS tunneling).
func (s *Server) handleHTTPConnect(conn net.Conn, req *http.Request) {
	host := req.Host
	if !strings.Contains(host, ":") {
		host = host + ":443"
	}

	targetHost, targetPortStr, err := net.SplitHostPort(host)
	if err != nil {
		fmt.Printf("[ERROR] Invalid CONNECT host: %s\n", host)
		conn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
		return
	}

	targetPort, _ := strconv.Atoi(targetPortStr)

	// Send 200 Connection Established
	_, err = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	if err != nil {
		return
	}

	fmt.Printf("[INFO] CONNECT %s:%d\n", targetHost, targetPort)

	// Perform MITM interception with auto-detection
	if err := s.interceptor.InterceptAuto(conn, targetHost, targetPort); err != nil {
		if !isConnectionClosed(err) {
			fmt.Printf("[ERROR] Intercept %s:%d: %v\n", targetHost, targetPort, err)
		}
	}
}

// handleHTTPRequest handles plain HTTP requests (non-CONNECT).
func (s *Server) handleHTTPRequest(clientConn net.Conn, req *http.Request, _ *bufio.Reader) {
	// Get target host
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	if host == "" {
		clientConn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
		return
	}

	// Add default port if missing
	if !strings.Contains(host, ":") {
		host = host + ":80"
	}

	var session *httpstream.Session
	if s.recorder != nil {
		session = s.recorder.NewSession(host)
		if err := s.prepareAndRecordPlainRequest(session, req, host); err != nil {
			fmt.Printf("[DEBUG] Plain HTTP request record error: %v\n", err)
			return
		}
	}

	// Connect to target
	targetConn, err := net.DialTimeout("tcp", host, 10*time.Second)
	if err != nil {
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer targetConn.Close()

	// Rewrite request for direct connection
	req.URL.Scheme = ""
	req.URL.Host = ""
	req.RequestURI = req.URL.RequestURI()

	// Forward the request
	if err := req.Write(targetConn); err != nil {
		return
	}

	// Read and forward response
	resp, err := http.ReadResponse(bufio.NewReader(targetConn), req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if session != nil {
		if err := s.recordAndWritePlainResponse(clientConn, session, resp, host); err != nil {
			fmt.Printf("[DEBUG] Plain HTTP response record error: %v\n", err)
		}
		return
	}

	// Write response back to client
	resp.Write(clientConn)
}

func (s *Server) prepareAndRecordPlainRequest(session *httpstream.Session, req *http.Request, host string) error {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return err
		}
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(body))
		if req.ContentLength >= 0 {
			req.ContentLength = int64(len(body))
		}
	}

	session.LogRequest(&httpstream.HTTPMessage{
		Direction: httpstream.ClientToServer,
		Request:   req,
		Host:      host,
		Timestamp: time.Now(),
	})
	logPlainBody(session, httpstream.ClientToServer, host, body, req.Header)

	req.Body = io.NopCloser(bytes.NewReader(body))
	if req.ContentLength >= 0 {
		req.ContentLength = int64(len(body))
	}
	return nil
}

func (s *Server) recordAndWritePlainResponse(clientConn net.Conn, session *httpstream.Session, resp *http.Response, host string) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()

	session.LogResponse(&httpstream.HTTPMessage{
		Direction: httpstream.ServerToClient,
		Response:  resp,
		Host:      host,
		Timestamp: time.Now(),
	})
	logPlainBody(session, httpstream.ServerToClient, host, body, resp.Header)

	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.TransferEncoding = nil
	return resp.Write(clientConn)
}

func logPlainBody(session *httpstream.Session, dir httpstream.Direction, host string, raw []byte, headers http.Header) {
	if len(raw) == 0 {
		return
	}
	reader := httpstream.NewBodyReader(io.NopCloser(bytes.NewReader(raw)), headers)
	if reader == nil {
		return
	}
	defer reader.Close()
	data, err := reader.ReadAll()
	if err != nil && err != io.EOF {
		session.Debug("plain HTTP body decode error: %v", err)
		data = raw
	}
	session.LogBody(dir, host, data)
}

// startSOCKS5Proxy starts the SOCKS5 proxy server.
func (s *Server) startSOCKS5Proxy() error {
	addr := fmt.Sprintf("127.0.0.1:%d", s.config.SOCKS5Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	s.socks5Listener = listener
	fmt.Printf("[INFO] SOCKS5 proxy listening on %s\n", addr)

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-s.stopChan:
				return nil
			default:
				fmt.Printf("[ERROR] SOCKS5 accept: %v\n", err)
				continue
			}
		}

		go s.handleSOCKS5Connection(conn)
	}
}

// handleSOCKS5Connection handles a SOCKS5 client connection.
func (s *Server) handleSOCKS5Connection(conn net.Conn) {
	defer conn.Close()

	// Use buffered reader to avoid over-reading
	reader := bufio.NewReader(conn)

	// Set timeout for handshake
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	// SOCKS5 greeting: VER (1) + NMETHODS (1) + METHODS (1-255)
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return
	}

	if header[0] != 0x05 {
		return // Not SOCKS5
	}

	// Read authentication methods
	nmethods := int(header[1])
	methods := make([]byte, nmethods)
	if _, err := io.ReadFull(reader, methods); err != nil {
		return
	}

	// No authentication required
	conn.Write([]byte{0x05, 0x00})

	// Read request header: VER (1) + CMD (1) + RSV (1) + ATYP (1)
	reqHeader := make([]byte, 4)
	if _, err := io.ReadFull(reader, reqHeader); err != nil {
		return
	}

	if reqHeader[0] != 0x05 || reqHeader[1] != 0x01 {
		// Only support CONNECT command
		conn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	// Parse address based on ATYP
	var targetHost string
	var targetPort int

	switch reqHeader[3] {
	case 0x01: // IPv4
		addr := make([]byte, 4)
		if _, err := io.ReadFull(reader, addr); err != nil {
			return
		}
		targetHost = net.IP(addr).String()
	case 0x03: // Domain
		lenByte := make([]byte, 1)
		if _, err := io.ReadFull(reader, lenByte); err != nil {
			return
		}
		domain := make([]byte, lenByte[0])
		if _, err := io.ReadFull(reader, domain); err != nil {
			return
		}
		targetHost = string(domain)
	case 0x04: // IPv6
		addr := make([]byte, 16)
		if _, err := io.ReadFull(reader, addr); err != nil {
			return
		}
		targetHost = net.IP(addr).String()
	default:
		conn.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	// Read port (2 bytes, big endian)
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(reader, portBytes); err != nil {
		return
	}
	targetPort = int(binary.BigEndian.Uint16(portBytes))

	fmt.Printf("[INFO] SOCKS5 CONNECT %s:%d\n", targetHost, targetPort)

	// Send success response
	// Response: VER, REP, RSV, ATYP, BND.ADDR, BND.PORT
	response := []byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	conn.Write(response)

	// Clear deadline for data transfer
	conn.SetDeadline(time.Time{})

	// Wrap connection with buffered reader to preserve any buffered data
	buffConn := &bufferedNetConn{Conn: conn, reader: reader}

	// Perform MITM interception with auto-detection
	if err := s.interceptor.InterceptAuto(buffConn, targetHost, targetPort); err != nil {
		if !isConnectionClosed(err) {
			fmt.Printf("[ERROR] SOCKS5 intercept %s:%d: %v\n", targetHost, targetPort, err)
		}
	}
}

// startAPIServer starts the management API server.
func (s *Server) startAPIServer() error {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]interface{}{
			"status":          "running",
			"http_port":       s.config.HTTPPort,
			"socks5_port":     s.config.SOCKS5Port,
			"api_port":        s.config.APIPort,
			"sqlite_path":     s.config.SQLitePath,
			"protocol_path":   s.config.ProtocolPath,
			"active_protocol": activeProtocolID(s.protocol),
			"ws_clients":      s.hub.ClientCount(),
		})
	})

	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		stats := fmt.Sprintf(`{"active_sessions":0,"total_sessions":0,"total_bytes_sent":0,"total_bytes_received":0,"ws_clients":%d}`, s.hub.ClientCount())
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(stats))
	})

	mux.HandleFunc("/api/ca/cert", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, s.ca.CertPath())
	})

	// Register WebSocket and REST API routes if recording is enabled
	if (s.recorder != nil || s.store != nil) && s.hub != nil {
		store := &recorderStore{recorder: s.recorder, db: s.store}
		handler := api.NewHandler(s.hub, store)
		handler.RegisterRoutes(mux)
		fmt.Printf("[INFO] WebSocket and REST API enabled\n")
	}

	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			writeCORS(w)
			return
		}
		if s.store == nil {
			writeJSON(w, []interface{}{})
			return
		}
		limit := parseLimit(r, 500)
		sessions, err := s.store.ListSessions(limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, sessions)
	})

	mux.HandleFunc("/api/protocols", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			writeCORS(w)
			return
		}
		if s.store == nil {
			if s.protocol != nil {
				writeJSON(w, []interface{}{s.protocol.StorageVersion(true)})
				return
			}
			writeJSON(w, []interface{}{})
			return
		}
		protocols, err := s.store.ListProtocols()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, protocols)
	})

	s.registerInspectorRoutes(mux)

	addr := fmt.Sprintf("127.0.0.1:%d", s.config.APIPort)
	s.apiServer = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	fmt.Printf("[INFO] API server listening on %s\n", addr)
	return s.apiServer.ListenAndServe()
}

// recorderStore adapts httpstream.Recorder to api.RecordStore interface.
type recorderStore struct {
	recorder *httpstream.Recorder
	db       *storage.DB
}

func (s *recorderStore) GetRecentRecords(limit int) []interface{} {
	if s.recorder != nil {
		return s.recorder.GetRecentRecords(limit)
	}
	if s.db != nil {
		records, err := s.db.RecentRecords(limit)
		if err == nil {
			out := make([]interface{}, 0, len(records))
			for _, rec := range records {
				out = append(out, rec)
			}
			return out
		}
	}
	return []interface{}{}
}

func (s *recorderStore) ClearRecords() error {
	if s.recorder != nil {
		s.recorder.ClearCache()
	}
	if s.db != nil {
		return s.db.ClearTraffic()
	}
	return nil
}

func writeJSON(w http.ResponseWriter, value interface{}) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}

func writeCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.WriteHeader(http.StatusOK)
}

func parseLimit(r *http.Request, fallback int) int {
	limit := fallback
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	return limit
}

func activeProtocolID(protocol *protoextract.Result) string {
	if protocol == nil {
		return ""
	}
	return protocol.ID
}

// isConnectionClosed checks if the error indicates a closed connection.
func isConnectionClosed(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "use of closed network connection") ||
		strings.Contains(errStr, "connection reset by peer") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "EOF") ||
		errors.Is(err, io.EOF)
}

// bufferedNetConn wraps a net.Conn with a buffered reader to preserve any buffered data.
type bufferedNetConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedNetConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}
