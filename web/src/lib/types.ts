// Record types from backend
export interface Record {
  ts: string;
  session: string;
  seq: number;
  index: number;
  type: 'request' | 'response' | 'grpc' | 'sse' | 'body' | 'error' | 'debug';

  // Request fields
  method?: string;
  url?: string;
  host?: string;

  // Response fields
  status?: number;
  status_text?: string;

  // Headers
  headers?: { [key: string]: string[] };

  // Body fields
  direction?: 'C2S' | 'S2C';
  size?: number;
  body?: string;
  body_base64?: string;
  body_encoding?: 'text' | 'base64';
  content_type?: string;

  // SSE fields
  event_type?: string;
  event_id?: string;
  event_data?: string;

  // gRPC fields
  grpc_service?: string;
  grpc_method?: string;
  grpc_data?: string;
  grpc_streaming?: boolean;
  grpc_frame_index?: number;
  grpc_compressed?: boolean;
  grpc_raw?: string;

  // Error
  error?: string;
}

export interface SessionInfo {
  id: string;
  seq: number;
  host: string;
  record_count: number;
  first_ts: string;
  last_ts: string;
  // gRPC info (extracted from records)
  grpc_service?: string;
  grpc_method?: string;
  url?: string;
  // Size stats
  request_size: number;  // Total C2S bytes
  response_size: number; // Total S2C bytes
  // First gRPC request preview
  grpc_preview?: string;
}

export interface RpcCall {
  id: string;
  seq: number;
  host: string;
  url?: string;
  service?: string;
  method?: string;
  full_method?: string;
  status: 'ok' | 'pending' | 'error' | string;
  http_status?: number;
  streaming: boolean;
  started_at: string;
  ended_at: string;
  duration_ms: number;
  record_count: number;
  frame_count: number;
  request_bytes: number;
  response_bytes: number;
  request_preview?: string;
  response_preview?: string;
  error?: string;
  origin: string;
}

export interface Capture {
  id: string;
  created_at: string;
  name: string;
  note?: string;
  call_count: number;
  record_count: number;
  records?: Record[];
}

export interface ReplayResult {
  call_id: string;
  status: number;
  status_text: string;
  headers: { [key: string]: string[] };
  records: Record[];
}

export interface ProtocolVersion {
  id: string;
  created_at: string;
  path: string;
  sha256: string;
  source_kind: string;
  messages: number;
  enums: number;
  services: number;
  diagnostics: string[];
  proto_sources: { [key: string]: string };
  active: boolean;
}

export interface RuntimeStatus {
  status: string;
  http_port: number;
  socks5_port: number;
  api_port: number;
  sqlite_path: string;
  protocol_path: string;
  active_protocol: string;
  ws_clients: number;
}

export interface RecordFilter {
  session?: string;
  type?: string;
  service?: string;
  method?: string;
  direction?: string;
  search?: string;
  limit?: number;
  offset?: number;
}

// Icon types for records
export type RecordIconType = 'request' | 'response' | 'grpc' | 'sse' | 'body' | 'error' | 'debug';

export function getRecordColor(record: Record): string {
  if (record.direction === 'C2S') {
    return 'text-blue-600';
  }
  if (record.direction === 'S2C') {
    return 'text-green-600';
  }
  switch (record.type) {
    case 'request':
      return 'text-blue-600';
    case 'response':
      return 'text-green-600';
    case 'error':
      return 'text-red-600';
    default:
      return 'text-gray-600';
  }
}

export function formatTimestamp(ts: string): string {
  const date = new Date(ts);
  return date.toLocaleTimeString('en-US', {
    hour12: false,
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    fractionalSecondDigits: 3,
  });
}

export function getRecordTitle(record: Record): string {
  switch (record.type) {
    case 'request':
      return `${record.method} ${record.url}`;
    case 'response':
      return `${record.status} ${record.status_text}`;
    case 'grpc':
      const frameInfo = record.grpc_streaming ? ` [${record.grpc_frame_index ?? 0}]` : '';
      return `${record.direction} ${record.grpc_service}/${record.grpc_method}${frameInfo}`;
    case 'sse':
      return `SSE ${record.event_type || 'message'}`;
    case 'body':
      return `Body ${record.direction} (${record.size} bytes)`;
    case 'error':
      return `Error: ${record.error}`;
    default:
      return record.type;
  }
}
