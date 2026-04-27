import { Record } from './types';

export type WSCallback = (record: Record) => void;
export type WSStatusCallback = (connected: boolean) => void;
export type WSReconnectCallback = () => void;

export class WSClient {
  private ws: WebSocket | null = null;
  private url: string;
  private onRecord: WSCallback;
  private onStatus: WSStatusCallback;
  private onReconnect?: WSReconnectCallback;
  private reconnectTimer: NodeJS.Timeout | null = null;
  private reconnectDelay = 1000;
  private maxReconnectDelay = 30000;
  private wasConnected = false;
  private reconnectAttempts = 0;
  private intentionalClose = false;

  constructor(
    url: string, 
    onRecord: WSCallback, 
    onStatus: WSStatusCallback,
    onReconnect?: WSReconnectCallback
  ) {
    this.url = url;
    this.onRecord = onRecord;
    this.onStatus = onStatus;
    this.onReconnect = onReconnect;
  }

  connect() {
    if (this.ws?.readyState === WebSocket.OPEN || this.ws?.readyState === WebSocket.CONNECTING) {
      return;
    }
    this.intentionalClose = false;

    try {
      this.ws = new WebSocket(this.url);

      this.ws.onopen = () => {
        console.log('WebSocket connected');
        this.onStatus(true);
        
        // If this is a reconnection (not first connect), trigger data recovery
        if (this.wasConnected && this.onReconnect) {
          console.log('Reconnected - recovering data...');
          this.onReconnect();
        }
        
        this.wasConnected = true;
        this.reconnectDelay = 1000;
        this.reconnectAttempts = 0;
      };

      this.ws.onmessage = (event) => {
        try {
          const record = JSON.parse(event.data) as Record;
          this.onRecord(record);
        } catch (e) {
          console.error('Failed to parse WebSocket message:', e);
        }
      };

      this.ws.onclose = () => {
        console.log('WebSocket disconnected');
        this.onStatus(false);
        if (!this.intentionalClose) {
          this.scheduleReconnect();
        }
      };

      this.ws.onerror = (error) => {
        console.error('WebSocket error:', error);
      };
    } catch (e) {
      console.error('Failed to create WebSocket:', e);
      this.scheduleReconnect();
    }
  }

  private scheduleReconnect() {
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
    }

    this.reconnectAttempts++;
    this.reconnectTimer = setTimeout(() => {
      console.log(`Reconnecting (attempt ${this.reconnectAttempts})...`);
      this.connect();
      this.reconnectDelay = Math.min(this.reconnectDelay * 2, this.maxReconnectDelay);
    }, this.reconnectDelay);
  }

  getReconnectAttempts(): number {
    return this.reconnectAttempts;
  }

  disconnect() {
    this.intentionalClose = true;
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.ws) {
      this.ws.close();
      this.ws = null;
    }
  }
}
