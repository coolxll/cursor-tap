package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/burpheart/cursor-tap/internal/protoextract"
	"github.com/burpheart/cursor-tap/internal/proxy"
	"github.com/burpheart/cursor-tap/internal/storage"
	"github.com/burpheart/cursor-tap/pkg/types"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed all:web/out
var webAssets embed.FS

type DesktopApp struct {
	ctx    context.Context
	mu     sync.Mutex
	server *proxy.Server
	config types.Config
}

type RuntimeConfig struct {
	HTTPPort         int    `json:"http_port"`
	SOCKS5Port       int    `json:"socks5_port"`
	APIPort          int    `json:"api_port"`
	DataDir          string `json:"data_dir"`
	SQLitePath       string `json:"sqlite_path"`
	ProtocolPath     string `json:"protocol_path"`
	DefaultCursorApp string `json:"default_cursor_app"`
	DefaultCursorJS  string `json:"default_cursor_js"`
}

func main() {
	assets, err := fs.Sub(webAssets, "web/out")
	if err != nil {
		panic(err)
	}

	app := newDesktopApp()
	if err := wails.Run(&options.App{
		Title:     "Cursor Tap",
		Width:     1440,
		Height:    920,
		MinWidth:  1100,
		MinHeight: 720,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		Mac: &mac.Options{
			TitleBar: mac.TitleBarHiddenInset(),
		},
		OnStartup:  app.startup,
		OnDomReady: app.domReady,
		OnShutdown: app.shutdown,
		Bind: []interface{}{
			app,
		},
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newDesktopApp() *DesktopApp {
	home, _ := os.UserHomeDir()
	baseDir := filepath.Join(home, ".cursor-tap")
	dataDir := filepath.Join(baseDir, "data")
	sqlitePath := filepath.Join(dataDir, "cursor-tap.sqlite")
	protocolPath := activeProtocolPath(sqlitePath)
	if protocolPath == "" {
		fixturePath := filepath.Join(dataDir, "cursor-fixture.js")
		if _, err := os.Stat(fixturePath); err == nil {
			protocolPath = fixturePath
		}
	}
	return &DesktopApp{
		config: types.Config{
			HTTPPort:          8080,
			SOCKS5Port:        1080,
			APIPort:           9090,
			CertDir:           baseDir,
			DataDir:           dataDir,
			SQLitePath:        sqlitePath,
			ProtocolPath:      protocolPath,
			EnableHTTPParsing: true,
			HTTPLogLevel:      types.LogLevelBasic,
		},
	}
}

func activeProtocolPath(sqlitePath string) string {
	db, err := storage.Open(sqlitePath)
	if err != nil {
		return ""
	}
	defer db.Close()
	path, err := db.GetSetting("active_protocol_path")
	if err != nil || path == "" {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

func (a *DesktopApp) startup(ctx context.Context) {
	a.ctx = ctx
	if err := ensureCursorProxyInjectionFiles(a.config.DataDir); err != nil {
		fmt.Fprintf(os.Stderr, "prepare cursor proxy injection files: %v\n", err)
	}
	if err := a.StartProxy(); err != nil {
		fmt.Fprintf(os.Stderr, "start proxy: %v\n", err)
	}
}

func (a *DesktopApp) domReady(ctx context.Context) {
	go func() {
		time.Sleep(250 * time.Millisecond)
		runtime.WindowSetSize(ctx, 1440, 920)
		runtime.WindowSetPosition(ctx, 1200, 500)
		time.Sleep(150 * time.Millisecond)
		runtime.WindowSetPosition(ctx, 80, 80)
		runtime.WindowShow(ctx)
	}()
}

func (a *DesktopApp) shutdown(ctx context.Context) {
	a.StopProxy()
}

func (a *DesktopApp) GetRuntimeConfig() RuntimeConfig {
	a.mu.Lock()
	defer a.mu.Unlock()

	cursorApp := protoextract.FindDefaultCursorApp()
	return RuntimeConfig{
		HTTPPort:         a.config.HTTPPort,
		SOCKS5Port:       a.config.SOCKS5Port,
		APIPort:          a.config.APIPort,
		DataDir:          a.config.DataDir,
		SQLitePath:       a.config.SQLitePath,
		ProtocolPath:     a.config.ProtocolPath,
		DefaultCursorApp: cursorApp,
		DefaultCursorJS:  protoextract.FindLikelyCursorJS(cursorApp),
	}
}

func (a *DesktopApp) StartProxy() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server != nil {
		return nil
	}
	if err := os.MkdirAll(a.config.DataDir, 0755); err != nil {
		return err
	}
	server, err := proxy.NewServer(a.config)
	if err != nil {
		return err
	}
	a.server = server
	go func() {
		if err := server.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "proxy stopped: %v\n", err)
		}
		a.mu.Lock()
		if a.server == server {
			a.server = nil
		}
		a.mu.Unlock()
	}()
	return nil
}

func (a *DesktopApp) StopProxy() {
	a.mu.Lock()
	server := a.server
	a.server = nil
	a.mu.Unlock()
	if server != nil {
		server.Stop()
	}
}

func (a *DesktopApp) PickCursorApp() (string, error) {
	if cursorApp := protoextract.FindDefaultCursorApp(); cursorApp != "" {
		return cursorApp, nil
	}
	if a.ctx == nil {
		return "", nil
	}
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:                      "Choose Cursor.app",
		DefaultDirectory:           "/Applications",
		TreatPackagesAsDirectories: false,
		Filters: []runtime.FileFilter{{
			DisplayName: "Applications",
			Pattern:     "*.app",
		}},
	})
}

func (a *DesktopApp) PickCursorJS() (string, error) {
	if a.ctx == nil {
		return "", nil
	}
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:                      "Choose Cursor.app, JavaScript, or proto source",
		TreatPackagesAsDirectories: false,
		Filters: []runtime.FileFilter{{
			DisplayName: "Protocol Source",
			Pattern:     "*.app;*.js;*.proto",
		}},
	})
}

func (a *DesktopApp) LoadProtocolFromJS(path string) (storage.ProtocolVersion, error) {
	result, err := protoextract.Load(path)
	if err != nil {
		return storage.ProtocolVersion{}, err
	}
	if _, err := protoextract.BuildRegistry(result); err != nil {
		return storage.ProtocolVersion{}, err
	}

	version := result.StorageVersion(true)
	db, err := storage.Open(a.config.SQLitePath)
	if err != nil {
		return storage.ProtocolVersion{}, err
	}
	defer db.Close()
	if err := db.SaveProtocol(version); err != nil {
		return storage.ProtocolVersion{}, err
	}
	db.SetSetting("active_protocol_path", path)

	a.StopProxy()
	a.mu.Lock()
	a.config.ProtocolPath = path
	a.mu.Unlock()
	if err := a.StartProxy(); err != nil {
		return storage.ProtocolVersion{}, err
	}
	return version, nil
}

func (a *DesktopApp) LaunchCursor(cursorPath string) error {
	if cursorPath == "" {
		cursorPath = protoextract.FindDefaultCursorApp()
	}
	executable := cursorExecutable(cursorPath)
	if executable == "" {
		return fmt.Errorf("cursor executable not found")
	}
	if cursorAlreadyRunning() {
		return fmt.Errorf("Cursor is already running. Quit Cursor completely, then launch it from Cursor Tap so the proxy environment and --proxy-server flags are applied")
	}
	preparedPath, err := prepareCursorAppForLaunch(cursorPath, a.config.DataDir)
	if err != nil {
		return err
	}
	executable = cursorExecutable(preparedPath)
	if executable == "" {
		return fmt.Errorf("prepared cursor executable not found")
	}
	cmd := exec.Command(executable)
	env, err := cursorLaunchEnv(os.Environ(), a.config.DataDir, a.config.HTTPPort, a.config.SOCKS5Port)
	if err != nil {
		return err
	}
	cmd.Env = env
	cmd.Args = append(cmd.Args, cursorLaunchArgs(a.config.DataDir, a.config.HTTPPort)...)
	return cmd.Start()
}

func cursorAlreadyRunning() bool {
	out, err := exec.Command("pgrep", "-x", "Cursor").Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

func cursorExecutable(path string) string {
	if path == "" {
		return ""
	}
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return path
	}
	candidate := filepath.Join(path, "Contents", "MacOS", "Cursor")
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
		return candidate
	}
	return ""
}
