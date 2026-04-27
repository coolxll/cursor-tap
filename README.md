# Cursor-Tap App

<p align="center">
  <img src="web/public/cursor_tracker_logo.svg" width="112" alt="Cursor-Tap App logo" />
</p>

<p align="center">
  <a href="README.md">English</a> · <a href="README.zh-CN.md">简体中文</a>
</p>

Cursor-Tap App is a macOS desktop app forked from `cursor-tap`. This fork turns the original traffic capture and protobuf decoding workflow into an app-first experience, with finer-grained gRPC frame inspection, request/response/stream breakdowns, one-click Cursor launch, and automatic JS/proto extraction from the Cursor `.app` bundle.

Wails provides the native macOS shell, Go powers the local proxy, MITM, protocol parsing, and SQLite persistence, while a static Next.js frontend renders the inspector UI. The goal is simple: open the app, start the proxy, load the protocol, launch Cursor, and inspect AI requests and streaming responses in real time.

## Screenshots

![Cursor-Tap App overview](assets/readme/app-overview.png)

![Cursor-Tap stream detail](assets/readme/stream-detail.png)

## What This Fork Adds

- **App-first workflow**: packaged as a macOS desktop app with Wails instead of treating the WebUI and CLI as the main entry points.
- **One-click launch path**: start the proxy, select or auto-locate Cursor, inject proxy environment variables, and launch Cursor from inside the app.
- **Automatic Cursor JS parsing**: extract proto sources from Cursor `.app` JavaScript files or local `.js/.proto` files, then register dynamic message types at runtime.
- **Finer-grained capture**: inspect traffic by RPC call, frame, direction, headers, payload, timing, and sequence diagram.
- **Local persistence**: records, sessions, protocol versions, and settings are stored in local SQLite; runtime captures stay out of the repository.
- **Release-ready packaging**: GitHub release notes classification, dual-architecture macOS builds, and zip/sha256 release asset upload workflow.

## Run The App

```bash
make start
```

Development mode starts Wails and connects the desktop shell to the local Next.js frontend.

## Build The macOS App

```bash
make build-mac
```

The Wails build output is written to `build/bin/`, which is intentionally ignored by Git.

To create a zip and checksum suitable for GitHub Releases:

```bash
make release-mac VERSION=v0.1.0
```

Release assets are written to `build/release/`, including the `.app` zip and `.sha256` file.

## GitHub Release

This repository includes GitHub Release automation:

- `.github/release.yml`: categorizes pull requests for GitHub's automatically generated release notes.
- `.github/workflows/release-macos.yml`: builds `arm64` and `amd64` macOS apps when a release is published or when the workflow is run manually, then uploads zip and sha256 assets.

Recommended flow:

```bash
git tag v0.1.0
git push origin v0.1.0
```

Then manually run the `Release macOS App` workflow with `v0.1.0` as the input tag. The workflow creates a draft release, asks GitHub to generate release notes, and uploads both macOS app artifacts. Review the notes and assets before publishing the draft release.

You can also publish `v0.1.0` directly from GitHub Releases; the release event will trigger the same workflow and upload assets afterward. If immutable releases are enabled, prefer the draft release flow above.

## Verification

```bash
make test
make e2e
```

- `make test`: runs frontend lint, frontend static build, and `go test ./...`.
- `make e2e`: starts the test proxy, temporary SQLite, temporary certificate, fixture protocol, fake upstream, fake Cursor client, and verifies the UI with Playwright.

## Usage

1. Run `make start` to open the desktop app.
2. Click `Start Proxy` to start the local proxy.
3. Click `Proto`, then select the Cursor `.app` bundle or a local `.js/.proto` protocol file; the app extracts and registers proto definitions automatically.
4. Click `Cursor` to launch Cursor with the proxy environment injected.
5. Trigger an AI request inside Cursor. The app will show services, calls, frames, payloads, headers, timing, and sequence diagrams in real time.

When launching Cursor, the app injects:

```bash
HTTP_PROXY=http://127.0.0.1:8080
HTTPS_PROXY=http://127.0.0.1:8080
ALL_PROXY=http://127.0.0.1:8080
NODE_TLS_REJECT_UNAUTHORIZED=0
```

Runtime data, SQLite files, logs, JSONL captures, and certificates should be written to a local user directory such as `~/.cursor-tap/data`, not committed to the repository.

## How It Works

1. **Desktop shell**: Wails v2 embeds the static Next.js `web/out` build; development mode points to `http://localhost:3000`.
2. **Proxy**: a local HTTP/SOCKS5 proxy receives Cursor `CONNECT` traffic, terminates TLS with a local server certificate, then forwards traffic to `api2.cursor.sh`.
3. **Protocol loading**: the app extracts proto sources from Cursor `.app` files or local `.js/.proto` files, then registers dynamic message types through `protocompile` and `dynamicpb`.
4. **Persistence**: records, sessions, protocol versions, and settings are stored in SQLite; new records are pushed to the UI over WebSocket.
5. **UI**: the HeroUI inspector shows a four-pane traffic view with Shiki-highlighted payloads/proto/headers and Mermaid sequence diagrams.

## CLI Fallback

The desktop app is the default entry point. If you only need the proxy, use the fallback CLI:

```bash
go run ./cmd/cursor-tap start \
  --http-port 8080 \
  --api-port 9090 \
  --sqlite ~/.cursor-tap/data/cursor-tap.sqlite \
  --protocol /path/to/cursor-or-fixture.js
```

Frontend development mode:

```bash
cd web
npm ci
npm run dev
```

## Project Structure

```text
├── main.go                 # Wails app entrypoint
├── wails.json              # Wails v2 configuration
├── Makefile                # Development, test, and packaging commands
├── .github/                # GitHub release notes and macOS app build workflow
├── build/                  # App icon and macOS packaging configuration
├── cmd/cursor-tap/         # Proxy CLI
├── cursor_proto/           # Cursor proto source and generated code
├── internal/
│   ├── ca/                 # Local server certificate
│   ├── e2e/                # Go end-to-end tests
│   ├── httpstream/         # Connect/gRPC parsing and recording
│   ├── mitm/               # TLS MITM and forwarding
│   ├── protoextract/       # Runtime protocol extraction and dynamic registration
│   ├── proxy/              # HTTP/SOCKS5/API/WebSocket server
│   └── storage/            # SQLite storage
└── web/                    # Next.js + HeroUI frontend
```
