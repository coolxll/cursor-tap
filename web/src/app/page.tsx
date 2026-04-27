'use client';

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { HTMLAttributes, KeyboardEvent as ReactKeyboardEvent, MouseEvent as ReactMouseEvent, ReactNode, Ref, UIEvent as ReactUIEvent } from 'react';
import {
  Button,
  Chip,
  Input,
  ScrollShadow,
} from '@heroui/react';
import {
  Activity,
  AlertTriangle,
  Archive,
  BoxSelect,
  CheckCircle2,
  ChevronRight,
  Circle,
  CircleDot,
  Clock3,
  Code2,
  Copy,
  FileCode2,
  Filter,
  HelpCircle,
  ListTree,
  Moon,
  Pause,
  Play,
  Radio,
  RefreshCw,
  Rocket,
  Save,
  Search,
  Send,
  Server,
  Sparkles,
  Sun,
  Trash2,
  Wand2,
  X,
} from 'lucide-react';
import { CodeBlock } from '@/components/code-block';
import { ResizablePanels } from '@/components/resizable-panels';
import { SequenceDiagram } from '@/components/sequence-diagram';
import { useWebSocket } from '@/hooks/use-websocket';
import type { Capture, ProtocolVersion, Record, ReplayResult, RpcCall, RuntimeStatus } from '@/lib/types';
import { formatTimestamp, getRecordTitle } from '@/lib/types';
import { cn } from '@/lib/utils';

const API_BASE = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:9090';
const ENABLE_TOP_TIMELINE = process.env.NEXT_PUBLIC_ENABLE_TOP_TIMELINE === 'true';
const TIMELINE_CALL_LIMIT = 240;
const LIST_LAZY_INITIAL = 80;
const LIST_LAZY_CHUNK = 80;
const LIST_LAZY_THRESHOLD_PX = 480;
const RAW_HIGHLIGHT_PREVIEW_CHARS = 100_000;
const RAW_LARGE_STRING_CHARS = 12_000;
const RAW_LARGE_ARRAY_ITEMS = 120;
const RAW_LARGE_OBJECT_KEYS = 160;

type DesktopBridge = {
  StartProxy: () => Promise<void>;
  StopProxy: () => Promise<void>;
  PickCursorJS: () => Promise<string>;
  PickCursorApp: () => Promise<string>;
  LoadProtocolFromJS: (path: string) => Promise<ProtocolVersion>;
  LaunchCursor: (path: string) => Promise<void>;
};

type WailsWindow = Window & {
  go?: {
    main?: {
      DesktopApp?: DesktopBridge;
    };
  };
};

type FilterKind = 'all' | 'streaming' | 'unary' | 'errors';

function desktopBridge() {
  return (window as WailsWindow).go?.main?.DesktopApp;
}

function formatDesktopError(error: unknown) {
  if (error instanceof Error) return error.message;
  if (typeof error === 'string') return error;
  try {
    return JSON.stringify(error);
  } catch {
    return 'Desktop action failed';
  }
}

async function fetchJSON<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(`${API_BASE}${path}`, init);
  if (!response.ok) {
    throw new Error(await response.text());
  }
  return response.json() as Promise<T>;
}

function safeFilename(value: string, fallback: string) {
  const clean = value.trim().replace(/[^a-z0-9_-]+/gi, '-').replace(/^-+|-+$/g, '');
  return clean || fallback;
}

function recordsToJSONL(value: Record[]) {
  if (value.length === 0) return '';
  return `${value.map((record) => JSON.stringify(record)).join('\n')}\n`;
}

function downloadTextFile(filename: string, value: string, type: string) {
  const blob = new Blob([value], { type });
  const href = URL.createObjectURL(blob);
  const anchor = document.createElement('a');
  anchor.href = href;
  anchor.download = filename;
  anchor.click();
  URL.revokeObjectURL(href);
}

export default function Home() {
  const [records, setRecords] = useState<Record[]>([]);
  const [calls, setCalls] = useState<RpcCall[]>([]);
  const [frames, setFrames] = useState<Record[]>([]);
  const [captures, setCaptures] = useState<Capture[]>([]);
  const [protocols, setProtocols] = useState<ProtocolVersion[]>([]);
  const [status, setStatus] = useState<RuntimeStatus | null>(null);
  const [kind, setKind] = useState<FilterKind>('all');
  const [search, setSearch] = useState('');
  const [debouncedSearch, setDebouncedSearch] = useState('');
  const [selectedService, setSelectedService] = useState<string | null>(null);
  const [selectedMethod, setSelectedMethod] = useState<string | null>(null);
  const [timeFrom, setTimeFrom] = useState('');
  const [timeTo, setTimeTo] = useState('');
  const [selectedCallID, setSelectedCallID] = useState<string | null>(null);
  const [selectedRecordKey, setSelectedRecordKey] = useState<string | null>(null);
  const [framesLoading, setFramesLoading] = useState(false);
  const [, setIsConnected] = useState(false);
  const [isPaused, setIsPaused] = useState(false);
  const [liveTail, setLiveTail] = useState(true);
  const [timelineCursor, setTimelineCursor] = useState<number | null>(null);
  const [isRecordingCapture, setIsRecordingCapture] = useState(false);
  const [recordingSessions, setRecordingSessions] = useState<string[]>([]);
  const [busy, setBusy] = useState('');
  const [showPalette, setShowPalette] = useState(false);
  const [showCaptures, setShowCaptures] = useState(false);
  const [showReplay, setShowReplay] = useState(false);
  const [captureName, setCaptureName] = useState('');
  const [activeCapture, setActiveCapture] = useState<Capture | null>(null);
  const [replayHeaders, setReplayHeaders] = useState('{}');
  const [replayBody, setReplayBody] = useState('{}');
  const [replayResult, setReplayResult] = useState<ReplayResult | null>(null);
  const [theme, setTheme] = useState<'light' | 'dark'>('light');
  const [serviceCatalogCalls, setServiceCatalogCalls] = useState<RpcCall[]>([]);
  const [clearWatermark, setClearWatermark] = useState<string | null>(null);
  const refreshCallsInFlight = useRef(false);
  const refreshCallsPending = useRef(false);
  const latestRefreshCalls = useRef<() => void>(() => {});
  const refreshCallsTimer = useRef<number | null>(null);
  const isClearingTraffic = useRef(false);
  const callsRequestID = useRef(0);
  const frameRequestID = useRef(0);
  const manualCallSelectionRef = useRef(false);
  const selectedCallIDRef = useRef<string | null>(null);
  const selectedRecordKeyRef = useRef<string | null>(null);
  const framesRef = useRef<Record[]>([]);
  const clearWatermarkRef = useRef<string | null>(null);

  useEffect(() => {
    const timer = window.setTimeout(() => setDebouncedSearch(search.trim()), 350);
    return () => window.clearTimeout(timer);
  }, [search]);

  const hasScopedCallFilters = useMemo(() => (
    debouncedSearch !== '' ||
    kind !== 'all' ||
    selectedService !== null ||
    selectedMethod !== null ||
    timeFrom !== '' ||
    timeTo !== ''
  ), [debouncedSearch, kind, selectedMethod, selectedService, timeFrom, timeTo]);

  const queryString = useMemo(() => {
    const params = new URLSearchParams({ limit: '500' });
    if (debouncedSearch) params.set('search', debouncedSearch);
    if (kind !== 'all') params.set('kind', kind);
    if (selectedService) params.set('service', selectedService);
    if (selectedMethod) params.set('method', selectedMethod);
    const from = maxAPITime(toAPITime(timeFrom), clearWatermark || '');
    const to = toAPITime(timeTo);
    if (from) params.set('started_after', from);
    if (to) params.set('started_before', to);
    return params.toString();
  }, [clearWatermark, debouncedSearch, kind, selectedMethod, selectedService, timeFrom, timeTo]);

  const refreshRuntime = useCallback(async () => {
    try {
      const [statusData, protocolData, captureData] = await Promise.all([
        fetchJSON<RuntimeStatus>('/api/status'),
        fetchJSON<ProtocolVersion[] | null>('/api/protocols'),
        fetchJSON<Capture[] | null>('/api/captures').catch(() => []),
      ]);
      setStatus(statusData);
      setProtocols(Array.isArray(protocolData) ? protocolData : []);
      setCaptures(Array.isArray(captureData) ? captureData : []);
    } catch {
      setStatus(null);
    }
  }, []);

  const refreshCalls = useCallback(async () => {
    if (isClearingTraffic.current) {
      return;
    }
    if (refreshCallsInFlight.current) {
      refreshCallsPending.current = true;
      return;
    }
    refreshCallsInFlight.current = true;
    const requestID = ++callsRequestID.current;
    try {
      const data = await fetchJSON<RpcCall[]>(`/api/calls?${queryString}`);
      if (requestID !== callsRequestID.current) {
        return;
      }
      const visibleCalls = clearWatermarkRef.current
        ? data.filter((call) => isAtOrAfterWatermark(call.started_at || call.ended_at, clearWatermarkRef.current))
        : data;
      setCalls(visibleCalls);
      if (!hasScopedCallFilters) {
        setServiceCatalogCalls(visibleCalls);
      } else {
        setServiceCatalogCalls((current) => (current.length ? current : visibleCalls));
      }
      setSelectedCallID((current) => {
        if (visibleCalls.length === 0) {
          manualCallSelectionRef.current = false;
          return null;
        }
        const currentStillVisible = current && visibleCalls.some((call) => call.id === current);
        if (currentStillVisible && manualCallSelectionRef.current) {
          return current;
        }
        if (currentStillVisible && (!liveTail || manualCallSelectionRef.current)) {
          return current;
        }
        if (!currentStillVisible) {
          manualCallSelectionRef.current = false;
        }
        if (!current || !currentStillVisible || liveTail) {
          return visibleCalls[0]?.id ?? null;
        }
        return current;
      });
    } catch {
      if (requestID !== callsRequestID.current) {
        return;
      }
      setCalls([]);
      setSelectedCallID(null);
    } finally {
      refreshCallsInFlight.current = false;
      if (refreshCallsPending.current) {
        refreshCallsPending.current = false;
        window.setTimeout(() => latestRefreshCalls.current(), 0);
      }
    }
  }, [hasScopedCallFilters, liveTail, queryString]);

  useEffect(() => {
    latestRefreshCalls.current = refreshCalls;
  }, [refreshCalls]);

  const scheduleRefreshCalls = useCallback((delay = 900) => {
    if (refreshCallsTimer.current !== null) {
      return;
    }
    refreshCallsTimer.current = window.setTimeout(() => {
      refreshCallsTimer.current = null;
      refreshCalls();
    }, delay);
  }, [refreshCalls]);

  const refreshRecords = useCallback(async () => {
    try {
      const data = await fetchJSON<Record[]>('/api/records?limit=100');
      const visibleRecords = clearWatermarkRef.current
        ? data.filter((record) => isAtOrAfterWatermark(record.ts, clearWatermarkRef.current))
        : data;
      setRecords(visibleRecords);
    } catch {
      setRecords([]);
    }
  }, []);

  const refreshSelectedFrames = useCallback(async (callID: string | null) => {
    const requestID = ++frameRequestID.current;
    if (!callID) {
      setFrames([]);
      setSelectedRecordKey(null);
      setFramesLoading(false);
      return;
    }
    setFramesLoading(true);
    setFrames([]);
    setSelectedRecordKey(null);
    try {
      const data = await fetchJSON<Record[]>(`/api/calls/${encodeURIComponent(callID)}/frames`);
      if (requestID !== frameRequestID.current) {
        return;
      }
      setFrames(data);
      setSelectedRecordKey(recordKey(pickInitialFrame(data)));
    } catch {
      if (requestID !== frameRequestID.current) {
        return;
      }
      setFrames([]);
      setSelectedRecordKey(null);
    } finally {
      if (requestID === frameRequestID.current) {
        setFramesLoading(false);
      }
    }
  }, []);

  useEffect(() => {
    refreshRuntime();
    refreshRecords();
    const timer = window.setInterval(refreshRuntime, 5000);
    return () => window.clearInterval(timer);
  }, [refreshRecords, refreshRuntime]);

  useEffect(() => {
    if (isPaused) {
      return;
    }
    refreshCalls();
    const timer = window.setInterval(refreshCalls, hasScopedCallFilters ? 10000 : 5000);
    return () => window.clearInterval(timer);
  }, [hasScopedCallFilters, isPaused, refreshCalls]);

  useEffect(() => {
    return () => {
      if (refreshCallsTimer.current !== null) {
        window.clearTimeout(refreshCallsTimer.current);
      }
    };
  }, []);

  useEffect(() => {
    refreshSelectedFrames(selectedCallID);
  }, [refreshSelectedFrames, selectedCallID]);

  useEffect(() => {
    selectedCallIDRef.current = selectedCallID;
  }, [selectedCallID]);

  useEffect(() => {
    selectedRecordKeyRef.current = selectedRecordKey;
  }, [selectedRecordKey]);

  useEffect(() => {
    framesRef.current = frames;
  }, [frames]);

  useEffect(() => {
    clearWatermarkRef.current = clearWatermark;
  }, [clearWatermark]);

  useEffect(() => {
    document.documentElement.classList.toggle('dark', theme === 'dark');
  }, [theme]);

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') {
        event.preventDefault();
        setShowPalette(true);
      }
      if (event.key === 'Escape') {
        setShowPalette(false);
        setShowCaptures(false);
        setShowReplay(false);
      }
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, []);

  const onRecord = useCallback((record: Record) => {
    if (isPaused) return;
    if (!isAtOrAfterWatermark(record.ts, clearWatermarkRef.current)) return;
    setRecords((current) => appendRecord(current, record, 2000));
    if (record.session === selectedCallIDRef.current) {
      const currentFrames = framesRef.current;
      const currentSelectedKey = selectedRecordKeyRef.current;
      const lastFrameKey = recordKey(currentFrames[currentFrames.length - 1]);
      const shouldFollowFrame = liveTail && (!currentSelectedKey || currentSelectedKey === lastFrameKey);
      setFrames((current) => appendRecord(current, record, 1000));
      if (shouldFollowFrame) {
        setSelectedRecordKey(recordKey(record));
      }
    }
    if (isRecordingCapture && record.session) {
      setRecordingSessions((current) => (current.includes(record.session) ? current : [...current, record.session]));
    }
    scheduleRefreshCalls();
  }, [isPaused, isRecordingCapture, liveTail, scheduleRefreshCalls]);

  const recoverAfterReconnect = useCallback(() => {
    refreshRecords();
    scheduleRefreshCalls(0);
  }, [refreshRecords, scheduleRefreshCalls]);

  useWebSocket(onRecord, setIsConnected, recoverAfterReconnect);

  const activeProtocol = useMemo(() => protocols.find((protocol) => protocol.active) || protocols[0] || null, [protocols]);
  const selectedCall = useMemo(() => calls.find((call) => call.id === selectedCallID) || null, [calls, selectedCallID]);
  const selectedRecord = useMemo(() => {
    return frames.find((record) => recordKey(record) === selectedRecordKey) || null;
  }, [frames, selectedRecordKey]);

  const serviceGroupsSource = serviceCatalogCalls.length ? serviceCatalogCalls : calls;
  const serviceGroups = useMemo(() => buildServiceGroups(serviceGroupsSource), [serviceGroupsSource]);
  const stats = useMemo(() => buildStats(calls, records), [calls, records]);
  const timelineCalls = useMemo(() => (
    ENABLE_TOP_TIMELINE ? calls.slice(0, TIMELINE_CALL_LIMIT).reverse() : []
  ), [calls]);
  const captureWindow = useMemo(() => timelineRange(timelineCalls), [timelineCalls]);
  const activeCallsAtCursor = useMemo(() => {
    if (!ENABLE_TOP_TIMELINE || timelineCursor == null) return [];
    return timelineCalls.filter((call) => callStart(call) <= timelineCursor && callEnd(call) >= timelineCursor);
  }, [timelineCalls, timelineCursor]);
  const paletteItems = useMemo(() => buildPaletteItems(serviceGroups, calls, records), [calls, records, serviceGroups]);

  const selectCall = useCallback((callID: string, manual = true) => {
    if (manual) {
      manualCallSelectionRef.current = true;
      setLiveTail(false);
    }
    setSelectedCallID((current) => (current === callID ? current : callID));
  }, []);

  const runDesktopAction = async (name: string, action: (bridge: DesktopBridge) => Promise<unknown>) => {
    const bridge = desktopBridge();
    if (!bridge) {
      window.alert('Desktop bridge is available in the Wails app.');
      return;
    }
    setBusy(name);
    try {
      await action(bridge);
      await refreshRuntime();
      await refreshCalls();
    } catch (error) {
      window.alert(formatDesktopError(error));
    } finally {
      setBusy('');
    }
  };

  const loadProtocol = () => runDesktopAction('protocol', async (bridge) => {
    const path = await bridge.PickCursorJS();
    if (path) {
      await bridge.LoadProtocolFromJS(path);
    }
  });

  const launchCursor = () => runDesktopAction('cursor', async (bridge) => {
    let path = '';
    try {
      path = await bridge.PickCursorApp();
    } catch {
      path = '';
    }
    await bridge.LaunchCursor(path);
  });

  const selectNewest = () => {
    manualCallSelectionRef.current = false;
    setSelectedCallID(calls[0]?.id ?? null);
  };

  const toggleTail = () => {
    setLiveTail((current) => {
      const next = !current;
      if (next) {
        manualCallSelectionRef.current = false;
        setTimelineCursor(null);
        selectNewest();
      }
      return next;
    });
  };

  const resumeLiveUpdates = useCallback(() => {
    setIsPaused((current) => {
      const next = !current;
      if (current) {
        window.setTimeout(() => {
          refreshRecords();
          refreshSelectedFrames(selectedCallIDRef.current);
        }, 0);
      }
      return next;
    });
  }, [refreshRecords, refreshSelectedFrames]);

  const clearTraffic = async () => {
    const clearStartedAt = new Date().toISOString();
    setBusy('clear');
    setClearWatermark(clearStartedAt);
    clearWatermarkRef.current = clearStartedAt;
    isClearingTraffic.current = true;
    callsRequestID.current += 1;
    frameRequestID.current += 1;
    refreshCallsPending.current = false;
    if (refreshCallsTimer.current !== null) {
      window.clearTimeout(refreshCallsTimer.current);
      refreshCallsTimer.current = null;
    }
    isClearingTraffic.current = false;
    manualCallSelectionRef.current = false;
    setRecords([]);
    setCalls([]);
    setServiceCatalogCalls([]);
    setFrames([]);
    setSelectedCallID(null);
    setSelectedRecordKey(null);
    setFramesLoading(false);
    setTimelineCursor(null);
    setActiveCapture(null);
    setRecordingSessions([]);
    setBusy('');
    window.setTimeout(() => latestRefreshCalls.current(), 0);
  };

  const saveCapture = async (sessionOverride?: string[]) => {
    const fallback = selectedCall?.full_method || selectedCall?.method || 'Cursor capture';
    const name = captureName.trim() || `${fallback} ${new Date().toLocaleTimeString()}`;
    const sessions = sessionOverride && sessionOverride.length
      ? sessionOverride
      : selectedCallID ? [selectedCallID] : calls.slice(0, 10).map((call) => call.id);
    const capture = await fetchJSON<Capture>('/api/captures', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name, sessions }),
    });
    setCaptureName('');
    setCaptures((current) => [capture, ...current.filter((item) => item.id !== capture.id)]);
    setActiveCapture(capture);
  };

  const toggleCaptureRecording = async () => {
    if (!isRecordingCapture) {
      setRecordingSessions([]);
      setIsRecordingCapture(true);
      return;
    }
    const sessions = recordingSessions;
    setIsRecordingCapture(false);
    setRecordingSessions([]);
    if (sessions.length > 0) {
      await saveCapture(sessions);
    }
  };

  const loadCapture = async (captureID: string) => {
    const capture = await fetchJSON<Capture>(`/api/captures?id=${encodeURIComponent(captureID)}`);
    setActiveCapture(capture);
    setRecords(capture.records || []);
  };

  const deleteCapture = async (captureID: string) => {
    await fetchJSON(`/api/captures?id=${encodeURIComponent(captureID)}`, { method: 'DELETE' });
    setCaptures((current) => current.filter((capture) => capture.id !== captureID));
    if (activeCapture?.id === captureID) {
      setActiveCapture(null);
    }
  };

  const exportCapture = (capture: Capture) => {
    downloadTextFile(
      `${safeFilename(capture.name, 'capture')}.json`,
      JSON.stringify(capture, null, 2),
      'application/json',
    );
  };

  const exportCaptureJSON = async (capture: Capture) => {
    const fullCapture = await fetchJSON<Capture>(`/api/captures?id=${encodeURIComponent(capture.id)}`);
    exportCapture(fullCapture);
  };

  const exportCaptureJSONL = async (capture: Capture) => {
    const fullCapture = await fetchJSON<Capture>(`/api/captures?id=${encodeURIComponent(capture.id)}`);
    downloadTextFile(
      `${safeFilename(fullCapture.name || capture.name, 'capture')}.jsonl`,
      recordsToJSONL(fullCapture.records || []),
      'application/x-ndjson',
    );
  };

  const exportSelectedJSONL = () => {
    const selectedFrames = frames.length ? frames : selectedRecord ? [selectedRecord] : [];
    if (selectedFrames.length === 0) {
      return;
    }
    const baseName = selectedCall?.full_method || selectedCall?.id || selectedRecord?.session || 'request-detail';
    downloadTextFile(
      `${safeFilename(baseName, 'request-detail')}.jsonl`,
      recordsToJSONL(selectedFrames),
      'application/x-ndjson',
    );
  };

  const openReplay = () => {
    const request = frames.find((record) => record.type === 'request');
    const grpc = selectedRecord?.type === 'grpc' && selectedRecord.direction === 'C2S'
      ? selectedRecord
      : frames.find((record) => record.type === 'grpc' && record.direction === 'C2S');
    setReplayHeaders(JSON.stringify(request?.headers || {}, null, 2));
    setReplayBody(formatJSON(grpc?.grpc_data || '{}'));
    setReplayResult(null);
    setShowReplay(true);
  };

  const submitReplay = async () => {
    if (!selectedCallID) return;
    const headers = JSON.parse(replayHeaders || '{}') as { [key: string]: string[] };
    const body = JSON.parse(replayBody || '{}') as unknown;
    const result = await fetchJSON<ReplayResult>('/api/replay', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        session_id: selectedCallID,
        record_index: selectedRecord?.type === 'grpc' ? selectedRecord.index : 0,
        headers,
        body_json: body,
      }),
    });
    setReplayResult(result);
    await refreshCalls();
    manualCallSelectionRef.current = false;
    setSelectedCallID(result.call_id);
  };

  return (
    <main className="cursor-tap-workbench flex h-dvh min-w-[960px] flex-col overflow-hidden bg-[var(--ct-bg-base)] text-[var(--ct-text)] dark:bg-[var(--ct-bg-base)] dark:text-[var(--ct-text)]">
      <TitleBar
        status={status}
        isPaused={isPaused}
        liveTail={liveTail}
        isRecordingCapture={isRecordingCapture}
        recordingCount={recordingSessions.length}
        savedCount={captures.length}
        busy={busy}
        theme={theme}
        onStartProxy={() => runDesktopAction('proxy', (bridge) => bridge.StartProxy())}
        onLoadProtocol={loadProtocol}
        onLaunchCursor={launchCursor}
        onTogglePause={resumeLiveUpdates}
        onShowPalette={() => setShowPalette(true)}
        onShowCaptures={() => setShowCaptures(true)}
        onToggleRecordCapture={toggleCaptureRecording}
        onToggleTail={toggleTail}
        onClear={clearTraffic}
        onToggleTheme={() => setTheme((value) => (value === 'light' ? 'dark' : 'light'))}
      />

      {ENABLE_TOP_TIMELINE && (
        <TopTimeline
          calls={timelineCalls}
          selectedCallID={selectedCallID}
          selectedService={selectedService}
          selectedMethod={selectedMethod}
          captureWindow={captureWindow}
          timelineCursor={timelineCursor}
          activeCallsAtCursor={activeCallsAtCursor}
          onSetTimelineCursor={setTimelineCursor}
          onSelectCall={(callID) => selectCall(callID)}
        />
      )}

      <section className="min-h-0 flex-1 border-y border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-950">
        <ResizablePanels
          className="ct-four-pane"
          defaultSizes={[18, 24, 27, 31]}
          minSizes={[145, 215, 230, 280]}
        >
          <ServicesPane
            groups={serviceGroups}
            selectedService={selectedService}
            selectedMethod={selectedMethod}
            totalCalls={serviceGroupsSource.length}
            onSelectAll={() => {
              manualCallSelectionRef.current = false;
              setSelectedService(null);
              setSelectedMethod(null);
              setSearch('');
              setTimeFrom('');
              setTimeTo('');
            }}
            onSelectService={(service) => {
              manualCallSelectionRef.current = false;
              setSelectedMethod(null);
              setSelectedService((current) => (current === service ? null : service));
              setSelectedCallID(null);
            }}
            onSelectMethod={(service, method) => {
              manualCallSelectionRef.current = false;
              setSelectedService(service);
              setSelectedMethod((current) => (selectedService === service && current === method ? null : method));
              setSelectedCallID(null);
            }}
          />
          <CallsPane
            kind={kind}
            search={search}
            timeFrom={timeFrom}
            timeTo={timeTo}
            calls={calls}
            selectedCallID={selectedCallID}
            selectedService={selectedService}
            selectedMethod={selectedMethod}
            onKindChange={setKind}
            onSearchChange={setSearch}
            onTimeFromChange={setTimeFrom}
            onTimeToChange={setTimeTo}
            onSelectCall={(callID) => selectCall(callID)}
          />
          <FramesPane
            callID={selectedCallID}
            frames={frames}
            isLoading={framesLoading}
            selectedRecordKey={selectedRecordKey}
            onSelectRecord={(record) => setSelectedRecordKey(recordKey(record))}
          />
          <DetailPane
            call={selectedCall}
            record={selectedRecord}
            frames={frames}
            protocol={activeProtocol}
            onReplay={openReplay}
            onSaveJSONL={exportSelectedJSONL}
          />
        </ResizablePanels>
      </section>

      <BottomDock stats={stats} host={selectedCall?.host || selectedRecord?.host || 'api2.cursor.sh'} paused={isPaused} />

      {showPalette && (
        <CommandPalette
          items={paletteItems}
          onClose={() => setShowPalette(false)}
          onSelect={(item) => {
            if (item.kind === 'call') {
              selectCall(item.id);
            } else if (item.kind === 'record') {
              const record = records.find((entry) => recordKey(entry) === item.id);
              if (record?.session) {
                selectCall(record.session);
              }
              setSelectedRecordKey(item.id);
            } else {
              setSelectedService(item.service || item.id);
              setSelectedMethod(item.kind === 'method' ? item.method || null : null);
            }
            setShowPalette(false);
          }}
        />
      )}

      {showCaptures && (
        <CapturesPanel
          captures={captures}
          activeCapture={activeCapture}
          captureName={captureName}
          onCaptureNameChange={setCaptureName}
          onSave={() => saveCapture()}
          onLoad={loadCapture}
          onDelete={deleteCapture}
          onExportJSON={exportCaptureJSON}
          onExportJSONL={exportCaptureJSONL}
          onClose={() => setShowCaptures(false)}
        />
      )}

      {showReplay && (
        <ReplayModal
          headers={replayHeaders}
          body={replayBody}
          result={replayResult}
          onHeadersChange={setReplayHeaders}
          onBodyChange={setReplayBody}
          onSubmit={submitReplay}
          onClose={() => setShowReplay(false)}
        />
      )}
    </main>
  );
}

function TitleBar({
  status,
  isPaused,
  liveTail,
  isRecordingCapture,
  recordingCount,
  savedCount,
  busy,
  theme,
  onStartProxy,
  onLoadProtocol,
  onLaunchCursor,
  onTogglePause,
  onShowPalette,
  onShowCaptures,
  onToggleRecordCapture,
  onToggleTail,
  onClear,
	onToggleTheme,
}: {
  status: RuntimeStatus | null;
  isPaused: boolean;
  liveTail: boolean;
  isRecordingCapture: boolean;
  recordingCount: number;
  savedCount: number;
  busy: string;
  theme: 'light' | 'dark';
  onStartProxy: () => void;
  onLoadProtocol: () => void;
  onLaunchCursor: () => void;
  onTogglePause: () => void;
  onShowPalette: () => void;
  onShowCaptures: () => void;
  onToggleRecordCapture: () => void;
  onToggleTail: () => void;
  onClear: () => void;
  onToggleTheme: () => void;
}) {
  const proxyPort = status?.http_port || 8080;
  return (
    <header className="ct-window-drag flex h-[42px] shrink-0 items-center overflow-hidden border-b border-[var(--ct-border)] bg-[var(--ct-bg-window)] pl-[78px] pr-[14px]">
      <div className="ct-window-no-drag ml-auto flex min-w-0 shrink items-center justify-end gap-1 overflow-hidden">
        <button type="button" className="ct-search-trigger mr-1 hidden min-[1180px]:flex" onClick={onShowPalette}>
          <Search className="h-[13px] w-[13px]" />
          <span className="truncate">Search calls, services, methods...</span>
          <kbd>⌘K</kbd>
        </button>
        <ToolbarButton onClick={onStartProxy} active={busy === 'proxy'} title={`Proxy :${proxyPort}`}>
          <Activity className="h-3.5 w-3.5" />
          <span className="hidden min-[1280px]:inline">Proxy</span>
        </ToolbarButton>
        <span className="ct-port-badge">:{proxyPort}</span>
        <ToolbarButton onClick={onShowPalette} title="Proxy configuration help" compact>
          <HelpCircle className="h-3.5 w-3.5" />
        </ToolbarButton>
        {isPaused && <span className="ct-proto-chip ct-proto-chip--warning">paused</span>}
        <ToolbarButton onClick={onLoadProtocol} active={busy === 'protocol'} title="Load Protocol">
          <FileCode2 className="h-3.5 w-3.5" />
          <span className="hidden min-[1280px]:inline">Proto</span>
        </ToolbarButton>
        <ToolbarButton onClick={onLaunchCursor} active={busy === 'cursor'} title="Launch Cursor">
          <Rocket className="h-3.5 w-3.5" />
          <span className="hidden min-[1280px]:inline">Cursor</span>
        </ToolbarButton>
        <span className="mx-1 h-[18px] w-px bg-[var(--ct-border)]" />
        <button
          type="button"
          className={cn('ct-record-button', isRecordingCapture && 'ct-record-button--active')}
          onClick={onToggleRecordCapture}
          aria-label={isRecordingCapture ? 'Stop and save capture' : 'Start recording capture'}
          title={isRecordingCapture ? 'Stop recording and save capture' : 'Start recording'}
        >
          <span />
          {isRecordingCapture ? 'Stop' : 'Record'}
        </button>
        {isRecordingCapture && (
          <span className="ct-proto-chip ct-proto-chip--danger">
            {recordingCount} sessions
          </span>
        )}
        <ToolbarButton onClick={onShowCaptures} title="Saved records">
          <Archive className="h-3.5 w-3.5" />
          <span>Records</span>
          {savedCount > 0 && <span className="ct-title-badge">{savedCount}</span>}
        </ToolbarButton>
        <span className="mx-1 h-[18px] w-px bg-[var(--ct-border)]" />
        <ToolbarButton onClick={onToggleTail} active={liveTail} title="Auto-scroll to latest">
          <RefreshCw className="h-3.5 w-3.5" />
          <span>Tail</span>
        </ToolbarButton>
        <ToolbarButton onClick={onTogglePause} active={isPaused} title={isPaused ? 'Resume' : 'Pause'}>
          {isPaused ? <Play className="h-3.5 w-3.5" /> : <Pause className="h-3.5 w-3.5" />}
          <span>{isPaused ? 'Resume' : 'Pause'}</span>
        </ToolbarButton>
        <ToolbarButton onClick={onClear} title="Clear local view">
          <Trash2 className="h-3.5 w-3.5" />
          <span>Clear</span>
        </ToolbarButton>
        <ToolbarButton onClick={onToggleTheme} title="Toggle theme">
          {theme === 'light' ? <Moon className="h-3.5 w-3.5" /> : <Sun className="h-3.5 w-3.5" />}
        </ToolbarButton>
      </div>
    </header>
  );
}

function ToolbarButton({
  active,
  children,
  compact,
  onClick,
  title,
}: {
  active?: boolean;
  children: ReactNode;
  compact?: boolean;
  onClick: () => void;
  title?: string;
}) {
  return (
    <button
      type="button"
      className={cn('ct-toolbar-button', compact && 'ct-toolbar-button--compact', active && 'ct-toolbar-button--active')}
      onClick={onClick}
      title={title}
    >
      {children}
    </button>
  );
}

function TopTimeline({
  calls,
  selectedCallID,
  selectedService,
  selectedMethod,
  captureWindow,
  timelineCursor,
  activeCallsAtCursor,
  onSetTimelineCursor,
  onSelectCall,
}: {
  calls: RpcCall[];
  selectedCallID: string | null;
  selectedService: string | null;
  selectedMethod: string | null;
  captureWindow: TimelineRange;
  timelineCursor: number | null;
  activeCallsAtCursor: RpcCall[];
  onSetTimelineCursor: (value: number | null) => void;
  onSelectCall: (id: string) => void;
}) {
  const ref = useRef<HTMLDivElement | null>(null);
  const hoverFrameRef = useRef<number | null>(null);
  const pendingHoverRef = useRef<number | null>(null);
  const [hoverT, setHoverT] = useState<number | null>(null);
  const laneModel = useMemo(() => {
    const ordered = Array.from(new Set([...calls].sort((a, b) => callStart(a) - callStart(b)).map((call) => call.service || 'unknown')));
    const visible = ordered.slice(0, 7);
    const overflow = ordered.slice(7);
    const lanes = overflow.length ? [...visible, 'Other'] : visible;
    return {
      lanes: lanes.length ? lanes : ['waiting'],
      overflowServices: new Set(overflow),
      overflowCount: overflow.length,
    };
  }, [calls]);
  const lanes = laneModel.lanes;
  const laneIndexByName = useMemo(() => new Map(lanes.map((lane, index) => [lane, index])), [lanes]);
  const buckets = useMemo(() => buildTimelineBuckets(calls, captureWindow, 80), [calls, captureWindow]);
  const peak = Math.max(...buckets, 1);
  const laneH = 14;
  const padTop = 6;
  const plotHeight = padTop + Math.max(lanes.length, 1) * laneH + 6;
  const cursorPct = timelineCursor == null ? null : ((timelineCursor - captureWindow.start) / captureWindow.span) * 100;
  const hoverCallsAtTime = useMemo(() => (
    hoverT == null ? [] : callsNearTime(calls, hoverT, captureWindow.span)
  ), [calls, captureWindow.span, hoverT]);
  const cursorCallsAtTime = useMemo(() => (
    timelineCursor == null ? [] : mergeCalls(activeCallsAtCursor, callsNearTime(calls, timelineCursor, captureWindow.span))
  ), [activeCallsAtCursor, calls, captureWindow.span, timelineCursor]);
  const inspectedTime = timelineCursor ?? hoverT;
  const inspectedCalls = timelineCursor != null ? cursorCallsAtTime : hoverCallsAtTime;
  const inspectedLabel = timelineCursor != null ? 'At cursor' : 'Hover';

  const setCursorFromX = (clientX: number) => {
    if (!ref.current) return;
    const rect = ref.current.getBoundingClientRect();
    const pct = Math.max(0, Math.min(1, (clientX - rect.left) / rect.width));
    onSetTimelineCursor(captureWindow.start + pct * captureWindow.span);
  };

  const setHoverFromX = (clientX: number) => {
    if (!ref.current) return;
    const rect = ref.current.getBoundingClientRect();
    pendingHoverRef.current = captureWindow.start + ((clientX - rect.left) / rect.width) * captureWindow.span;
    if (hoverFrameRef.current != null) return;
    hoverFrameRef.current = window.requestAnimationFrame(() => {
      hoverFrameRef.current = null;
      setHoverT(pendingHoverRef.current);
    });
  };

  useEffect(() => {
    return () => {
      if (hoverFrameRef.current != null) {
        window.cancelAnimationFrame(hoverFrameRef.current);
      }
    };
  }, []);

  const onMouseDown = (event: ReactMouseEvent<HTMLDivElement>) => {
    setCursorFromX(event.clientX);
    const onMove = (moveEvent: MouseEvent) => setCursorFromX(moveEvent.clientX);
    const onUp = () => {
      window.removeEventListener('mousemove', onMove);
      window.removeEventListener('mouseup', onUp);
    };
    window.addEventListener('mousemove', onMove);
    window.addEventListener('mouseup', onUp);
  };

  const highlightedLabel = selectedMethod
    ? `${shortService(selectedService || '')}.${selectedMethod}`
    : selectedService ? shortService(selectedService) : null;

  return (
    <section className="ct-timeline shrink-0 border-b border-[var(--ct-border)] bg-[var(--ct-bg-window)] px-[14px] py-2">
      <div className="mb-1.5 flex flex-wrap items-center justify-between gap-x-3 gap-y-1">
        <div className="flex min-w-0 items-center gap-2.5 overflow-hidden">
          <span className="text-[10px] font-bold uppercase tracking-[0.06em] text-[var(--ct-text-2)]">Timeline</span>
          <span className="mono hidden text-[10px] text-[var(--ct-text-4)] min-[1040px]:inline">
            {formatTimelineTime(captureWindow.start)} → {formatTimelineTime(captureWindow.end)} · {(captureWindow.span / 1000).toFixed(1)}s
          </span>
          {timelineCursor != null && (
            <button type="button" className="ct-proto-chip ct-proto-chip--primary" onClick={() => onSetTimelineCursor(null)}>
              <CircleDot className="h-2.5 w-2.5" />
              <span className="mono">{formatTimelineTime(timelineCursor)}</span>
              <span className="text-[var(--ct-text-3)]">· {activeCallsAtCursor.length} active</span>
              <span className="font-bold">×</span>
            </button>
          )}
          {highlightedLabel && (
            <span className="ct-proto-chip ct-proto-chip--warning">
              highlighting {highlightedLabel}
            </span>
          )}
        </div>
        <div className="ml-auto flex shrink-0 items-center gap-2.5 text-[10px] text-[var(--ct-text-3)]">
          <Legend color="var(--ct-c2s)" label="unary" />
          <Legend color="var(--ct-stream)" label="streaming" />
          <Legend color="var(--ct-danger)" label="error" />
          <span className="hidden text-[var(--ct-text-4)] min-[1120px]:inline">· drag to scrub</span>
        </div>
      </div>

      <div
        ref={ref}
        onMouseMove={(event) => setHoverFromX(event.clientX)}
        onMouseLeave={() => {
          pendingHoverRef.current = null;
          setHoverT(null);
        }}
        onMouseDown={onMouseDown}
        className="relative cursor-crosshair select-none overflow-hidden rounded-lg border border-[var(--ct-border)] bg-[var(--ct-bg-pane)]"
        style={{ height: plotHeight }}
        aria-label="Timeline scrubber"
      >
        <svg
          width="100%"
          height="100%"
          preserveAspectRatio="none"
          className="pointer-events-none absolute inset-0 opacity-70"
          viewBox={`0 0 ${buckets.length} 100`}
        >
          <defs>
            <linearGradient id="timelineThroughput" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="rgba(0,111,238,0.22)" />
              <stop offset="100%" stopColor="rgba(0,111,238,0)" />
            </linearGradient>
          </defs>
          <path
            d={`M0,100 ${buckets.map((value, index) => `L${index},${100 - (value / peak) * 62}`).join(' ')} L${buckets.length},100 Z`}
            fill="url(#timelineThroughput)"
          />
        </svg>
        {[0.25, 0.5, 0.75].map((position) => (
          <span
            key={position}
            className="absolute top-0 bottom-0 w-px bg-[var(--ct-border)] opacity-60"
            style={{ left: `${position * 100}%` }}
          />
        ))}
        {lanes.map((lane, index) => (
          <span
            key={lane}
            className="ct-timeline-lane-label pointer-events-none absolute z-[1] truncate text-[9px] font-semibold text-[var(--ct-text-4)]"
            style={{ left: 6, top: padTop + index * laneH, maxWidth: 150, lineHeight: `${laneH}px` }}
          >
            {lane === 'Other' ? `Other (${laneModel.overflowCount})` : shortService(lane)}
          </span>
        ))}
        {calls.map((call) => {
          const service = call.service || 'unknown';
          const laneName = laneModel.overflowServices.has(service) ? 'Other' : service;
          const laneIndex = laneIndexByName.get(laneName) ?? 0;
          const left = ((callStart(call) - captureWindow.start) / captureWindow.span) * 100;
          const width = Math.max(0.35, ((callEnd(call) - callStart(call)) / captureWindow.span) * 100);
          const active = call.id === selectedCallID;
          const highlighted = (!selectedService || selectedService === call.service) && (!selectedMethod || selectedMethod === call.method);
          const color = call.status === 'error' ? 'var(--ct-danger)' : call.streaming ? 'var(--ct-stream)' : 'var(--ct-c2s)';
          return (
            <button
              key={call.id}
              type="button"
              onMouseDown={(event) => event.stopPropagation()}
              onClick={() => onSelectCall(call.id)}
              aria-label={`${callDisplayLabel(call)} ${call.duration_ms}ms ${call.frame_count} frames`}
              className="absolute z-[2] overflow-hidden rounded-sm transition"
              style={{
                left: `${Math.max(0, Math.min(99.8, left))}%`,
                top: padTop + laneIndex * laneH + 2,
                width: `${Math.min(100, width)}%`,
                height: laneH - 4,
                background: color,
                opacity: highlighted ? (active ? 1 : 0.86) : 0.18,
                boxShadow: active
                  ? `0 0 0 1.5px var(--ct-bg-pane), 0 0 0 2.5px ${color}, 0 0 8px ${color}`
                  : highlighted && highlightedLabel ? `0 0 6px ${color}` : 'none',
              }}
              title={`${call.full_method || call.method} · ${call.duration_ms}ms · ${call.frame_count} frames`}
            >
              {width > 7 && (
                <span className="ct-timeline-call-label">
                  {callDisplayLabel(call)} · {call.duration_ms}ms
                </span>
              )}
            </button>
          );
        })}
        {cursorPct != null && (
          <>
            <span
              className="pointer-events-none absolute top-0 bottom-0 z-10 w-[1.5px] bg-[var(--ct-text)]"
              style={{ left: `${Math.max(0, Math.min(100, cursorPct))}%`, boxShadow: '0 0 6px rgba(0,0,0,0.22)' }}
            />
            <span
              className="pointer-events-none absolute top-[-1px] z-10 h-[9px] w-[9px] bg-[var(--ct-text)]"
              style={{ left: `${Math.max(0, Math.min(100, cursorPct))}%`, marginLeft: -4.5, clipPath: 'polygon(50% 100%, 0 0, 100% 0)' }}
            />
          </>
        )}
        {hoverT != null && (
          <>
            <span
              className="pointer-events-none absolute top-0 bottom-0 z-[8] w-px bg-[var(--ct-text)] opacity-20"
              style={{ left: `${((hoverT - captureWindow.start) / captureWindow.span) * 100}%` }}
            />
            <span
              className="mono pointer-events-none absolute bottom-1 z-[9] rounded border border-[var(--ct-border-strong)] bg-[var(--ct-bg-active)] px-1.5 py-[1px] text-[9.5px] text-[var(--ct-text)]"
              style={{ left: `${((hoverT - captureWindow.start) / captureWindow.span) * 100}%`, transform: 'translateX(6px)' }}
            >
              {formatTimelineTime(hoverT)}
            </span>
          </>
        )}
        {calls.length === 0 && (
          <div className="absolute inset-0 flex items-center justify-center text-[11px] text-[var(--ct-text-4)]">
            Waiting for captured RPC calls
          </div>
        )}
      </div>

      {inspectedTime != null && inspectedCalls.length > 0 && (
        <div className="ct-timeline-inspector mt-2 flex flex-wrap items-center gap-2 rounded-lg border border-[var(--ct-border)] bg-[var(--ct-bg-pane)] px-2.5 py-2">
          <span className="mr-1 text-[10px] font-bold uppercase tracking-[0.05em] text-[var(--ct-text-3)]">
            {inspectedLabel}
          </span>
          <span className="mono text-[10px] text-[var(--ct-text-4)]">{formatTimelineTime(inspectedTime)}</span>
          {inspectedCalls.slice(0, 12).map((call) => {
            const elapsed = Math.max(0, inspectedTime - callStart(call));
            const duration = Math.max(callEnd(call) - callStart(call), call.duration_ms, 1);
            const pct = Math.min(100, (elapsed / duration) * 100);
            return (
              <button
                key={call.id}
                type="button"
                onClick={() => onSelectCall(call.id)}
                className={cn(
                  'ct-timeline-call-card flex items-center gap-1.5 rounded-md border px-2 py-1 text-[11px]',
                  call.id === selectedCallID ? 'border-[var(--ct-primary)] bg-[var(--accent-soft)]' : 'border-[var(--ct-border)] bg-[var(--ct-bg-pane-2)]',
                )}
              >
                <span className={cn('ct-proto-chip', call.streaming ? 'ct-proto-chip--stream' : 'ct-proto-chip--primary')}>
                  {call.streaming ? 'stream' : 'unary'}
                </span>
                {call.status === 'error' && <span className="ct-mini-chip ct-mini-chip--danger">error</span>}
                <span className="font-semibold text-[var(--ct-text)]">{callDisplayLabel(call)}</span>
                <span className="mono text-[9.5px] text-[var(--ct-text-3)]">{shortService(call.service || call.host)}</span>
                <span className="mono text-[9.5px] text-[var(--ct-text-3)]">{call.duration_ms}ms</span>
                <span className="text-[9.5px] text-[var(--ct-text-4)]">{call.frame_count}f</span>
                <span className="text-[9.5px] text-[var(--ct-text-4)]">{formatBytes(callBytes(call))}</span>
                <span className="h-[3px] w-10 overflow-hidden rounded bg-[var(--ct-bg-active)]">
                  <span className="block h-full bg-[var(--ct-primary)]" style={{ width: `${pct}%` }} />
                </span>
                <span className="mono text-[9.5px] text-[var(--ct-text-3)]">{Math.round(elapsed)}/{Math.max(call.duration_ms, 1)}ms</span>
              </button>
            );
          })}
          {inspectedCalls.length > 12 && (
            <span className="text-[10px] text-[var(--ct-text-3)]">+{inspectedCalls.length - 12} more</span>
          )}
        </div>
      )}
    </section>
  );
}

function Legend({ color, label }: { color: string; label: string }) {
  return (
    <span className="inline-flex items-center gap-1">
      <span className="h-2 w-2 rounded-sm" style={{ background: color }} />
      {label}
    </span>
  );
}

function PaneShell({
  as = 'section',
  children,
  className,
}: {
  as?: 'aside' | 'section';
  children: ReactNode;
  className?: string;
}) {
  const Component = as;
  return (
    <Component className={cn('flex h-full min-h-0 min-w-0 flex-col overflow-hidden', className)}>
      {children}
    </Component>
  );
}

function PaneScroll({
  children,
  className,
  region,
  scrollRef,
  ...props
}: {
  children: ReactNode;
  className?: string;
  region?: string;
  scrollRef?: Ref<HTMLDivElement>;
} & HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      ref={scrollRef}
      data-scroll-region={region}
      className={cn('min-h-0 flex-1 overflow-y-auto overscroll-contain', className)}
      {...props}
    >
      {children}
    </div>
  );
}

function useScrollLazyCount(
  total: number,
  { ensureIndex = -1, initial, resetKey = '', step }: { ensureIndex?: number; initial: number; resetKey?: string; step: number },
) {
  const [visibleCount, setVisibleCount] = useState(() => Math.min(total, Math.max(initial, ensureIndex + 1)));
  const resetKeyRef = useRef(resetKey);
  const minimumVisible = Math.min(total, Math.max(initial, ensureIndex + 1));

  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      if (cancelled) return;
      setVisibleCount((current) => {
        if (resetKeyRef.current !== resetKey) {
          resetKeyRef.current = resetKey;
          return minimumVisible;
        }
        return Math.min(total, Math.max(current, minimumVisible));
      });
    });
    return () => {
      cancelled = true;
    };
  }, [minimumVisible, resetKey, total]);

  const loadMore = useCallback(() => {
    setVisibleCount((current) => Math.min(total, Math.max(current, minimumVisible) + step));
  }, [minimumVisible, step, total]);

  const onScroll = useCallback((event: ReactUIEvent<HTMLDivElement>) => {
    const element = event.currentTarget;
    const remaining = element.scrollHeight - element.scrollTop - element.clientHeight;
    if (remaining <= LIST_LAZY_THRESHOLD_PX) {
      loadMore();
    }
  }, [loadMore]);

  const safeVisibleCount = Math.min(visibleCount, total);
  return {
    hasMore: safeVisibleCount < total,
    loadMore,
    onScroll,
    visibleCount: safeVisibleCount,
  };
}

function ServicesPane({
  groups,
  selectedService,
  selectedMethod,
  totalCalls,
  onSelectAll,
  onSelectService,
  onSelectMethod,
}: {
  groups: ServiceGroup[];
  selectedService: string | null;
  selectedMethod: string | null;
  totalCalls: number;
  onSelectAll: () => void;
  onSelectService: (service: string) => void;
  onSelectMethod: (service: string, method: string) => void;
}) {
  const [expanded, setExpanded] = useState<Set<string>>(() => new Set());

  const toggleExpanded = (service: string) => {
    setExpanded((current) => {
      const next = new Set(current);
      if (next.has(service)) {
        next.delete(service);
      } else {
        next.add(service);
      }
      return next;
    });
  };

  return (
    <PaneShell as="aside" className="border-r border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-950">
      <PaneHeader icon={<ListTree className="h-4 w-4" />} title="Services" meta={`${groups.length}`} />
      <PaneScroll region="services">
        <div className="py-1">
          <button
            type="button"
            onClick={onSelectAll}
            className={cn('ct-service-row', !selectedService && 'ct-service-row--active')}
          >
            <Sparkles className="h-3.5 w-3.5 text-[var(--ct-primary)]" />
            <span className="min-w-0 flex-1 truncate">All Calls</span>
            <span className="ct-count-badge">{totalCalls}</span>
          </button>
          {groups.map((group) => {
            const isExpanded = expanded.has(group.service) || selectedService === group.service;
            const serviceActive = selectedService === group.service && !selectedMethod;
            return (
              <div key={group.service}>
                <div className={cn('ct-service-row', selectedService === group.service && 'ct-service-row--active')}>
                  <button
                    type="button"
                    onClick={() => toggleExpanded(group.service)}
                    className="flex h-5 w-4 shrink-0 items-center justify-center rounded hover:bg-[var(--ct-bg-hover)]"
                    aria-label={`Toggle ${group.service}`}
                  >
                    <ChevronRight className={cn('h-3 w-3 transition', isExpanded && 'rotate-90')} />
                  </button>
                  <button
                    type="button"
                    onClick={() => onSelectService(group.service)}
                    className={cn(
                      'min-w-0 flex-1 text-left text-[11.5px] font-semibold transition',
                      serviceActive && 'text-[var(--ct-text)]',
                    )}
                  >
                    <div className="truncate">{shortService(group.service)}</div>
                  </button>
                  <span className="ct-count-badge">{group.count}</span>
                </div>
                {isExpanded && (
                  <div>
                    {group.methods.map((method) => {
                      const methodActive = selectedService === group.service && selectedMethod === method.name;
                      return (
                        <button
                          key={method.name}
                          type="button"
                          onClick={() => onSelectMethod(group.service, method.name)}
                          className={cn('ct-service-row ct-service-row--method', methodActive && 'ct-service-row--active')}
                        >
                          <span className="truncate">{method.name}</span>
                          <span className="ct-count-badge">{method.count}</span>
                        </button>
                      );
                    })}
                  </div>
                )}
              </div>
            );
          })}
          {groups.length === 0 && <EmptyState icon={<Server className="h-5 w-5" />} label="Waiting for services" />}
        </div>
      </PaneScroll>
    </PaneShell>
  );
}

function CallsPane({
  kind,
  search,
  timeFrom,
  timeTo,
  calls,
  selectedCallID,
  selectedService,
  selectedMethod,
  onKindChange,
  onSearchChange,
  onTimeFromChange,
  onTimeToChange,
  onSelectCall,
}: {
  kind: FilterKind;
  search: string;
  timeFrom: string;
  timeTo: string;
  calls: RpcCall[];
  selectedCallID: string | null;
  selectedService: string | null;
  selectedMethod: string | null;
  onKindChange: (kind: FilterKind) => void;
  onSearchChange: (value: string) => void;
  onTimeFromChange: (value: string) => void;
  onTimeToChange: (value: string) => void;
  onSelectCall: (id: string) => void;
}) {
  const frameTotal = useMemo(() => calls.reduce((total, call) => total + call.frame_count, 0), [calls]);
  const captureWindow = useMemo(() => timelineRange(calls), [calls]);
  const [focusedCallID, setFocusedCallID] = useState<string | null>(null);
  const callButtonRefs = useRef<Map<string, HTMLButtonElement>>(new Map());
  const focusBasis = focusedCallID || selectedCallID || calls[0]?.id || null;
  const selectedCallIndex = selectedCallID ? calls.findIndex((call) => call.id === selectedCallID) : -1;
  const {
    hasMore: hasMoreCalls,
    loadMore: loadMoreCalls,
    onScroll: handleCallsScroll,
    visibleCount: renderedCallCount,
  } = useScrollLazyCount(calls.length, {
    ensureIndex: selectedCallIndex,
    initial: LIST_LAZY_INITIAL,
    resetKey: `${kind}:${search}:${timeFrom}:${timeTo}:${selectedService ?? ''}:${selectedMethod ?? ''}`,
    step: LIST_LAZY_CHUNK,
  });
  const renderedCalls = useMemo(() => calls.slice(0, renderedCallCount), [calls, renderedCallCount]);

  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      if (cancelled) return;
      setFocusedCallID((current) => {
        if (current && calls.some((call) => call.id === current)) {
          return current;
        }
        if (selectedCallID && calls.some((call) => call.id === selectedCallID)) {
          return selectedCallID;
        }
        return calls[0]?.id ?? null;
      });
    });
    return () => {
      cancelled = true;
    };
  }, [calls, selectedCallID]);

  const focusCallAt = (index: number) => {
    const next = renderedCalls[Math.max(0, Math.min(renderedCalls.length - 1, index))];
    if (!next) return;
    setFocusedCallID(next.id);
    callButtonRefs.current.get(next.id)?.focus();
    callButtonRefs.current.get(next.id)?.scrollIntoView({ block: 'nearest' });
  };

  const handleKeyDown = (event: ReactKeyboardEvent<HTMLDivElement>) => {
    if (!renderedCalls.length) return;
    const currentIndex = Math.max(0, renderedCalls.findIndex((call) => call.id === focusBasis));
    if (event.key === 'ArrowDown') {
      event.preventDefault();
      focusCallAt(currentIndex + 1);
    }
    if (event.key === 'ArrowUp') {
      event.preventDefault();
      focusCallAt(currentIndex - 1);
    }
    if (event.key === 'Enter') {
      event.preventDefault();
      const target = renderedCalls[currentIndex];
      if (target) {
        onSelectCall(target.id);
      }
    }
  };

  return (
    <PaneShell className="border-r border-slate-200 dark:border-slate-800">
      <div className="shrink-0 border-b border-slate-200 px-2 py-1.5 dark:border-slate-800">
        <div className="mb-2 flex items-center justify-between">
          <div className="flex items-center gap-2 text-[12px] font-semibold uppercase text-[var(--ct-text-2)]">
            <Radio className="h-4 w-4" />
            Calls
          </div>
          <span className="ct-count-badge">{calls.length}</span>
        </div>
        <div className="relative">
          <Search className="pointer-events-none absolute left-2 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-[var(--ct-text-4)]" />
          <input
            className="ct-inspector-input w-full pl-8"
            value={search}
            onChange={(event) => onSearchChange(event.target.value)}
            placeholder="Search RPC calls"
            aria-label="Search calls"
          />
        </div>
        <div className="mt-2 flex flex-wrap gap-1" aria-label="Call filters">
          {(['all', 'streaming', 'unary', 'errors'] as FilterKind[]).map((item) => (
            <button
              key={item}
              type="button"
              className={cn('ct-filter-chip', kind === item && 'ct-filter-chip--active')}
              onClick={() => onKindChange(item)}
            >
              {item === 'all' && <Filter className="h-3.5 w-3.5" />}
              {item}
            </button>
          ))}
        </div>
        <div className="mt-2 grid grid-cols-2 gap-1.5">
          <input
            className="ct-inspector-input px-2"
            type="datetime-local"
            step={1}
            value={timeFrom}
            onChange={(event) => onTimeFromChange(event.target.value)}
            aria-label="Call start from"
            title="Call start from"
          />
          <input
            className="ct-inspector-input px-2"
            type="datetime-local"
            step={1}
            value={timeTo}
            onChange={(event) => onTimeToChange(event.target.value)}
            aria-label="Call start to"
            title="Call start to"
          />
        </div>
        <div className="mono mt-1 text-right text-[10px] text-[var(--ct-text-3)]">{frameTotal.toLocaleString()} frames</div>
      </div>
      <PaneScroll
        region="calls"
        tabIndex={0}
        onFocus={() => setFocusedCallID((current) => current ?? selectedCallID ?? calls[0]?.id ?? null)}
        onKeyDown={handleKeyDown}
        onScroll={handleCallsScroll}
      >
        <div className="py-1">
          {renderedCalls.map((call) => (
            <CallCard
              key={call.id}
              buttonRef={(element) => {
                if (element) {
                  callButtonRefs.current.set(call.id, element);
                } else {
                  callButtonRefs.current.delete(call.id);
                }
              }}
              call={call}
              captureWindow={captureWindow}
              active={call.id === selectedCallID}
              focused={call.id === focusBasis}
              highlighted={(!selectedService || selectedService === call.service) && (!selectedMethod || selectedMethod === call.method)}
              onFocus={() => setFocusedCallID(call.id)}
              onClick={() => onSelectCall(call.id)}
            />
          ))}
          {hasMoreCalls && (
            <LazyLoadStatus current={renderedCallCount} total={calls.length} label="Rendered calls" onLoadMore={loadMoreCalls} />
          )}
          {calls.length === 0 && <EmptyState icon={<BoxSelect className="h-5 w-5" />} label="No calls captured" />}
        </div>
      </PaneScroll>
    </PaneShell>
  );
}

function FramesPane({
  callID,
  frames,
  isLoading,
  selectedRecordKey,
  onSelectRecord,
}: {
  callID: string | null;
  frames: Record[];
  isLoading: boolean;
  selectedRecordKey: string | null;
  onSelectRecord: (record: Record) => void;
}) {
  const [frameQuery, setFrameQuery] = useState('');
  const [isPlaying, setIsPlaying] = useState(false);
  const scrollRef = useRef<HTMLDivElement | null>(null);
  const visibleFrames = useMemo(() => {
    const query = frameQuery.trim().toLowerCase();
    if (!query) return frames;
    return frames.filter((record) => recordSearchText(record).toLowerCase().includes(query));
  }, [frameQuery, frames]);
  const grpcFrames = visibleFrames.filter((record) => record.type === 'grpc');
  const selectedIndex = Math.max(0, visibleFrames.findIndex((record) => recordKey(record) === selectedRecordKey));
  const progress = visibleFrames.length ? ((selectedIndex + 1) / visibleFrames.length) * 100 : 0;
  const {
    hasMore: hasMoreFrames,
    loadMore: loadMoreFrames,
    onScroll: handleFramesScroll,
    visibleCount: renderedFrameCount,
  } = useScrollLazyCount(visibleFrames.length, {
    ensureIndex: selectedIndex,
    initial: LIST_LAZY_INITIAL,
    resetKey: `${callID ?? ''}:${frameQuery}`,
    step: LIST_LAZY_CHUNK,
  });
  const renderedFrames = useMemo(() => visibleFrames.slice(0, renderedFrameCount), [renderedFrameCount, visibleFrames]);

  useEffect(() => {
    if (!isPlaying || visibleFrames.length === 0) return;
    const timer = window.setInterval(() => {
      const currentIndex = visibleFrames.findIndex((record) => recordKey(record) === selectedRecordKey);
      const next = visibleFrames[(Math.max(currentIndex, -1) + 1) % visibleFrames.length];
      if (next) onSelectRecord(next);
    }, 850);
    return () => window.clearInterval(timer);
  }, [isPlaying, onSelectRecord, selectedRecordKey, visibleFrames]);

  const markerFrames = renderedFrames.map((frame, index) => ({
    key: recordKey(frame),
    pct: visibleFrames.length > 1 ? (index / (visibleFrames.length - 1)) * 100 : 0,
    direction: frame.direction,
  }));

  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      if (cancelled) return;
      setFrameQuery('');
      setIsPlaying(false);
    });
    scrollRef.current?.scrollTo({ top: 0 });
    return () => {
      cancelled = true;
    };
  }, [callID]);

  return (
    <PaneShell className="border-r border-slate-200 dark:border-slate-800">
      <PaneHeader icon={<Clock3 className="h-4 w-4" />} title="Stream" meta={isLoading ? 'loading' : `${frames.length}`} />
      <div className="shrink-0 border-b border-slate-200 px-3 py-2 dark:border-slate-800">
        <div className="mb-2 flex items-center justify-between text-[11px] text-slate-500 dark:text-slate-400">
          <span>{visibleFrames.length ? `${selectedIndex + 1}/${visibleFrames.length}` : '0/0'}</span>
          <span>{grpcFrames.length} gRPC frames</span>
        </div>
        <div className="ct-stream-scrubber mb-2">
          <button
            type="button"
            className="ct-play-button"
            onClick={() => setIsPlaying((value) => !value)}
            disabled={visibleFrames.length < 2}
            aria-label={isPlaying ? 'Pause frame playback' : 'Play frame playback'}
          >
            {isPlaying ? <Pause className="h-3.5 w-3.5" /> : <Play className="h-3.5 w-3.5" />}
          </button>
          <div className="relative h-[16px] min-w-0 flex-1">
            <span className="ct-scrub-track" />
            <span className="ct-scrub-fill" style={{ width: `${Number.isFinite(progress) ? progress : 0}%` }} />
            {markerFrames.map((marker) => (
              <span
                key={marker.key}
                className={cn('ct-frame-marker', marker.direction === 'S2C' ? 'ct-frame-marker--s2c' : 'ct-frame-marker--c2s')}
                style={{ left: `${marker.pct}%` }}
              />
            ))}
            <span className="ct-scrub-handle" style={{ left: `${Number.isFinite(progress) ? progress : 0}%` }} />
            <input
              className="ct-frame-range"
              type="range"
              min={0}
              max={Math.max(visibleFrames.length - 1, 0)}
              value={visibleFrames.length ? selectedIndex : 0}
              onChange={(event) => {
                const next = visibleFrames[Number(event.target.value)];
                if (next) onSelectRecord(next);
              }}
              aria-label="Scrub frames"
            />
          </div>
        </div>
        <div className="relative mt-2">
          <Search className="pointer-events-none absolute left-2 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-[var(--ct-text-4)]" />
          <input
            className="ct-inspector-input w-full pl-8"
            value={frameQuery}
            onChange={(event) => setFrameQuery(event.target.value)}
            placeholder="Search frames"
            aria-label="Search frames"
          />
        </div>
      </div>
      <PaneScroll region="frames" scrollRef={scrollRef} onScroll={handleFramesScroll}>
        <div className="py-1">
          {isLoading ? (
            <FrameSkeleton />
          ) : (
            renderedFrames.map((frame) => (
              <FrameRow
                key={recordKey(frame)}
                record={frame}
                active={recordKey(frame) === selectedRecordKey}
                onClick={() => onSelectRecord(frame)}
              />
            ))
          )}
          {!isLoading && hasMoreFrames && (
            <LazyLoadStatus current={renderedFrameCount} total={visibleFrames.length} label="Rendered frames" onLoadMore={loadMoreFrames} />
          )}
          {!isLoading && visibleFrames.length === 0 && (
            <EmptyState
              icon={<Radio className="h-5 w-5" />}
              label={!callID ? 'Select a call to inspect stream' : frames.length === 0 ? 'No stream records' : 'No frames match'}
            />
          )}
        </div>
      </PaneScroll>
    </PaneShell>
  );
}

function DetailPane({
  call,
  record,
  frames,
  protocol,
  onReplay,
  onSaveJSONL,
}: {
  call: RpcCall | null;
  record: Record | null;
  frames: Record[];
	protocol: ProtocolVersion | null;
	onReplay: () => void;
  onSaveJSONL: () => void;
}) {
	const [activeTab, setActiveTab] = useState('grpc');
	const tabs = ['gRPC', 'Headers', 'Raw', 'Timing', 'Sequence', 'Protocol'];
	  const headers = useMemo(() => record?.headers || frames.find((item) => item.type === 'request')?.headers || {}, [frames, record]);
	  const agentSummary = useMemo(() => buildAgentRunSummary(call, frames), [call, frames]);
	  const selectedResponseFrame = useMemo(() => buildFrameResponse(record), [record]);
	  const payload = record?.grpc_data || record?.body || record?.event_data || record?.grpc_raw || '{}';
	const raw = useMemo(() => (
    activeTab === 'raw' ? JSON.stringify(record || call || {}, null, 2) : ''
  ), [activeTab, call, record]);
	const sequence = useMemo(() => (
    activeTab === 'sequence' ? buildSequence(call, frames) : ''
  ), [activeTab, call, frames]);
	const protoSource = useMemo(() => (
    activeTab === 'protocol' && protocol
      ? Object.entries(protocol.proto_sources || {}).map(([name, source]) => `// ${name}\n${source}`).join('\n')
      : ''
  ), [activeTab, protocol]);

  return (
    <PaneShell className="bg-white dark:bg-slate-950">
      <div className="flex h-11 shrink-0 items-center justify-between border-b border-slate-200 px-3 dark:border-slate-800">
        <div className="min-w-0">
          <div className="truncate text-sm font-semibold">{call?.full_method || recordTitle(record)}</div>
          <div className="truncate text-[11px] text-slate-500 dark:text-slate-400">{call?.host || record?.host || 'No active call'}</div>
        </div>
        <div className="flex shrink-0 items-center gap-1">
          <Button isIconOnly size="sm" variant="tertiary" aria-label="Copy detail" onPress={() => navigator.clipboard.writeText(JSON.stringify(record || call || {}, null, 2))}>
            <Copy className="h-4 w-4" />
          </Button>
          <Button size="sm" variant="secondary" onPress={onSaveJSONL} isDisabled={frames.length === 0 && !record}>
            <Save className="h-4 w-4" /> JSONL
          </Button>
          <Button size="sm" variant="primary" onPress={onReplay} isDisabled={!call}>
            <Wand2 className="h-4 w-4" /> Replay
          </Button>
        </div>
      </div>
      <div className="flex min-h-10 flex-wrap items-center gap-1 border-b border-slate-200 px-3 py-1 dark:border-slate-800" role="tablist" aria-label="Record detail tabs">
        {tabs.map((tab) => {
          const key = tab.toLowerCase();
          return (
            <button
              key={tab}
              type="button"
              role="tab"
              aria-selected={activeTab === key}
              onClick={() => setActiveTab(key)}
              className={cn(
                'shrink-0 rounded-md px-2 py-1 text-[11px] font-medium text-slate-500 transition hover:bg-slate-100 dark:text-slate-400 dark:hover:bg-slate-900',
                activeTab === key && 'bg-slate-950 text-white dark:bg-white dark:text-slate-950',
              )}
            >
              {tab}
            </button>
          );
        })}
      </div>
			<div className="min-h-0 flex-1 overflow-hidden" role="tabpanel">
					{activeTab === 'grpc' && (
						  <DetailScroll>
							  {agentSummary && <AgentRunSummaryPanel summary={agentSummary} />}
							  {selectedResponseFrame && <SelectedFrameResponsePanel frame={selectedResponseFrame} />}
							  <SummaryGrid call={call} record={record} />
							  <PayloadInspector record={record} fallbackPayload={payload} />
						  </DetailScroll>
					  )}
				{activeTab === 'headers' && (
					<DetailScroll>
						<CodeBlock value={JSON.stringify(headers, null, 2)} language="json" maxHeight="620px" />
					</DetailScroll>
				)}
				{activeTab === 'raw' && (
					<DetailScroll>
						<RawInspector value={raw} />
					</DetailScroll>
				)}
				{activeTab === 'timing' && (
					<DetailScroll>
						<TimingWaterfall frames={frames} />
					</DetailScroll>
				)}
				{activeTab === 'sequence' && (
					<DetailScroll>
						<SequenceDiagram chart={sequence} />
					</DetailScroll>
				)}
				{activeTab === 'protocol' && (
					<DetailScroll>
						<CodeBlock value={protoSource || '// Load a Cursor JS file to inspect dynamic protocol sources.'} language="proto" maxHeight="620px" />
					</DetailScroll>
				)}
			</div>
		</PaneShell>
	);
}

function BottomDock({ stats, host, paused }: { stats: InspectorStats; host: string; paused: boolean }) {
  const peak = Math.max(...stats.spark, 1);
  return (
    <footer className="ct-bottom-dock flex h-9 shrink-0 items-center gap-[18px] border-t border-slate-200 bg-[var(--ct-bg-window)] px-[14px] text-[11px] text-[var(--ct-text-2)] dark:border-slate-800">
      <div className="ct-bottom-stats flex min-w-0 items-center gap-[18px] overflow-hidden">
        <DockStat icon={<CheckCircle2 className="h-3 w-3 text-[var(--ct-success)]" />} label="Status" value={paused ? 'Paused' : 'Live'} tone={paused ? 'warning' : 'success'} />
        <DockStat label="Calls" value={stats.calls.toLocaleString()} />
        <DockStat label="Frames" value={stats.frames.toLocaleString()} />
        <DockStat label="C2S" value={formatBytes(stats.c2s)} tone="c2s" />
        <DockStat label="S2C" value={formatBytes(stats.s2c)} tone="s2c" />
        <DockStat label="Streams" value={stats.streams.toLocaleString()} />
        <DockStat label="Errors" value={stats.errors.toLocaleString()} tone={stats.errors ? 'danger' : undefined} />
      </div>
      <div className="ct-throughput ml-auto flex shrink-0 items-center gap-2">
        <span className="hidden text-[10px] text-[var(--ct-text-3)] min-[1120px]:inline">Throughput</span>
        <span className="flex h-5 w-[120px] items-end gap-px">
          {stats.spark.map((value, index) => (
            <span key={`${value}-${index}`} className="w-[2px] rounded-t-sm bg-[var(--ct-primary)] opacity-80" style={{ height: `${Math.max(2, (value / peak) * 18)}px` }} />
          ))}
        </span>
        <span className="mono text-[10px] text-[var(--ct-text-3)]">{host}<span className="text-[var(--ct-text-4)]">:443</span></span>
      </div>
    </footer>
  );
}

function DockStat({ icon, label, value, tone }: { icon?: ReactNode; label: string; value: string; tone?: 'success' | 'warning' | 'danger' | 'c2s' | 's2c' }) {
  const color = tone === 'success'
    ? 'var(--ct-success)'
    : tone === 'warning'
      ? 'var(--ct-warning)'
      : tone === 'danger'
        ? 'var(--ct-danger)'
        : tone === 'c2s'
          ? 'var(--ct-c2s)'
          : tone === 's2c'
            ? 'var(--ct-s2c)'
            : 'var(--ct-text-2)';
  return (
    <span className="ct-dock-stat flex shrink-0 items-center gap-[5px]">
      {icon}
      <span className="text-[10px] font-semibold uppercase tracking-[0.05em] text-[var(--ct-text-3)]">{label}</span>
      <span className="mono font-semibold" style={{ color }}>{value}</span>
    </span>
  );
}

function CommandPalette({
  items,
  onSelect,
  onClose,
}: {
  items: PaletteItem[];
  onSelect: (item: PaletteItem) => void;
  onClose: () => void;
}) {
  const [query, setQuery] = useState('');
  const [activeIndex, setActiveIndex] = useState(0);
  const inputRef = useRef<HTMLInputElement | null>(null);
  const visibleItems = useMemo(() => {
    const normalized = query.trim().toLowerCase();
    if (!normalized) return items;
    return items.filter((item) => `${item.title} ${item.subtitle}`.toLowerCase().includes(normalized));
  }, [items, query]);

  useEffect(() => {
    inputRef.current?.focus();
  }, []);

  const handleKeyDown = (event: ReactKeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'ArrowDown') {
      event.preventDefault();
      setActiveIndex((index) => Math.min(index + 1, Math.max(visibleItems.length - 1, 0)));
    }
    if (event.key === 'ArrowUp') {
      event.preventDefault();
      setActiveIndex((index) => Math.max(index - 1, 0));
    }
    if (event.key === 'Enter' && visibleItems[activeIndex]) {
      event.preventDefault();
      onSelect(visibleItems[activeIndex]);
    }
    if (event.key === 'Escape') {
      event.preventDefault();
      onClose();
    }
  };

  return (
    <Overlay onClose={onClose}>
      <div className="w-[620px] overflow-hidden rounded-md border border-slate-200 bg-white shadow-2xl dark:border-slate-800 dark:bg-slate-950">
        <div className="flex h-12 items-center gap-2 border-b border-slate-200 px-3 dark:border-slate-800">
          <Search className="h-4 w-4 text-slate-400" />
          <input
            ref={inputRef}
            value={query}
            onChange={(event) => {
              setQuery(event.target.value);
              setActiveIndex(0);
            }}
            onKeyDown={handleKeyDown}
            className="min-w-0 flex-1 bg-transparent text-sm font-medium outline-none placeholder:text-slate-400"
            placeholder="Jump to service, method, call, or record"
            aria-label="Command palette search"
          />
          <span className="ml-auto text-xs text-slate-400">services · methods · recent calls</span>
        </div>
        <div className="max-h-[420px] overflow-auto p-2">
          {visibleItems.map((item, index) => (
            <button
              key={`${item.kind}-${item.id}`}
              type="button"
              onClick={() => onSelect(item)}
              className={cn(
                'flex w-full items-center gap-3 rounded-md px-3 py-2 text-left hover:bg-slate-100 dark:hover:bg-slate-900',
                index === activeIndex && 'bg-slate-100 dark:bg-slate-900',
              )}
            >
              {item.kind === 'call' && <Radio className="h-4 w-4 text-blue-500" />}
              {item.kind === 'record' && <Code2 className="h-4 w-4 text-emerald-500" />}
              {item.kind === 'service' && <Server className="h-4 w-4 text-violet-500" />}
              {item.kind === 'method' && <ChevronRight className="h-4 w-4 text-amber-500" />}
              <div className="min-w-0 flex-1">
                <div className="truncate text-sm font-medium">{item.title}</div>
                <div className="truncate text-xs text-slate-500 dark:text-slate-400">{item.subtitle}</div>
              </div>
              <ChevronRight className="h-4 w-4 text-slate-400" />
            </button>
          ))}
          {visibleItems.length === 0 && <EmptyState icon={<Search className="h-5 w-5" />} label="No commands available" />}
        </div>
      </div>
    </Overlay>
  );
}

function CapturesPanel({
  captures,
  activeCapture,
  captureName,
  onCaptureNameChange,
  onSave,
  onLoad,
  onDelete,
  onExportJSON,
  onExportJSONL,
  onClose,
}: {
  captures: Capture[];
  activeCapture: Capture | null;
  captureName: string;
  onCaptureNameChange: (value: string) => void;
  onSave: () => void;
  onLoad: (id: string) => void;
  onDelete: (id: string) => void;
  onExportJSON: (capture: Capture) => void;
  onExportJSONL: (capture: Capture) => void;
  onClose: () => void;
}) {
  return (
    <div role="dialog" aria-label="Saved records" className="absolute inset-y-0 right-0 z-40 w-[420px] border-l border-slate-200 bg-white shadow-2xl dark:border-slate-800 dark:bg-slate-950">
      <div className="flex h-12 items-center justify-between border-b border-slate-200 px-3 dark:border-slate-800">
        <div className="flex items-center gap-2 text-sm font-semibold"><Archive className="h-4 w-4" /> Records</div>
        <Button isIconOnly size="sm" variant="tertiary" onPress={onClose} aria-label="Close records">
          <X className="h-4 w-4" />
        </Button>
      </div>
      <div className="border-b border-slate-200 p-3 dark:border-slate-800">
        <Input className="w-full" value={captureName} onChange={(event) => onCaptureNameChange(event.target.value)} placeholder="Capture name" aria-label="Capture name" />
        <Button className="mt-2 w-full" size="sm" variant="primary" onPress={onSave}>
          <Save className="h-4 w-4" /> Save Current Capture
        </Button>
        {activeCapture && (
          <div className="mt-2 rounded-md bg-emerald-50 p-2 text-xs text-emerald-700 dark:bg-emerald-950/40 dark:text-emerald-200">
            Loaded {activeCapture.name}
          </div>
        )}
      </div>
      <ScrollShadow className="h-[calc(100%-145px)]">
        <div className="space-y-2 p-3">
          {captures.map((capture) => (
            <div key={capture.id} className="rounded-md border border-slate-200 p-3 dark:border-slate-800">
              <div className="flex items-start justify-between gap-2">
                <div className="min-w-0">
                  <div className="truncate text-sm font-semibold">{capture.name}</div>
                  <div className="text-xs text-slate-500 dark:text-slate-400">
                    {capture.call_count} calls · {capture.record_count} records · {formatTimestamp(capture.created_at)}
                  </div>
                </div>
                <Chip size="sm" variant="soft">{capture.id.slice(0, 6)}</Chip>
              </div>
              <div className="mt-3 flex gap-1">
                <Button size="sm" variant="secondary" onPress={() => onLoad(capture.id)}>Load</Button>
                <Button size="sm" variant="secondary" onPress={() => onExportJSON(capture)}>JSON</Button>
                <Button size="sm" variant="secondary" onPress={() => onExportJSONL(capture)}>JSONL</Button>
                <Button isIconOnly size="sm" variant="danger" onPress={() => onDelete(capture.id)} aria-label="Delete capture">
                  <Trash2 className="h-4 w-4" />
                </Button>
              </div>
            </div>
          ))}
          {captures.length === 0 && <EmptyState icon={<Archive className="h-5 w-5" />} label="No saved captures" />}
        </div>
      </ScrollShadow>
    </div>
  );
}

function ReplayModal({
  headers,
  body,
  result,
  onHeadersChange,
  onBodyChange,
  onSubmit,
  onClose,
}: {
  headers: string;
  body: string;
  result: ReplayResult | null;
  onHeadersChange: (value: string) => void;
  onBodyChange: (value: string) => void;
  onSubmit: () => void;
  onClose: () => void;
}) {
  const [sending, setSending] = useState(false);
  const submit = async () => {
    setSending(true);
    try {
      await onSubmit();
    } finally {
      setSending(false);
    }
  };

  return (
    <Overlay onClose={onClose}>
      <div className="grid h-[720px] w-[980px] grid-cols-[1fr_1fr] overflow-hidden rounded-md border border-slate-200 bg-white shadow-2xl dark:border-slate-800 dark:bg-slate-950">
        <div className="min-w-0 border-r border-slate-200 dark:border-slate-800">
          <div className="flex h-12 items-center justify-between border-b border-slate-200 px-3 dark:border-slate-800">
            <div className="flex items-center gap-2 text-sm font-semibold"><Send className="h-4 w-4" /> Replay Editor</div>
            <Button isIconOnly size="sm" variant="tertiary" onPress={onClose} aria-label="Close replay">
              <X className="h-4 w-4" />
            </Button>
          </div>
          <div className="grid h-[calc(100%-96px)] grid-rows-[1fr_1fr] gap-3 p-3">
            <Editor label="Body JSON" value={body} onChange={onBodyChange} />
            <Editor label="Headers JSON" value={headers} onChange={onHeadersChange} />
          </div>
          <div className="flex h-12 items-center justify-end gap-2 border-t border-slate-200 px-3 dark:border-slate-800">
            <Button size="sm" variant="secondary" onPress={() => navigator.clipboard.writeText(`headers=${headers}\nbody=${body}`)}>
              Copy cURL Parts
            </Button>
            <Button size="sm" variant="primary" isDisabled={sending} onPress={submit}>
              <Send className="h-4 w-4" /> Send
            </Button>
          </div>
        </div>
        <div className="min-w-0">
          <PaneHeader icon={<Sparkles className="h-4 w-4" />} title="Replay Result" meta={result ? String(result.status) : 'pending'} />
          <div className="h-[calc(100%-44px)] overflow-auto p-3">
            {result ? (
              <div className="space-y-3">
                <div className="rounded-md border border-slate-200 p-3 text-sm dark:border-slate-800">
                  <div className="font-semibold">{result.status_text}</div>
                  <div className="text-xs text-slate-500 dark:text-slate-400">saved as {result.call_id}</div>
                </div>
                <CodeBlock value={JSON.stringify(result.records, null, 2)} language="json" maxHeight="560px" />
              </div>
            ) : (
              <EmptyState icon={<Send className="h-5 w-5" />} label="Send replay to see parsed response" />
            )}
          </div>
        </div>
      </div>
    </Overlay>
  );
}

function Editor({ label, value, onChange }: { label: string; value: string; onChange: (value: string) => void }) {
  return (
    <label className="flex min-h-0 flex-col gap-2 text-xs font-semibold text-slate-500 dark:text-slate-400">
      {label}
      <textarea
        className="min-h-0 flex-1 resize-none rounded-md border border-slate-200 bg-slate-50 p-3 font-mono text-xs text-slate-900 outline-none focus:border-blue-400 dark:border-slate-800 dark:bg-slate-900 dark:text-slate-100"
        value={value}
        onChange={(event) => onChange(event.target.value)}
      />
    </label>
  );
}

function Overlay({ children, onClose }: { children: ReactNode; onClose: () => void }) {
  return (
    <div className="absolute inset-0 z-50 flex items-center justify-center bg-slate-950/30 p-6 backdrop-blur-sm" onMouseDown={onClose}>
      <div onMouseDown={(event) => event.stopPropagation()}>{children}</div>
    </div>
  );
}

function PaneHeader({ icon, title, meta }: { icon: ReactNode; title: string; meta?: string }) {
  return (
    <div className="flex h-11 shrink-0 items-center justify-between border-b border-slate-200 bg-[var(--ct-bg-pane)] px-3 dark:border-slate-800">
      <div className="flex items-center gap-2 text-[12px] font-semibold uppercase text-[var(--ct-text-2)]">
        {icon}
        {title}
      </div>
      {meta && <span className="mono text-[11px] text-[var(--ct-text-4)]">{meta}</span>}
    </div>
  );
}

function CallCard({
  call,
  captureWindow,
  active,
  focused,
  highlighted,
  buttonRef,
  onFocus,
  onClick,
}: {
  call: RpcCall;
  captureWindow: TimelineRange;
  active: boolean;
  focused: boolean;
  highlighted: boolean;
  buttonRef: (element: HTMLButtonElement | null) => void;
  onFocus: () => void;
  onClick: () => void;
}) {
  const bytes = call.request_bytes + call.response_bytes;
  const left = ((callStart(call) - captureWindow.start) / captureWindow.span) * 100;
  const width = Math.max(0.8, ((callEnd(call) - callStart(call)) / captureWindow.span) * 100);
  const agentRun = call.service === 'agent.v1.AgentService' && call.method === 'Run';
  const agentPrompt = agentRun ? extractAgentPromptFromPreview(call.request_preview) : '';
  const agentResponse = agentRun ? compactPreview(call.response_preview, 120) : '';
  return (
    <button
      type="button"
      ref={buttonRef}
      onClick={onClick}
      onFocus={onFocus}
      aria-label={`${call.method || 'unknown'} ${call.service || call.host || ''}`}
      className={cn(
        'ct-call-row',
        active && 'ct-call-row--active',
        focused && 'ct-call-row--focused',
        !highlighted && 'ct-call-row--dimmed',
      )}
    >
      <div className="mb-1 flex items-center gap-1.5">
        <span className={cn('ct-mini-chip', call.streaming ? 'ct-mini-chip--stream' : 'ct-mini-chip--default')}>
          {call.streaming ? 'stream' : 'unary'}
        </span>
        {call.status === 'error' && <span className="ct-mini-chip ct-mini-chip--danger">error</span>}
        <div className="min-w-0">
          <div className="truncate text-[12px] font-semibold text-[var(--ct-text)]">{call.method || 'unknown method'}</div>
        </div>
        <span className="mono ml-auto shrink-0 text-[9.5px] text-[var(--ct-text-3)]">{call.duration_ms}ms</span>
      </div>
      <div className="mono mb-1 truncate text-[10px] text-[var(--ct-text-3)]">{call.service || call.host}</div>
      {agentRun && (
        <div className="mb-1 space-y-0.5 rounded-[5px] border border-blue-100 bg-blue-50 px-2 py-1 text-left dark:border-blue-900/60 dark:bg-blue-950/30">
          <div className="truncate text-[10.5px] font-semibold text-blue-700 dark:text-blue-200">
            LLM request: {agentPrompt || 'decoded request'}
          </div>
          <div className="truncate text-[10.5px] text-emerald-700 dark:text-emerald-200">
            Response: {agentResponse || 'streaming...'}
          </div>
        </div>
      )}
      <div className="ct-call-mini-timeline">
        <span
          className={cn('ct-call-mini-bar', call.status === 'error' ? 'ct-call-mini-bar--error' : call.streaming ? 'ct-call-mini-bar--stream' : 'ct-call-mini-bar--unary')}
          style={{ left: `${Math.max(0, Math.min(99, left))}%`, width: `${Math.min(100, width)}%` }}
        />
        {call.streaming && Array.from({ length: Math.min(call.frame_count, 18) }).map((_, index) => (
          <span
            key={index}
            className="ct-call-mini-marker"
            style={{ left: `${Math.max(0, Math.min(100, left + (width * index) / Math.max(call.frame_count - 1, 1)))}%` }}
          />
        ))}
      </div>
      <div className="mt-1 flex items-center gap-2 text-[10px] text-[var(--ct-text-3)]">
        <span>{call.duration_ms}ms</span>
        <span>{call.frame_count} frames</span>
        <span>{formatBytes(bytes)}</span>
      </div>
    </button>
  );
}

function FrameRow({ record, active, onClick }: { record: Record; active: boolean; onClick: () => void }) {
  const directionColor = record.direction === 'C2S' ? 'text-blue-600' : record.direction === 'S2C' ? 'text-emerald-600' : 'text-slate-500';
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn('ct-frame-row', active && 'ct-frame-row--active')}
    >
      <div className="flex items-center gap-2">
        <Circle className={cn('h-3 w-3 fill-current', directionColor)} />
        <span className="mono text-[10px] text-[var(--ct-text-4)]">#{record.index}</span>
        <span className={cn('text-[11px] font-semibold', directionColor)}>{record.direction || record.type}</span>
        <span className="ml-auto text-[10.5px] text-[var(--ct-text-4)]">{record.size ? formatBytes(record.size) : formatTimestamp(record.ts)}</span>
      </div>
      <div className="mt-1 truncate text-[11.5px] text-[var(--ct-text)]">{recordTitle(record)}</div>
    </button>
  );
}

function DetailScroll({ children }: { children: ReactNode }) {
  return <div className="h-full min-w-0 overflow-auto overscroll-contain p-3">{children}</div>;
}

function RawInspector({ value }: { value: string }) {
  const report = useMemo(() => lintRawJSON(value), [value]);
  const display = useMemo(() => buildRawDisplay(value, report.parsed), [report.parsed, value]);
  const hasPreview = display !== value;

  return (
    <div className="grid gap-2">
      <div
        className={cn(
          'rounded-md border px-3 py-2 text-[11px]',
          report.ok
            ? 'border-emerald-200 bg-emerald-50 text-emerald-900 dark:border-emerald-900/60 dark:bg-emerald-950/30 dark:text-emerald-100'
            : 'border-rose-200 bg-rose-50 text-rose-900 dark:border-rose-900/60 dark:bg-rose-950/30 dark:text-rose-100',
        )}
      >
        <div className="flex flex-wrap items-center gap-2">
          <span className="flex items-center gap-1.5 font-bold uppercase">
            {report.ok ? <CheckCircle2 className="h-3.5 w-3.5" /> : <AlertTriangle className="h-3.5 w-3.5" />}
            Raw JSON Lint
          </span>
          <span className="ct-mini-chip ct-mini-chip--default">{report.ok ? 'valid json' : 'invalid json'}</span>
          <span className="mono ml-auto text-[10px] opacity-75">
            {report.lines.toLocaleString()} lines · {formatBytes(report.bytes)}
          </span>
        </div>
        <div className="mt-1 leading-5">
          {report.ok ? (
            <span>
              Parsed successfully. {hasPreview ? 'Large fields are shortened for highlighted preview; copy still uses the full raw payload.' : 'Full payload is highlighted below.'}
            </span>
          ) : (
            <span>
              {report.message}
              {report.line && report.column ? ` at line ${report.line}, column ${report.column}` : ''}
            </span>
          )}
        </div>
      </div>
      <CodeBlock value={display} copyValue={value} language={jsonLike(display) ? 'json' : 'text'} maxHeight="620px" />
    </div>
  );
}

function PayloadInspector({ record, fallbackPayload }: { record: Record | null; fallbackPayload: string }) {
  const view = useMemo(() => buildPayloadView(record, fallbackPayload), [fallbackPayload, record]);
  return (
    <section className="overflow-hidden rounded-md border border-slate-200 bg-white text-slate-900 dark:border-slate-800 dark:bg-slate-950 dark:text-slate-100">
      <div className="flex flex-wrap items-center gap-2 border-b border-slate-200 px-3 py-2 dark:border-slate-800">
        <span className="flex items-center gap-1.5 text-xs font-bold uppercase text-slate-700 dark:text-slate-200">
          <FileCode2 className="h-3.5 w-3.5" />
          {view.title}
        </span>
        <span className="ct-mini-chip ct-mini-chip--default">{view.source}</span>
        {view.kind && <span className="ct-mini-chip ct-mini-chip--stream">{view.kind}</span>}
        <span className="mono ml-auto text-[10px] text-[var(--ct-text-3)]">
          {view.bytesLabel}
        </span>
      </div>
      <div className="grid gap-2 p-3">
        {view.textDelta && (
          <PayloadCallout
            label="interactionUpdate.textDelta.text"
            tone="emerald"
            value={view.textDelta}
          />
        )}
        <div className="grid grid-cols-2 gap-2 text-[11px] md:grid-cols-4">
          <PayloadMeta label="record" value={view.recordLabel} />
          <PayloadMeta label="direction" value={view.direction || '-'} />
          <PayloadMeta label="frame" value={view.frameLabel} />
          <PayloadMeta label="encoding" value={view.encoding || 'json/text'} />
        </div>
        {view.topKeys.length > 0 && (
          <div className="flex flex-wrap gap-1.5">
            {view.topKeys.map((key) => (
              <span key={key} className="rounded border border-slate-200 bg-slate-50 px-1.5 py-0.5 font-mono text-[10px] text-slate-600 dark:border-slate-800 dark:bg-slate-900 dark:text-slate-300">
                {key}
              </span>
            ))}
          </div>
        )}
        <CodeBlock value={view.display} copyValue={view.raw} language={view.language} maxHeight="520px" />
      </div>
    </section>
  );
}

function ResponseFrameCard({ frame }: { frame: AgentResponseFrame }) {
  return (
    <div className="border-b border-sky-50 px-3 py-2 last:border-b-0 dark:border-sky-950/70">
      <div className="mb-2 flex flex-wrap items-center gap-2">
        <span className="mono text-[10px] font-semibold text-sky-600 dark:text-sky-300">
          #{frame.recordIndex}
          {frame.frameIndex >= 0 ? ` · f${frame.frameIndex}` : ''}
        </span>
        <span className="ct-mini-chip ct-mini-chip--default">{frame.direction || 'S2C'}</span>
        {frame.kind && <span className="ct-mini-chip ct-mini-chip--stream">{frame.kind}</span>}
        {frame.textDelta && <span className="ct-mini-chip ct-mini-chip--stream">textDelta</span>}
        <span className="mono ml-auto text-[10px] text-[var(--ct-text-3)]">{frame.bytesLabel}</span>
      </div>
      {frame.textDelta && (
        <PayloadCallout
          label="payload.interactionUpdate.textDelta.text"
          tone="emerald"
          value={frame.textDelta}
        />
      )}
      {frame.topKeys.length > 0 && (
        <div className="mb-2 flex flex-wrap gap-1.5">
          {frame.topKeys.map((key) => (
            <span key={key} className="rounded border border-slate-200 bg-slate-50 px-1.5 py-0.5 font-mono text-[10px] text-slate-600 dark:border-slate-800 dark:bg-slate-900 dark:text-slate-300">
              {key}
            </span>
          ))}
        </div>
      )}
      <CodeBlock value={frame.payload} copyValue={frame.rawPayload} language={frame.language} maxHeight="220px" />
    </div>
  );
}

function PayloadMeta({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-md border border-slate-200 bg-slate-50 px-2 py-1.5 dark:border-slate-800 dark:bg-slate-900">
      <div className="text-[9px] font-bold uppercase text-slate-400">{label}</div>
      <div className="mt-1 truncate font-mono text-[11px] text-slate-700 dark:text-slate-200">{value}</div>
    </div>
  );
}

function PayloadCallout({ label, tone, value }: { label: string; tone: 'emerald' | 'sky'; value: string }) {
  const toneClass = tone === 'emerald'
    ? 'border-emerald-100 bg-emerald-50 text-emerald-950 dark:border-emerald-900/70 dark:bg-emerald-950/30 dark:text-emerald-100'
    : 'border-sky-100 bg-sky-50 text-sky-950 dark:border-sky-900/70 dark:bg-sky-950/30 dark:text-sky-100';
  return (
    <div className={cn('mb-2 rounded-md border px-3 py-2', toneClass)}>
      <div className="mb-1 text-[10px] font-bold uppercase opacity-80">{label}</div>
      <div className="whitespace-pre-wrap break-words font-mono text-[11px] leading-5">{value}</div>
    </div>
  );
}

function AgentRunSummaryPanel({ summary }: { summary: AgentRunSummary }) {
  const response = summary.response || 'No assistant text delta captured yet';
  return (
    <section className="mb-3 overflow-hidden rounded-md border border-blue-200 bg-blue-50/70 text-slate-900 dark:border-blue-900/60 dark:bg-blue-950/30 dark:text-slate-100">
      <div className="flex flex-wrap items-center gap-2 border-b border-blue-200/80 px-3 py-2 dark:border-blue-900/60">
        <span className="flex items-center gap-1.5 text-xs font-bold uppercase text-blue-700 dark:text-blue-200">
          <Sparkles className="h-3.5 w-3.5" />
          LLM Agent Run
        </span>
        <span className="ct-mini-chip ct-mini-chip--stream">{summary.model || 'model unknown'}</span>
        <span className="ct-mini-chip ct-mini-chip--default">{summary.mode || 'agent'}</span>
        <span className="mono ml-auto text-[10px] text-[var(--ct-text-3)]">
          {summary.frameCount} frames · {summary.durationMs}ms
        </span>
      </div>
      <div className="grid gap-2 p-3">
        <div>
          <div className="mb-1 text-[10px] font-bold uppercase text-blue-700/80 dark:text-blue-200/80">User prompt</div>
          <div className="rounded-md border border-blue-100 bg-white px-3 py-2 text-xs leading-5 dark:border-blue-900/70 dark:bg-slate-950">
            {summary.prompt || 'No prompt text decoded'}
          </div>
        </div>
        <div>
          <div className="mb-1 flex items-center justify-between gap-2 text-[10px] font-bold uppercase text-emerald-700/80 dark:text-emerald-200/80">
            <span>Assistant response</span>
            {summary.tokens > 0 && <span className="mono font-semibold normal-case text-[var(--ct-text-3)]">{summary.tokens} tokens</span>}
          </div>
          <div className="rounded-md border border-emerald-100 bg-white px-3 py-2 text-xs leading-5 dark:border-emerald-900/70 dark:bg-slate-950">
            {response}
          </div>
        </div>
        <div>
          <div className="mb-1 flex items-center justify-between gap-2 text-[10px] font-bold uppercase text-sky-700/80 dark:text-sky-200/80">
            <span>Response frames</span>
            <span className="mono font-semibold normal-case text-[var(--ct-text-3)]">
              every S2C frame · grpc_data
            </span>
          </div>
          {summary.responseFrames.length > 0 ? (
            <div className="max-h-80 overflow-auto rounded-md border border-sky-100 bg-white text-xs dark:border-sky-900/70 dark:bg-slate-950">
              {summary.responseFrames.map((frame) => (
                <ResponseFrameCard key={`${frame.recordIndex}-${frame.frameIndex}-${frame.ts}`} frame={frame} />
              ))}
            </div>
          ) : (
            <div className="rounded-md border border-sky-100 bg-white px-3 py-2 text-xs text-slate-500 dark:border-sky-900/70 dark:bg-slate-950 dark:text-slate-400">
              No S2C response frames decoded yet.
            </div>
          )}
        </div>
        <div>
          <div className="mb-1 flex items-center justify-between gap-2 text-[10px] font-bold uppercase text-violet-700/80 dark:text-violet-200/80">
            <span>Text deltas</span>
            <span className="mono font-semibold normal-case text-[var(--ct-text-3)]">
              payload.interactionUpdate.textDelta.text
            </span>
          </div>
          {summary.deltas.length > 0 ? (
            <div className="max-h-44 overflow-auto rounded-md border border-violet-100 bg-white text-xs dark:border-violet-900/70 dark:bg-slate-950">
              {summary.deltas.map((delta) => (
                <div key={`${delta.recordIndex}-${delta.frameIndex}`} className="grid grid-cols-[72px_1fr] gap-2 border-b border-violet-50 px-3 py-2 last:border-b-0 dark:border-violet-950/70">
                  <div className="mono text-[10px] font-semibold text-violet-600 dark:text-violet-300">
                    #{delta.recordIndex}
                    {delta.frameIndex >= 0 ? ` · f${delta.frameIndex}` : ''}
                  </div>
                  <div className="whitespace-pre-wrap break-words font-mono text-[11px] leading-5 text-slate-800 dark:text-slate-100">
                    {delta.text}
                  </div>
                </div>
              ))}
            </div>
          ) : (
            <div className="rounded-md border border-violet-100 bg-white px-3 py-2 text-xs text-slate-500 dark:border-violet-900/70 dark:bg-slate-950 dark:text-slate-400">
              No text delta frames decoded yet.
            </div>
          )}
        </div>
      </div>
    </section>
  );
}

function SelectedFrameResponsePanel({ frame }: { frame: AgentResponseFrame }) {
  return (
    <section className="mb-3 overflow-hidden rounded-md border border-emerald-200 bg-emerald-50 text-slate-900 dark:border-emerald-900/60 dark:bg-emerald-950/30 dark:text-slate-100">
      <div className="flex flex-wrap items-center gap-2 border-b border-emerald-100 px-3 py-2 dark:border-emerald-900/70">
        <span className="text-[10px] font-bold uppercase text-emerald-700 dark:text-emerald-200">
          Selected response frame
        </span>
        <span className="mono text-[10px] text-[var(--ct-text-3)]">
          #{frame.recordIndex}{frame.frameIndex >= 0 ? ` · f${frame.frameIndex}` : ''} · payload.interactionUpdate.textDelta.text
        </span>
        <span className="ct-mini-chip ct-mini-chip--default">{frame.direction || 'S2C'}</span>
        {frame.kind && <span className="ct-mini-chip ct-mini-chip--stream">{frame.kind}</span>}
        <span className="mono ml-auto text-[10px] text-[var(--ct-text-3)]">{frame.bytesLabel}</span>
      </div>
      <div className="grid gap-2 p-3">
        {frame.textDelta && (
          <PayloadCallout
            label="payload.interactionUpdate.textDelta.text"
            tone="emerald"
            value={frame.textDelta}
          />
        )}
        {frame.topKeys.length > 0 && (
          <div className="flex flex-wrap gap-1.5">
            {frame.topKeys.map((key) => (
              <span key={key} className="rounded border border-emerald-100 bg-white/70 px-1.5 py-0.5 font-mono text-[10px] text-emerald-900 dark:border-emerald-900/70 dark:bg-slate-950 dark:text-emerald-100">
                {key}
              </span>
            ))}
          </div>
        )}
        <CodeBlock value={frame.payload} copyValue={frame.rawPayload} language={frame.language} maxHeight="260px" />
      </div>
    </section>
  );
}

function FrameSkeleton() {
  return (
    <div aria-label="Loading stream" className="space-y-1 px-2 py-2">
      {Array.from({ length: 7 }).map((_, index) => (
        <div key={index} className="rounded-md border border-[var(--ct-border)] p-2">
          <div className="h-3 w-2/5 animate-pulse rounded bg-[var(--ct-bg-active)]" />
          <div className="mt-2 h-2.5 w-4/5 animate-pulse rounded bg-[var(--ct-bg-pane-2)]" />
        </div>
      ))}
    </div>
  );
}

function SummaryGrid({ call, record }: { call: RpcCall | null; record: Record | null }) {
  const items = [
    ['service', call?.service || record?.grpc_service || '-'],
    ['method', call?.method || record?.grpc_method || '-'],
    ['host', call?.host || record?.host || '-'],
    ['status', call?.status || record?.status_text || '-'],
    ['duration', call ? `${call.duration_ms}ms` : '-'],
    ['bytes', call ? `${formatBytes(call.request_bytes)} / ${formatBytes(call.response_bytes)}` : record?.size ? formatBytes(record.size) : '-'],
  ];
  return (
    <div className="mb-3 grid grid-cols-3 gap-2">
      {items.map(([label, value]) => (
        <div key={label} className="rounded-md border border-slate-200 p-2 dark:border-slate-800">
          <div className="text-[10px] uppercase text-slate-400">{label}</div>
          <div className="mt-1 truncate text-xs font-semibold">{value}</div>
        </div>
      ))}
    </div>
  );
}

function TimingWaterfall({ frames }: { frames: Record[] }) {
  const range = timelineRangeFromRecords(frames);
  return (
    <div className="space-y-2">
      {frames.map((frame) => {
        const left = range.span ? ((timeValue(frame.ts) - range.start) / range.span) * 100 : 0;
        return (
          <div key={recordKey(frame)} className="grid grid-cols-[120px_1fr_80px] items-center gap-3 text-xs">
            <div className="truncate text-slate-500 dark:text-slate-400">{frame.type} {frame.direction}</div>
            <div className="relative h-5 rounded bg-slate-100 dark:bg-slate-900">
              <div className="absolute top-1 h-3 rounded-sm bg-blue-500" style={{ left: `${Math.max(0, left)}%`, width: `${Math.max(2, Math.min(30, (frame.size || 20) / 120))}%` }} />
            </div>
            <div className="font-mono text-[11px] text-slate-400">{formatTimestamp(frame.ts)}</div>
          </div>
        );
      })}
      {frames.length === 0 && <EmptyState icon={<Clock3 className="h-5 w-5" />} label="No timing data" />}
    </div>
  );
}

function EmptyState({ icon, label }: { icon: ReactNode; label: string }) {
  return (
    <div className="flex h-40 flex-col items-center justify-center gap-2 rounded-md border border-dashed border-slate-200 text-sm text-slate-400 dark:border-slate-800">
      {icon}
      <span>{label}</span>
    </div>
  );
}

function LazyLoadStatus({ current, total, label, onLoadMore }: { current: number; total: number; label: string; onLoadMore: () => void }) {
  const pct = total > 0 ? Math.round((current / total) * 100) : 100;
  return (
    <div className="mx-2 my-2 rounded-md border border-slate-200 bg-slate-50 px-3 py-2 text-[11px] text-slate-500 dark:border-slate-800 dark:bg-slate-900/60 dark:text-slate-400">
      <div className="mb-1 flex items-center justify-between gap-3">
        <span>{label}</span>
        <span className="mono">{current}/{total}</span>
      </div>
      <div className="mb-2 flex items-center justify-between gap-3">
        <span>Scroll to load more</span>
        <button type="button" className="text-[var(--ct-primary)] hover:underline" onClick={onLoadMore}>
          Load more
        </button>
      </div>
      <div className="h-1 overflow-hidden rounded-full bg-slate-200 dark:bg-slate-800">
        <span className="block h-full bg-[var(--ct-primary)]" style={{ width: `${pct}%` }} />
      </div>
    </div>
  );
}

interface ServiceGroup {
  service: string;
  count: number;
  methods: { name: string; count: number }[];
}

interface InspectorStats {
  calls: number;
  frames: number;
  c2s: number;
  s2c: number;
  streams: number;
  errors: number;
  spark: number[];
}

interface TimelineRange {
  start: number;
  end: number;
  span: number;
  duration: number;
}

interface PaletteItem {
  id: string;
  kind: 'call' | 'record' | 'service' | 'method';
  title: string;
  subtitle: string;
  service?: string;
  method?: string;
}

interface AgentRunSummary {
  prompt: string;
  response: string;
  model: string;
  mode: string;
  tokens: number;
  frameCount: number;
  durationMs: number;
  deltas: AgentTextDelta[];
  responseFrames: AgentResponseFrame[];
}

interface AgentTextDelta {
  recordIndex: number;
  frameIndex: number;
  direction: string;
  text: string;
  ts: string;
}

interface AgentResponseFrame {
  recordIndex: number;
  frameIndex: number;
  direction: string;
  kind: string;
  textDelta: string;
  payload: string;
  rawPayload: string;
  language: string;
  topKeys: string[];
  bytesLabel: string;
  ts: string;
}

interface PayloadView {
  title: string;
  source: string;
  kind: string;
  raw: string;
  display: string;
  language: string;
  textDelta: string;
  topKeys: string[];
  direction: string;
  frameLabel: string;
  recordLabel: string;
  encoding: string;
  bytesLabel: string;
}

type JSONRecord = { [key: string]: unknown };

function appendRecord(current: Record[], record: Record, limit: number) {
  const key = recordKey(record);
  if (key && current.some((item) => recordKey(item) === key)) {
    return current;
  }
  return [...current, record].slice(-limit);
}

function pickInitialFrame(records: Record[]) {
  return records.find((record) => (
    record.type === 'grpc' &&
    record.direction === 'C2S' &&
    typeof record.grpc_data === 'string' &&
    record.grpc_data.includes('"runRequest"')
  )) ||
    records.find((record) => record.type === 'grpc' && record.direction === 'C2S') ||
    records.find((record) => (
      record.type === 'grpc' &&
      !(record.grpc_data || '').includes('"heartbeat"')
    )) ||
    records.find((record) => record.type === 'grpc') ||
    records[0];
}

function recordKey(record?: Record) {
  if (!record) return null;
  return `${record.session}-${record.index}`;
}

function recordTitle(record: Record | null) {
  if (!record) return 'No record selected';
  return getRecordTitle(record);
}

function shortService(service: string) {
  const parts = service.split('.');
  return parts.slice(-2).join('.');
}

function buildServiceGroups(calls: RpcCall[]): ServiceGroup[] {
  const groups = new Map<string, Map<string, number>>();
  for (const call of calls) {
    const service = call.service || 'unknown';
    const method = call.method || 'unknown';
    if (!groups.has(service)) groups.set(service, new Map());
    const methods = groups.get(service);
    if (methods) methods.set(method, (methods.get(method) || 0) + 1);
  }
  return Array.from(groups.entries()).map(([service, methods]) => ({
    service,
    count: Array.from(methods.values()).reduce((sum, count) => sum + count, 0),
    methods: Array.from(methods.entries()).map(([name, count]) => ({ name, count })).sort((a, b) => b.count - a.count),
  })).sort((a, b) => b.count - a.count);
}

function buildStats(calls: RpcCall[], records: Record[]): InspectorStats {
  const c2s = calls.reduce((sum, call) => sum + call.request_bytes, 0);
  const s2c = calls.reduce((sum, call) => sum + call.response_bytes, 0);
  const frames = calls.reduce((sum, call) => sum + call.frame_count, 0) || records.filter((record) => record.type === 'grpc').length;
  return {
    calls: calls.length,
    frames,
    c2s,
    s2c,
    streams: calls.filter((call) => call.streaming).length,
    errors: calls.filter((call) => call.status === 'error').length,
    spark: calls.slice(0, 24).map((call) => Math.max(3, Math.min(18, (call.request_bytes + call.response_bytes) / 160))),
  };
}

function recordSearchText(record: Record) {
  return [
    record.session,
    record.type,
    record.method,
    record.url,
    record.host,
    record.status_text,
    record.direction,
    record.grpc_service,
    record.grpc_method,
    record.grpc_data,
    record.event_type,
    record.event_data,
    record.error,
  ].filter(Boolean).join(' ');
}

function buildAgentRunSummary(call: RpcCall | null, frames: Record[]): AgentRunSummary | null {
  const isAgentRun = call?.service === 'agent.v1.AgentService' && call?.method === 'Run';
  const hasAgentFrames = frames.some((frame) => frame.grpc_service === 'agent.v1.AgentService' && frame.grpc_method === 'Run');
  if (!isAgentRun && !hasAgentFrames) {
    return null;
  }

  let prompt = '';
  let model = '';
  let mode = '';
  let response = '';
  let tokens = 0;
  const deltas: AgentTextDelta[] = [];
  const responseFrames: AgentResponseFrame[] = [];

  for (const frame of frames) {
    const payload = parseRecordJSON(frame.grpc_data);
    const responseFrame = buildFrameResponse(frame);
    if (responseFrame) {
      responseFrames.push(responseFrame);
    }
    if (!payload) continue;

    const runRequest = asRecord(payload.runRequest);
    const userMessage = asRecord(asRecord(asRecord(runRequest?.action)?.userMessageAction)?.userMessage);
    if (!prompt && typeof userMessage?.text === 'string') {
      prompt = userMessage.text;
    }
    if (!mode && typeof userMessage?.mode === 'string') {
      mode = userMessage.mode.replace(/^AGENT_MODE_/, '').toLowerCase();
    }
    const modelDetails = asRecord(runRequest?.modelDetails);
    if (!model && typeof modelDetails?.modelId === 'string') {
      model = modelDetails.modelId;
    }

    const textDelta = extractTextDelta(payload);
    if (textDelta) {
      response += textDelta;
      deltas.push({
        recordIndex: frame.index,
        frameIndex: typeof frame.grpc_frame_index === 'number' ? frame.grpc_frame_index : -1,
        direction: frame.direction || '',
        text: textDelta,
        ts: frame.ts,
      });
    }
    const interactionUpdate = asRecord(payload.interactionUpdate);
    const tokenDelta = asRecord(interactionUpdate?.tokenDelta);
    if (typeof tokenDelta?.tokens === 'number') {
      tokens += tokenDelta.tokens;
    }
  }

  if (!response && call?.response_preview && !call.response_preview.trim().startsWith('{')) {
    response = call.response_preview;
  }
  if (!prompt && call?.request_preview) {
    const preview = parseRecordJSON(call.request_preview);
    const previewUserMessage = asRecord(asRecord(asRecord(asRecord(preview?.runRequest)?.action)?.userMessageAction)?.userMessage);
    if (typeof previewUserMessage?.text === 'string') {
      prompt = previewUserMessage.text;
    }
  }

  return {
    prompt,
    response,
    model: model || extractModelFromPreview(call?.request_preview) || 'unknown',
    mode: mode || 'agent',
    tokens,
    frameCount: frames.length || call?.frame_count || 0,
    durationMs: call?.duration_ms || 0,
    deltas,
    responseFrames,
  };
}

function buildFrameResponse(record: Record | null): AgentResponseFrame | null {
  if (!record || record.direction !== 'S2C') {
    return null;
  }
  const rawPayload = record.grpc_data || record.body || record.event_data || record.grpc_raw || record.error || '';
  if (!rawPayload) {
    return null;
  }
  const payload = parseRecordJSON(record.grpc_data);
  const text = extractTextDelta(payload);
  const display = formatFramePayload(rawPayload);
  return {
    recordIndex: record.index,
    frameIndex: typeof record.grpc_frame_index === 'number' ? record.grpc_frame_index : -1,
    direction: record.direction || '',
    kind: payloadKind(payload),
    textDelta: text,
    payload: display,
    rawPayload,
    language: jsonLike(display) ? 'json' : 'text',
    topKeys: topLevelKeys(payload),
    bytesLabel: formatBytes(rawByteLength(rawPayload)),
    ts: record.ts,
  };
}

function buildPayloadView(record: Record | null, fallbackPayload: string): PayloadView {
  const source = payloadSource(record);
  const raw = payloadRawValue(record, fallbackPayload);
  const display = formatFramePayload(raw);
  const parsed = parseRecordJSON(raw);
  return {
    title: payloadTitle(source),
    source,
    kind: payloadKind(parsed),
    raw,
    display,
    language: jsonLike(display) ? 'json' : 'text',
    textDelta: extractTextDelta(parsed),
    topKeys: topLevelKeys(parsed),
    direction: record?.direction || '',
    frameLabel: typeof record?.grpc_frame_index === 'number' ? `f${record.grpc_frame_index}` : '-',
    recordLabel: typeof record?.index === 'number' ? `#${record.index}` : '-',
    encoding: record?.body_encoding || (source.includes('base64') || source === 'grpc_raw' ? 'base64' : ''),
    bytesLabel: formatBytes(rawByteLength(raw)),
  };
}

function payloadRawValue(record: Record | null, fallbackPayload: string) {
  return record?.grpc_data ||
    record?.body ||
    record?.event_data ||
    record?.grpc_raw ||
    record?.body_base64 ||
    record?.error ||
    fallbackPayload ||
    '{}';
}

function payloadSource(record: Record | null) {
  if (record?.grpc_data) return 'grpc_data';
  if (record?.body) return 'body';
  if (record?.event_data) return 'event_data';
  if (record?.grpc_raw) return 'grpc_raw';
  if (record?.body_base64) return 'body_base64';
  if (record?.error) return 'error';
  return 'payload';
}

function payloadTitle(source: string) {
  switch (source) {
    case 'grpc_data':
      return 'Decoded gRPC Data';
    case 'grpc_raw':
      return 'Raw gRPC Payload';
    case 'body_base64':
      return 'Base64 Body';
    case 'event_data':
      return 'Event Data';
    case 'body':
      return 'HTTP Body';
    case 'error':
      return 'Error Payload';
    default:
      return 'Payload';
  }
}

function payloadKind(payload: JSONRecord | null) {
  if (!payload) return '';
  if (payload.interactionUpdate) return 'interactionUpdate';
  if (payload.runRequest) return 'runRequest';
  if (payload.conversationSummary) return 'conversationSummary';
  const first = Object.keys(payload)[0];
  return first || '';
}

function topLevelKeys(payload: JSONRecord | null) {
  if (!payload) return [];
  return Object.keys(payload).slice(0, 8);
}

function formatFramePayload(value: string) {
  const formatted = formatJSON(value || '{}');
  return formatted.length > RAW_HIGHLIGHT_PREVIEW_CHARS
    ? `${formatted.slice(0, RAW_HIGHLIGHT_PREVIEW_CHARS)}\n... <truncated ${formatBytes(rawByteLength(formatted) - rawByteLength(formatted.slice(0, RAW_HIGHLIGHT_PREVIEW_CHARS)))}>`
    : formatted;
}

function extractTextDelta(payload: JSONRecord | null) {
  const interactionUpdate = asRecord(payload?.interactionUpdate);
  const textDelta = interactionUpdate?.textDelta;
  if (typeof textDelta === 'string') {
    return textDelta;
  }
  const textDeltaRecord = asRecord(textDelta);
  return typeof textDeltaRecord?.text === 'string' ? textDeltaRecord.text : '';
}

function parseRecordJSON(value?: string) {
  if (!value) return null;
  try {
    return JSON.parse(value) as JSONRecord;
  } catch {
    return null;
  }
}

function asRecord(value: unknown): JSONRecord | null {
  return value && typeof value === 'object' && !Array.isArray(value) ? value as JSONRecord : null;
}

function extractModelFromPreview(value?: string) {
  const payload = parseRecordJSON(value);
  const modelDetails = asRecord(asRecord(payload?.runRequest)?.modelDetails);
  return typeof modelDetails?.modelId === 'string' ? modelDetails.modelId : '';
}

function extractAgentPromptFromPreview(value?: string) {
  const payload = parseRecordJSON(value);
  const direct = asRecord(asRecord(payload?.runRequest)?.userMessage);
  const nested = asRecord(asRecord(asRecord(asRecord(payload?.runRequest)?.action)?.userMessageAction)?.userMessage);
  const text = direct?.text || nested?.text;
  return typeof text === 'string' ? text : '';
}

function compactPreview(value?: string, maxLength = 140) {
  if (!value) return '';
  const clean = value.replace(/\s+/g, ' ').trim();
  return clean.length > maxLength ? `${clean.slice(0, maxLength - 1)}...` : clean;
}

function buildPaletteItems(serviceGroups: ServiceGroup[], calls: RpcCall[], records: Record[]): PaletteItem[] {
  const serviceItems = serviceGroups.slice(0, 8).map((group) => ({
    id: group.service,
    kind: 'service' as const,
    title: shortService(group.service),
    subtitle: `${group.service} · ${group.count} calls`,
    service: group.service,
  }));
  const methodItems = serviceGroups.flatMap((group) => group.methods.map((method) => ({
    id: `${group.service}/${method.name}`,
    kind: 'method' as const,
    title: method.name,
    subtitle: `${group.service} · ${method.count} calls`,
    service: group.service,
    method: method.name,
  })));
  const callItems = calls.slice(0, 12).map((call) => ({
    id: call.id,
    kind: 'call' as const,
    title: call.full_method || call.method || call.id,
    subtitle: `${call.host} · ${call.frame_count} frames`,
  }));
  const recordItems = records.slice(-8).reverse().map((record) => ({
    id: recordKey(record) || '',
    kind: 'record' as const,
    title: recordTitle(record),
    subtitle: `${record.session} · ${record.host || 'local'}`,
  }));
  return [...serviceItems, ...methodItems, ...callItems, ...recordItems];
}

function timelineRange(calls: RpcCall[]): TimelineRange {
  if (calls.length === 0) {
    return { start: 0, end: 1, span: 1, duration: 1 };
  }
  const times = calls.flatMap((call) => [callStart(call), callEnd(call)]).filter(Boolean);
  const start = times.length ? Math.min(...times) : 0;
  const end = times.length ? Math.max(...times) : start + 1;
  return { start, end, span: Math.max(1, end - start), duration: Math.max(1, Math.max(...calls.map((call) => call.duration_ms || 1), 1)) };
}

function callStart(call: RpcCall) {
  return timeValue(call.started_at) || timeValue(call.ended_at);
}

function callEnd(call: RpcCall) {
  const explicitEnd = timeValue(call.ended_at);
  if (explicitEnd) return explicitEnd;
  return callStart(call) + Math.max(call.duration_ms, 1);
}

function callsNearTime(calls: RpcCall[], time: number, span: number) {
  const windowMs = Math.max(50, Math.min(1000, span * 0.012));
  return calls
    .filter((call) => callStart(call) - windowMs <= time && callEnd(call) + windowMs >= time)
    .sort((a, b) => distanceToCall(time, a) - distanceToCall(time, b))
    .slice(0, 24);
}

function distanceToCall(time: number, call: RpcCall) {
  const start = callStart(call);
  const end = callEnd(call);
  if (time >= start && time <= end) return 0;
  return Math.min(Math.abs(time - start), Math.abs(time - end));
}

function mergeCalls(primary: RpcCall[], secondary: RpcCall[]) {
  const seen = new Set<string>();
  const merged: RpcCall[] = [];
  for (const call of [...primary, ...secondary]) {
    if (seen.has(call.id)) continue;
    seen.add(call.id);
    merged.push(call);
  }
  return merged;
}

function callDisplayLabel(call: RpcCall) {
  if (call.method) return call.method;
  if (call.full_method) return call.full_method.split('/').filter(Boolean).pop() || call.full_method;
  if (call.url) return call.url.split('/').filter(Boolean).pop() || call.url;
  return 'unknown';
}

function callBytes(call: RpcCall) {
  return (call.request_bytes || 0) + (call.response_bytes || 0);
}

function buildTimelineBuckets(calls: RpcCall[], range: TimelineRange, bucketCount: number) {
  const buckets = new Array(bucketCount).fill(0) as number[];
  for (const call of calls) {
    const index = Math.floor(((callStart(call) - range.start) / range.span) * bucketCount);
    if (index >= 0 && index < bucketCount) {
      buckets[index] += Math.max(call.frame_count, 1);
    }
  }
  return buckets;
}

function timelineRangeFromRecords(records: Record[]) {
  const times = records.map((record) => timeValue(record.ts)).filter(Boolean);
  const start = times.length ? Math.min(...times) : Date.now();
  const end = times.length ? Math.max(...times) : start + 1;
  return { start, end, span: Math.max(1, end - start) };
}

function timeValue(value?: string) {
  if (!value) return 0;
  const parsed = new Date(value).getTime();
  return Number.isFinite(parsed) ? parsed : 0;
}

function toAPITime(value: string) {
  const trimmed = value.trim();
  if (!trimmed) return '';
  const parsed = new Date(trimmed).getTime();
  return Number.isFinite(parsed) ? new Date(parsed).toISOString() : '';
}

function maxAPITime(primary: string, secondary: string) {
  if (!primary) return secondary;
  if (!secondary) return primary;
  return timeValue(primary) >= timeValue(secondary) ? primary : secondary;
}

function isAtOrAfterWatermark(value?: string, watermark?: string | null) {
  if (!watermark) return true;
  const current = timeValue(value);
  const threshold = timeValue(watermark);
  if (!current || !threshold) return true;
  return current >= threshold;
}

function formatTimelineTime(value: number) {
  return new Date(value).toLocaleTimeString('en-US', {
    hour12: false,
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    fractionalSecondDigits: 3,
  });
}

function formatBytes(value: number) {
  if (!value) return '0 B';
  if (value < 1024) return `${value} B`;
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KB`;
  return `${(value / (1024 * 1024)).toFixed(1)} MB`;
}

function formatJSON(value: string) {
  try {
    return JSON.stringify(JSON.parse(value), null, 2);
  } catch {
    return value || '{}';
  }
}

type RawLintReport = {
  ok: boolean;
  bytes: number;
  lines: number;
  message: string;
  line?: number;
  column?: number;
  parsed?: unknown;
};

function lintRawJSON(value: string): RawLintReport {
  const input = value || '{}';
  try {
    const parsed = JSON.parse(input);
    return {
      ok: true,
      bytes: rawByteLength(input),
      lines: countLines(input),
      message: 'Valid JSON',
      parsed,
    };
  } catch (error) {
    const message = error instanceof Error ? error.message : 'Invalid JSON';
    const position = jsonErrorPosition(message);
    const location = position == null ? undefined : lineColumnFromPosition(input, position);
    return {
      ok: false,
      bytes: rawByteLength(input),
      lines: countLines(input),
      message,
      line: location?.line,
      column: location?.column,
    };
  }
}

function buildRawDisplay(value: string, parsed: unknown) {
  if (parsed === undefined) {
    return value || '{}';
  }
  const formatted = JSON.stringify(parsed, null, 2);
  if (formatted.length <= RAW_HIGHLIGHT_PREVIEW_CHARS) {
    return formatted;
  }
  return JSON.stringify(trimRawForHighlight(parsed), null, 2);
}

function trimRawForHighlight(value: unknown): unknown {
  if (typeof value === 'string') {
    if (value.length <= RAW_LARGE_STRING_CHARS) {
      return value;
    }
    return `${value.slice(0, RAW_LARGE_STRING_CHARS)}... <truncated ${formatBytes(rawByteLength(value) - rawByteLength(value.slice(0, RAW_LARGE_STRING_CHARS)))}>`;
  }
  if (Array.isArray(value)) {
    const items = value.slice(0, RAW_LARGE_ARRAY_ITEMS).map(trimRawForHighlight);
    if (value.length > RAW_LARGE_ARRAY_ITEMS) {
      items.push({ __cursorTapPreview: `truncated ${value.length - RAW_LARGE_ARRAY_ITEMS} array items` });
    }
    return items;
  }
  if (value && typeof value === 'object') {
    const entries = Object.entries(value as { [key: string]: unknown });
    const trimmed: { [key: string]: unknown } = {};
    for (const [key, entryValue] of entries.slice(0, RAW_LARGE_OBJECT_KEYS)) {
      trimmed[key] = trimRawForHighlight(entryValue);
    }
    if (entries.length > RAW_LARGE_OBJECT_KEYS) {
      trimmed.__cursorTapPreview = `truncated ${entries.length - RAW_LARGE_OBJECT_KEYS} object keys`;
    }
    return trimmed;
  }
  return value;
}

function rawByteLength(value: string) {
  return new TextEncoder().encode(value).length;
}

function countLines(value: string) {
  if (!value) return 1;
  return value.split('\n').length;
}

function jsonErrorPosition(message: string) {
  const match = message.match(/position\s+(\d+)/i);
  if (!match) return null;
  return Number(match[1]);
}

function lineColumnFromPosition(value: string, position: number) {
  const before = value.slice(0, Math.max(0, position));
  const lines = before.split('\n');
  return {
    line: lines.length,
    column: lines[lines.length - 1].length + 1,
  };
}

function jsonLike(value: string) {
  const trimmed = value.trim();
  return trimmed.startsWith('{') || trimmed.startsWith('[');
}

function buildSequence(call: RpcCall | null, frames: Record[]) {
  const method = call?.full_method || frames.find((frame) => frame.grpc_service)?.url || 'RPC';
  const lines = [
    'sequenceDiagram',
    '  participant Cursor',
    '  participant Proxy',
    '  participant Upstream as api2.cursor.sh',
    `  Cursor->>Proxy: ${escapeMermaid(method)}`,
    '  Proxy->>Upstream: CONNECT/TLS + gRPC',
  ];
  for (const frame of frames.filter((item) => item.type === 'grpc').slice(0, 6)) {
    const arrow = frame.direction === 'C2S' ? 'Cursor->>Proxy' : 'Upstream-->>Proxy';
    lines.push(`  ${arrow}: frame #${frame.index} ${escapeMermaid(frame.grpc_method || frame.type)}`);
  }
  lines.push('  Proxy-->>Cursor: parsed record + WebSocket');
  return lines.join('\n');
}

function escapeMermaid(value: string) {
  return value.replace(/[:;]/g, ' ').slice(0, 80);
}
