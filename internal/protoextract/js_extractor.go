package protoextract

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// isGooglePkg checks if a package is a Google standard package that should not be generated
func isGooglePkg(pkg string) bool {
	return pkg == "google.protobuf" || pkg == "google.rpc"
}

// Scalar type mapping
var scalarTypes = map[int]string{
	1:  "double",
	2:  "float",
	3:  "int64",
	4:  "uint64",
	5:  "int32",
	6:  "fixed64",
	7:  "fixed32",
	8:  "bool",
	9:  "string",
	12: "bytes",
	13: "uint32",
	15: "sfixed32",
	16: "sfixed64",
	17: "sint32",
	18: "sint64",
}

type Field struct {
	No           int    `json:"no"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	T            any    `json:"T"`     // int for scalar, string for message ref
	Oneof        string `json:"oneof"` // oneof group name
	Repeated     bool   `json:"repeated"`
	Opt          bool   `json:"opt"` // optional
	MapKey       int    `json:"K"`   // map key type (scalar type number)
	MapValueKind string // "scalar" or "message"
	MapValueT    any    // scalar type number or message var name
}

type Message struct {
	TypeName     string
	VarName      string // JS external variable name (e.g., tPe)
	InternalName string // JS internal class name (e.g., bd)
	Fields       []Field
	Package      string
	ShortName    string
}

type Enum struct {
	TypeName  string
	VarName   string
	Values    []EnumValue
	Package   string
	ShortName string
}

type EnumValue struct {
	No   int
	Name string
}

type Service struct {
	TypeName  string
	VarName   string
	Methods   []Method
	Package   string
	ShortName string
}

type Method struct {
	Name       string
	InputType  string // variable name
	OutputType string // variable name
	Kind       string // Unary, ServerStreaming, ClientStreaming, BiDiStreaming
}

// ExtractProtos extracts proto definitions from formatted JS file
func ExtractProtos(inputFile, outputDir string) {
	content, err := os.ReadFile(inputFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading file: %v\n", err)
		os.Exit(1)
	}

	text := string(content)

	// Extract messages, enums, and services
	messages := extractMessages(text)
	enums := extractEnums(text)
	services := extractServices(text)

	// Build var -> typeName mapping (both external var name and internal class name)
	varToType := make(map[string]string)
	for _, msg := range messages {
		if msg.VarName != "" {
			varToType[msg.VarName] = msg.TypeName
		}
		if msg.InternalName != "" && msg.InternalName != msg.VarName {
			varToType[msg.InternalName] = msg.TypeName
		}
	}
	for _, enum := range enums {
		if enum.VarName != "" {
			varToType[enum.VarName] = enum.TypeName
		}
	}

	// Generate proto files
	generateProtos(messages, enums, services, varToType, outputDir)

	fmt.Printf("提取完成: %d 个消息, %d 个枚举, %d 个服务\n", len(messages), len(enums), len(services))
}

func extractCursorProtoSources(data []byte) (map[string]string, int, int, int, []string) {
	text := string(data)
	messages := extractMessages(text)
	enums := extractEnums(text)
	services := extractServices(text)
	if len(messages) == 0 && len(enums) == 0 && len(services) == 0 {
		return nil, 0, 0, 0, []string{"no protobuf-es descriptors found in JS"}
	}

	varToType := make(map[string]string)
	for _, msg := range messages {
		if msg.VarName != "" {
			varToType[msg.VarName] = msg.TypeName
		}
		if msg.InternalName != "" && msg.InternalName != msg.VarName {
			varToType[msg.InternalName] = msg.TypeName
		}
	}
	for _, enum := range enums {
		if enum.VarName != "" {
			varToType[enum.VarName] = enum.TypeName
		}
	}

	tmp, err := os.MkdirTemp("", "cursor-tap-js-proto-*")
	if err != nil {
		return nil, len(messages), len(enums), len(services), []string{fmt.Sprintf("create temp proto dir: %v", err)}
	}
	defer os.RemoveAll(tmp)

	generateProtos(messages, enums, services, varToType, tmp)

	sources := make(map[string]string)
	err = filepath.WalkDir(tmp, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".proto" {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(tmp, path)
		if err != nil {
			return err
		}
		sources[filepath.ToSlash(name)] = string(body)
		return nil
	})
	if err != nil {
		return nil, len(messages), len(enums), len(services), []string{fmt.Sprintf("read generated proto: %v", err)}
	}
	return sources, len(messages), len(enums), len(services), []string{
		fmt.Sprintf("extracted protobuf-es descriptors from JS: %d messages, %d enums, %d services", len(messages), len(enums), len(services)),
	}
}

func extractMessages(text string) []Message {
	var messages []Message

	// Pattern 1: VarName = class InternalName extends l { ... this.typeName = "..." ... this.fields = ... }
	// 先找所有 "变量名 = class 内部类名" 定义
	// JS 变量名可以包含 $ 符号，如 B$e, qg 等
	// 需要同时捕获外部变量名和内部类名，因为字段引用可能用任一个
	classDefRe := regexp.MustCompile(`([\w$]+)\s*=\s*class\s+([\w$]+)\s+extends\s+[\w$]+\s*\{`)
	classMatches := classDefRe.FindAllStringSubmatchIndex(text, -1)

	// Pattern: this.typeName = "xxx.v1.YYY" (any package)
	typeNameRe := regexp.MustCompile(`this\.typeName\s*=\s*"([\w.]+)"`)

	// Pattern: this.fields = n.util.newFieldList(() => [...])
	fieldsRe := regexp.MustCompile(`this\.fields\s*=\s*\w+(?:\.proto3)?\.util\.newFieldList\s*\(\s*\(\s*\)\s*=>\s*\[`)

	for _, classMatch := range classMatches {
		varName := text[classMatch[2]:classMatch[3]]
		internalName := text[classMatch[4]:classMatch[5]]
		classStart := classMatch[0]

		// 找到类的结束位置（匹配大括号）
		classEnd := findClassEnd(text, classMatch[1]-1)
		if classEnd == -1 {
			continue
		}

		classBody := text[classStart:classEnd]

		// 在类体内查找 typeName
		typeMatch := typeNameRe.FindStringSubmatch(classBody)
		if typeMatch == nil {
			continue
		}
		typeName := typeMatch[1]

		// 在类体内查找 fields
		fieldsMatch := fieldsRe.FindStringIndex(classBody)
		if fieldsMatch == nil {
			continue
		}

		// 找到 fields 数组的开始位置
		bracketPos := classStart + fieldsMatch[1] - 1
		fields := extractFieldArray(text, bracketPos)

		pkg, shortName := parseTypeName(typeName)
		msg := Message{
			TypeName:     typeName,
			VarName:      varName,
			InternalName: internalName,
			Fields:       fields,
			Package:      pkg,
			ShortName:    shortName,
		}
		messages = append(messages, msg)
	}

	return messages
}

// findClassEnd finds the matching closing brace for a class definition
func findClassEnd(text string, openBrace int) int {
	depth := 0
	for i := openBrace; i < len(text); i++ {
		if text[i] == '{' {
			depth++
		} else if text[i] == '}' {
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

func extractFieldArray(text string, start int) []Field {
	// Find matching bracket
	depth := 0
	end := start
	for i := start; i < len(text); i++ {
		if text[i] == '[' {
			depth++
		} else if text[i] == ']' {
			depth--
			if depth == 0 {
				end = i + 1
				break
			}
		}
	}

	arrayText := text[start:end]

	// Parse individual field objects by extracting each {...} block
	var fields []Field

	// Find each field object
	fieldObjects := extractFieldObjects(arrayText)

	for _, fieldObj := range fieldObjects {
		field := parseFieldObject(fieldObj)
		if field != nil {
			fields = append(fields, *field)
		}
	}

	return fields
}

// extractFieldObjects extracts individual {...} objects from array text
func extractFieldObjects(arrayText string) []string {
	var objects []string
	depth := 0
	start := -1

	for i := 0; i < len(arrayText); i++ {
		if arrayText[i] == '{' {
			if depth == 0 {
				start = i
			}
			depth++
		} else if arrayText[i] == '}' {
			depth--
			if depth == 0 && start >= 0 {
				objects = append(objects, arrayText[start:i+1])
				start = -1
			}
		}
	}

	return objects
}

// parseFieldObject parses a single field object like { no: 1, name: "foo", kind: "scalar", T: 9, opt: !0 }
func parseFieldObject(obj string) *Field {
	// Extract no
	noRe := regexp.MustCompile(`no:\s*(\d+)`)
	noMatch := noRe.FindStringSubmatch(obj)
	if noMatch == nil {
		return nil
	}
	no, _ := strconv.Atoi(noMatch[1])

	// Extract name
	nameRe := regexp.MustCompile(`name:\s*"([^"]+)"`)
	nameMatch := nameRe.FindStringSubmatch(obj)
	if nameMatch == nil {
		return nil
	}

	// Extract kind
	kindRe := regexp.MustCompile(`kind:\s*"([^"]+)"`)
	kindMatch := kindRe.FindStringSubmatch(obj)
	if kindMatch == nil {
		return nil
	}

	field := &Field{
		No:   no,
		Name: nameMatch[1],
		Kind: kindMatch[1],
	}

	// Extract T (type) - can be:
	// 1. number (scalar): T: 9
	// 2. variable name: T: SPe
	// 3. getEnumType call: T: n.getEnumType(SPe) or T: n.proto3.getEnumType(SPe)

	// Try getEnumType pattern first (for enums)
	enumTypeRe := regexp.MustCompile(`[,\s]T:\s*\w+(?:\.\w+)*\.getEnumType\s*\(\s*([\w$]+)\s*\)`)
	if enumMatch := enumTypeRe.FindStringSubmatch(obj); enumMatch != nil {
		field.T = enumMatch[1]
	} else {
		// Try simple T: value pattern
		tRe := regexp.MustCompile(`[,\s]T:\s*([\w$]+)`)
		if tMatch := tRe.FindStringSubmatch(obj); tMatch != nil {
			if t, err := strconv.Atoi(tMatch[1]); err == nil {
				field.T = t
			} else {
				field.T = tMatch[1]
			}
		}
	}

	// Check for oneof (within THIS object only)
	oneofRe := regexp.MustCompile(`oneof:\s*"([^"]+)"`)
	if oneofMatch := oneofRe.FindStringSubmatch(obj); oneofMatch != nil {
		field.Oneof = oneofMatch[1]
	}

	// Check for repeated (within THIS object only)
	// !0 means true in minified JS
	repeatedRe := regexp.MustCompile(`repeated:\s*(!0|true)`)
	if repeatedRe.MatchString(obj) {
		field.Repeated = true
	}

	// Check for optional (within THIS object only)
	optRe := regexp.MustCompile(`opt:\s*(!0|true)`)
	if optRe.MatchString(obj) {
		field.Opt = true
	}

	// Check for map type: K: keyType, V: { kind: "scalar"|"message", T: valueType }
	if field.Kind == "map" {
		// Extract K (key type)
		keyRe := regexp.MustCompile(`[,\s]K:\s*(\d+)`)
		if keyMatch := keyRe.FindStringSubmatch(obj); keyMatch != nil {
			field.MapKey, _ = strconv.Atoi(keyMatch[1])
		}

		// Extract V (value type) - { kind: "xxx", T: yyy }
		valueRe := regexp.MustCompile(`V:\s*\{\s*kind:\s*"(\w+)"\s*,\s*T:\s*([\w$]+)`)
		if valueMatch := valueRe.FindStringSubmatch(obj); valueMatch != nil {
			field.MapValueKind = valueMatch[1]
			if t, err := strconv.Atoi(valueMatch[2]); err == nil {
				field.MapValueT = t
			} else {
				field.MapValueT = valueMatch[2]
			}
		}
	}

	return field
}

func extractEnums(text string) []Enum {
	var enums []Enum

	// Pattern for enum: setEnumType(XXX, "xxx.v1.EnumName", [...]) (any package)
	// JS 变量名可以包含 $ 符号
	enumRe := regexp.MustCompile(`setEnumType\s*\(\s*([\w$]+)\s*,\s*"([\w.]+)"\s*,\s*\[`)

	matches := enumRe.FindAllStringSubmatchIndex(text, -1)
	for _, match := range matches {
		varName := text[match[2]:match[3]]
		typeName := text[match[4]:match[5]]

		// Extract enum values array
		bracketStart := match[1] - 1
		values := extractEnumValues(text, bracketStart)

		pkg, shortName := parseTypeName(typeName)
		enum := Enum{
			TypeName:  typeName,
			VarName:   varName,
			Values:    values,
			Package:   pkg,
			ShortName: shortName,
		}
		enums = append(enums, enum)
	}

	return enums
}

func extractServices(text string) []Service {
	var services []Service

	// Pattern: VarName = { typeName: "xxx.v1.ServiceName", methods: { ... } }
	// Service definitions are object literals, not classes
	serviceRe := regexp.MustCompile(`([\w$]+)\s*=\s*\{\s*typeName:\s*"([\w.]+)"\s*,\s*methods:\s*\{`)

	matches := serviceRe.FindAllStringSubmatchIndex(text, -1)
	for _, match := range matches {
		varName := text[match[2]:match[3]]
		typeName := text[match[4]:match[5]]

		// Find the end of the methods object
		methodsStart := match[1] - 1 // position of '{'
		methodsEnd := findMatchingBrace(text, methodsStart)
		if methodsEnd == -1 {
			continue
		}

		methodsText := text[methodsStart:methodsEnd]
		methods := extractMethods(methodsText)

		pkg, shortName := parseTypeName(typeName)
		service := Service{
			TypeName:  typeName,
			VarName:   varName,
			Methods:   methods,
			Package:   pkg,
			ShortName: shortName,
		}
		services = append(services, service)
	}

	return services
}

func extractMethods(methodsText string) []Method {
	var methods []Method

	// Pattern: methodName: { name: "MethodName", I: InputVar, O: OutputVar, kind: w.Unary }
	methodRe := regexp.MustCompile(`\w+:\s*\{\s*name:\s*"([^"]+)"\s*,\s*I:\s*([\w$]+)\s*,\s*O:\s*([\w$]+)\s*,\s*kind:\s*\w+\.(Unary|ServerStreaming|ClientStreaming|BiDiStreaming)`)

	matches := methodRe.FindAllStringSubmatch(methodsText, -1)
	for _, m := range matches {
		method := Method{
			Name:       m[1],
			InputType:  m[2],
			OutputType: m[3],
			Kind:       m[4],
		}
		methods = append(methods, method)
	}

	return methods
}

func findMatchingBrace(text string, start int) int {
	depth := 0
	for i := start; i < len(text); i++ {
		if text[i] == '{' {
			depth++
		} else if text[i] == '}' {
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

func extractEnumValues(text string, start int) []EnumValue {
	// Find matching bracket
	depth := 0
	end := start
	for i := start; i < len(text); i++ {
		if text[i] == '[' {
			depth++
		} else if text[i] == ']' {
			depth--
			if depth == 0 {
				end = i + 1
				break
			}
		}
	}

	arrayText := text[start:end]

	var values []EnumValue
	valueRe := regexp.MustCompile(`\{\s*no:\s*(\d+)\s*,\s*name:\s*"([^"]+)"`)

	matches := valueRe.FindAllStringSubmatch(arrayText, -1)
	for _, m := range matches {
		no, _ := strconv.Atoi(m[1])
		values = append(values, EnumValue{No: no, Name: m[2]})
	}

	return values
}

func generateProtos(messages []Message, enums []Enum, services []Service, varToType map[string]string, outputDir string) {
	os.MkdirAll(outputDir, 0755)

	// Group by package
	packages := make(map[string]struct {
		messages []Message
		enums    []Enum
		services []Service
	})

	for _, msg := range messages {
		pkg := packages[msg.Package]
		pkg.messages = append(pkg.messages, msg)
		packages[msg.Package] = pkg
	}

	for _, enum := range enums {
		pkg := packages[enum.Package]
		pkg.enums = append(pkg.enums, enum)
		packages[enum.Package] = pkg
	}

	for _, svc := range services {
		pkg := packages[svc.Package]
		replaced := false
		for i := range pkg.services {
			if pkg.services[i].TypeName == svc.TypeName {
				if len(svc.Methods) > len(pkg.services[i].Methods) {
					pkg.services[i] = svc
				}
				replaced = true
				break
			}
		}
		if !replaced {
			pkg.services = append(pkg.services, svc)
		}
		packages[svc.Package] = pkg
	}

	// Build global type maps for copying
	allMessages := make(map[string]*Message) // shortName -> Message
	allEnums := make(map[string]*Enum)       // shortName -> Enum
	msgByVarName := make(map[string]*Message)
	enumByVarName := make(map[string]*Enum)

	for pkgName, pkg := range packages {
		if isGooglePkg(pkgName) {
			continue
		}
		for i := range pkg.messages {
			msg := &pkg.messages[i]
			allMessages[msg.TypeName] = msg
			msgByVarName[msg.VarName] = msg
			if msg.InternalName != "" {
				msgByVarName[msg.InternalName] = msg
			}
		}
		for i := range pkg.enums {
			enum := &pkg.enums[i]
			allEnums[enum.TypeName] = enum
			enumByVarName[enum.VarName] = enum
		}
	}

	// Reset copiedTypes tracking
	copiedTypes = make(map[string]map[string]string)

	for pkgName, pkg := range packages {
		// Skip Google standard packages - use official proto files instead
		if isGooglePkg(pkgName) {
			fmt.Printf("跳过: %s (使用官方 proto 文件)\n", pkgName)
			continue
		}

		// Copy all external types referenced by this package
		augmentedPkg := copyAllExternalTypes(pkgName, pkg, varToType, allMessages, allEnums, msgByVarName, enumByVarName)
		generateProtoFile(pkgName, augmentedPkg.messages, augmentedPkg.enums, pkg.services, varToType, outputDir)
	}
}

// copyAllExternalTypes copies all externally referenced types into the current package
func copyAllExternalTypes(pkgName string, pkg struct {
	messages []Message
	enums    []Enum
	services []Service
}, varToType map[string]string, allMessages map[string]*Message, allEnums map[string]*Enum,
	msgByVarName map[string]*Message, enumByVarName map[string]*Enum) struct {
	messages []Message
	enums    []Enum
	services []Service
} {
	if copiedTypes[pkgName] == nil {
		copiedTypes[pkgName] = make(map[string]string)
	}

	// Build set of types already in this package
	// Also record them in copiedTypes so resolveFieldTypeWithPkg can use local names
	localTypes := make(map[string]bool)
	for _, msg := range pkg.messages {
		localTypes[msg.ShortName] = true
		// Mark as "local" - empty string means original type in this package
		if copiedTypes[pkgName][msg.ShortName] == "" {
			copiedTypes[pkgName][msg.ShortName] = "local:" + msg.TypeName
		}
	}
	for _, enum := range pkg.enums {
		localTypes[enum.ShortName] = true
		if copiedTypes[pkgName][enum.ShortName] == "" {
			copiedTypes[pkgName][enum.ShortName] = "local:" + enum.TypeName
		}
	}

	// Result starts with original types
	result := struct {
		messages []Message
		enums    []Enum
		services []Service
	}{
		messages: append([]Message{}, pkg.messages...),
		enums:    append([]Enum{}, pkg.enums...),
		services: pkg.services,
	}

	totalCopied := 0

	// Iterate until no new types need to be copied
	for round := 1; ; round++ {
		// Collect all external type references from current messages
		neededTypes := make(map[string]bool)

		for _, msg := range result.messages {
			for _, f := range msg.Fields {
				collectFieldRefsSimple(f, pkgName, varToType, neededTypes, localTypes)
			}
		}
		for _, svc := range result.services {
			for _, m := range svc.Methods {
				collectMethodRefsSimple(m.InputType, pkgName, varToType, neededTypes, localTypes)
				collectMethodRefsSimple(m.OutputType, pkgName, varToType, neededTypes, localTypes)
			}
		}

		// Copy needed types
		copiedThisRound := 0
		for typeName := range neededTypes {
			refPkg, shortName := parseTypeName(typeName)
			if refPkg == pkgName || isGooglePkg(refPkg) {
				continue
			}

			// Check if already local
			if localTypes[shortName] {
				continue
			}

			// Copy message
			if msg, ok := allMessages[typeName]; ok {
				msgCopy := *msg
				msgCopy.Package = pkgName
				// Keep original TypeName for source reference in comments
				// msgCopy.TypeName will be used for reference, store original separately
				result.messages = append(result.messages, msgCopy)
				copiedTypes[pkgName][shortName] = typeName // original full type name
				localTypes[shortName] = true
				copiedThisRound++
				fmt.Printf("  [%s] 轮%d 复制: %s\n", pkgName, round, typeName)
			} else if enum, ok := allEnums[typeName]; ok {
				// Copy enum
				enumCopy := *enum
				enumCopy.Package = pkgName
				result.enums = append(result.enums, enumCopy)
				copiedTypes[pkgName][shortName] = typeName
				localTypes[shortName] = true
				copiedThisRound++
				fmt.Printf("  [%s] 轮%d 复制枚举: %s\n", pkgName, round, typeName)
			} else {
				// Type not found - add to copiedTypes anyway to use local reference
				// This handles cases where the type exists locally but wasn't in our extraction
				copiedTypes[pkgName][shortName] = typeName
				localTypes[shortName] = true
				fmt.Printf("  [%s] 轮%d 警告: 类型未找到 %s，标记为本地引用\n", pkgName, round, typeName)
			}
		}

		totalCopied += copiedThisRound

		if copiedThisRound == 0 {
			break // No more types to copy
		}

		if round > 20 {
			fmt.Printf("  [%s] 警告: 复制轮次超过20，可能存在问题\n", pkgName)
			break
		}
	}

	if totalCopied > 0 {
		fmt.Printf("  [%s] 共复制 %d 个外部类型\n", pkgName, totalCopied)
	}

	return result
}

// collectFieldRefsSimple collects external type references from a field (non-recursive, just this field)
func collectFieldRefsSimple(f Field, currentPkg string, varToType map[string]string,
	neededTypes map[string]bool, localTypes map[string]bool) {

	var varNames []string
	if f.Kind == "message" || f.Kind == "enum" {
		if v, ok := f.T.(string); ok {
			varNames = append(varNames, v)
		}
	}
	if f.Kind == "map" && f.MapValueKind == "message" {
		if v, ok := f.MapValueT.(string); ok {
			varNames = append(varNames, v)
		}
	}

	for _, varName := range varNames {
		typeName, exists := varToType[varName]
		if !exists {
			continue
		}

		refPkg, shortName := parseTypeName(typeName)
		if refPkg == "" || refPkg == currentPkg || isGooglePkg(refPkg) {
			continue
		}

		// Skip if already local
		if localTypes[shortName] {
			continue
		}

		neededTypes[typeName] = true
	}
}

// collectMethodRefsSimple collects external type references from a method type
func collectMethodRefsSimple(varName string, currentPkg string, varToType map[string]string,
	neededTypes map[string]bool, localTypes map[string]bool) {

	typeName, exists := varToType[varName]
	if !exists {
		return
	}

	refPkg, shortName := parseTypeName(typeName)
	if refPkg == "" || refPkg == currentPkg || isGooglePkg(refPkg) {
		return
	}

	if localTypes[shortName] {
		return
	}

	neededTypes[typeName] = true
}

// Global map to track copied types: targetPkg -> shortName -> original typeName
var copiedTypes = make(map[string]map[string]string)

// TypeNode represents a node in the nested type tree
type TypeNode struct {
	Name     string
	Message  *Message
	Enum     *Enum
	Children map[string]*TypeNode
}

// collectImports collects only Google standard imports (all other types are copied locally)
func collectImports(currentPkg string, messages []Message, services []Service, varToType map[string]string) map[string]bool {
	imports := make(map[string]bool)

	addImport := func(varName string) {
		if typeName, exists := varToType[varName]; exists {
			refPkg, _ := parseTypeName(typeName)
			// Only import Google standard types - all others are copied locally
			if refPkg == "google.protobuf" {
				_, shortName := parseTypeName(typeName)
				var importFile string
				switch shortName {
				case "Struct", "Value", "ListValue", "NullValue":
					importFile = "google/protobuf/struct.proto"
				case "Timestamp":
					importFile = "google/protobuf/timestamp.proto"
				case "Duration":
					importFile = "google/protobuf/duration.proto"
				case "Any":
					importFile = "google/protobuf/any.proto"
				case "Empty":
					importFile = "google/protobuf/empty.proto"
				case "FieldMask":
					importFile = "google/protobuf/field_mask.proto"
				case "BoolValue", "BytesValue", "DoubleValue", "FloatValue",
					"Int32Value", "Int64Value", "StringValue", "UInt32Value", "UInt64Value":
					importFile = "google/protobuf/wrappers.proto"
				default:
					importFile = "google/protobuf/descriptor.proto"
				}
				imports[importFile] = true
			} else if refPkg == "google.rpc" {
				_, shortName := parseTypeName(typeName)
				var importFile string
				switch shortName {
				case "Status":
					importFile = "google/rpc/status.proto"
				case "Code":
					importFile = "google/rpc/code.proto"
				default:
					importFile = "google/rpc/status.proto"
				}
				imports[importFile] = true
			}
		}
	}

	for _, msg := range messages {
		for _, f := range msg.Fields {
			if f.Kind == "message" || f.Kind == "enum" {
				if varName, ok := f.T.(string); ok {
					addImport(varName)
				}
			}
			// Also check map value types
			if f.Kind == "map" && f.MapValueKind == "message" {
				if varName, ok := f.MapValueT.(string); ok {
					addImport(varName)
				}
			}
		}
	}

	for _, svc := range services {
		for _, m := range svc.Methods {
			addImport(m.InputType)
			addImport(m.OutputType)
			if _, ok := varToType[m.InputType]; !ok {
				imports["google/protobuf/any.proto"] = true
			}
			if _, ok := varToType[m.OutputType]; !ok {
				imports["google/protobuf/any.proto"] = true
			}
		}
	}

	return imports
}

func generateProtoFile(pkgName string, messages []Message, enums []Enum, services []Service, varToType map[string]string, outputDir string) {
	// First, collect all cross-package imports
	imports := collectImports(pkgName, messages, services, varToType)

	var sb strings.Builder

	sb.WriteString(`syntax = "proto3";` + "\n\n")
	sb.WriteString(fmt.Sprintf("package %s;\n\n", pkgName))

	// Write imports
	if len(imports) > 0 {
		sortedImports := make([]string, 0, len(imports))
		for imp := range imports {
			sortedImports = append(sortedImports, imp)
		}
		sort.Strings(sortedImports)
		for _, imp := range sortedImports {
			sb.WriteString(fmt.Sprintf("import \"%s\";\n", imp))
		}
		sb.WriteString("\n")
	}

	goPackagePath := strings.ReplaceAll(pkgName, ".", "/")
	goPackageName := strings.ReplaceAll(pkgName, ".", "")
	sb.WriteString(fmt.Sprintf(`option go_package = "github.com/burpheart/cursor-tap/cursor_proto/gen/%s;%s";`+"\n\n", goPackagePath, goPackageName))

	// Build type tree
	root := &TypeNode{Children: make(map[string]*TypeNode)}

	for i := range messages {
		msg := &messages[i]
		path := getNestedPath(msg.ShortName)
		insertMessage(root, path, msg)
	}

	for i := range enums {
		enum := &enums[i]
		path := getNestedPath(enum.ShortName)
		insertEnum(root, path, enum)
	}

	// Write all top-level types
	writeTypeTree(root, &sb, varToType, 0, pkgName)

	// Write services
	sort.Slice(services, func(i, j int) bool {
		return services[i].ShortName < services[j].ShortName
	})

	for _, svc := range services {
		// Write source comment for service
		sb.WriteString(fmt.Sprintf("// Source: %s (var: %s)\n", svc.TypeName, svc.VarName))
		sb.WriteString(fmt.Sprintf("service %s {\n", svc.ShortName))
		for _, m := range svc.Methods {
			inputType := resolveMethodType(m.InputType, varToType, pkgName)
			outputType := resolveMethodType(m.OutputType, varToType, pkgName)

			switch m.Kind {
			case "ServerStreaming":
				sb.WriteString(fmt.Sprintf("  rpc %s(%s) returns (stream %s) {}\n", m.Name, inputType, outputType))
			case "ClientStreaming":
				sb.WriteString(fmt.Sprintf("  rpc %s(stream %s) returns (%s) {}\n", m.Name, inputType, outputType))
			case "BiDiStreaming":
				sb.WriteString(fmt.Sprintf("  rpc %s(stream %s) returns (stream %s) {}\n", m.Name, inputType, outputType))
			default: // Unary
				sb.WriteString(fmt.Sprintf("  rpc %s(%s) returns (%s) {}\n", m.Name, inputType, outputType))
			}
		}
		sb.WriteString("}\n\n")
	}

	// Write to file - single flat directory
	fileName := strings.ReplaceAll(pkgName, ".", "_") + ".proto"
	filePath := filepath.Join(outputDir, fileName)

	os.WriteFile(filePath, []byte(sb.String()), 0644)
	fmt.Printf("Generated: %s (%d messages, %d enums, %d services)\n", filePath, len(messages), len(enums), len(services))
}

func resolveMethodType(varName string, varToType map[string]string, currentPkg string) string {
	if typeName, exists := varToType[varName]; exists {
		refPkg, shortName := parseTypeName(typeName)
		if refPkg == currentPkg {
			return shortName
		}
		// Check if this type was copied to current package
		if copied := copiedTypes[currentPkg]; copied != nil {
			if _, isCopied := copied[shortName]; isCopied {
				return shortName
			}
		}
		if refPkg != "" {
			return refPkg + "." + shortName
		}
		return shortName
	}
	return "google.protobuf.Any"
}

func insertMessage(node *TypeNode, path []string, msg *Message) {
	if len(path) == 0 {
		return
	}

	name := path[0]
	if node.Children == nil {
		node.Children = make(map[string]*TypeNode)
	}

	child, exists := node.Children[name]
	if !exists {
		child = &TypeNode{Name: name, Children: make(map[string]*TypeNode)}
		node.Children[name] = child
	}

	if len(path) == 1 {
		child.Message = msg
	} else {
		insertMessage(child, path[1:], msg)
	}
}

func insertEnum(node *TypeNode, path []string, enum *Enum) {
	if len(path) == 0 {
		return
	}

	name := path[0]
	if node.Children == nil {
		node.Children = make(map[string]*TypeNode)
	}

	child, exists := node.Children[name]
	if !exists {
		child = &TypeNode{Name: name, Children: make(map[string]*TypeNode)}
		node.Children[name] = child
	}

	if len(path) == 1 {
		child.Enum = enum
	} else {
		insertEnum(child, path[1:], enum)
	}
}

func writeTypeTree(node *TypeNode, sb *strings.Builder, varToType map[string]string, indent int, currentPkg string) {
	// Get sorted child names
	var names []string
	for name := range node.Children {
		names = append(names, name)
	}
	sort.Strings(names)

	indentStr := strings.Repeat("  ", indent)

	for _, name := range names {
		child := node.Children[name]

		if child.Enum != nil {
			// Check if this is a copied type
			originalType := ""
			if copied := copiedTypes[currentPkg]; copied != nil {
				if orig, ok := copied[child.Enum.ShortName]; ok {
					originalType = orig
				}
			}

			// Write source comment for enum
			if originalType != "" {
				sb.WriteString(fmt.Sprintf("%s// Copied from: %s (var: %s)\n", indentStr, originalType, child.Enum.VarName))
			} else {
				sb.WriteString(fmt.Sprintf("%s// Source: %s (var: %s)\n", indentStr, child.Enum.TypeName, child.Enum.VarName))
			}
			// Write enum
			sb.WriteString(fmt.Sprintf("%senum %s {\n", indentStr, name))
			for _, v := range child.Enum.Values {
				sb.WriteString(fmt.Sprintf("%s  %s = %d;\n", indentStr, v.Name, v.No))
			}
			sb.WriteString(fmt.Sprintf("%s}\n\n", indentStr))
		} else if child.Message != nil || len(child.Children) > 0 {
			// Write source comment for message
			if child.Message != nil {
				varInfo := child.Message.VarName
				if child.Message.InternalName != "" && child.Message.InternalName != child.Message.VarName {
					varInfo = fmt.Sprintf("%s, class: %s", child.Message.VarName, child.Message.InternalName)
				}

				// Check if this is a copied type
				originalType := ""
				if copied := copiedTypes[currentPkg]; copied != nil {
					if orig, ok := copied[child.Message.ShortName]; ok {
						originalType = orig
					}
				}

				if originalType != "" {
					sb.WriteString(fmt.Sprintf("%s// Copied from: %s (var: %s)\n", indentStr, originalType, varInfo))
				} else {
					sb.WriteString(fmt.Sprintf("%s// Source: %s (var: %s)\n", indentStr, child.Message.TypeName, varInfo))
				}
			}
			// Write message (even if just a container for nested types)
			sb.WriteString(fmt.Sprintf("%smessage %s {\n", indentStr, name))

			// Write nested types first
			writeTypeTree(child, sb, varToType, indent+1, currentPkg)

			// Write fields if this node has a message
			if child.Message != nil {
				writeMessageFields(child.Message, sb, varToType, indent+1)
			}

			sb.WriteString(fmt.Sprintf("%s}\n\n", indentStr))
		}
	}
}

func writeMessageFields(msg *Message, sb *strings.Builder, varToType map[string]string, indent int) {
	indentStr := strings.Repeat("  ", indent)

	// Get the current message's path prefix for relative type resolution
	msgPath := msg.ShortName
	currentPkg := msg.Package

	// Group fields by oneof
	oneofGroups := make(map[string][]Field)
	var regularFields []Field

	for _, f := range msg.Fields {
		if f.Oneof != "" {
			oneofGroups[f.Oneof] = append(oneofGroups[f.Oneof], f)
		} else {
			regularFields = append(regularFields, f)
		}
	}

	// Write regular fields
	for _, f := range regularFields {
		fieldType := resolveFieldTypeWithPkg(f, varToType, msgPath, currentPkg)
		prefix := ""
		if f.Repeated {
			prefix = "repeated "
		} else if f.Opt {
			prefix = "optional "
		}
		sb.WriteString(fmt.Sprintf("%s%s%s %s = %d;\n", indentStr, prefix, fieldType, f.Name, f.No))
	}

	// Write oneof groups
	var oneofNames []string
	for name := range oneofGroups {
		oneofNames = append(oneofNames, name)
	}
	sort.Strings(oneofNames)

	for _, oneofName := range oneofNames {
		fields := oneofGroups[oneofName]
		sb.WriteString(fmt.Sprintf("%soneof %s {\n", indentStr, oneofName))
		for _, f := range fields {
			fieldType := resolveFieldTypeWithPkg(f, varToType, msgPath, currentPkg)
			sb.WriteString(fmt.Sprintf("%s  %s %s = %d;\n", indentStr, fieldType, f.Name, f.No))
		}
		sb.WriteString(fmt.Sprintf("%s}\n", indentStr))
	}
}

// parseTypeName extracts package and full nested path from type name
// "agent.v1.Foo" -> ("agent.v1", "Foo")
// "agent.v1.Foo.Bar" -> ("agent.v1", "Foo.Bar")
// "anyrun.v1.PodStatus" -> ("anyrun.v1", "PodStatus")
// "google.protobuf.Timestamp" -> ("google.protobuf", "Timestamp")
func parseTypeName(typeName string) (pkg, shortName string) {
	// Find pattern: xxx.v1.Rest or xxx.vN.Rest
	versionRe := regexp.MustCompile(`^([\w.]+\.v\d+)\.(.+)$`)
	if match := versionRe.FindStringSubmatch(typeName); match != nil {
		return match[1], match[2]
	}

	// Handle google.protobuf.XXX pattern
	if strings.HasPrefix(typeName, "google.protobuf.") {
		rest := strings.TrimPrefix(typeName, "google.protobuf.")
		return "google.protobuf", rest
	}

	// Handle google.rpc.XXX pattern
	if strings.HasPrefix(typeName, "google.rpc.") {
		rest := strings.TrimPrefix(typeName, "google.rpc.")
		return "google.rpc", rest
	}

	// Fallback: split at last dot
	parts := strings.Split(typeName, ".")
	if len(parts) > 1 {
		return strings.Join(parts[:len(parts)-1], "."), parts[len(parts)-1]
	}
	return "", typeName
}

// getNestedPath returns the path components for a nested type
// "Foo" -> ["Foo"]
// "Foo.Bar" -> ["Foo", "Bar"]
// "Foo.Bar.Baz" -> ["Foo", "Bar", "Baz"]
func getNestedPath(shortName string) []string {
	return strings.Split(shortName, ".")
}

func resolveFieldType(f Field, varToType map[string]string) string {
	return resolveFieldTypeWithPkg(f, varToType, "", "")
}

// resolveFieldTypeWithPkg resolves field type with package awareness
// parentPath is like "ConversationMessage" or "ConversationMessage.ToolResult"
// currentPkg is the package of the current message being written (e.g., "agent.v1")
func resolveFieldTypeWithPkg(f Field, varToType map[string]string, parentPath string, currentPkg string) string {
	if f.Kind == "scalar" {
		if t, ok := f.T.(int); ok {
			return scalarTypes[t]
		}
		if t, ok := f.T.(float64); ok {
			return scalarTypes[int(t)]
		}
	}

	if f.Kind == "message" || f.Kind == "enum" {
		if varName, ok := f.T.(string); ok {
			if typeName, exists := varToType[varName]; exists {
				// Get package and short name from full type name
				refPkg, shortName := parseTypeName(typeName)

				// If the type is nested under the same parent, use relative path
				if parentPath != "" && strings.HasPrefix(shortName, parentPath+".") {
					// ConversationMessage.CodeChunk -> CodeChunk (when inside ConversationMessage)
					return strings.TrimPrefix(shortName, parentPath+".")
				}

				// If same package, use short name only
				if refPkg == currentPkg {
					return shortName
				}

				// Check if this type was copied to current package (circular import resolution)
				if copied := copiedTypes[currentPkg]; copied != nil {
					if _, isCopied := copied[shortName]; isCopied {
						// This type exists locally as a copy, use short name
						return shortName
					}
				}

				// For cross-package references, use full type name
				if refPkg != "" {
					return refPkg + "." + shortName
				}

				return shortName
			}
			if f.Kind == "enum" {
				return "int32"
			}
			return "bytes"
		}
	}

	if f.Kind == "map" {
		// Handle map types: map<KeyType, ValueType>
		keyType := scalarTypes[f.MapKey]
		if keyType == "" {
			keyType = "string" // default
		}

		var valueType string
		if f.MapValueKind == "scalar" {
			if t, ok := f.MapValueT.(int); ok {
				valueType = scalarTypes[t]
			} else if t, ok := f.MapValueT.(float64); ok {
				valueType = scalarTypes[int(t)]
			}
		} else if f.MapValueKind == "message" {
			if varName, ok := f.MapValueT.(string); ok {
				if typeName, exists := varToType[varName]; exists {
					refPkg, shortName := parseTypeName(typeName)
					if refPkg == currentPkg {
						valueType = shortName
					} else if copied := copiedTypes[currentPkg]; copied != nil {
						// Check if this type was copied to current package
						if _, isCopied := copied[shortName]; isCopied {
							valueType = shortName
						} else if refPkg != "" {
							valueType = refPkg + "." + shortName
						} else {
							valueType = shortName
						}
					} else if refPkg != "" {
						valueType = refPkg + "." + shortName
					} else {
						valueType = shortName
					}
				} else {
					valueType = "bytes"
				}
			}
		}
		if valueType == "" {
			valueType = "bytes"
		}

		return fmt.Sprintf("map<%s, %s>", keyType, valueType)
	}

	return "bytes" // fallback
}
