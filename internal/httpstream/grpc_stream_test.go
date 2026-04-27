package httpstream

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net/http"
	"strings"
	"testing"

	agentv1 "github.com/burpheart/cursor-tap/cursor_proto/gen/agent/v1"
	aiserverv1 "github.com/burpheart/cursor-tap/cursor_proto/gen/aiserver/v1"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/protobuf/proto"
)

func TestApplicationProtoStreamingResponseUsesServiceDescriptor(t *testing.T) {
	registry := DefaultGRPCRegistry()
	info := registry.MethodInfo("aiserver.v1.ChatService", "StreamUnifiedChat")
	if !info.ServerStreaming || info.ClientStreaming {
		t.Fatalf("unexpected ChatService/StreamUnifiedChat metadata: %+v", info)
	}

	body := testGRPCFrame(t, 0, &aiserverv1.StreamUnifiedChatResponse{
		Text: "cursor tap response chunk",
	})
	body = append(body, testGRPCEnvelope(0x02, []byte(`{"metadata":{},"error":null}`))...)

	messages := ParseGRPCBody(
		body,
		"aiserver.v1.ChatService",
		"StreamUnifiedChat",
		false,
		registry,
		"application/proto",
	)

	if len(messages) != 2 {
		t.Fatalf("expected response frame plus end-stream frame, got %d", len(messages))
	}
	if messages[0].Error != "" {
		t.Fatalf("unexpected decode error: %s", messages[0].Error)
	}
	if !messages[0].IsStreaming || messages[0].FrameIndex != 0 {
		t.Fatalf("expected first message to be marked streaming frame 0: %+v", messages[0])
	}
	if !strings.Contains(messages[0].JSON, "cursor tap response chunk") {
		t.Fatalf("decoded chat response missing text: %s", messages[0].JSON)
	}
	if !strings.Contains(messages[1].JSON, "connect_end_stream") {
		t.Fatalf("end-stream envelope was not represented as JSON: %s", messages[1].JSON)
	}
}

func TestApplicationProtoInferenceStreamResponse(t *testing.T) {
	registry := DefaultGRPCRegistry()
	info := registry.MethodInfo("aiserver.v1.InferenceService", "Stream")
	if !info.ServerStreaming || info.ClientStreaming {
		t.Fatalf("unexpected InferenceService/Stream metadata: %+v", info)
	}

	body := testGRPCFrame(t, 0, &aiserverv1.InferenceStreamResponse{
		Response: &aiserverv1.InferenceStreamResponse_TextPart{
			TextPart: &aiserverv1.InferenceTextStreamPart{
				Text:    "llm response token",
				IsFinal: false,
			},
		},
	})

	messages := ParseGRPCBody(
		body,
		"aiserver.v1.InferenceService",
		"Stream",
		false,
		registry,
		"application/proto",
	)

	if len(messages) != 1 {
		t.Fatalf("expected one inference frame, got %d", len(messages))
	}
	if messages[0].Error != "" {
		t.Fatalf("unexpected decode error: %s", messages[0].Error)
	}
	if !messages[0].IsStreaming || messages[0].FrameIndex != 0 {
		t.Fatalf("expected streaming inference frame: %+v", messages[0])
	}
	if !strings.Contains(messages[0].JSON, "llm response token") {
		t.Fatalf("decoded inference response missing token: %s", messages[0].JSON)
	}
}

func TestParserEmitsEmptyUnaryProtoMessage(t *testing.T) {
	var got []*GRPCMessage
	parser := NewParser(
		"api2.cursor.sh",
		WithGRPCRegistry(DefaultGRPCRegistry()),
		WithOnGRPC(func(msg *GRPCMessage) {
			got = append(got, msg)
		}),
	)
	body := NewBodyReader(
		io.NopCloser(bytes.NewReader(nil)),
		http.Header{"Content-Type": []string{"application/proto"}},
	)

	parser.parseGRPCBody(body, "/aiserver.v1.DashboardService/GetTeams", true, "application/proto")

	if len(got) != 1 {
		t.Fatalf("expected empty unary protobuf to be emitted, got %d", len(got))
	}
	if got[0].Direction != ClientToServer || got[0].JSON != "{}" || got[0].Error != "" {
		t.Fatalf("unexpected empty unary message: %+v", got[0])
	}
}

func TestConnectJSONUnaryResponse(t *testing.T) {
	messages := ParseGRPCBody(
		[]byte(`{"text":"json response token"}`),
		"aiserver.v1.ChatService",
		"StreamUnifiedChat",
		false,
		DefaultGRPCRegistry(),
		"application/json",
	)

	if len(messages) != 1 {
		t.Fatalf("expected one json message, got %d", len(messages))
	}
	if messages[0].Error != "" {
		t.Fatalf("unexpected json decode error: %s", messages[0].Error)
	}
	if !strings.Contains(messages[0].JSON, "json response token") {
		t.Fatalf("decoded json response missing text: %s", messages[0].JSON)
	}
}

func TestConnectJSONStreamingFrame(t *testing.T) {
	body := testGRPCEnvelope(0, []byte(`{"text":"json stream token"}`))
	body = append(body, testGRPCEnvelope(0x02, []byte(`{"metadata":{},"error":null}`))...)

	messages := ParseGRPCBody(
		body,
		"aiserver.v1.ChatService",
		"StreamUnifiedChat",
		false,
		DefaultGRPCRegistry(),
		"application/connect+json",
	)

	if len(messages) != 2 {
		t.Fatalf("expected json stream frame plus end-stream frame, got %d", len(messages))
	}
	if messages[0].Error != "" || !strings.Contains(messages[0].JSON, "json stream token") {
		t.Fatalf("json stream frame did not decode: %+v", messages[0])
	}
	if !strings.Contains(messages[1].JSON, "connect_end_stream") {
		t.Fatalf("end-stream envelope was not represented as JSON: %s", messages[1].JSON)
	}
}

func TestGRPCWebTextProtoResponse(t *testing.T) {
	framed := testGRPCFrame(t, 0, &aiserverv1.StreamUnifiedChatResponse{
		Text: "grpc-web-text token",
	})
	body := []byte(base64.StdEncoding.EncodeToString(framed))

	messages := ParseGRPCBody(
		body,
		"aiserver.v1.ChatService",
		"StreamUnifiedChat",
		false,
		DefaultGRPCRegistry(),
		"application/grpc-web-text+proto",
	)

	if len(messages) != 1 {
		t.Fatalf("expected one grpc-web-text frame, got %d", len(messages))
	}
	if messages[0].Error != "" || !strings.Contains(messages[0].JSON, "grpc-web-text token") {
		t.Fatalf("grpc-web-text frame did not decode: %+v", messages[0])
	}
}

func TestRPCContentTypeDetectionKeepsOrdinaryJSONAsHTTP(t *testing.T) {
	if IsRPCRequest("application/json", "/mcp") {
		t.Fatalf("ordinary JSON endpoint should not be treated as RPC")
	}
	if !IsRPCRequest("application/json", "/aiserver.v1.ChatService/StreamUnifiedChat") {
		t.Fatalf("Connect JSON RPC endpoint should be treated as RPC")
	}
}

func TestHTTP2ParserEmitsAgentRunFrames(t *testing.T) {
	logger := &captureHTTP2Logger{}
	parser := NewParser(
		"agentn.us.api5.cursor.sh",
		WithParserLogger(logger),
		WithGRPCRegistry(DefaultGRPCRegistry()),
	)

	var c2s bytes.Buffer
	c2s.WriteString(http2.ClientPreface)
	c2sFramer := http2.NewFramer(&c2s, nil)
	if err := c2sFramer.WriteSettings(); err != nil {
		t.Fatalf("write client settings: %v", err)
	}
	writeHTTP2Headers(t, c2sFramer, 1, false, []hpack.HeaderField{
		{Name: ":method", Value: http.MethodPost},
		{Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: "agentn.us.api5.cursor.sh"},
		{Name: ":path", Value: "/agent.v1.AgentService/Run"},
		{Name: "content-type", Value: "application/grpc+proto"},
	})
	if err := c2sFramer.WriteData(1, false, testGRPCFrame(t, 0, &agentv1.AgentClientMessage{})); err != nil {
		t.Fatalf("write client data: %v", err)
	}

	parser.parseHTTP2Stream(bytes.NewReader(c2s.Bytes()), ClientToServer)

	var s2c bytes.Buffer
	s2cFramer := http2.NewFramer(&s2c, nil)
	if err := s2cFramer.WriteSettings(); err != nil {
		t.Fatalf("write server settings: %v", err)
	}
	writeHTTP2Headers(t, s2cFramer, 1, false, []hpack.HeaderField{
		{Name: ":status", Value: "200"},
		{Name: "content-type", Value: "application/grpc+proto"},
	})
	if err := s2cFramer.WriteData(1, false, testGRPCFrame(t, 0, &agentv1.AgentServerMessage{})); err != nil {
		t.Fatalf("write server data: %v", err)
	}
	writeHTTP2Headers(t, s2cFramer, 1, true, []hpack.HeaderField{
		{Name: "grpc-status", Value: "0"},
	})

	parser.parseHTTP2Stream(bytes.NewReader(s2c.Bytes()), ServerToClient)

	if len(logger.requests) != 1 || logger.requests[0].Request.Host != "agentn.us.api5.cursor.sh" {
		t.Fatalf("expected one agent h2 request, got %+v", logger.requests)
	}
	if len(logger.responses) != 1 || logger.responses[0].Response.StatusCode != http.StatusOK {
		t.Fatalf("expected one h2 response, got %+v", logger.responses)
	}
	if !logger.hasGRPC(ClientToServer, "agent.v1.AgentService", "Run") {
		t.Fatalf("missing C2S AgentService/Run grpc frame: %+v", logger.grpc)
	}
	if !logger.hasGRPC(ServerToClient, "agent.v1.AgentService", "Run") {
		t.Fatalf("missing S2C AgentService/Run grpc frame: %+v", logger.grpc)
	}
}

func testGRPCFrame(t *testing.T, flags byte, msg proto.Message) []byte {
	t.Helper()
	payload, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal %T: %v", msg, err)
	}
	return testGRPCEnvelope(flags, payload)
}

func testGRPCEnvelope(flags byte, payload []byte) []byte {
	var buf bytes.Buffer
	buf.WriteByte(flags)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	buf.Write(lenBuf[:])
	buf.Write(payload)
	return buf.Bytes()
}

func writeHTTP2Headers(t *testing.T, framer *http2.Framer, streamID uint32, endStream bool, fields []hpack.HeaderField) {
	t.Helper()
	var block bytes.Buffer
	encoder := hpack.NewEncoder(&block)
	for _, field := range fields {
		if err := encoder.WriteField(field); err != nil {
			t.Fatalf("encode header %s: %v", field.Name, err)
		}
	}
	if err := framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: block.Bytes(),
		EndHeaders:    true,
		EndStream:     endStream,
	}); err != nil {
		t.Fatalf("write headers: %v", err)
	}
}

type captureHTTP2Logger struct {
	requests  []*HTTPMessage
	responses []*HTTPMessage
	grpc      []*GRPCMessage
}

func (l *captureHTTP2Logger) LogRequest(msg *HTTPMessage) {
	l.requests = append(l.requests, msg)
}

func (l *captureHTTP2Logger) LogResponse(msg *HTTPMessage) {
	l.responses = append(l.responses, msg)
}

func (l *captureHTTP2Logger) LogSSE(host string, event *SSEEvent) {}
func (l *captureHTTP2Logger) LogBody(dir Direction, host string, data []byte) {
}
func (l *captureHTTP2Logger) LogGRPC(msg *GRPCMessage) {
	l.grpc = append(l.grpc, msg)
}
func (l *captureHTTP2Logger) Debug(format string, args ...interface{}) {}

func (l *captureHTTP2Logger) hasGRPC(dir Direction, service, method string) bool {
	for _, msg := range l.grpc {
		if msg.Direction == dir && msg.Service == service && msg.Method == method && msg.Error == "" {
			return true
		}
	}
	return false
}
