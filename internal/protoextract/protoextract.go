package protoextract

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bufbuild/protocompile"
	"github.com/burpheart/cursor-tap/internal/httpstream"
	"github.com/burpheart/cursor-tap/internal/storage"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Result describes a loaded Cursor protocol source.
type Result struct {
	ID          string
	Path        string
	SHA256      string
	SourceKind  string
	Messages    int
	Enums       int
	Services    int
	Diagnostics []string
	ProtoSource map[string]string
}

// Load extracts protocol sources from a .proto file, a fixture-style .js file,
// or a Cursor.app directory.
func Load(path string) (*Result, error) {
	if path == "" {
		return nil, fmt.Errorf("protocol path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return loadDir(path)
	}
	return loadFile(path)
}

func loadDir(path string) (*Result, error) {
	var candidates []string
	err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if ext == ".proto" || ext == ".js" {
			candidates = append(candidates, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(candidates)
	for _, p := range candidates {
		result, err := loadFile(p)
		if err == nil && len(result.ProtoSource) > 0 {
			result.Path = path
			return result, nil
		}
	}
	return &Result{
		Path:        path,
		SourceKind:  "directory",
		Diagnostics: []string{"no loadable .proto or embedded proto JS file found"},
		ProtoSource: map[string]string{},
	}, nil
}

func loadFile(path string) (*Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	result := &Result{
		Path:        path,
		SHA256:      hex.EncodeToString(sum[:]),
		SourceKind:  strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), "."),
		ProtoSource: map[string]string{},
	}
	if result.SourceKind == "" {
		result.SourceKind = "file"
	}

	switch strings.ToLower(filepath.Ext(path)) {
	case ".proto":
		result.ProtoSource[filepath.Base(path)] = string(data)
	case ".js":
		extractEmbeddedProto(result, data)
		if len(result.ProtoSource) == 0 {
			sources, messages, enums, services, diagnostics := extractCursorProtoSources(data)
			result.Diagnostics = append(result.Diagnostics, diagnostics...)
			result.Messages = messages
			result.Enums = enums
			result.Services = services
			if len(sources) > 0 {
				result.SourceKind = "cursor-js"
				result.ProtoSource = sources
			} else {
				result.Diagnostics = append(result.Diagnostics,
					"no // cursor-tap-proto-begin block found; saved JS fingerprint only",
				)
				result.Messages = len(regexp.MustCompile(`this\.typeName\s*=`).FindAll(data, -1))
				result.Services = len(regexp.MustCompile(`typeName:\s*"`).FindAll(data, -1))
			}
		}
	default:
		return nil, fmt.Errorf("unsupported protocol source: %s", path)
	}

	if len(result.ProtoSource) > 0 {
		result.Messages, result.Enums, result.Services = countProto(result.ProtoSource)
	}
	result.ID = result.SHA256
	if len(result.ID) > 16 {
		result.ID = result.ID[:16]
	}
	return result, nil
}

func extractEmbeddedProto(result *Result, data []byte) {
	// Test fixtures and hand-authored protocol shims can embed source as:
	// // cursor-tap-proto-begin: e2e_v1.proto
	// syntax = "proto3";
	// ...
	// // cursor-tap-proto-end
	blockRe := regexp.MustCompile(`(?s)//\s*cursor-tap-proto-begin:\s*([^\n\r]+)\r?\n(.*?)//\s*cursor-tap-proto-end`)
	matches := blockRe.FindAllSubmatch(data, -1)
	for i, match := range matches {
		name := strings.TrimSpace(string(match[1]))
		if name == "" {
			name = fmt.Sprintf("cursor_tap_%d.proto", i+1)
		}
		result.ProtoSource[name] = strings.TrimSpace(string(match[2])) + "\n"
	}

	// Also support a compact assignment for simple local experiments.
	assignRe := regexp.MustCompile("(?s)cursorTapProto\\s*=\\s*`(.*?)`")
	if match := assignRe.FindSubmatch(data); match != nil {
		result.ProtoSource["cursor_tap_inline.proto"] = strings.TrimSpace(string(match[1])) + "\n"
	}
}

func countProto(sources map[string]string) (messages, enums, services int) {
	msgRe := regexp.MustCompile(`(?m)^\s*message\s+\w+`)
	enumRe := regexp.MustCompile(`(?m)^\s*enum\s+\w+`)
	serviceRe := regexp.MustCompile(`(?m)^\s*service\s+\w+`)
	for _, src := range sources {
		messages += len(msgRe.FindAllString(src, -1))
		enums += len(enumRe.FindAllString(src, -1))
		services += len(serviceRe.FindAllString(src, -1))
	}
	return messages, enums, services
}

// BuildRegistry compiles extracted proto sources into a dynamic message registry.
func BuildRegistry(result *Result) (*httpstream.MessageRegistry, error) {
	registry := httpstream.DefaultGRPCRegistry()
	if result == nil || len(result.ProtoSource) == 0 {
		return registry, nil
	}

	tmp, err := os.MkdirTemp("", "cursor-tap-proto-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	var files []string
	for name, src := range result.ProtoSource {
		name = filepath.Clean(name)
		if filepath.IsAbs(name) || strings.HasPrefix(name, "..") {
			return nil, fmt.Errorf("invalid proto source name %q", name)
		}
		full := filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(full, []byte(src), 0644); err != nil {
			return nil, err
		}
		files = append(files, name)
	}
	sort.Strings(files)

	compiler := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{ImportPaths: []string{tmp}}),
	}
	compiled, err := compiler.Compile(context.Background(), files...)
	if err != nil {
		return nil, fmt.Errorf("compile protocol: %w", err)
	}

	for _, fd := range compiled {
		registerFile(registry, fd.Services())
	}
	return registry, nil
}

func registerFile(registry *httpstream.MessageRegistry, services protoreflect.ServiceDescriptors) {
	for i := 0; i < services.Len(); i++ {
		svc := services.Get(i)
		methods := svc.Methods()
		for j := 0; j < methods.Len(); j++ {
			method := methods.Get(j)
			registry.RegisterMethod(
				string(svc.FullName()),
				string(method.Name()),
				dynamicpb.NewMessageType(method.Input()),
				dynamicpb.NewMessageType(method.Output()),
				httpstream.MethodInfo{
					ClientStreaming: method.IsStreamingClient(),
					ServerStreaming: method.IsStreamingServer(),
				},
			)
		}
	}
}

func (r *Result) StorageVersion(active bool) storage.ProtocolVersion {
	if r == nil {
		return storage.ProtocolVersion{}
	}
	return storage.ProtocolVersion{
		ID:          r.ID,
		Path:        r.Path,
		SHA256:      r.SHA256,
		SourceKind:  r.SourceKind,
		Messages:    r.Messages,
		Enums:       r.Enums,
		Services:    r.Services,
		Diagnostics: append([]string(nil), r.Diagnostics...),
		ProtoSource: cloneSources(r.ProtoSource),
		Active:      active,
	}
}

func cloneSources(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// FindDefaultCursorApp returns the first standard macOS Cursor.app path.
func FindDefaultCursorApp() string {
	home, _ := os.UserHomeDir()
	candidates := []string{
		"/Applications/Cursor.app",
		filepath.Join(home, "Applications", "Cursor.app"),
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return ""
}

// FindLikelyCursorJS returns a likely protocol-bearing JS file inside Cursor.app.
func FindLikelyCursorJS(cursorApp string) string {
	if cursorApp == "" {
		return ""
	}
	root := filepath.Join(cursorApp, "Contents", "Resources", "app")
	var best string
	var bestScore int
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".js" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		score := bytes.Count(data, []byte("newFieldList"))*3 +
			bytes.Count(data, []byte("typeName")) +
			bytes.Count(data, []byte("methods:"))
		if score > bestScore {
			best = path
			bestScore = score
		}
		return nil
	})
	return best
}
