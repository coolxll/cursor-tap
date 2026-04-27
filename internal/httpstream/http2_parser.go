package httpstream

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type http2StreamState struct {
	mu sync.Mutex
	id uint32

	method    string
	path      string
	authority string

	requestContentType  string
	responseContentType string
	requestIsRPC        bool
	responseIsRPC       bool
	requestFramed       bool
	responseFramed      bool

	requestBuffer  []byte
	responseBuffer []byte
	requestFrame   int
	responseFrame  int
}

// ForwardHTTP2 performs bidirectional forwarding and mirrors HTTP/2 frames for
// request, response, body, and gRPC record extraction.
func (p *Parser) ForwardHTTP2(client, server net.Conn) error {
	var wg sync.WaitGroup
	wg.Add(2)

	errC2S := make(chan error, 1)
	errS2C := make(chan error, 1)

	go func() {
		defer wg.Done()
		err := p.pipeWithHTTP2Mirror(server, client, ClientToServer)
		errC2S <- err
		closeWrite(server)
	}()

	go func() {
		defer wg.Done()
		err := p.pipeWithHTTP2Mirror(client, server, ServerToClient)
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

func (p *Parser) pipeWithHTTP2Mirror(dst io.Writer, src io.Reader, dir Direction) error {
	pr, pw := io.Pipe()
	tee := io.TeeReader(src, pw)

	parserDone := make(chan struct{})
	go func() {
		defer close(parserDone)
		p.parseHTTP2Stream(pr, dir)
		io.Copy(io.Discard, pr)
	}()

	_, err := io.Copy(dst, tee)
	pw.Close()
	<-parserDone
	return err
}

func (p *Parser) parseHTTP2Stream(r io.Reader, dir Direction) {
	if dir == ClientToServer {
		preface := make([]byte, len(http2.ClientPreface))
		if _, err := io.ReadFull(r, preface); err != nil {
			return
		}
		if !bytes.Equal(preface, []byte(http2.ClientPreface)) {
			p.logger.Debug("http2 client preface mismatch: %q", string(preface))
			return
		}
	}

	framer := http2.NewFramer(io.Discard, r)
	framer.ReadMetaHeaders = hpack.NewDecoder(64<<10, nil)

	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			if err != io.EOF {
				p.logger.Debug("http2 frame read error (%s): %v", dir.String(), err)
			}
			return
		}

		switch f := frame.(type) {
		case *http2.MetaHeadersFrame:
			p.handleHTTP2Headers(f, dir)
		case *http2.DataFrame:
			p.handleHTTP2Data(f, dir)
		case *http2.RSTStreamFrame:
			p.deleteHTTP2Stream(f.StreamID)
		}
	}
}

func (p *Parser) handleHTTP2Headers(frame *http2.MetaHeadersFrame, dir Direction) {
	headers, pseudo := splitHTTP2Headers(frame.Fields)
	streamID := frame.Header().StreamID

	if dir == ClientToServer {
		method := firstNonEmpty(pseudo[":method"], http.MethodGet)
		path := firstNonEmpty(pseudo[":path"], "/")
		authority := firstNonEmpty(pseudo[":authority"], p.host)
		reqURL := parseHTTP2URL(path)

		req := &http.Request{
			Method:        method,
			URL:           reqURL,
			Host:          authority,
			Header:        headers,
			Proto:         "HTTP/2.0",
			ProtoMajor:    2,
			ProtoMinor:    0,
			Body:          http.NoBody,
			ContentLength: -1,
		}
		msg := &HTTPMessage{
			Direction: ClientToServer,
			Request:   req,
			Host:      authority,
			Timestamp: time.Now(),
		}
		p.logger.LogRequest(msg)
		if p.onRequest != nil {
			p.onRequest(msg)
		}

		contentType := headers.Get("Content-Type")
		service, methodName, _ := ParseMethodFromURL(path)
		if p.grpcRegistry != nil {
			p.grpcRegistry.TryParseFromGlobalRegistry(service, methodName)
		}
		isRPC := method == http.MethodPost && IsRPCRequest(contentType, path)
		framed := false
		if isRPC {
			framed = p.usesEnvelopeFraming(service, methodName, true, contentType)
		}

		state := p.getHTTP2Stream(streamID)
		state.mu.Lock()
		defer state.mu.Unlock()
		state.method = method
		state.path = path
		state.authority = authority
		state.requestContentType = contentType
		state.requestIsRPC = isRPC
		state.requestFramed = framed

		if frame.StreamEnded() {
			p.finishHTTP2BodyLocked(state, ClientToServer)
		}
		return
	}

	statusCode := parseHTTP2Status(pseudo[":status"])
	if statusCode == 0 {
		p.handleHTTP2Trailers(streamID, headers)
		if frame.StreamEnded() {
			p.finishHTTP2Body(streamID, ServerToClient)
		}
		return
	}

	resp := &http.Response{
		StatusCode:    statusCode,
		Status:        fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
		Header:        headers,
		Proto:         "HTTP/2.0",
		ProtoMajor:    2,
		ProtoMinor:    0,
		Body:          http.NoBody,
		ContentLength: -1,
	}
	msg := &HTTPMessage{
		Direction: ServerToClient,
		Response:  resp,
		Host:      p.host,
		Timestamp: time.Now(),
	}
	p.logger.LogResponse(msg)
	if p.onResponse != nil {
		p.onResponse(msg)
	}

	state := p.getHTTP2Stream(streamID)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.responseContentType = headers.Get("Content-Type")
	service, methodName, _ := ParseMethodFromURL(state.path)
	contentType := firstNonEmpty(state.responseContentType, state.requestContentType)
	if p.grpcRegistry != nil {
		p.grpcRegistry.TryParseFromGlobalRegistry(service, methodName)
	}
	state.responseIsRPC = state.requestIsRPC && IsRPCRequest(contentType, state.path)
	if state.responseIsRPC {
		state.responseFramed = p.usesEnvelopeFraming(service, methodName, false, contentType)
	}

	if frame.StreamEnded() {
		p.finishHTTP2BodyLocked(state, ServerToClient)
	}
}

func (p *Parser) handleHTTP2Trailers(streamID uint32, headers http.Header) {
	if headers.Get("grpc-status") == "" {
		return
	}
	state := p.getHTTP2Stream(streamID)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.path == "" {
		return
	}
	trailerJSON := fmt.Sprintf(
		`{"grpc_status":%q,"grpc_message":%q}`,
		headers.Get("grpc-status"),
		headers.Get("grpc-message"),
	)
	service, method, _ := ParseMethodFromURL(state.path)
	msg := &GRPCMessage{
		Service:     service,
		Method:      method,
		FullMethod:  "/" + service + "/" + method,
		Direction:   ServerToClient,
		JSON:        trailerJSON,
		IsStreaming: true,
		FrameIndex:  state.responseFrame,
	}
	state.responseFrame++
	p.logger.LogGRPC(msg)
	if p.onGRPC != nil {
		p.onGRPC(msg)
	}
}

func (p *Parser) handleHTTP2Data(frame *http2.DataFrame, dir Direction) {
	streamID := frame.Header().StreamID
	data := append([]byte(nil), frame.Data()...)
	if len(data) == 0 {
		if frame.StreamEnded() {
			p.finishHTTP2Body(streamID, dir)
		}
		return
	}

	state := p.getHTTP2Stream(streamID)
	state.mu.Lock()
	defer state.mu.Unlock()
	if dir == ClientToServer {
		if state.requestIsRPC {
			state.requestBuffer = append(state.requestBuffer, data...)
			if state.requestFramed {
				p.drainHTTP2GRPCFrames(state, true)
			}
		} else {
			p.logger.LogBody(ClientToServer, firstNonEmpty(state.authority, p.host), data)
			if p.onBody != nil {
				p.onBody(ClientToServer, data)
			}
		}
	} else {
		if state.responseIsRPC {
			state.responseBuffer = append(state.responseBuffer, data...)
			if state.responseFramed {
				p.drainHTTP2GRPCFrames(state, false)
			}
		} else {
			p.logger.LogBody(ServerToClient, firstNonEmpty(state.authority, p.host), data)
			if p.onBody != nil {
				p.onBody(ServerToClient, data)
			}
		}
	}

	if frame.StreamEnded() {
		p.finishHTTP2BodyLocked(state, dir)
	}
}

func (p *Parser) finishHTTP2Body(streamID uint32, dir Direction) {
	state := p.getHTTP2Stream(streamID)
	state.mu.Lock()
	defer state.mu.Unlock()
	p.finishHTTP2BodyLocked(state, dir)
}

func (p *Parser) finishHTTP2BodyLocked(state *http2StreamState, dir Direction) {
	if dir == ClientToServer {
		if state.requestIsRPC {
			if state.requestFramed {
				p.drainHTTP2GRPCFrames(state, true)
			} else {
				p.parseHTTP2UnaryBody(state, true)
			}
		}
		return
	}

	if state.responseIsRPC {
		if state.responseFramed {
			p.drainHTTP2GRPCFrames(state, false)
		} else {
			p.parseHTTP2UnaryBody(state, false)
		}
	}
}

func (p *Parser) parseHTTP2UnaryBody(state *http2StreamState, isRequest bool) {
	service, method, _ := ParseMethodFromURL(state.path)
	contentType := state.requestContentType
	body := state.requestBuffer
	if !isRequest {
		contentType = firstNonEmpty(state.responseContentType, state.requestContentType)
		body = state.responseBuffer
	}
	messages := ParseGRPCBody(body, service, method, isRequest, p.grpcRegistry, contentType)
	for _, msg := range messages {
		p.logger.LogGRPC(msg)
		if p.onGRPC != nil {
			p.onGRPC(msg)
		}
	}
	if isRequest {
		state.requestBuffer = nil
	} else {
		state.responseBuffer = nil
	}
}

func (p *Parser) drainHTTP2GRPCFrames(state *http2StreamState, isRequest bool) {
	service, method, _ := ParseMethodFromURL(state.path)
	if p.grpcRegistry != nil {
		p.grpcRegistry.TryParseFromGlobalRegistry(service, method)
	}
	grpcParser := NewGRPCParser(p.grpcRegistry)
	buffer := state.requestBuffer
	frameIndex := state.requestFrame
	if !isRequest {
		buffer = state.responseBuffer
		frameIndex = state.responseFrame
	}

	for len(buffer) >= 5 {
		length := int(binary.BigEndian.Uint32(buffer[1:5]))
		if length > 16*1024*1024 {
			p.logger.Debug("http2 grpc frame too large on stream %d: %d", state.id, length)
			break
		}
		if len(buffer) < 5+length {
			break
		}

		flags := buffer[0]
		rawData := append([]byte(nil), buffer[5:5+length]...)
		frame := &GRPCFrame{
			Flags:      flags,
			Compressed: flags&0x01 != 0,
			EndStream:  flags&0x02 != 0 || flags&0x80 != 0,
			RawData:    rawData,
			Data:       rawData,
		}
		if frame.Compressed {
			if decompressed, err := decompressGzip(rawData); err == nil {
				frame.Data = decompressed
			} else {
				frame.Data = nil
			}
		}

		msg := grpcParser.ParseMessage(frame, service, method, isRequest)
		msg.IsStreaming = true
		msg.FrameIndex = frameIndex
		msg.Compressed = frame.Compressed
		frameIndex++

		p.logger.LogGRPC(msg)
		if p.onGRPC != nil {
			p.onGRPC(msg)
		}

		buffer = buffer[5+length:]
	}

	if isRequest {
		state.requestBuffer = buffer
		state.requestFrame = frameIndex
	} else {
		state.responseBuffer = buffer
		state.responseFrame = frameIndex
	}
}

func (p *Parser) usesEnvelopeFraming(service, method string, isRequest bool, contentType string) bool {
	if p.grpcRegistry != nil {
		return p.grpcRegistry.UsesEnvelopeFraming(service, method, isRequest, contentType)
	}
	return ParseContentType(contentType).HasEnvelopeFraming()
}

func (p *Parser) getHTTP2Stream(id uint32) *http2StreamState {
	p.http2Mutex.Lock()
	defer p.http2Mutex.Unlock()
	state := p.http2Streams[id]
	if state == nil {
		state = &http2StreamState{id: id}
		p.http2Streams[id] = state
	}
	return state
}

func (p *Parser) deleteHTTP2Stream(id uint32) {
	p.http2Mutex.Lock()
	delete(p.http2Streams, id)
	p.http2Mutex.Unlock()
}

func splitHTTP2Headers(fields []hpack.HeaderField) (http.Header, map[string]string) {
	headers := make(http.Header)
	pseudo := make(map[string]string)
	for _, field := range fields {
		name := strings.ToLower(field.Name)
		if strings.HasPrefix(name, ":") {
			pseudo[name] = field.Value
			continue
		}
		headers.Add(http.CanonicalHeaderKey(name), field.Value)
	}
	return headers, pseudo
}

func parseHTTP2URL(path string) *url.URL {
	u, err := url.ParseRequestURI(path)
	if err == nil {
		return u
	}
	return &url.URL{Path: path}
}

func parseHTTP2Status(status string) int {
	if status == "" {
		return 0
	}
	code, err := strconv.Atoi(status)
	if err != nil {
		return 0
	}
	return code
}
