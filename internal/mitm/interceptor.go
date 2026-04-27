package mitm

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/burpheart/cursor-tap/internal/ca"
	"github.com/burpheart/cursor-tap/internal/httpstream"
)

// Interceptor handles TLS MITM interception.
type Interceptor struct {
	ca            *ca.CA
	keyLog        *KeyLogWriter
	dialer        *Dialer
	upstreamProxy string

	// HTTP parsing options
	enableHTTPParsing bool
	httpLogger        httpstream.Logger
	recorder          *httpstream.Recorder
	grpcRegistry      *httpstream.MessageRegistry
	onRequest         func(*httpstream.HTTPMessage)
	onResponse        func(*httpstream.HTTPMessage)
	onSSE             func(*httpstream.SSEEvent)
	onGRPC            func(*httpstream.GRPCMessage)
}

// InterceptorOption configures an Interceptor.
type InterceptorOption func(*Interceptor)

// WithHTTPParsing enables HTTP stream parsing.
func WithHTTPParsing(enable bool) InterceptorOption {
	return func(i *Interceptor) { i.enableHTTPParsing = enable }
}

// WithHTTPLogger sets the HTTP logger.
func WithHTTPLogger(logger httpstream.Logger) InterceptorOption {
	return func(i *Interceptor) { i.httpLogger = logger }
}

// WithOnRequest sets the HTTP request callback.
func WithOnRequest(fn func(*httpstream.HTTPMessage)) InterceptorOption {
	return func(i *Interceptor) { i.onRequest = fn }
}

// WithOnResponse sets the HTTP response callback.
func WithOnResponse(fn func(*httpstream.HTTPMessage)) InterceptorOption {
	return func(i *Interceptor) { i.onResponse = fn }
}

// WithOnSSE sets the SSE event callback.
func WithOnSSE(fn func(*httpstream.SSEEvent)) InterceptorOption {
	return func(i *Interceptor) { i.onSSE = fn }
}

// WithRecorder sets the JSONL recorder.
func WithRecorder(recorder *httpstream.Recorder) InterceptorOption {
	return func(i *Interceptor) { i.recorder = recorder }
}

// WithGRPCRegistry sets the gRPC message registry.
func WithGRPCRegistry(registry *httpstream.MessageRegistry) InterceptorOption {
	return func(i *Interceptor) { i.grpcRegistry = registry }
}

// WithOnGRPC sets the gRPC message callback.
func WithOnGRPC(fn func(*httpstream.GRPCMessage)) InterceptorOption {
	return func(i *Interceptor) { i.onGRPC = fn }
}

// NewInterceptor creates a new TLS interceptor.
func NewInterceptor(ca *ca.CA, keyLog *KeyLogWriter, upstreamProxy string, opts ...InterceptorOption) *Interceptor {
	i := &Interceptor{
		ca:                ca,
		keyLog:            keyLog,
		dialer:            NewDialer(upstreamProxy),
		upstreamProxy:     upstreamProxy,
		enableHTTPParsing: false,
		httpLogger:        httpstream.NopLogger{},
	}
	for _, opt := range opts {
		opt(i)
	}
	return i
}

// InterceptAuto auto-detects TLS by peeking at the first bytes (magic number detection).
func (i *Interceptor) InterceptAuto(clientConn net.Conn, targetHost string, targetPort int) error {
	peekConn := NewPeekableConn(clientConn)

	isTLS, sni, err := DetectTLSWithSNI(peekConn)
	if err != nil {
		fmt.Printf("[DEBUG] DetectTLS error for %s:%d: %v\n", targetHost, targetPort, err)
		return fmt.Errorf("detect protocol: %w", err)
	}

	if isTLS {
		host := targetHost
		if sni != "" {
			host = sni
			fmt.Printf("[DEBUG] TLS detected for %s:%d, SNI=%s, performing MITM\n", targetHost, targetPort, sni)
		} else {
			fmt.Printf("[DEBUG] TLS detected for %s:%d (no SNI), performing MITM\n", targetHost, targetPort)
		}
		return i.interceptTLS(peekConn, host, targetPort)
	}

	return i.interceptPlain(peekConn, targetHost, targetPort)
}

// Intercept performs TLS MITM on the given connection (assumes TLS).
func (i *Interceptor) Intercept(clientConn net.Conn, targetHost string, targetPort int) error {
	peekConn, ok := clientConn.(*PeekableConn)
	if !ok {
		peekConn = NewPeekableConn(clientConn)
	}
	return i.interceptTLS(peekConn, targetHost, targetPort)
}

// interceptTLS performs TLS MITM on the given connection.
func (i *Interceptor) interceptTLS(clientConn *PeekableConn, targetHost string, targetPort int) error {
	serverAddr := fmt.Sprintf("%s:%d", targetHost, targetPort)

	cert, err := i.ca.GetOrCreateCert(targetHost)
	if err != nil {
		return fmt.Errorf("get cert: %w", err)
	}

	clientTLSConfig := &tls.Config{
		Certificates: []tls.Certificate{*cert},
		NextProtos:   []string{"h2", "http/1.1"},
	}
	if i.keyLog != nil {
		clientTLSConfig.KeyLogWriter = i.keyLog
	}

	tlsClientConn := tls.Server(clientConn, clientTLSConfig)
	fmt.Printf("[DEBUG] Client TLS handshake starting for %s\n", targetHost)
	if err := tlsClientConn.Handshake(); err != nil {
		return fmt.Errorf("client handshake: %w", err)
	}
	clientProto := tlsClientConn.ConnectionState().NegotiatedProtocol
	fmt.Printf("[DEBUG] Client TLS handshake completed for %s, ALPN: %q\n", targetHost, clientProto)
	defer tlsClientConn.Close()

	fmt.Printf("[DEBUG] Connecting to server %s\n", serverAddr)
	serverTCPConn, err := i.dialer.Dial("tcp", serverAddr)
	if err != nil {
		return fmt.Errorf("dial server: %w", err)
	}
	defer serverTCPConn.Close()

	serverNextProtos := []string{"http/1.1"}
	if clientProto == "h2" {
		serverNextProtos = []string{"h2"}
	} else if clientProto == "" {
		serverNextProtos = nil
	}

	serverTLSConfig := &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         targetHost,
		NextProtos:         serverNextProtos,
	}
	if i.keyLog != nil {
		serverTLSConfig.KeyLogWriter = i.keyLog
	}

	serverConn := tls.Client(serverTCPConn, serverTLSConfig)
	fmt.Printf("[DEBUG] Server TLS handshake starting for %s\n", targetHost)
	if err := serverConn.Handshake(); err != nil {
		return fmt.Errorf("server handshake: %w", err)
	}
	serverProto := serverConn.ConnectionState().NegotiatedProtocol
	fmt.Printf("[DEBUG] Server TLS handshake completed for %s, ALPN: %q\n", targetHost, serverProto)
	defer serverConn.Close()

	if clientProto == "h2" && serverProto != "h2" {
		return fmt.Errorf("server did not negotiate h2 for h2 client (server ALPN: %q)", serverProto)
	}

	fmt.Printf("[DEBUG] Starting pipe for %s using %q\n", targetHost, clientProto)
	err = i.pipeWithProtocol(tlsClientConn, serverConn, targetHost, clientProto)
	fmt.Printf("[DEBUG] Pipe finished for %s, err=%v\n", targetHost, err)
	return err
}

// pipe performs bidirectional data forwarding with optional HTTP parsing.
func (i *Interceptor) pipe(client, server net.Conn, host string) error {
	return i.pipeWithProtocol(client, server, host, "")
}

func (i *Interceptor) pipeWithProtocol(client, server net.Conn, host, protocol string) error {
	// Use HTTP parsing if enabled
	if i.enableHTTPParsing {
		if protocol == "h2" {
			return i.pipeWithHTTP2Parsing(client, server, host)
		}
		return i.pipeWithHTTPParsing(client, server, host)
	}

	// Simple forwarding without parsing
	return i.pipeSimple(client, server)
}

// pipeSimple performs zero-buffer bidirectional data forwarding.
func (i *Interceptor) pipeSimple(client, server net.Conn) error {
	var wg sync.WaitGroup
	wg.Add(2)

	errC2S := make(chan error, 1)
	errS2C := make(chan error, 1)

	// Client -> Server
	go func() {
		defer wg.Done()
		_, err := io.Copy(server, client)
		errC2S <- err
		closeWrite(server)
	}()

	// Server -> Client
	go func() {
		defer wg.Done()
		_, err := io.Copy(client, server)
		errS2C <- err
		closeWrite(client)
	}()

	wg.Wait()

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

// pipeWithHTTPParsing performs forwarding with HTTP stream parsing.
func (i *Interceptor) pipeWithHTTPParsing(client, server net.Conn, host string) error {
	parser := i.newHTTPParser(host)
	return parser.Forward(client, server)
}

func (i *Interceptor) pipeWithHTTP2Parsing(client, server net.Conn, host string) error {
	parser := i.newHTTPParser(host)
	return parser.ForwardHTTP2(client, server)
}

func (i *Interceptor) newHTTPParser(host string) *httpstream.Parser {
	var logger httpstream.Logger = i.httpLogger

	// If recorder is set, create session logger
	var session *httpstream.Session
	if i.recorder != nil {
		session = i.recorder.NewSession(host)
		logger = session.Logger()
	}

	opts := []httpstream.ParserOption{
		httpstream.WithParserLogger(logger),
	}

	if i.onRequest != nil {
		opts = append(opts, httpstream.WithOnRequest(i.onRequest))
	}
	if i.onResponse != nil {
		opts = append(opts, httpstream.WithOnResponse(i.onResponse))
	}
	if i.onSSE != nil {
		opts = append(opts, httpstream.WithOnSSE(i.onSSE))
	}
	if i.onGRPC != nil {
		opts = append(opts, httpstream.WithOnGRPC(i.onGRPC))
	}

	// Add gRPC registry
	grpcRegistry := i.grpcRegistry
	if grpcRegistry == nil {
		grpcRegistry = httpstream.DefaultGRPCRegistry()
	}
	opts = append(opts, httpstream.WithGRPCRegistry(grpcRegistry))

	// If we have a session, use its ID
	if session != nil {
		opts = append(opts, httpstream.WithSessionID(session.ID))
	}

	return httpstream.NewParser(host, opts...)
}

// closeWrite closes the write side of a connection if supported.
func closeWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}

// InterceptPlain handles plain (non-TLS) connections.
func (i *Interceptor) InterceptPlain(clientConn net.Conn, targetHost string, targetPort int) error {
	peekConn, ok := clientConn.(*PeekableConn)
	if !ok {
		peekConn = NewPeekableConn(clientConn)
	}
	return i.interceptPlain(peekConn, targetHost, targetPort)
}

// interceptPlain handles plain (non-TLS) connections with PeekableConn.
func (i *Interceptor) interceptPlain(clientConn *PeekableConn, targetHost string, targetPort int) error {
	serverAddr := fmt.Sprintf("%s:%d", targetHost, targetPort)
	serverConn, err := i.dialer.Dial("tcp", serverAddr)
	if err != nil {
		return fmt.Errorf("dial server: %w", err)
	}
	defer serverConn.Close()

	return i.pipe(clientConn, serverConn, targetHost)
}
