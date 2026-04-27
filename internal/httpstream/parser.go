package httpstream

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// generateSessionID generates a short unique session ID.
func generateSessionID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Parser handles bidirectional HTTP stream parsing with zero-copy passthrough.
// Data flow is client-driven; parsing is done on mirrored data asynchronously.
type Parser struct {
	host         string
	sessionID    string
	logger       Logger
	grpcRegistry *MessageRegistry

	// Shared state for request/response correlation
	lastRequestURL         string
	lastRequestIsGRPC      bool   // Whether the request was gRPC/Connect
	lastRequestContentType string // Content-Type of the request
	lastRequestMutex       sync.Mutex

	http2Streams map[uint32]*http2StreamState
	http2Mutex   sync.Mutex

	// Callbacks (called asynchronously, don't block main flow)
	onRequest  func(*HTTPMessage)
	onResponse func(*HTTPMessage)
	onSSE      func(*SSEEvent)
	onBody     func(Direction, []byte)
	onGRPC     func(*GRPCMessage)
}

// ParserOption configures a Parser.
type ParserOption func(*Parser)

// WithParserLogger sets the logger.
func WithParserLogger(logger Logger) ParserOption {
	return func(p *Parser) { p.logger = logger }
}

// WithOnRequest sets the request callback.
func WithOnRequest(fn func(*HTTPMessage)) ParserOption {
	return func(p *Parser) { p.onRequest = fn }
}

// WithOnResponse sets the response callback.
func WithOnResponse(fn func(*HTTPMessage)) ParserOption {
	return func(p *Parser) { p.onResponse = fn }
}

// WithOnSSE sets the SSE event callback.
func WithOnSSE(fn func(*SSEEvent)) ParserOption {
	return func(p *Parser) { p.onSSE = fn }
}

// WithOnBody sets the body chunk callback.
func WithOnBody(fn func(Direction, []byte)) ParserOption {
	return func(p *Parser) { p.onBody = fn }
}

// WithOnGRPC sets the gRPC message callback.
func WithOnGRPC(fn func(*GRPCMessage)) ParserOption {
	return func(p *Parser) { p.onGRPC = fn }
}

// WithGRPCRegistry sets the gRPC message registry.
func WithGRPCRegistry(registry *MessageRegistry) ParserOption {
	return func(p *Parser) { p.grpcRegistry = registry }
}

// WithSessionID sets the session ID for tracking.
func WithSessionID(id string) ParserOption {
	return func(p *Parser) { p.sessionID = id }
}

// NewParser creates a new HTTP stream parser.
func NewParser(host string, opts ...ParserOption) *Parser {
	p := &Parser{
		host:         host,
		sessionID:    generateSessionID(),
		logger:       NopLogger{},
		http2Streams: make(map[uint32]*http2StreamState),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// SessionID returns the session ID.
func (p *Parser) SessionID() string {
	return p.sessionID
}

// Forward performs bidirectional forwarding with async HTTP parsing.
// Data flow is driven by client reads; parsing happens on mirrored data.
func (p *Parser) Forward(client, server net.Conn) error {
	var wg sync.WaitGroup
	wg.Add(2)

	errC2S := make(chan error, 1)
	errS2C := make(chan error, 1)

	// Client -> Server (requests)
	go func() {
		defer wg.Done()
		err := p.pipeWithMirror(server, client, ClientToServer)
		errC2S <- err
		closeWrite(server)
	}()

	// Server -> Client (responses)
	go func() {
		defer wg.Done()
		err := p.pipeWithMirror(client, server, ServerToClient)
		errS2C <- err
		closeWrite(client)
	}()

	wg.Wait()

	// Return first error if any
	select {
	case err := <-errC2S:
		if err != nil && err != io.EOF {
			return err
		}
	default:
	}
	select {
	case err := <-errS2C:
		if err != nil && err != io.EOF {
			return err
		}
	default:
	}

	return nil
}

// pipeWithMirror copies data from src to dst while mirroring to async parser.
// Main flow: io.Copy(dst, src) - client-driven, zero latency
// Side flow: mirrored data -> async parser goroutine
func (p *Parser) pipeWithMirror(dst io.Writer, src io.Reader, dir Direction) error {
	// Create pipe for mirroring
	pr, pw := io.Pipe()

	// TeeReader: every read from src is also written to pw
	tee := io.TeeReader(src, pw)

	// Start async parser goroutine (consumes mirrored data)
	parserDone := make(chan struct{})
	go func() {
		defer close(parserDone)
		p.parseStream(pr, dir)
		// Drain any remaining data to prevent blocking
		io.Copy(io.Discard, pr)
	}()

	// Main copy: client-driven, blocks until EOF or error
	_, err := io.Copy(dst, tee)

	// Close pipe writer to signal parser EOF
	pw.Close()

	// Wait for parser to finish (non-blocking drain ensures this completes)
	<-parserDone

	return err
}

// parseStream parses HTTP messages from mirrored stream asynchronously.
// This runs in a separate goroutine and doesn't block main data flow.
func (p *Parser) parseStream(r io.Reader, dir Direction) {
	reader := bufio.NewReader(r)

	if dir == ClientToServer {
		p.parseRequests(reader)
	} else {
		p.parseResponses(reader)
	}
}

// parseRequests parses HTTP requests from mirrored stream.
func (p *Parser) parseRequests(reader *bufio.Reader) {
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			return // EOF or parse error, stop parsing
		}

		// Create body reader for request body
		var bodyReader *BodyReader
		if req.Body != nil {
			bodyReader = NewBodyReader(req.Body, req.Header)
		}

		// Create message
		msg := &HTTPMessage{
			Direction: ClientToServer,
			Request:   req,
			Body:      bodyReader,
			Host:      p.host,
			Timestamp: time.Now(),
		}

		// Log and callback
		p.logger.LogRequest(msg)
		if p.onRequest != nil {
			p.onRequest(msg)
		}

		// Check if this is a gRPC/Connect request. JSON Connect is only treated
		// as RPC when the URL has the canonical /package.Service/Method shape,
		// so ordinary JSON HTTP APIs still land as plain HTTP bodies.
		contentType := req.Header.Get("Content-Type")
		if bodyReader != nil && IsRPCRequest(contentType, req.URL.Path) && req.Method == "POST" {
			// Store URL and content type for response correlation
			p.lastRequestMutex.Lock()
			p.lastRequestURL = req.URL.Path
			p.lastRequestIsGRPC = true
			p.lastRequestContentType = contentType
			p.lastRequestMutex.Unlock()

			p.parseGRPCBody(bodyReader, req.URL.Path, true, contentType)
			continue
		}

		// Log request body if present
		if bodyReader != nil {
			p.logBody(bodyReader, ClientToServer)
		}
	}
}

// parseResponses parses HTTP responses from mirrored stream.
func (p *Parser) parseResponses(reader *bufio.Reader) {
	for {
		resp, err := http.ReadResponse(reader, nil)
		if err != nil {
			return // EOF or parse error, stop parsing
		}

		// Create body reader for decoded access
		bodyReader := NewBodyReader(resp.Body, resp.Header)

		// Create message
		msg := &HTTPMessage{
			Direction: ServerToClient,
			Response:  resp,
			Body:      bodyReader,
			Host:      p.host,
			Timestamp: time.Now(),
		}

		// Log and callback
		p.logger.LogResponse(msg)
		if p.onResponse != nil {
			p.onResponse(msg)
		}

		requestPath, requestWasGRPC, requestContentType := p.takeLastRequestForResponse()

		// Check if this is a gRPC response
		contentType := resp.Header.Get("Content-Type")

		// Case 1: Response is explicitly gRPC/Connect
		if bodyReader != nil && IsRPCRequest(contentType, requestPath) && requestPath != "" {
			p.parseGRPCBody(bodyReader, requestPath, false, contentType)
			continue
		}

		// Case 2: Request was gRPC/Connect but response is SSE (gRPC-over-SSE tunnel)
		if bodyReader != nil && requestWasGRPC && requestPath != "" && bodyReader.IsSSE() {
			service, method, _ := ParseMethodFromURL(requestPath)
			p.parseGRPCSSEEvents(bodyReader, service, method)
			continue
		}

		// Case 3: Some servers omit Content-Type on RPC responses. Fall back to
		// the request content type so unary protobuf/JSON responses still decode.
		if bodyReader != nil && requestWasGRPC && requestPath != "" && contentType == "" && requestContentType != "" {
			p.parseGRPCBody(bodyReader, requestPath, false, requestContentType)
			continue
		}

		// Handle true SSE: parse events for logging
		if bodyReader != nil && bodyReader.IsSSE() {
			p.parseSSEEvents(bodyReader)
			continue
		}

		// For non-SSE: log full body
		if bodyReader != nil {
			p.logBody(bodyReader, ServerToClient)
		}
	}
}

func (p *Parser) takeLastRequestForResponse() (path string, wasGRPC bool, contentType string) {
	for attempt := 0; attempt < 100; attempt++ {
		p.lastRequestMutex.Lock()
		path = p.lastRequestURL
		wasGRPC = p.lastRequestIsGRPC
		contentType = p.lastRequestContentType
		if path != "" {
			p.lastRequestURL = ""
			p.lastRequestIsGRPC = false
			p.lastRequestContentType = ""
			p.lastRequestMutex.Unlock()
			return path, wasGRPC, contentType
		}
		p.lastRequestMutex.Unlock()
		time.Sleep(time.Millisecond)
	}
	return "", false, ""
}

// parseSSEEvents parses SSE events from body for logging.
func (p *Parser) parseSSEEvents(bodyReader *BodyReader) {
	sseParser := bodyReader.SSE()
	for {
		event, err := sseParser.Next()
		if err != nil {
			return
		}
		p.logger.LogSSE(p.host, event)
		if p.onSSE != nil {
			p.onSSE(event)
		}
	}
}

// parseGRPCSSEEvents records SSE transport events and best-effort decodes each
// event payload as JSON, base64 protobuf, or framed protobuf.
func (p *Parser) parseGRPCSSEEvents(bodyReader *BodyReader, service, method string) {
	sseParser := bodyReader.SSE()
	frameIndex := 0
	for {
		event, err := sseParser.Next()
		if err != nil {
			break
		}
		p.logger.LogSSE(p.host, event)
		if p.onSSE != nil {
			p.onSSE(event)
		}

		data := bytes.TrimSpace([]byte(event.Data))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}

		var messages []*GRPCMessage
		if jsonPayload := normalizeRawJSON(data); jsonPayload != "" {
			msg := parseConnectJSONBody(data, service, method, false, p.grpcRegistry)
			msg.IsStreaming = true
			msg.FrameIndex = frameIndex
			messages = append(messages, msg)
		} else if decoded, err := decodeSSEPayload(data); err == nil {
			if looksLikeGRPCFrame(decoded) {
				messages = ParseGRPCBody(decoded, service, method, false, p.grpcRegistry, "application/connect+proto")
			} else {
				messages = ParseGRPCBody(decoded, service, method, false, p.grpcRegistry, "application/proto")
			}
		}

		for _, msg := range messages {
			if !msg.IsStreaming {
				msg.IsStreaming = true
			}
			if msg.FrameIndex == 0 && frameIndex > 0 {
				msg.FrameIndex = frameIndex
			}
			frameIndex++
			p.logger.LogGRPC(msg)
			if p.onGRPC != nil {
				p.onGRPC(msg)
			}
		}
	}
	bodyReader.Close()
}

// parseGRPCBody parses gRPC body frames.
func (p *Parser) parseGRPCBody(bodyReader *BodyReader, urlPath string, isRequest bool, contentType string) {
	// Parse service and method from URL
	service, method, _ := ParseMethodFromURL(urlPath)

	// Try to auto-register from global registry if not found
	if p.grpcRegistry != nil {
		p.grpcRegistry.TryParseFromGlobalRegistry(service, method)
	}

	ctInfo := ParseContentType(contentType)

	if ctInfo.IsConnectJSON || ctInfo.NeedsFullBodyParsing() {
		data, err := bodyReader.ReadAll()
		if err != nil && err != io.EOF {
			p.logger.Debug("gRPC body read error: %v", err)
			return
		}
		messages := ParseGRPCBody(data, service, method, isRequest, p.grpcRegistry, contentType)
		for _, msg := range messages {
			p.logger.LogGRPC(msg)
			if p.onGRPC != nil {
				p.onGRPC(msg)
			}
		}
		bodyReader.Close()
		return
	}

	// For streaming methods, Cursor can still use application/proto, so use
	// protobuf service metadata in addition to content type.
	if (p.grpcRegistry != nil && p.grpcRegistry.UsesEnvelopeFraming(service, method, isRequest, contentType)) ||
		(p.grpcRegistry == nil && ctInfo.HasEnvelopeFraming()) {
		p.parseGRPCStream(bodyReader, service, method, isRequest)
		return
	}

	// For unary (no framing): read all at once
	data, err := bodyReader.ReadAll()
	if err != nil && err != io.EOF {
		p.logger.Debug("gRPC body read error: %v", err)
		return
	}

	// Parse as raw protobuf
	messages := ParseGRPCBody(data, service, method, isRequest, p.grpcRegistry, contentType)
	for _, msg := range messages {
		p.logger.LogGRPC(msg)
		if p.onGRPC != nil {
			p.onGRPC(msg)
		}
	}

	bodyReader.Close()
}

// parseGRPCStream parses streaming gRPC/Connect frames as they arrive.
// Each frame has a 5-byte header: [compressed:1][length:4]
// When compressed flag = 1, the frame payload is gzip compressed.
func (p *Parser) parseGRPCStream(bodyReader *BodyReader, service, method string, isRequest bool) {
	grpcParser := NewGRPCParser(p.grpcRegistry)
	frameIndex := 0

	// Read frames one by one (streaming)
	for {
		frame, err := grpcParser.ReadFrame(bodyReader)
		if err == io.EOF {
			break
		}
		if err != nil {
			p.logger.Debug("gRPC stream frame error: %v", err)
			break
		}

		// Parse and log each frame immediately
		msg := grpcParser.ParseMessage(frame, service, method, isRequest)
		msg.IsStreaming = true
		msg.FrameIndex = frameIndex
		msg.Compressed = frame.Compressed
		frameIndex++

		p.logger.LogGRPC(msg)
		if p.onGRPC != nil {
			p.onGRPC(msg)
		}
	}

	bodyReader.Close()
}

func decodeSSEPayload(data []byte) ([]byte, error) {
	clean := make([]byte, 0, len(data))
	for _, b := range data {
		switch b {
		case '\r', '\n', '\t', ' ':
			continue
		default:
			clean = append(clean, b)
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(string(clean))
	if err == nil {
		return decoded, nil
	}
	return base64.RawStdEncoding.DecodeString(string(clean))
}

func looksLikeGRPCFrame(data []byte) bool {
	if len(data) < 5 {
		return false
	}
	length := int(data[1])<<24 | int(data[2])<<16 | int(data[3])<<8 | int(data[4])
	return length >= 0 && len(data) >= 5+length
}

// logBody reads and logs the full body content.
func (p *Parser) logBody(bodyReader *BodyReader, dir Direction) {
	// Read full body for logging
	data, err := bodyReader.ReadAll()
	if err != nil && err != io.EOF {
		p.logger.Debug("body read error: %v", err)
	}

	if len(data) > 0 {
		p.logger.LogBody(dir, p.host, data)
		if p.onBody != nil {
			p.onBody(dir, data)
		}
	}

	bodyReader.Close()
}

// closeWrite closes the write side of a connection if supported.
func closeWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}
