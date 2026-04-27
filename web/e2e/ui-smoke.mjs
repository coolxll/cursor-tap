import { chromium } from '@playwright/test';
import { spawn } from 'node:child_process';
import { readFileSync } from 'node:fs';
import http from 'node:http';

const apiPort = await freePort();
const webPort = await freePort();
const apiBase = `http://127.0.0.1:${apiPort}`;
const baseTime = Date.now();
const now = new Date(baseTime).toISOString();
const at = (offsetMs) => new Date(baseTime + offsetMs).toISOString();

const requestRecord = {
  ts: now,
  session: 'e2e-session',
  seq: 1,
  index: 1,
  type: 'request',
  method: 'POST',
  host: 'api2.cursor.sh',
  url: '/e2e.v1.EchoService/Echo',
  content_type: 'application/connect+proto',
  headers: {
    'Content-Type': ['application/connect+proto'],
    Authorization: ['Bearer e2e-token'],
  },
};

const c2sRecord = {
  ts: now,
  session: 'e2e-session',
  seq: 1,
  index: 2,
  type: 'grpc',
  direction: 'C2S',
  host: 'api2.cursor.sh',
  url: '/e2e.v1.EchoService/Echo',
  grpc_service: 'e2e.v1.EchoService',
  grpc_method: 'Echo',
  grpc_data: JSON.stringify({ message: 'ping' }),
  grpc_streaming: true,
  grpc_frame_index: 0,
  content_type: 'application/connect+proto',
  size: 8,
};

const responseRecord = {
  ts: now,
  session: 'e2e-session',
  seq: 1,
  index: 3,
  type: 'response',
  status: 200,
  status_text: '200 OK',
  host: 'api2.cursor.sh',
  content_type: 'application/connect+proto',
  headers: {
    'Content-Type': ['application/connect+proto'],
  },
};

const s2cRecord = {
  ...c2sRecord,
  index: 4,
  direction: 'S2C',
  grpc_data: JSON.stringify({ message: 'pong' }),
};
const timedCallStart = at(60_000);
const timedCallEnd = at(61_000);

const streamRecords = Array.from({ length: 56 }, (_, index) => ({
  ...c2sRecord,
  ts: at(50 + index * 3),
  index: index + 5,
  direction: index % 2 === 0 ? 'S2C' : 'C2S',
  grpc_data: JSON.stringify({ message: `stream-${index}` }),
  grpc_frame_index: index + 1,
  size: 12 + index,
}));
const records = [requestRecord, c2sRecord, responseRecord, s2cRecord, ...streamRecords];
const e2eFrameCount = records.filter((record) => record.type === 'grpc').length;
const calls = [{
  id: 'e2e-session',
  seq: 1,
  host: 'api2.cursor.sh',
  url: '/e2e.v1.EchoService/Echo',
  service: 'e2e.v1.EchoService',
  method: 'Echo',
  full_method: '/e2e.v1.EchoService/Echo',
  status: 'ok',
  http_status: 200,
  streaming: true,
  started_at: now,
  ended_at: now,
  duration_ms: 42,
  record_count: records.length,
  frame_count: e2eFrameCount,
  request_bytes: 8,
  response_bytes: 8,
  request_preview: JSON.stringify({ message: 'ping' }),
  response_preview: JSON.stringify({ message: 'pong' }),
  origin: 'live',
}, ...Array.from({ length: 9 }, (_, index) => ({
  id: `lane-session-${index}`,
  seq: index + 10,
  host: 'api2.cursor.sh',
  url: `/lane.v1.Service${index}/Ping`,
  service: `lane.v1.Service${index}`,
  method: 'Ping',
  full_method: `/lane.v1.Service${index}/Ping`,
  status: 'ok',
  http_status: 200,
  streaming: index % 2 === 0,
  started_at: at(10 + index * 8),
  ended_at: at(24 + index * 8),
  duration_ms: 14,
  record_count: 1,
  frame_count: 1,
  request_bytes: 4,
  response_bytes: 4,
  request_preview: JSON.stringify({ lane: index }),
  response_preview: JSON.stringify({ ok: true }),
  origin: 'live',
})), ...Array.from({ length: 10 }, (_, index) => ({
  id: `tool-method-${index}`,
  seq: index + 30,
  host: 'api2.cursor.sh',
  url: `/bulk.v1.ToolService/Tool${index}`,
  service: 'bulk.v1.ToolService',
  method: `Tool${index}`,
  full_method: `/bulk.v1.ToolService/Tool${index}`,
  status: 'ok',
  http_status: 200,
  streaming: index % 3 === 0,
  started_at: at(120 + index * 3),
  ended_at: at(260 + index * 3),
  duration_ms: 140,
  record_count: 1,
  frame_count: 3,
  request_bytes: 32 + index,
  response_bytes: 64 + index,
  request_preview: JSON.stringify({ tool: index }),
  response_preview: JSON.stringify({ done: true }),
  origin: 'live',
})), {
  id: 'timed-session',
  seq: 80,
  host: 'api2.cursor.sh',
  url: '/time.v1.ClockService/Tick',
  service: 'time.v1.ClockService',
  method: 'Tick',
  full_method: '/time.v1.ClockService/Tick',
  status: 'ok',
  http_status: 200,
  streaming: false,
  started_at: timedCallStart,
  ended_at: timedCallEnd,
  duration_ms: 1000,
  record_count: 2,
  frame_count: 1,
  request_bytes: 16,
  response_bytes: 32,
  request_preview: JSON.stringify({ tick: true }),
  response_preview: JSON.stringify({ ok: true }),
  origin: 'live',
}];
const framesBySession = new Map([
  ['e2e-session', records],
  ...calls.slice(1).map((call, index) => [call.id, makeCallFrames(call, index)]),
]);
const captures = [];
const callRequests = [];
let trafficCleared = false;

const api = http.createServer(async (req, res) => {
  res.setHeader('Access-Control-Allow-Origin', '*');
  res.setHeader('Access-Control-Allow-Headers', 'Content-Type');
  res.setHeader('Access-Control-Allow-Methods', 'GET, POST, DELETE, OPTIONS');
  res.setHeader('Content-Type', 'application/json');
  if (req.method === 'OPTIONS') {
    res.end('{}');
    return;
  }
  if (req.url?.startsWith('/api/status')) {
    res.end(JSON.stringify({
      status: 'running',
      http_port: 8080,
      socks5_port: 1080,
      api_port: apiPort,
      sqlite_path: '/tmp/cursor-tap.sqlite',
      protocol_path: 'cursor-fixture.js',
      active_protocol: 'fixture',
      ws_clients: 0,
    }));
    return;
  }
  if (req.url?.startsWith('/api/protocols')) {
    res.end(JSON.stringify([{
      id: 'fixture',
      created_at: now,
      path: 'cursor-fixture.js',
      sha256: 'fixture',
      source_kind: 'js',
      messages: 2,
      enums: 0,
      services: 1,
      diagnostics: [],
      proto_sources: {
        'e2e_v1.proto': 'syntax = "proto3";\npackage e2e.v1;\nmessage EchoRequest { string message = 1; }\nmessage EchoResponse { string message = 1; }\nservice EchoService { rpc Echo(EchoRequest) returns (EchoResponse) {} }\n',
      },
      active: true,
    }]));
    return;
  }
  if (req.url?.startsWith('/api/records')) {
    if (req.method === 'DELETE') {
      trafficCleared = true;
      res.end(JSON.stringify({ ok: true }));
      return;
    }
    res.end(JSON.stringify(trafficCleared ? [] : records));
    return;
  }
  if (req.url?.startsWith('/api/sessions')) {
    res.end(JSON.stringify([]));
    return;
  }
  if (req.url?.startsWith('/api/calls/') && req.url.endsWith('/frames')) {
    const sessionID = decodeURIComponent(req.url.slice('/api/calls/'.length, -'/frames'.length));
    res.end(JSON.stringify(trafficCleared ? [] : framesBySession.get(sessionID) || []));
    return;
  }
  if (req.url?.startsWith('/api/calls')) {
    const parsed = new URL(req.url, apiBase);
    callRequests.push(parsed);
    const query = (parsed.searchParams.get('search') || parsed.searchParams.get('q') || '').toLowerCase();
    const kind = parsed.searchParams.get('kind') || 'all';
    const service = parsed.searchParams.get('service') || '';
    const method = parsed.searchParams.get('method') || '';
    const startedAfter = Date.parse(parsed.searchParams.get('started_after') || '');
    const startedBefore = Date.parse(parsed.searchParams.get('started_before') || '');
    const filteredCalls = (trafficCleared ? [] : calls).filter((call) => {
      if (service && call.service !== service) return false;
      if (method && call.method !== method) return false;
      const startedAt = Date.parse(call.started_at);
      if (Number.isFinite(startedAfter) && startedAt < startedAfter) return false;
      if (Number.isFinite(startedBefore) && startedAt > startedBefore) return false;
      if (kind === 'streaming' && !call.streaming) return false;
      if (kind === 'unary' && call.streaming) return false;
      if (kind === 'errors' && call.status !== 'error') return false;
      if (!query) return true;
      return [
        call.id,
        call.host,
        call.url,
        call.service,
        call.method,
        call.full_method,
      ].filter(Boolean).join(' ').toLowerCase().includes(query);
    });
    res.end(JSON.stringify(filteredCalls));
    return;
  }
  if (req.url?.startsWith('/api/captures')) {
    const parsed = new URL(req.url, apiBase);
    if (req.method === 'POST') {
      const body = await readJSON(req);
      const capture = {
        id: `capture-${captures.length + 1}`,
        created_at: new Date().toISOString(),
        name: body.name || 'E2E capture',
        call_count: 1,
        record_count: records.length,
        records,
      };
      captures.unshift(capture);
      res.end(JSON.stringify(capture));
      return;
    }
    if (req.method === 'DELETE') {
      const id = parsed.searchParams.get('id');
      const index = captures.findIndex((capture) => capture.id === id);
      if (index >= 0) captures.splice(index, 1);
      res.end(JSON.stringify({ ok: true }));
      return;
    }
    const id = parsed.searchParams.get('id');
    if (id) {
      res.end(JSON.stringify(captures.find((capture) => capture.id === id) || captures[0]));
      return;
    }
    res.end(JSON.stringify(captures));
    return;
  }
  if (req.url?.startsWith('/api/replay')) {
    const body = await readJSON(req);
    const replayRecords = [{
      ...c2sRecord,
      session: 'replay-session',
      seq: 2,
      index: 1,
      grpc_data: JSON.stringify(body.body_json || { message: 'ping-replay' }),
    }, {
      ...s2cRecord,
      session: 'replay-session',
      seq: 2,
      index: 2,
    }];
    framesBySession.set('replay-session', replayRecords);
    res.end(JSON.stringify({
      call_id: 'replay-session',
      status: 200,
      status_text: '200 OK',
      headers: { 'Content-Type': ['application/connect+proto'] },
      records: replayRecords,
    }));
    return;
  }
  res.statusCode = 404;
  res.end(JSON.stringify({ error: 'not found' }));
});

await listen(api, apiPort);

const next = spawn('npm', ['run', 'dev', '--', '--port', String(webPort), '--hostname', '127.0.0.1'], {
  stdio: ['ignore', 'pipe', 'pipe'],
  env: {
    ...process.env,
    NEXT_PUBLIC_API_URL: apiBase,
    NEXT_PUBLIC_WS_URL: `ws://127.0.0.1:${apiPort}/ws/records`,
  },
});

let browser;
let context;
try {
  await waitForHTTP(`http://127.0.0.1:${webPort}`, 30000);
  browser = await chromium.launch();
  context = await browser.newContext({ acceptDownloads: true, viewport: { width: 1440, height: 920 } });
  const page = await context.newPage();
  await page.goto(`http://127.0.0.1:${webPort}`, { waitUntil: 'networkidle' });
  await page.getByRole('button', { name: 'Proxy', exact: true }).waitFor();
  await page.getByRole('button', { name: /All Calls/ }).waitFor();
  await page.getByLabel('Toggle bulk.v1.ToolService').click();
  await page.getByRole('button', { name: 'Tool9 1', exact: true }).waitFor();
  await page.locator('.ct-call-mini-timeline').first().waitFor();
  await page.locator('.ct-stream-scrubber').waitFor();
  await page.locator('[data-scroll-region="calls"]').waitFor();
  await page.locator('[data-scroll-region="frames"]').waitFor();
  await page.locator('.ct-bottom-dock').getByText('Status').waitFor();
  await assertWorkbenchViewport(page, 1440, 920, 'desktop');
  await assertWorkbenchViewport(page, 1120, 820, 'wails-min');
  await assertWorkbenchViewport(page, 960, 760, 'compact');
  await page.setViewportSize({ width: 1440, height: 920 });
  await assertIndependentScroll(page, 'calls');
  await assertIndependentScroll(page, 'frames');
  const toolServiceRow = page.locator('.ct-service-row').filter({ hasText: 'v1.ToolService' }).first();
  await toolServiceRow.locator('button').nth(1).click();
  await page.locator('.ct-call-row').filter({ hasText: 'Tool9' }).first().waitFor();
  const serviceFilteredEchoRows = await page.locator('.ct-call-row').filter({ hasText: 'Echo' }).count();
  if (serviceFilteredEchoRows !== 0) {
    throw new Error(`Service filter leaked non-selected calls: ${serviceFilteredEchoRows} Echo rows`);
  }
  const lastServiceRequest = callRequests.at(-1);
  if (lastServiceRequest?.searchParams.get('service') !== 'bulk.v1.ToolService') {
    throw new Error(`Service filter was not sent to API: ${lastServiceRequest?.search || '<none>'}`);
  }
  await page.getByRole('button', { name: /All Calls/ }).click();
  await page.locator('.ct-call-row').filter({ hasText: 'Echo' }).first().waitFor();
  await page.getByLabel('Call start from').fill(toDateTimeLocal(baseTime + 59_000));
  await page.getByLabel('Call start to').fill(toDateTimeLocal(baseTime + 62_000));
  await page.locator('.ct-call-row').filter({ hasText: 'Tick' }).first().waitFor();
  const timeFilteredEchoRows = await page.locator('.ct-call-row').filter({ hasText: 'Echo' }).count();
  if (timeFilteredEchoRows !== 0) {
    throw new Error(`Time filter leaked non-window calls: ${timeFilteredEchoRows} Echo rows`);
  }
  const lastTimeRequest = callRequests.at(-1);
  if (!lastTimeRequest?.searchParams.get('started_after') || !lastTimeRequest.searchParams.get('started_before')) {
    throw new Error(`Time filter was not sent to API: ${lastTimeRequest?.search || '<none>'}`);
  }
  await page.getByRole('button', { name: /All Calls/ }).click();
  await page.locator('.ct-call-row').filter({ hasText: 'Echo' }).first().waitFor();
  const resizerBox = await page.locator('.ct-panel-resizer').first().boundingBox();
  if (!resizerBox) throw new Error('Missing resizable pane handle');
  await page.mouse.move(resizerBox.x + 2, resizerBox.y + resizerBox.height / 2);
  await page.mouse.down();
  await page.mouse.move(resizerBox.x + 46, resizerBox.y + resizerBox.height / 2);
  await page.mouse.up();
  await assertWorkbenchViewport(page, 1440, 920, 'after-resize');
  await page.locator('.ct-call-row').filter({ hasText: 'Echo' }).first().click();
  await page.getByLabel('Scrub frames').evaluate((element) => {
    element.value = '1';
    element.dispatchEvent(new Event('input', { bubbles: true }));
    element.dispatchEvent(new Event('change', { bubbles: true }));
  });
  await page.getByLabel('Play frame playback').click();
  await page.getByLabel('Pause frame playback').waitFor();
  await page.getByLabel('Pause frame playback').click();
  await page.getByLabel('Search frames').fill('pong');
  await page.getByText('S2C e2e.v1.EchoService/Echo').first().waitFor();
  await page.getByLabel('Search frames').fill('');
  await page.locator('[data-scroll-region="calls"]').focus();
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('Enter');
  await page.locator('[data-scroll-region="frames"]').getByText('C2S lane.v1.Service0/Ping').first().waitFor();
  await page.locator('.ct-call-row').filter({ hasText: 'Echo' }).first().click();
  await page.locator('[data-scroll-region="frames"]').getByText('C2S e2e.v1.EchoService/Echo').first().waitFor();
  await page.locator('.ct-call-row').filter({ hasText: 'Tool9' }).first().click();
  await page.locator('[data-scroll-region="frames"]').getByText('C2S bulk.v1.ToolService/Tool9').first().waitFor();
  const staleEchoFrameCount = await page.locator('[data-scroll-region="frames"]').getByText('e2e.v1.EchoService/Echo').count();
  if (staleEchoFrameCount !== 0) {
    throw new Error(`Expected stream pane to only show selected call frames, found ${staleEchoFrameCount} stale Echo frames`);
  }
  await page.locator('.ct-call-row').filter({ hasText: 'Echo' }).first().click();
  const searchRequestStart = callRequests.length;
  await page.getByLabel('Search calls').fill('T');
  await page.getByLabel('Search calls').fill('To');
  await page.getByLabel('Search calls').fill('Tool9');
  await page.locator('[data-scroll-region="frames"]').getByText('C2S bulk.v1.ToolService/Tool9').first().waitFor();
  const searchRequestCount = callRequests.length - searchRequestStart;
  if (searchRequestCount > 3) {
    throw new Error(`Search debounce made too many /api/calls requests: ${searchRequestCount}`);
  }
  await page.getByLabel('Search calls').fill('');
  await page.locator('.ct-call-row').filter({ hasText: 'Echo' }).first().click();
  await page.keyboard.press(process.platform === 'darwin' ? 'Meta+K' : 'Control+K');
  await page.getByLabel('Command palette search').fill('Echo');
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('Enter');
  await page.locator('.shiki').first().waitFor();
  await page.getByRole('tab', { name: 'Headers' }).click();
  await page.getByText('application/connect+proto').waitFor();
  await page.getByText('Bearer e2e-token').waitFor();
  await page.getByRole('tab', { name: 'Sequence' }).click();
  await page.locator('svg').first().waitFor();
  await page.getByRole('tab', { name: 'Protocol' }).click();
  await page.getByText('EchoRequest').first().waitFor();
  const [detailDownload] = await Promise.all([
    page.waitForEvent('download'),
    page.getByRole('button', { name: 'JSONL' }).click(),
  ]);
  await assertJSONLDownload(detailDownload, 'e2e-session');
  await page.getByRole('button', { name: 'Pause' }).click();
  await page.getByRole('button', { name: 'Resume' }).click();
  await page.locator('.ct-call-row').filter({ hasText: 'Echo' }).first().waitFor();
  await page.getByLabel('Start recording capture').click();
  await assertRecordingAnimation(page);
  await page.getByLabel('Stop and save capture').waitFor();
  await page.getByLabel('Stop and save capture').click();
  await page.getByRole('button', { name: 'Records' }).click();
  await page.getByRole('button', { name: 'Save Current Capture' }).click();
  await page.getByText('captur').first().waitFor();
  const [captureDownload] = await Promise.all([
    page.waitForEvent('download'),
    page.getByRole('dialog', { name: 'Saved records' }).getByRole('button', { name: 'JSONL' }).first().click(),
  ]);
  await assertJSONLDownload(captureDownload, 'e2e-session');
  await page.keyboard.press('Escape');
  await page.getByRole('button', { name: 'Replay' }).click();
  await page.getByRole('button', { name: 'Send' }).click();
  await page.getByText('saved as replay-session').waitFor();
  await page.keyboard.press('Escape');
  await page.getByRole('button', { name: 'Clear' }).click();
  await page.getByText('Select a call to inspect stream').waitFor();
  const remainingCalls = await page.locator('.ct-call-row').count();
  if (remainingCalls !== 0) {
    throw new Error(`Clear left ${remainingCalls} calls visible`);
  }
  await browser.close();
} finally {
  if (context) {
    await context.close().catch(() => {});
  }
  if (browser) {
    await browser.close().catch(() => {});
  }
  next.kill('SIGTERM');
  api.close();
}

function listen(server, port) {
  return new Promise((resolve) => server.listen(port, '127.0.0.1', resolve));
}

function freePort() {
  return new Promise((resolve, reject) => {
    const server = http.createServer();
    server.listen(0, '127.0.0.1', () => {
      const address = server.address();
      server.close(() => resolve(address.port));
    });
    server.on('error', reject);
  });
}

function readJSON(req) {
  return new Promise((resolve, reject) => {
    let body = '';
    req.on('data', (chunk) => {
      body += chunk;
    });
    req.on('end', () => {
      try {
        resolve(body ? JSON.parse(body) : {});
      } catch (error) {
        reject(error);
      }
    });
  });
}

function makeCallFrames(call, seed) {
  const request = {
    ...requestRecord,
    ts: call.started_at,
    session: call.id,
    seq: call.seq,
    index: 1,
    url: call.url,
    headers: {
      'Content-Type': ['application/connect+proto'],
      'X-E2E-Session': [call.id],
    },
  };
  const grpc = Array.from({ length: Math.max(4, call.frame_count + 8) }, (_, index) => ({
    ...c2sRecord,
    ts: at(180 + seed * 20 + index * 4),
    session: call.id,
    seq: call.seq,
    index: index + 2,
    direction: index % 2 === 0 ? 'C2S' : 'S2C',
    url: call.url,
    grpc_service: call.service,
    grpc_method: call.method,
    grpc_data: JSON.stringify({ call: call.id, frame: index }),
    grpc_streaming: call.streaming,
    grpc_frame_index: index,
    size: 10 + index,
  }));
  return [request, ...grpc];
}

function toDateTimeLocal(value) {
  const date = new Date(value);
  const pad = (part) => String(part).padStart(2, '0');
  return [
    date.getFullYear(),
    '-',
    pad(date.getMonth() + 1),
    '-',
    pad(date.getDate()),
    'T',
    pad(date.getHours()),
    ':',
    pad(date.getMinutes()),
    ':',
    pad(date.getSeconds()),
  ].join('');
}

async function assertJSONLDownload(download, expectedSession) {
  const path = await download.path();
  if (!path) {
    throw new Error('JSONL download did not produce a file path');
  }
  const text = readFileSync(path, 'utf8').trim();
  if (!text) {
    throw new Error('JSONL download was empty');
  }
  const rows = text.split('\n').map((line) => JSON.parse(line));
  if (!rows.some((row) => row.session === expectedSession)) {
    throw new Error(`JSONL download did not include session ${expectedSession}: ${text.slice(0, 200)}`);
  }
}

async function waitForHTTP(target, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(target);
      if (res.ok) return;
    } catch {
      // keep waiting
    }
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  throw new Error(`Timed out waiting for ${target}`);
}

async function assertIndependentScroll(page, region) {
  const locator = page.locator(`[data-scroll-region="${region}"]`);
  await locator.evaluate((element) => {
    element.scrollTop = 0;
    document.documentElement.scrollTop = 0;
    document.body.scrollTop = 0;
  });
  const before = await locator.evaluate((element) => ({
    regionTop: element.scrollTop,
    scrollHeight: element.scrollHeight,
    clientHeight: element.clientHeight,
    docTop: document.documentElement.scrollTop,
    bodyTop: document.body.scrollTop,
  }));
  if (before.scrollHeight <= before.clientHeight + 1) {
    throw new Error(`${region} region is not independently scrollable: ${JSON.stringify(before)}`);
  }
  const box = await locator.boundingBox();
  if (!box) {
    throw new Error(`Missing ${region} scroll region bounds`);
  }
  await page.mouse.move(box.x + box.width / 2, box.y + Math.min(box.height - 8, box.height / 2));
  await page.mouse.wheel(0, 520);
  await page.waitForTimeout(160);
  const after = await locator.evaluate((element) => ({
    regionTop: element.scrollTop,
    docTop: document.documentElement.scrollTop,
    bodyTop: document.body.scrollTop,
  }));
  if (after.regionTop <= before.regionTop || after.docTop !== 0 || after.bodyTop !== 0) {
    throw new Error(`${region} independent scroll failed: ${JSON.stringify({ before, after })}`);
  }
}

async function assertWorkbenchViewport(page, width, height, label) {
  await page.setViewportSize({ width, height });
  await page.waitForTimeout(250);
  const result = await page.evaluate(() => {
    const visible = (element) => {
      const style = window.getComputedStyle(element);
      const rect = element.getBoundingClientRect();
      return style.display !== 'none'
        && style.visibility !== 'hidden'
        && Number.parseFloat(style.opacity || '1') > 0
        && rect.width > 0
        && rect.height > 0;
    };
    const viewportWidth = window.innerWidth;
    const scrollWidth = Math.max(document.documentElement.scrollWidth, document.body.scrollWidth);
    const selectors = [
      'main.cursor-tap-workbench',
      'header',
      '.ct-bottom-dock',
      '.ct-dock-stat',
      '.ct-panel-resizer',
      'aside',
      'section',
      'button',
      'input',
      '[role="tab"]',
    ];
    const overflowing = Array.from(document.querySelectorAll(selectors.join(',')))
      .filter(visible)
      .map((element) => {
        const rect = element.getBoundingClientRect();
        return {
          tag: element.tagName,
          text: (element.textContent || element.getAttribute('aria-label') || element.getAttribute('title') || '').trim().slice(0, 80),
          left: Math.round(rect.left),
          right: Math.round(rect.right),
          width: Math.round(rect.width),
        };
      })
      .filter((entry) => entry.left < -1 || entry.right > viewportWidth + 1);
    const icons = Array.from(document.querySelectorAll('header svg'))
      .filter(visible)
      .map((element) => {
        const rect = element.getBoundingClientRect();
        const owner = element.closest('button') || element.closest('[aria-label]') || element.parentElement;
        const ownerRect = owner?.getBoundingClientRect();
        return {
          label: owner?.getAttribute('title') || owner?.getAttribute('aria-label') || owner?.textContent?.trim() || element.tagName,
          width: rect.width,
          height: rect.height,
          clipped: !!ownerRect && (rect.left < ownerRect.left - 1 || rect.right > ownerRect.right + 1 || rect.top < ownerRect.top - 1 || rect.bottom > ownerRect.bottom + 1),
        };
      });
    const badIcons = icons.filter((icon) => icon.width < 8 || icon.height < 8 || icon.clipped);
    return { viewportWidth, scrollWidth, overflowing, badIcons };
  });
  if (result.scrollWidth > result.viewportWidth + 1 || result.overflowing.length || result.badIcons.length) {
    throw new Error(`${label} layout check failed: ${JSON.stringify(result, null, 2)}`);
  }
}

async function assertRecordingAnimation(page) {
  const animationName = await page.locator('.ct-record-button--active > span').evaluate((element) => {
    return window.getComputedStyle(element).animationName;
  });
  if (!animationName.includes('ct-rec-pulse')) {
    throw new Error(`Record pulse animation missing: ${animationName}`);
  }
}
