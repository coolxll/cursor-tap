package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const cursorTapNodeBootstrapName = "cursor-tap-node-proxy-bootstrap.cjs"
const cursorTapInjectorExtensionDirName = "cursor-tap-proxy-injector"
const cursorTapInjectedCursorDirName = "injected-cursor"
const cursorTapBootstrapForkMarker = "cursor-tap-bootstrap-fork-injection"

func cursorLaunchEnv(base []string, dataDir string, httpPort int, socksPort int) ([]string, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}
	if err := ensureCursorProxyInjectionFiles(dataDir); err != nil {
		return nil, err
	}

	bootstrapPath := cursorProxyBootstrapPath(dataDir)
	httpProxy := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	socksProxy := fmt.Sprintf("socks5://127.0.0.1:%d", socksPort)
	env := envMap(base)
	env["HTTP_PROXY"] = httpProxy
	env["http_proxy"] = httpProxy
	env["HTTPS_PROXY"] = httpProxy
	env["https_proxy"] = httpProxy
	env["ALL_PROXY"] = socksProxy
	env["all_proxy"] = socksProxy
	env["NO_PROXY"] = "127.0.0.1,localhost,::1"
	env["no_proxy"] = env["NO_PROXY"]
	env["NODE_TLS_REJECT_UNAUTHORIZED"] = "0"
	env["NODE_OPTIONS"] = mergeNodeOptions(env["NODE_OPTIONS"], bootstrapPath)
	env["CURSOR_TAP_HTTP_PROXY"] = httpProxy
	env["CURSOR_TAP_SOCKS_PROXY"] = socksProxy
	env["CURSOR_TAP_NODE_BOOTSTRAP"] = bootstrapPath
	env["CURSOR_TAP_INJECT_LOG"] = filepath.Join(dataDir, "cursor-tap-inject.log")
	env["CURSOR_TAP_INJECTED"] = "1"
	return flattenEnv(env), nil
}

func mergeNodeOptions(current string, bootstrapPath string) string {
	requireOpt := "--require=" + bootstrapPath
	if strings.Contains(current, requireOpt) {
		return current
	}
	if strings.TrimSpace(current) == "" {
		return requireOpt
	}
	return requireOpt + " " + current
}

func cursorProxyBootstrapPath(dataDir string) string {
	return filepath.Join(dataDir, cursorTapNodeBootstrapName)
}

func cursorLaunchArgs(dataDir string, httpPort int) []string {
	return []string{
		fmt.Sprintf("--proxy-server=http://127.0.0.1:%d", httpPort),
		"--ignore-certificate-errors",
		"--extensionDevelopmentPath=" + cursorInjectorExtensionPath(dataDir),
	}
}

func cursorInjectorExtensionPath(dataDir string) string {
	return filepath.Join(dataDir, cursorTapInjectorExtensionDirName)
}

func prepareCursorAppForLaunch(cursorPath string, dataDir string) (string, error) {
	info, err := os.Stat(cursorPath)
	if err != nil || !info.IsDir() || filepath.Ext(cursorPath) != ".app" {
		return cursorPath, nil
	}
	sourceBootstrap := cursorBootstrapForkPath(cursorPath)
	sourceHash, err := sha256File(sourceBootstrap)
	if err != nil {
		return cursorPath, nil
	}

	targetRoot := filepath.Join(dataDir, cursorTapInjectedCursorDirName)
	targetApp := filepath.Join(targetRoot, fmt.Sprintf("Cursor-%s.app", sourceHash[:12]))
	if _, err := os.Stat(cursorExecutable(targetApp)); err != nil {
		if _, statErr := os.Stat(targetApp); statErr == nil {
			targetApp = filepath.Join(targetRoot, fmt.Sprintf("Cursor-%s-%d.app", sourceHash[:12], time.Now().Unix()))
		}
		if err := copyCursorApp(cursorPath, targetApp); err != nil {
			return "", err
		}
	}
	if err := patchCursorBootstrapFork(targetApp); err != nil {
		return "", err
	}
	if err := ensureAdHocSignedCursorApp(targetApp); err != nil {
		return "", err
	}
	return targetApp, nil
}

func cursorBootstrapForkPath(cursorApp string) string {
	return filepath.Join(cursorApp, "Contents", "Resources", "app", "out", "bootstrap-fork.js")
}

func sha256File(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func copyCursorApp(sourceApp string, targetApp string) error {
	if err := os.MkdirAll(filepath.Dir(targetApp), 0755); err != nil {
		return err
	}
	out, err := exec.Command("/usr/bin/ditto", sourceApp, targetApp).CombinedOutput()
	if err != nil {
		return fmt.Errorf("copy injected Cursor app: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func patchCursorBootstrapFork(cursorApp string) error {
	path := cursorBootstrapForkPath(cursorApp)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if strings.Contains(string(data), cursorTapBootstrapForkMarker) {
		return nil
	}
	backupPath := path + ".cursor-tap-original"
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		if err := os.WriteFile(backupPath, data, 0644); err != nil {
			return err
		}
	}
	injection := `import { createRequire as __cursorTapCreateRequire } from "node:module";
/* ` + cursorTapBootstrapForkMarker + `:start */
try {
  if (process?.env?.CURSOR_TAP_NODE_BOOTSTRAP && !globalThis.__cursorTapBootstrapForkInjected) {
    globalThis.__cursorTapBootstrapForkInjected = true;
    __cursorTapCreateRequire(import.meta.url)(process.env.CURSOR_TAP_NODE_BOOTSTRAP);
  }
} catch (error) {
  console.error("[cursor-tap] early bootstrap injection failed", error);
}
/* ` + cursorTapBootstrapForkMarker + `:end */
`
	return os.WriteFile(path, append([]byte(injection), data...), 0644)
}

func ensureAdHocSignedCursorApp(cursorApp string) error {
	if exec.Command("codesign", "--verify", "--deep", "--strict", cursorApp).Run() == nil {
		return nil
	}
	out, err := exec.Command("codesign", "--force", "--deep", "--sign", "-", cursorApp).CombinedOutput()
	if err != nil {
		return fmt.Errorf("codesign injected Cursor app: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func ensureCursorProxyInjectorExtension(dataDir string) error {
	extensionDir := cursorInjectorExtensionPath(dataDir)
	if err := os.MkdirAll(extensionDir, 0755); err != nil {
		return err
	}
	if err := writeFileIfChanged(filepath.Join(extensionDir, "package.json"), []byte(cursorTapInjectorPackageJSON)); err != nil {
		return err
	}
	return writeFileIfChanged(filepath.Join(extensionDir, "extension.js"), []byte(cursorTapInjectorExtensionJS))
}

func ensureCursorProxyInjectionFiles(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return err
	}
	if err := writeFileIfChanged(cursorProxyBootstrapPath(dataDir), []byte(cursorTapNodeBootstrap)); err != nil {
		return err
	}
	return ensureCursorProxyInjectorExtension(dataDir)
}

func writeFileIfChanged(path string, next []byte) error {
	current, err := os.ReadFile(path)
	if err == nil && string(current) == string(next) {
		return nil
	}
	return os.WriteFile(path, next, 0644)
}

func envMap(values []string) map[string]string {
	env := make(map[string]string, len(values))
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		if ok {
			env[key] = val
		}
	}
	return env
}

func flattenEnv(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, key+"="+env[key])
	}
	return values
}

const cursorTapNodeBootstrap = `'use strict';

(() => {
  if (globalThis.__cursorTapProxyBootstrapInstalled) {
    return;
  }
  globalThis.__cursorTapProxyBootstrapInstalled = true;

  const fs = require('node:fs');
  const net = require('node:net');
  const { Duplex } = require('node:stream');

  const proxySpec =
    process.env.CURSOR_TAP_HTTP_PROXY ||
    process.env.HTTPS_PROXY ||
    process.env.HTTP_PROXY ||
    process.env.https_proxy ||
    process.env.http_proxy;

  if (!proxySpec) {
    return;
  }

  let proxyURL;
  try {
    proxyURL = new URL(proxySpec);
  } catch (error) {
    debug('invalid proxy url: ' + error.message);
    return;
  }

  if (proxyURL.protocol !== 'http:') {
    debug('only http proxy bootstrap is supported for CONNECT tunneling');
    return;
  }

  process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0';

  const proxyHost = proxyURL.hostname || '127.0.0.1';
  const proxyPort = Number(proxyURL.port || 80);
  const originalConnect = net.connect.bind(net);
  const originalSocketConnect = net.Socket.prototype.connect;
  const localHosts = new Set(['localhost', '127.0.0.1', '::1', '0.0.0.0']);

  function debug(message) {
    if (process.env.CURSOR_TAP_INJECT_LOG) {
      try {
        fs.appendFileSync(
          process.env.CURSOR_TAP_INJECT_LOG,
          new Date().toISOString() + ' pid=' + process.pid + ' ' + message + '\n',
        );
      } catch (_) {}
    }
    if (process.env.CURSOR_TAP_INJECT_DEBUG === '1') {
      process.stderr.write('[cursor-tap inject pid=' + process.pid + '] ' + message + '\n');
    }
  }

  function parseArgs(args) {
    const list = Array.from(args);
    const callback = list.find((item) => typeof item === 'function');
    let options = {};

    if (typeof list[0] === 'object' && list[0] !== null) {
      options = { ...list[0] };
    } else if (typeof list[0] === 'number') {
      options.port = list[0];
      if (typeof list[1] === 'string') {
        options.host = list[1];
      }
      const optionArg = list.find((item, index) => index > 0 && typeof item === 'object' && item !== null);
      if (optionArg) {
        options = { ...optionArg, ...options };
      }
    } else if (typeof list[0] === 'string') {
      options.path = list[0];
    }

    options.host = options.host || options.hostname;
    if (!options.host && options.port) {
      options.host = 'localhost';
    }
    if (options.port !== undefined) {
      options.port = Number(options.port);
    }

    return { callback, options };
  }

  function shouldTunnel(options) {
    if (!options || options.path || options.socket) {
      return false;
    }
    const host = String(options.host || '').replace(/^\[|\]$/g, '');
    const port = Number(options.port || 0);
    if (!host || !port) {
      return false;
    }
    if (host === proxyHost && port === proxyPort) {
      return false;
    }
    if (localHosts.has(host)) {
      return false;
    }
    if (process.env.CURSOR_TAP_TUNNEL_ALL_TCP === '1') {
      return true;
    }
    return port === 443 || port === 8443;
  }

  function connectRequest(host, port) {
    const target = host.includes(':') && !host.startsWith('[') ? '[' + host + ']:' + port : host + ':' + port;
    return [
      'CONNECT ' + target + ' HTTP/1.1',
      'Host: ' + target,
      'Proxy-Connection: keep-alive',
      'Connection: keep-alive',
      '',
      '',
    ].join('\r\n');
  }

  function patchProxySocket(socket, targetHost, targetPort) {
    const originalEmit = socket.emit;
    const originalWrite = socket.write;
    let tunnelReady = false;
    let handshaking = false;
    let buffer = Buffer.alloc(0);
    let timeout = null;
    const pendingWrites = [];
    function consumeHandshakeData() {}
    socket.on('data', consumeHandshakeData);

    function cleanup() {
      if (timeout) {
        clearTimeout(timeout);
        timeout = null;
      }
      socket.removeListener('data', consumeHandshakeData);
    }

    function flushPendingWrites() {
      while (pendingWrites.length) {
        const args = pendingWrites.shift();
        originalWrite.apply(socket, args);
      }
    }

    function fail(error) {
      cleanup();
      debug('CONNECT ' + targetHost + ':' + targetPort + ' failed: ' + error.message);
      socket.destroy(error);
    }

    function finish(rest) {
      cleanup();
      tunnelReady = true;
      debug('CONNECT ' + targetHost + ':' + targetPort + ' via ' + proxyHost + ':' + proxyPort);
      flushPendingWrites();
      originalEmit.call(socket, 'connect');
      if (rest && rest.length > 0) {
        originalEmit.call(socket, 'data', rest);
      }
    }

    function handleHandshakeData(chunk) {
      buffer = Buffer.concat([buffer, chunk]);
      const end = buffer.indexOf('\r\n\r\n');
      if (end === -1) {
        return;
      }
      const header = buffer.slice(0, end).toString('latin1');
      const firstLine = header.split('\r\n')[0] || '';
      if (!/^HTTP\/1\.[01] 2\d\d\b/.test(firstLine)) {
        fail(new Error('proxy CONNECT failed: ' + firstLine));
        return;
      }
      finish(buffer.slice(end + 4));
    }

    socket.write = function patchedWrite(...writeArgs) {
      if (!tunnelReady) {
        pendingWrites.push(writeArgs);
        return true;
      }
      return originalWrite.apply(socket, writeArgs);
    };

    socket.emit = function patchedEmit(eventName, ...eventArgs) {
      if (eventName === 'data' && !tunnelReady) {
        handleHandshakeData(eventArgs[0]);
        return true;
      }
      if (eventName === 'connect' && !tunnelReady) {
        if (!handshaking) {
          handshaking = true;
          timeout = setTimeout(() => fail(new Error('proxy CONNECT timeout')), 15000);
          originalWrite.call(socket, connectRequest(targetHost, targetPort));
          // Some callers hand the socket to TLS/fetch while it is still paused.
          // Force the proxy response into a data event so the CONNECT handshake
          // cannot sit unread until our timeout fires.
          if (typeof socket.resume === 'function') {
            socket.resume();
          }
        }
        return true;
      }
      return originalEmit.call(socket, eventName, ...eventArgs);
    };
  }

  function createTunnelSocket(targetHost, targetPort) {
    const socket = new net.Socket();
    patchProxySocket(socket, targetHost, targetPort);
    originalSocketConnect.call(socket, { host: proxyHost, port: proxyPort });
    return socket;
  }

  function createTunnelTransport(targetHost, targetPort) {
    const raw = originalConnect({ host: proxyHost, port: proxyPort });
    const pendingWrites = [];
    let ready = false;
    let ended = false;
    let handshakeBuffer = Buffer.alloc(0);
    let timeout = null;

    const transport = new Duplex({
      read() {
        if (ready && typeof raw.resume === 'function') {
          raw.resume();
        }
      },
      write(chunk, encoding, callback) {
        if (!ready) {
          pendingWrites.push([Buffer.from(chunk), callback]);
          return;
        }
        raw.write(chunk, encoding, callback);
      },
      final(callback) {
        ended = true;
        if (ready) {
          raw.end();
        }
        callback();
      },
      destroy(error, callback) {
        cleanup();
        raw.destroy(error || undefined);
        callback(error);
      },
    });

    transport.connecting = true;
    transport.setNoDelay = function setNoDelay(...args) {
      if (typeof raw.setNoDelay === 'function') {
        raw.setNoDelay(...args);
      }
      return transport;
    };
    transport.setKeepAlive = function setKeepAlive(...args) {
      if (typeof raw.setKeepAlive === 'function') {
        raw.setKeepAlive(...args);
      }
      return transport;
    };
    transport.setTimeout = function setTimeoutOnRaw(...args) {
      if (typeof raw.setTimeout === 'function') {
        raw.setTimeout(...args);
      }
      return transport;
    };
    transport.address = function address() {
      return typeof raw.address === 'function' ? raw.address() : {};
    };
    transport.destroySoon = function destroySoon() {
      transport.end();
      return transport;
    };
    transport.ref = function ref() {
      if (typeof raw.ref === 'function') {
        raw.ref();
      }
      return transport;
    };
    transport.unref = function unref() {
      if (typeof raw.unref === 'function') {
        raw.unref();
      }
      return transport;
    };
    Object.defineProperty(transport, 'remoteAddress', { get: () => targetHost });
    Object.defineProperty(transport, 'remotePort', { get: () => targetPort });
    Object.defineProperty(transport, 'localAddress', { get: () => raw.localAddress });
    Object.defineProperty(transport, 'localPort', { get: () => raw.localPort });

    function cleanup() {
      if (timeout) {
        clearTimeout(timeout);
        timeout = null;
      }
    }

    function fail(error) {
      cleanup();
      debug('CONNECT ' + targetHost + ':' + targetPort + ' failed: ' + error.message);
      transport.destroy(error);
    }

    function flushPendingWrites() {
      while (pendingWrites.length) {
        const [chunk, callback] = pendingWrites.shift();
        raw.write(chunk, callback);
      }
      if (ended) {
        raw.end();
      }
    }

    function finish(rest) {
      cleanup();
      ready = true;
      transport.connecting = false;
      debug('CONNECT ' + targetHost + ':' + targetPort + ' via ' + proxyHost + ':' + proxyPort);
      flushPendingWrites();
      transport.emit('connect');
      if (rest && rest.length > 0 && !transport.push(rest)) {
        raw.pause();
      }
    }

    function handleHandshakeData(chunk) {
      handshakeBuffer = Buffer.concat([handshakeBuffer, chunk]);
      const end = handshakeBuffer.indexOf('\r\n\r\n');
      if (end === -1) {
        return;
      }
      const header = handshakeBuffer.slice(0, end).toString('latin1');
      const firstLine = header.split('\r\n')[0] || '';
      if (!/^HTTP\/1\.[01] 2\d\d\b/.test(firstLine)) {
        fail(new Error('proxy CONNECT failed: ' + firstLine));
        return;
      }
      finish(handshakeBuffer.slice(end + 4));
    }

    raw.on('connect', () => {
      timeout = setTimeout(() => fail(new Error('proxy CONNECT timeout')), 15000);
      raw.write(connectRequest(targetHost, targetPort));
      raw.resume();
    });
    raw.on('data', (chunk) => {
      if (!ready) {
        handleHandshakeData(chunk);
        return;
      }
      if (!transport.push(chunk)) {
        raw.pause();
      }
    });
    raw.on('end', () => transport.push(null));
    raw.on('error', (error) => transport.destroy(error));
    raw.on('close', () => transport.emit('close'));

    return transport;
  }

	  function connectExistingSocketViaProxy(socket, targetHost, targetPort, callback) {
	    if (callback) {
	      socket.once('connect', callback);
	    }
	    const tunnel = createTunnelTransport(targetHost, targetPort);
	    socket.__cursorTapTunnel = tunnel;
	    socket.__cursorTapTargetHost = targetHost;
	    socket.__cursorTapTargetPort = targetPort;
	    socket.connecting = true;
	    let outerClosed = false;

    tunnel.on('connect', () => {
      socket.connecting = false;
      socket.emit('connect');
    });
    tunnel.on('data', (chunk) => socket.emit('data', chunk));
    tunnel.on('end', () => socket.emit('end'));
    tunnel.on('close', (hadError) => {
      if (outerClosed) {
        return;
      }
      outerClosed = true;
      socket.emit('close', hadError);
    });
    tunnel.on('error', (error) => socket.emit('error', error));
    tunnel.on('timeout', () => socket.emit('timeout'));
    tunnel.on('drain', () => socket.emit('drain'));

    socket.write = (...args) => tunnel.write(...args);
    socket.end = (...args) => {
      tunnel.end(...args);
      return socket;
    };
    socket.destroy = (error) => {
      tunnel.destroy(error);
      return socket;
    };
    socket.setTimeout = (...args) => {
      tunnel.setTimeout(...args);
      return socket;
    };
    socket.setNoDelay = (...args) => {
      tunnel.setNoDelay(...args);
      return socket;
    };
    socket.setKeepAlive = (...args) => {
      tunnel.setKeepAlive(...args);
      return socket;
    };
    socket.ref = () => {
      tunnel.ref();
      return socket;
    };
    socket.unref = () => {
      tunnel.unref();
      return socket;
    };
    socket.address = () => tunnel.address();
    try {
      Object.defineProperty(socket, 'remoteAddress', { configurable: true, get: () => targetHost });
      Object.defineProperty(socket, 'remotePort', { configurable: true, get: () => targetPort });
      Object.defineProperty(socket, 'localAddress', { configurable: true, get: () => tunnel.localAddress });
      Object.defineProperty(socket, 'localPort', { configurable: true, get: () => tunnel.localPort });
    } catch (_) {}
    return socket;
  }

  net.Socket.prototype.connect = function patchedSocketPrototypeConnect() {
    const { callback, options } = parseArgs(arguments);
    if (!shouldTunnel(options)) {
      return originalSocketConnect.apply(this, arguments);
    }
    const targetHost = String(options.host || options.hostname);
    const targetPort = Number(options.port);
    return connectExistingSocketViaProxy(this, targetHost, targetPort, callback);
  };

  function proxiedConnect() {
    const { callback, options } = parseArgs(arguments);
    if (!shouldTunnel(options)) {
      return originalConnect.apply(net, arguments);
    }

    const targetHost = String(options.host || options.hostname);
    const targetPort = Number(options.port);
    const socket = createTunnelTransport(targetHost, targetPort);

    if (callback) {
      socket.once('connect', callback);
    }
    return socket;
  }

  net.connect = proxiedConnect;
  net.createConnection = proxiedConnect;

  try {
    const tls = require('node:tls');
    const originalTLSConnect = tls.connect.bind(tls);
    tls.DEFAULT_REJECT_UNAUTHORIZED = false;

    tls.connect = function proxiedTLSConnect() {
      const { callback, options } = parseArgs(arguments);
      if (options.socket && options.socket.__cursorTapTunnel) {
        const targetHost = String(
          options.servername ||
            options.socket.__cursorTapTargetHost ||
            options.host ||
            options.hostname ||
            'localhost',
        );
        const tunnel = options.socket.__cursorTapTunnel;
        const nextOptions = {
          ...options,
          socket: tunnel,
          servername: options.servername || targetHost,
          rejectUnauthorized: false,
        };
        delete nextOptions.host;
        delete nextOptions.hostname;
        delete nextOptions.port;

        const tlsSocket = originalTLSConnect(nextOptions);
        if (callback) {
          tlsSocket.once('secureConnect', callback);
        }
        debug('tls existing CONNECT ' + targetHost + ':' + (options.socket.__cursorTapTargetPort || 443) + ' via ' + proxyHost + ':' + proxyPort);
        return tlsSocket;
      }
      if (!shouldTunnel(options) || options.socket) {
        return originalTLSConnect.apply(tls, arguments);
      }

      const targetHost = String(options.host || options.hostname || options.servername);
      const targetPort = Number(options.port || 443);
      const tunnel = createTunnelTransport(targetHost, targetPort);
      const nextOptions = {
        ...options,
        socket: tunnel,
        servername: options.servername || targetHost,
        rejectUnauthorized: false,
      };
      delete nextOptions.host;
      delete nextOptions.hostname;
      delete nextOptions.port;

      const tlsSocket = originalTLSConnect(nextOptions);
      if (callback) {
        tlsSocket.once('secureConnect', callback);
      }
      debug('tls CONNECT ' + targetHost + ':' + targetPort + ' via ' + proxyHost + ':' + proxyPort);
      return tlsSocket;
    };

    try {
      const http2 = require('node:http2');
      const originalHTTP2Connect = http2.connect.bind(http2);
      http2.connect = function proxiedHTTP2Connect(authority, options, listener) {
        if (typeof options === 'function') {
          listener = options;
          options = {};
        }
        const nextOptions = { ...(options || {}) };
        let parsed = null;
        try {
          parsed = typeof authority === 'string' ? new URL(authority) : authority;
        } catch (_) {}

        const protocol = String(parsed?.protocol || nextOptions.protocol || 'https:');
        const targetHost = String(
          nextOptions.servername ||
            nextOptions.host ||
            nextOptions.hostname ||
            parsed?.hostname ||
            '',
        );
        const targetPort = Number(nextOptions.port || parsed?.port || (protocol === 'https:' ? 443 : 80));

        if (protocol === 'https:' && shouldTunnel({ host: targetHost, port: targetPort })) {
          nextOptions.rejectUnauthorized = false;
          nextOptions.createConnection = function cursorTapHTTP2CreateConnection(authorityOptions, connectionOptions) {
            const opts = { ...(authorityOptions || {}), ...(connectionOptions || {}) };
            const host = String(opts.servername || opts.host || opts.hostname || targetHost);
            const port = Number(opts.port || targetPort || 443);
            const tunnel = createTunnelTransport(host, port);
            const tlsSocket = originalTLSConnect({
              ...opts,
              socket: tunnel,
              servername: opts.servername || host,
              rejectUnauthorized: false,
              ALPNProtocols: opts.ALPNProtocols || ['h2', 'http/1.1'],
            });
            debug('http2 TLS CONNECT ' + host + ':' + port + ' via ' + proxyHost + ':' + proxyPort);
            return tlsSocket;
          };
          debug('http2 proxy installed for ' + targetHost + ':' + targetPort);
        }
        return originalHTTP2Connect(authority, nextOptions, listener);
      };
    } catch (_) {
      // Some Cursor worker processes do not expose node:http2.
    }
  } catch (_) {
    // Keep the bootstrap dependency-free for Cursor's extension hosts.
  }

  try {
    const undici = require('undici');
    if (undici.ProxyAgent && undici.setGlobalDispatcher) {
      undici.setGlobalDispatcher(new undici.ProxyAgent(proxyURL.toString()));
      debug('undici ProxyAgent installed');
    }
  } catch (_) {
    // Node's bundled fetch still goes through net/tls in most extension-host builds.
  }

  debug('bootstrap installed for ' + process.argv.join(' '));
})();
`

const cursorTapInjectorPackageJSON = `{
  "name": "aaa-cursor-tap-proxy-injector",
  "displayName": "Cursor Tap Proxy Injector",
  "description": "Runtime proxy bootstrap for Cursor Tap captures.",
  "version": "0.0.1",
  "publisher": "aaa-cursor-tap",
  "engines": {
    "vscode": "^1.105.0"
  },
  "categories": [
    "Other"
  ],
  "activationEvents": [
    "*"
  ],
  "main": "./extension.js"
}
`

const cursorTapInjectorExtensionJS = `'use strict';

function log(message) {
  if (process.env.CURSOR_TAP_INJECT_DEBUG === '1') {
    process.stderr.write('[cursor-tap inject extension pid=' + process.pid + '] ' + message + '\n');
  }
}

function activate() {
  const bootstrapPath = process.env.CURSOR_TAP_NODE_BOOTSTRAP;
  if (!bootstrapPath) {
    log('CURSOR_TAP_NODE_BOOTSTRAP is not set');
    return;
  }
  try {
    require(bootstrapPath);
    log('activated ' + bootstrapPath);
  } catch (error) {
    log('activation failed: ' + (error && error.stack ? error.stack : String(error)));
    throw error;
  }
}

function deactivate() {}

module.exports = { activate, deactivate };
`
