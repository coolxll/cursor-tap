package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCursorLaunchEnvWritesBootstrapAndInjectsProxy(t *testing.T) {
	dataDir := t.TempDir()
	env, err := cursorLaunchEnv([]string{"PATH=/bin", "NODE_OPTIONS=--trace-warnings"}, dataDir, 18080, 11080)
	if err != nil {
		t.Fatalf("cursorLaunchEnv: %v", err)
	}

	got := envToMap(env)
	wantProxy := "http://127.0.0.1:18080"
	if got["HTTP_PROXY"] != wantProxy || got["http_proxy"] != wantProxy {
		t.Fatalf("missing http proxy env: %#v", got)
	}
	if got["HTTPS_PROXY"] != wantProxy || got["https_proxy"] != wantProxy {
		t.Fatalf("missing https proxy env: %#v", got)
	}
	if got["NODE_TLS_REJECT_UNAUTHORIZED"] != "0" {
		t.Fatalf("expected TLS verification bypass, got %q", got["NODE_TLS_REJECT_UNAUTHORIZED"])
	}

	bootstrapPath := filepath.Join(dataDir, cursorTapNodeBootstrapName)
	if _, err := os.Stat(bootstrapPath); err != nil {
		t.Fatalf("bootstrap was not written: %v", err)
	}
	bootstrap, err := os.ReadFile(bootstrapPath)
	if err != nil {
		t.Fatalf("read bootstrap: %v", err)
	}
	if !strings.Contains(string(bootstrap), "net.Socket.prototype.connect = function patchedSocketPrototypeConnect") {
		t.Fatalf("bootstrap must patch net.Socket.prototype.connect so extension-host HTTPS cannot bypass the proxy")
	}
	if !strings.Contains(string(bootstrap), "socket.resume();") {
		t.Fatalf("bootstrap must resume the proxy socket while waiting for CONNECT response")
	}
	if !strings.Contains(string(bootstrap), "socket.on('data', consumeHandshakeData);") {
		t.Fatalf("bootstrap must attach a temporary data listener so net sockets enter flowing mode during CONNECT")
	}
	if !strings.Contains(string(bootstrap), "function createTunnelTransport") {
		t.Fatalf("bootstrap must complete proxy CONNECT before handing TLS a transport stream")
	}
	if !strings.Contains(string(bootstrap), "const tunnel = createTunnelTransport(targetHost, targetPort);") {
		t.Fatalf("tls.connect must use the CONNECT-aware transport stream")
	}
	if !strings.Contains(string(bootstrap), "const socket = createTunnelTransport(targetHost, targetPort);") {
		t.Fatalf("net.connect must use the CONNECT-aware transport stream")
	}
	if !strings.Contains(string(bootstrap), "const tunnel = createTunnelTransport(targetHost, targetPort);") ||
		!strings.Contains(string(bootstrap), "socket.write = (...args) => tunnel.write(...args);") {
		t.Fatalf("Socket.prototype.connect must proxy an existing socket through the CONNECT-aware transport stream")
	}
	if !strings.Contains(string(bootstrap), "options.socket.__cursorTapTunnel") ||
		!strings.Contains(string(bootstrap), "debug('tls existing CONNECT '") {
		t.Fatalf("tls.connect must unwrap Cursor Tap sockets to the real CONNECT-aware tunnel")
	}
	if !strings.Contains(string(bootstrap), "http2.connect = function proxiedHTTP2Connect") ||
		!strings.Contains(string(bootstrap), "debug('http2 TLS CONNECT '") {
		t.Fatalf("bootstrap must force node:http2/grpc-js TLS sessions through the CONNECT-aware tunnel")
	}
	extensionPath := cursorInjectorExtensionPath(dataDir)
	if _, err := os.Stat(filepath.Join(extensionPath, "package.json")); err != nil {
		t.Fatalf("injector extension package was not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(extensionPath, "extension.js")); err != nil {
		t.Fatalf("injector extension entrypoint was not written: %v", err)
	}
	wantNodeOptions := "--require=" + bootstrapPath + " --trace-warnings"
	if got["NODE_OPTIONS"] != wantNodeOptions {
		t.Fatalf("NODE_OPTIONS should preload Cursor Tap bootstrap and preserve existing flags: %q", got["NODE_OPTIONS"])
	}

	env, err = cursorLaunchEnv(env, dataDir, 18080, 11080)
	if err != nil {
		t.Fatalf("cursorLaunchEnv second call: %v", err)
	}
	got = envToMap(env)
	if got["NODE_OPTIONS"] != wantNodeOptions {
		t.Fatalf("NODE_OPTIONS should not duplicate bootstrap on repeated calls: %q", got["NODE_OPTIONS"])
	}

	args := cursorLaunchArgs(dataDir, 18080)
	if len(args) != 3 || args[0] != "--proxy-server=http://127.0.0.1:18080" {
		t.Fatalf("unexpected launch args: %#v", args)
	}
	if args[1] != "--ignore-certificate-errors" {
		t.Fatalf("certificate bypass arg missing: %#v", args)
	}
	if args[2] != "--extensionDevelopmentPath="+extensionPath {
		t.Fatalf("injector extension arg missing: %#v", args)
	}
}

func envToMap(values []string) map[string]string {
	env := make(map[string]string, len(values))
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		if ok {
			env[key] = val
		}
	}
	return env
}
