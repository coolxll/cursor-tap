package e2e

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burpheart/cursor-tap/internal/httpstream"
	"github.com/burpheart/cursor-tap/internal/proxy"
	"github.com/burpheart/cursor-tap/pkg/types"
	"github.com/gorilla/websocket"
)

type observedRequest struct {
	Path          string
	Authorization string
	Body          []byte
}

func TestProxySQLiteWebSocketEndToEnd(t *testing.T) {
	observed := make(chan observedRequest, 8)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		switch r.URL.Path {
		case "/e2e.v1.EchoService/Echo":
			if !bytes.Contains(body, []byte("ping")) {
				http.Error(w, "missing ping payload", http.StatusBadRequest)
				return
			}
			observed <- observedRequest{Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Body: body}
			w.Header().Set("Content-Type", "application/connect+proto")
			w.WriteHeader(http.StatusOK)
			w.Write(grpcFrame(protoString("pong")))
		case "/aiserver.v1.ChatService/StreamUnifiedChat":
			if !bytes.Contains(body, []byte("prompt-chat-stream")) {
				http.Error(w, "missing chat prompt payload", http.StatusBadRequest)
				return
			}
			observed <- observedRequest{Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Body: body}
			w.Header().Set("Content-Type", "application/proto")
			w.WriteHeader(http.StatusOK)
			w.Write(grpcFrame(protoString("stream-response-token")))
			w.Write(grpcEnvelope(0x02, []byte(`{"metadata":{},"error":null}`)))
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
	}))
	defer upstream.Close()

	plainObserved := make(chan observedRequest, 2)
	plainUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if r.URL.Path != "/plain-json" || !bytes.Contains(body, []byte("plain-request")) {
			http.Error(w, "unexpected plain http request", http.StatusBadRequest)
			return
		}
		plainObserved <- observedRequest{Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Body: body}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"ok":true,"message":"plain-response"}`))
	}))
	defer plainUpstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	_, upstreamPort, _ := net.SplitHostPort(upstreamURL.Host)

	tempDir := t.TempDir()
	httpPort := freePort(t)
	socksPort := freePort(t)
	apiPort := freePort(t)
	config := types.Config{
		HTTPPort:          httpPort,
		SOCKS5Port:        socksPort,
		APIPort:           apiPort,
		CertDir:           filepath.Join(tempDir, "certs"),
		DataDir:           filepath.Join(tempDir, "data"),
		SQLitePath:        filepath.Join(tempDir, "data", "cursor-tap.sqlite"),
		ProtocolPath:      filepath.Join("testdata", "cursor-fixture.js"),
		EnableHTTPParsing: true,
		HTTPLogLevel:      types.LogLevelNone,
	}

	server, err := proxy.NewServer(config)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.Start() }()
	defer server.Stop()
	waitForAPI(t, apiPort)

	wsConn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://127.0.0.1:%d/ws/records", apiPort), nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer wsConn.Close()

	if err := sendCursorLikeRequest(httpPort, "127.0.0.1", upstreamPort); err != nil {
		t.Fatalf("fake cursor request: %v", err)
	}
	firstSeen := readObserved(t, observed)
	if firstSeen.Path != "/e2e.v1.EchoService/Echo" || firstSeen.Authorization != "Bearer original-token" {
		t.Fatalf("expected original echo auth header, got path=%q auth=%q", firstSeen.Path, firstSeen.Authorization)
	}

	if err := sendCursorLikeChatStreamRequest(httpPort, "127.0.0.1", upstreamPort); err != nil {
		t.Fatalf("fake cursor chat stream request: %v", err)
	}
	chatSeen := readObserved(t, observed)
	if chatSeen.Path != "/aiserver.v1.ChatService/StreamUnifiedChat" || chatSeen.Authorization != "Bearer original-token" {
		t.Fatalf("expected original chat auth header, got path=%q auth=%q", chatSeen.Path, chatSeen.Authorization)
	}

	if err := sendCursorLikeConnectJSONRequest(httpPort, "127.0.0.1", upstreamPort); err != nil {
		t.Fatalf("fake cursor connect json request: %v", err)
	}
	jsonSeen := readObserved(t, observed)
	if jsonSeen.Path != "/e2e.v1.EchoService/Echo" || jsonSeen.Authorization != "Bearer original-token" {
		t.Fatalf("expected original json echo auth header, got path=%q auth=%q", jsonSeen.Path, jsonSeen.Authorization)
	}

	records := waitForRecords(t, apiPort)
	var sawPing, sawPong, sawChatResponse, sawJSON bool
	for _, rec := range records {
		if rec.Type == "grpc" && rec.GRPCMethod == "Echo" && strings.Contains(rec.GRPCData, "ping") {
			sawPing = true
		}
		if rec.Type == "grpc" && rec.GRPCMethod == "Echo" && strings.Contains(rec.GRPCData, "pong") {
			sawPong = true
		}
		if rec.Type == "grpc" && rec.GRPCService == "aiserver.v1.ChatService" &&
			rec.GRPCMethod == "StreamUnifiedChat" && rec.Direction == "S2C" &&
			rec.GRPCStreaming && strings.Contains(rec.GRPCData, "stream-response-token") {
			sawChatResponse = true
		}
		if rec.Type == "grpc" && rec.GRPCMethod == "Echo" && strings.Contains(rec.GRPCData, "ping-json") {
			sawJSON = true
		}
	}
	if !sawPing || !sawPong || !sawChatResponse || !sawJSON {
		t.Fatalf("expected parsed echo, json echo, and chat stream response records, sawPing=%v sawPong=%v sawChatResponse=%v sawJSON=%v records=%+v", sawPing, sawPong, sawChatResponse, sawJSON, records)
	}

	if err := sendPlainHTTPProxyRequest(httpPort, plainUpstream.URL); err != nil {
		t.Fatalf("plain http proxy request: %v", err)
	}
	plainSeen := readObserved(t, plainObserved)
	if plainSeen.Path != "/plain-json" || !bytes.Contains(plainSeen.Body, []byte("plain-request")) {
		t.Fatalf("unexpected plain upstream request: %+v", plainSeen)
	}
	if !waitForPlainHTTPRecords(t, apiPort) {
		t.Fatalf("expected plain HTTP request/response/body records")
	}

	gotWS := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		wsConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, msg, err := wsConn.ReadMessage()
		if err != nil {
			continue
		}
		var rec httpstream.Record
		if json.Unmarshal(msg, &rec) == nil && rec.SessionID != "" {
			gotWS = true
			break
		}
	}
	if !gotWS {
		t.Fatalf("expected websocket record")
	}

	sessionsResp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/sessions", apiPort))
	if err != nil {
		t.Fatalf("get sessions: %v", err)
	}
	defer sessionsResp.Body.Close()
	var sessions []map[string]interface{}
	if err := json.NewDecoder(sessionsResp.Body).Decode(&sessions); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	if len(sessions) == 0 {
		t.Fatalf("expected at least one session")
	}

	callsResp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/calls", apiPort))
	if err != nil {
		t.Fatalf("get calls: %v", err)
	}
	defer callsResp.Body.Close()
	var calls []map[string]interface{}
	if err := json.NewDecoder(callsResp.Body).Decode(&calls); err != nil {
		t.Fatalf("decode calls: %v", err)
	}
	if len(calls) == 0 {
		t.Fatalf("expected at least one call")
	}
	callID := findCallID(t, calls, "Echo")
	if callID == "" {
		t.Fatalf("echo call id was empty: %+v", calls)
	}

	framesResp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/calls/%s/frames", apiPort, callID))
	if err != nil {
		t.Fatalf("get frames: %v", err)
	}
	defer framesResp.Body.Close()
	var frames []httpstream.Record
	if err := json.NewDecoder(framesResp.Body).Decode(&frames); err != nil {
		t.Fatalf("decode frames: %v", err)
	}
	if !hasEchoGRPC(frames) {
		t.Fatalf("expected call frames to contain parsed ping and pong")
	}

	captureBody := strings.NewReader(fmt.Sprintf(`{"name":"e2e capture","sessions":["%s"]}`, callID))
	captureResp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/api/captures", apiPort), "application/json", captureBody)
	if err != nil {
		t.Fatalf("save capture: %v", err)
	}
	defer captureResp.Body.Close()
	var capture map[string]interface{}
	if err := json.NewDecoder(captureResp.Body).Decode(&capture); err != nil {
		t.Fatalf("decode capture: %v", err)
	}
	if capture["id"] == "" || capture["record_count"].(float64) == 0 {
		t.Fatalf("invalid capture response: %+v", capture)
	}

	replayPayload := fmt.Sprintf(`{
		"session_id": %q,
		"body_json": {"message":"ping-replay"},
		"headers": {
			"Content-Type": ["application/connect+proto"],
			"Connect-Protocol-Version": ["1"],
			"Authorization": ["Bearer replay-token"]
		}
	}`, callID)
	replayResp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/api/replay", apiPort), "application/json", strings.NewReader(replayPayload))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	defer replayResp.Body.Close()
	if replayResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(replayResp.Body)
		t.Fatalf("replay status %s: %s", replayResp.Status, body)
	}
	var replay map[string]interface{}
	if err := json.NewDecoder(replayResp.Body).Decode(&replay); err != nil {
		t.Fatalf("decode replay: %v", err)
	}
	if replay["call_id"] == "" || replay["status"].(float64) != 200 {
		t.Fatalf("invalid replay response: %+v", replay)
	}
	replaySeen := readObserved(t, observed)
	if replaySeen.Authorization != "Bearer replay-token" || !bytes.Contains(replaySeen.Body, []byte("ping-replay")) {
		t.Fatalf("replay did not forward edited auth/body: auth=%q body=%x", replaySeen.Authorization, replaySeen.Body)
	}

	server.Stop()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("server returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("server did not stop")
	}
}

func sendCursorLikeRequest(proxyPort int, host, port string) error {
	raw, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), 3*time.Second)
	if err != nil {
		return err
	}
	defer raw.Close()

	target := net.JoinHostPort(host, port)
	if _, err := fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		return err
	}
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("CONNECT status: %s", resp.Status)
	}

	tlsConn := tls.Client(&bufferedConn{Conn: raw, reader: br}, &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         host,
		NextProtos:         []string{"http/1.1"},
	})
	if err := tlsConn.Handshake(); err != nil {
		return err
	}
	defer tlsConn.Close()

	body := grpcFrame(protoString("ping"))
	req := &http.Request{
		Method:        http.MethodPost,
		URL:           &url.URL{Path: "/e2e.v1.EchoService/Echo"},
		Host:          target,
		Header:        make(http.Header),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	req.Header.Set("Content-Type", "application/connect+proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Authorization", "Bearer original-token")
	if err := req.Write(tlsConn); err != nil {
		return err
	}
	tlsReader := bufio.NewReader(tlsConn)
	httpResp, err := http.ReadResponse(tlsReader, req)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()
	_, err = io.ReadAll(httpResp.Body)
	return err
}

func sendCursorLikeChatStreamRequest(proxyPort int, host, port string) error {
	raw, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), 3*time.Second)
	if err != nil {
		return err
	}
	defer raw.Close()

	target := net.JoinHostPort(host, port)
	if _, err := fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		return err
	}
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("CONNECT status: %s", resp.Status)
	}

	tlsConn := tls.Client(&bufferedConn{Conn: raw, reader: br}, &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         host,
		NextProtos:         []string{"http/1.1"},
	})
	if err := tlsConn.Handshake(); err != nil {
		return err
	}
	defer tlsConn.Close()

	body := protoString("prompt-chat-stream")
	req := &http.Request{
		Method:        http.MethodPost,
		URL:           &url.URL{Path: "/aiserver.v1.ChatService/StreamUnifiedChat"},
		Host:          target,
		Header:        make(http.Header),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Authorization", "Bearer original-token")
	if err := req.Write(tlsConn); err != nil {
		return err
	}
	tlsReader := bufio.NewReader(tlsConn)
	httpResp, err := http.ReadResponse(tlsReader, req)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()
	_, err = io.ReadAll(httpResp.Body)
	return err
}

func sendCursorLikeConnectJSONRequest(proxyPort int, host, port string) error {
	raw, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), 3*time.Second)
	if err != nil {
		return err
	}
	defer raw.Close()

	target := net.JoinHostPort(host, port)
	if _, err := fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		return err
	}
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("CONNECT status: %s", resp.Status)
	}

	tlsConn := tls.Client(&bufferedConn{Conn: raw, reader: br}, &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         host,
		NextProtos:         []string{"http/1.1"},
	})
	if err := tlsConn.Handshake(); err != nil {
		return err
	}
	defer tlsConn.Close()

	body := []byte(`{"message":"ping-json"}`)
	req := &http.Request{
		Method:        http.MethodPost,
		URL:           &url.URL{Path: "/e2e.v1.EchoService/Echo"},
		Host:          target,
		Header:        make(http.Header),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Authorization", "Bearer original-token")
	if err := req.Write(tlsConn); err != nil {
		return err
	}
	tlsReader := bufio.NewReader(tlsConn)
	httpResp, err := http.ReadResponse(tlsReader, req)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()
	_, err = io.ReadAll(httpResp.Body)
	return err
}

func sendPlainHTTPProxyRequest(proxyPort int, upstreamURL string) error {
	proxyURL, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", proxyPort))
	if err != nil {
		return err
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		Timeout:   5 * time.Second,
	}
	req, err := http.NewRequest(http.MethodPost, upstreamURL+"/plain-json", strings.NewReader(`{"message":"plain-request"}`))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer plain-token")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusAccepted || !bytes.Contains(body, []byte("plain-response")) {
		return fmt.Errorf("plain response status=%s body=%s", resp.Status, body)
	}
	return nil
}

func protoString(value string) []byte {
	payload := []byte(value)
	out := []byte{0x0a, byte(len(payload))}
	return append(out, payload...)
}

func grpcFrame(payload []byte) []byte {
	return grpcEnvelope(0, payload)
}

func grpcEnvelope(flags byte, payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = flags
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

func waitForAPI(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/status", port))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("api did not become ready")
}

func waitForRecords(t *testing.T, port int) []httpstream.Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last []httpstream.Record
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/records?limit=100", port))
		if err == nil {
			var records []httpstream.Record
			err = json.NewDecoder(resp.Body).Decode(&records)
			resp.Body.Close()
			last = records
			if err == nil && hasEchoGRPC(records) && hasChatStreamGRPC(records) && hasConnectJSONGRPC(records) {
				return records
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("parsed gRPC records did not appear; last records=%+v", last)
	return nil
}

func waitForPlainHTTPRecords(t *testing.T, port int) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/records?limit=200", port))
		if err == nil {
			var records []httpstream.Record
			err = json.NewDecoder(resp.Body).Decode(&records)
			resp.Body.Close()
			if err == nil && hasPlainHTTPRecords(records) {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func readObserved(t *testing.T, observed <-chan observedRequest) observedRequest {
	t.Helper()
	select {
	case req := <-observed:
		return req
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for upstream request")
	}
	return observedRequest{}
}

func hasEchoGRPC(records []httpstream.Record) bool {
	var sawPing, sawPong bool
	for _, rec := range records {
		if rec.Type == "grpc" && rec.GRPCMethod == "Echo" && strings.Contains(rec.GRPCData, "ping") {
			sawPing = true
		}
		if rec.Type == "grpc" && rec.GRPCMethod == "Echo" && strings.Contains(rec.GRPCData, "pong") {
			sawPong = true
		}
	}
	return sawPing && sawPong
}

func hasConnectJSONGRPC(records []httpstream.Record) bool {
	for _, rec := range records {
		if rec.Type == "grpc" &&
			rec.GRPCService == "e2e.v1.EchoService" &&
			rec.GRPCMethod == "Echo" &&
			rec.Direction == "C2S" &&
			strings.Contains(rec.GRPCData, "ping-json") {
			return true
		}
	}
	return false
}

func hasChatStreamGRPC(records []httpstream.Record) bool {
	for _, rec := range records {
		if rec.Type == "grpc" &&
			rec.Direction == "S2C" &&
			rec.GRPCService == "aiserver.v1.ChatService" &&
			rec.GRPCMethod == "StreamUnifiedChat" &&
			rec.GRPCStreaming &&
			strings.Contains(rec.GRPCData, "stream-response-token") {
			return true
		}
	}
	return false
}

func hasPlainHTTPRecords(records []httpstream.Record) bool {
	var session string
	for _, rec := range records {
		if rec.Type == "request" && rec.URL == "/plain-json" && rec.Method == http.MethodPost {
			session = rec.SessionID
			break
		}
	}
	if session == "" {
		return false
	}
	var sawResponse, sawRequestBody, sawResponseBody bool
	for _, rec := range records {
		if rec.SessionID != session {
			continue
		}
		if rec.Type == "response" && rec.Status == http.StatusAccepted {
			sawResponse = true
		}
		if rec.Type == "body" && rec.Direction == "C2S" && strings.Contains(rec.Body, "plain-request") {
			sawRequestBody = true
		}
		if rec.Type == "body" && rec.Direction == "S2C" && strings.Contains(rec.Body, "plain-response") {
			sawResponseBody = true
		}
	}
	return sawResponse && sawRequestBody && sawResponseBody
}

func findCallID(t *testing.T, calls []map[string]interface{}, method string) string {
	t.Helper()
	for _, call := range calls {
		if got, _ := call["method"].(string); got == method {
			id, _ := call["id"].(string)
			return id
		}
	}
	t.Fatalf("call method %q not found in %+v", method, calls)
	return ""
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}
