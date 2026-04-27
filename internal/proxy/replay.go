package proxy

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/burpheart/cursor-tap/internal/httpstream"
	"github.com/burpheart/cursor-tap/internal/storage"
)

type captureRequest struct {
	Name     string   `json:"name"`
	Note     string   `json:"note"`
	Sessions []string `json:"sessions"`
}

type replayRequest struct {
	SessionID   string              `json:"session_id"`
	RecordIndex int64               `json:"record_index"`
	BodyJSON    json.RawMessage     `json:"body_json"`
	RawBase64   string              `json:"raw_base64"`
	Headers     map[string][]string `json:"headers"`
}

type replayResult struct {
	CallID     string              `json:"call_id"`
	Status     int                 `json:"status"`
	StatusText string              `json:"status_text"`
	Headers    map[string][]string `json:"headers"`
	Records    []httpstream.Record `json:"records"`
}

func (s *Server) registerInspectorRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/calls", s.handleCalls)
	mux.HandleFunc("/api/calls/", s.handleCallFrames)
	mux.HandleFunc("/api/captures", s.handleCaptures)
	mux.HandleFunc("/api/replay", s.handleReplay)
}

func (s *Server) handleCalls(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		writeCORS(w)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.store == nil {
		writeJSON(w, []storage.CallRow{})
		return
	}

	query := r.URL.Query()
	filter := storage.CallFilter{
		Limit:         parseLimit(r, 500),
		Query:         firstNonEmpty(query.Get("search"), query.Get("q")),
		Kind:          query.Get("kind"),
		Status:        query.Get("status"),
		Service:       query.Get("service"),
		Method:        query.Get("method"),
		StartedAfter:  firstNonEmpty(query.Get("started_after"), query.Get("from"), query.Get("start")),
		StartedBefore: firstNonEmpty(query.Get("started_before"), query.Get("to"), query.Get("end")),
	}
	calls, err := s.store.ListCalls(filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, calls)
}

func (s *Server) handleCallFrames(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		writeCORS(w)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.store == nil {
		writeJSON(w, []httpstream.Record{})
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/calls/")
	sessionID := strings.TrimSuffix(path, "/frames")
	if sessionID == "" || sessionID == path {
		http.NotFound(w, r)
		return
	}
	frames, err := s.store.SessionRecords(sessionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, frames)
}

func (s *Server) handleCaptures(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		writeCORS(w)
		return
	}
	if s.store == nil {
		http.Error(w, "database is not available", http.StatusServiceUnavailable)
		return
	}

	switch r.Method {
	case http.MethodGet:
		id := r.URL.Query().Get("id")
		if id != "" {
			capture, err := s.store.GetCapture(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			writeJSON(w, capture)
			return
		}
		captures, err := s.store.ListCaptures(parseLimit(r, 100))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, captures)
	case http.MethodPost:
		var req captureRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		capture, err := s.store.SaveCapture(req.Name, req.Note, req.Sessions)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, capture)
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "id is required", http.StatusBadRequest)
			return
		}
		if err := s.store.DeleteCapture(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		writeCORS(w)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.store == nil {
		http.Error(w, "database is not available", http.StatusServiceUnavailable)
		return
	}

	var req replayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	result, err := s.replay(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, result)
}

func (s *Server) replay(req replayRequest) (replayResult, error) {
	if req.SessionID == "" {
		return replayResult{}, fmt.Errorf("session_id is required")
	}
	records, err := s.store.SessionRecords(req.SessionID)
	if err != nil {
		return replayResult{}, err
	}
	if len(records) == 0 {
		return replayResult{}, fmt.Errorf("session %s was not found", req.SessionID)
	}

	requestRec, grpcRec, err := selectReplayRecords(records, req.RecordIndex)
	if err != nil {
		return replayResult{}, err
	}

	service := grpcRec.GRPCService
	method := grpcRec.GRPCMethod
	if service == "" || method == "" {
		service, method, _ = httpstream.ParseMethodFromURL(requestRec.URL)
	}
	if service == "" || method == "" {
		return replayResult{}, fmt.Errorf("could not infer grpc method")
	}

	headers := cloneHeaderMap(requestRec.Headers)
	for key, values := range req.Headers {
		headers[key] = append([]string(nil), values...)
	}
	contentType := firstNonEmpty(headerValue(headers, "Content-Type"), requestRec.ContentType, "application/connect+proto")
	payload, payloadJSON, err := s.replayPayload(req, grpcRec, service, method, contentType)
	if err != nil {
		return replayResult{}, err
	}

	targetURL, err := replayURL(requestRec)
	if err != nil {
		return replayResult{}, err
	}

	httpReq, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(payload))
	if err != nil {
		return replayResult{}, err
	}
	for key, values := range headers {
		for _, value := range values {
			httpReq.Header.Add(key, value)
		}
	}
	httpReq.Header.Del("Content-Length")
	httpReq.Header.Del("Proxy-Connection")
	httpReq.Header.Set("Content-Type", contentType)
	httpReq.ContentLength = int64(len(payload))

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return replayResult{}, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return replayResult{}, err
	}

	replaySession := "replay-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seq := time.Now().UnixNano()
	now := time.Now().Format(time.RFC3339Nano)
	outRecords := []httpstream.Record{
		{
			Timestamp:   now,
			SessionID:   replaySession,
			SessionSeq:  seq,
			RecordIndex: 1,
			Type:        "request",
			Method:      http.MethodPost,
			URL:         requestRec.URL,
			Host:        requestRec.Host,
			Headers:     headers,
			ContentType: contentType,
		},
		{
			Timestamp:      now,
			SessionID:      replaySession,
			SessionSeq:     seq,
			RecordIndex:    2,
			Type:           "grpc",
			Direction:      "C2S",
			Host:           requestRec.Host,
			URL:            "/" + service + "/" + method,
			GRPCService:    service,
			GRPCMethod:     method,
			GRPCData:       payloadJSON,
			GRPCStreaming:  httpstream.ParseContentType(contentType).HasEnvelopeFraming(),
			GRPCFrameIndex: 0,
			GRPCRawData:    rawPayloadBase64(req, grpcRec, payload, contentType),
			Size:           len(payload),
			ContentType:    contentType,
		},
		{
			Timestamp:   time.Now().Format(time.RFC3339Nano),
			SessionID:   replaySession,
			SessionSeq:  seq,
			RecordIndex: 3,
			Type:        "response",
			Status:      resp.StatusCode,
			StatusText:  resp.Status,
			Host:        requestRec.Host,
			Headers:     cloneHeaderMap(resp.Header),
			ContentType: resp.Header.Get("Content-Type"),
		},
	}

	responseContentType := resp.Header.Get("Content-Type")
	parsed := httpstream.ParseGRPCBody(respBody, service, method, false, s.registry, responseContentType)
	for _, msg := range parsed {
		idx := int64(len(outRecords) + 1)
		rec := httpstream.Record{
			Timestamp:      time.Now().Format(time.RFC3339Nano),
			SessionID:      replaySession,
			SessionSeq:     seq,
			RecordIndex:    idx,
			Type:           "grpc",
			Direction:      "S2C",
			Host:           requestRec.Host,
			URL:            "/" + service + "/" + method,
			GRPCService:    service,
			GRPCMethod:     method,
			GRPCData:       msg.JSON,
			GRPCStreaming:  httpstream.ParseContentType(responseContentType).HasEnvelopeFraming(),
			GRPCFrameIndex: msg.FrameIndex,
			GRPCCompressed: msg.Compressed,
			Error:          msg.Error,
			ContentType:    responseContentType,
		}
		if msg.Frame != nil {
			rec.Size = len(msg.Frame.Data)
			if len(msg.Frame.Data) > 0 {
				rec.GRPCRawData = base64.StdEncoding.EncodeToString(msg.Frame.Data)
			}
		}
		outRecords = append(outRecords, rec)
	}
	if len(parsed) == 0 && len(respBody) > 0 {
		outRecords = append(outRecords, httpstream.Record{
			Timestamp:    time.Now().Format(time.RFC3339Nano),
			SessionID:    replaySession,
			SessionSeq:   seq,
			RecordIndex:  int64(len(outRecords) + 1),
			Type:         "body",
			Direction:    "S2C",
			Host:         requestRec.Host,
			Size:         len(respBody),
			BodyBase64:   base64.StdEncoding.EncodeToString(respBody),
			BodyEncoding: "base64",
			ContentType:  responseContentType,
		})
	}

	for _, rec := range outRecords {
		if err := s.store.SaveRecord(rec); err != nil {
			return replayResult{}, err
		}
		if s.hub != nil {
			s.hub.Broadcast(rec)
		}
	}

	return replayResult{
		CallID:     replaySession,
		Status:     resp.StatusCode,
		StatusText: resp.Status,
		Headers:    cloneHeaderMap(resp.Header),
		Records:    outRecords,
	}, nil
}

func selectReplayRecords(records []httpstream.Record, recordIndex int64) (httpstream.Record, httpstream.Record, error) {
	var requestRec httpstream.Record
	var grpcRec httpstream.Record
	for _, rec := range records {
		if rec.Type == "request" && requestRec.Type == "" {
			requestRec = rec
		}
		if rec.Type == "grpc" && rec.Direction == "C2S" {
			if recordIndex == 0 || rec.RecordIndex == recordIndex {
				grpcRec = rec
				if requestRec.Type != "" {
					break
				}
			}
		}
	}
	if requestRec.Type == "" {
		return requestRec, grpcRec, fmt.Errorf("request record was not found")
	}
	if grpcRec.Type == "" {
		return requestRec, grpcRec, fmt.Errorf("client grpc frame was not found")
	}
	return requestRec, grpcRec, nil
}

func (s *Server) replayPayload(req replayRequest, rec httpstream.Record, service, method, contentType string) ([]byte, string, error) {
	if req.RawBase64 != "" {
		raw, err := base64.StdEncoding.DecodeString(req.RawBase64)
		if err != nil {
			return nil, "", err
		}
		return framePayload(raw, contentType), rec.GRPCData, nil
	}

	jsonPayload := normalizeJSON(req.BodyJSON)
	if jsonPayload == "" {
		jsonPayload = rec.GRPCData
	}
	if jsonPayload != "" {
		raw, err := s.registry.MarshalJSON(service, method, true, []byte(jsonPayload))
		if err != nil {
			return nil, "", err
		}
		return framePayload(raw, contentType), jsonPayload, nil
	}
	if rec.GRPCRawData == "" {
		return nil, "", fmt.Errorf("record has no replayable grpc payload")
	}
	raw, err := base64.StdEncoding.DecodeString(rec.GRPCRawData)
	if err != nil {
		return nil, "", err
	}
	return framePayload(raw, contentType), rec.GRPCData, nil
}

func normalizeJSON(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	return string(raw)
}

func replayURL(rec httpstream.Record) (string, error) {
	if strings.HasPrefix(rec.URL, "http://") || strings.HasPrefix(rec.URL, "https://") {
		return rec.URL, nil
	}
	host := rec.Host
	if host == "" {
		host = headerValue(rec.Headers, "Host")
	}
	if host == "" {
		return "", fmt.Errorf("request host is missing")
	}
	path := rec.URL
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := url.URL{Scheme: "https", Host: host}
	if queryAt := strings.Index(path, "?"); queryAt >= 0 {
		u.Path = path[:queryAt]
		u.RawQuery = path[queryAt+1:]
	} else {
		u.Path = path
	}
	return u.String(), nil
}

func framePayload(raw []byte, contentType string) []byte {
	if !httpstream.ParseContentType(contentType).HasEnvelopeFraming() {
		return raw
	}
	out := make([]byte, 5+len(raw))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(raw)))
	copy(out[5:], raw)
	return out
}

func rawPayloadBase64(req replayRequest, rec httpstream.Record, payload []byte, contentType string) string {
	if req.RawBase64 != "" {
		return req.RawBase64
	}
	if rec.GRPCRawData != "" && len(req.BodyJSON) == 0 {
		return rec.GRPCRawData
	}
	if httpstream.ParseContentType(contentType).HasEnvelopeFraming() && len(payload) >= 5 {
		return base64.StdEncoding.EncodeToString(payload[5:])
	}
	return base64.StdEncoding.EncodeToString(payload)
}

func cloneHeaderMap(headers map[string][]string) map[string][]string {
	if headers == nil {
		return map[string][]string{}
	}
	out := make(map[string][]string, len(headers))
	for key, values := range headers {
		out[key] = append([]string(nil), values...)
	}
	return out
}

func headerValue(headers map[string][]string, key string) string {
	for header, values := range headers {
		if strings.EqualFold(header, key) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
