'use client';

import { useEffect, useRef } from 'react';
import { WSClient } from '@/lib/ws-client';
import { Record } from '@/lib/types';

const WS_URL = process.env.NEXT_PUBLIC_WS_URL || 'ws://localhost:9090/ws/records';

export function useWebSocket(
  onRecord: (record: Record) => void,
  onStatus: (connected: boolean) => void,
  onReconnect?: () => void
) {
  const clientRef = useRef<WSClient | null>(null);
  const onRecordRef = useRef(onRecord);
  const onStatusRef = useRef(onStatus);
  const onReconnectRef = useRef(onReconnect);

  useEffect(() => {
    onRecordRef.current = onRecord;
  }, [onRecord]);

  useEffect(() => {
    onStatusRef.current = onStatus;
  }, [onStatus]);

  useEffect(() => {
    onReconnectRef.current = onReconnect;
  }, [onReconnect]);

  useEffect(() => {
    const clientID = createClientID();
    const client = new WSClient(
      withClientID(WS_URL, clientID),
      (record) => onRecordRef.current(record),
      (connected) => onStatusRef.current(connected),
      () => onReconnectRef.current?.()
    );
    clientRef.current = client;
    client.connect();

    return () => {
      client.disconnect();
      clientRef.current = null;
    };
  }, []);
}

function createClientID() {
  if (typeof window !== 'undefined' && window.crypto?.randomUUID) {
    return window.crypto.randomUUID();
  }
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

function withClientID(rawURL: string, clientID: string) {
  const url = new URL(rawURL);
  url.searchParams.set('client_id', clientID);
  return url.toString();
}
