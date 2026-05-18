// tools_list scans internal/tools/*.go and emits a JSON list of every tool
// available to a kikubot agent — both the always-on core tools and each
// registry-configurable tool key. Other processes (deployment scripts, docs
// generators, tool discovery in agent tooling) consume the JSON.
//
// Each entry contains:
//   - key:         the agents.yaml registry key ("" for core tools)
//   - name:        the ToolDefinition.Name from the source
//   - description: the ToolDefinition.Description (or registryDescriptions
//     override when the factory builds tools dynamically)
//   - core:        true for tools loaded unconditionally by CoreTools()
//
// Approach mirrors scripts/configurator/registry.go — AST-walk the package
// rather than importing it, so this stays a zero-dependency standalone build.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

type toolEntry struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Core        bool   `json:"core,omitempty"`
}

// toolPair is a (Name, Description) pair pulled from one ToolDefinition literal.
type toolPair struct {
	Name        string
	Description string
}

func main() {
	var (
		root   string
		output string
	)
	flag.StringVar(&root, "root", "", "kikubot repo root (defaults to repo root relative to this file)")
	flag.StringVar(&output, "o", "", "output file (default: stdout)")
	flag.Parse()

	if root == "" {
		root = defaultRoot()
	}

	entries, err := buildEntries(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	buf, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal:", err)
		os.Exit(1)
	}
	buf = append(buf, '\n')

	if output == "" {
		os.Stdout.Write(buf)
		return
	}
	if err := os.WriteFile(output, buf, 0644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
}

func defaultRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func buildEntries(root string) ([]toolEntry, error) {
	dir := filepath.Join(root, "internal", "tools")
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", dir, err)
	}

	var (
		orderedKeys []string
		keyFn       = map[string]string{}
		keyOverride = map[string]string{}
		fnPairs     = map[string][]toolPair{}
		fnCalls     = map[string][]string{}
		coreFn      = "CoreTools"
	)

	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			collectRegistry(file, &orderedKeys, keyFn)
			collectStringMap(file, "registryDescriptions", keyOverride)
			collectFunctions(file, fnPairs, fnCalls)
		}
	}

	var entries []toolEntry

	for _, pair := range resolvePairs(coreFn, fnPairs, fnCalls, map[string]bool{}) {
		entries = append(entries, toolEntry{
			Name:        pair.Name,
			Description: pair.Description,
			Core:        true,
		})
	}

	for _, key := range orderedKeys {
		pairs := resolvePairs(keyFn[key], fnPairs, fnCalls, map[string]bool{})
		if len(pairs) == 0 {
			// Dynamic factory (typically MCP bridge) — fall back to override.
			entries = append(entries, toolEntry{
				Key:         key,
				Description: keyOverride[key],
			})
			continue
		}
		for _, pair := range pairs {
			desc := pair.Description
			if desc == "" {
				desc = keyOverride[key]
			}
			entries = append(entries, toolEntry{
				Key:         key,
				Name:        pair.Name,
				Description: desc,
			})
		}
	}

	return entries, nil
}

// resolvePairs gathers (Name, Description) pairs from `fn` and the same-package
// functions it transitively calls. Cycles are bounded by `seen`.
func resolvePairs(fn string, fnPairs map[string][]toolPair, fnCalls map[string][]string, seen map[string]bool) []toolPair {
	if fn == "" || seen[fn] {
		return nil
	}
	seen[fn] = true
	out := append([]toolPair(nil), fnPairs[fn]...)
	for _, called := range fnCalls[fn] {
		out = append(out, resolvePairs(called, fnPairs, fnCalls, seen)...)
	}
	return out
}

func collectRegistry(file *ast.File, ordered *[]string, keyFn map[string]string) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if name.Name != "registry" || i >= len(vs.Values) {
					continue
				}
				cl, ok := vs.Values[i].(*ast.CompositeLit)
				if !ok {
					continue
				}
				for _, elt := range cl.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					lit, ok := kv.Key.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					key, err := strconv.Unquote(lit.Value)
					if err != nil {
						continue
					}
					fn := factoryName(kv.Value)
					if fn == "" {
						continue
					}
					*ordered = append(*ordered, key)
					keyFn[key] = fn
				}
			}
		}
	}
}

func factoryName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.CallExpr:
		if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "wrap" && len(v.Args) > 0 {
			if a, ok := v.Args[0].(*ast.Ident); ok {
				return a.Name
			}
		}
	}
	return ""
}

func collectStringMap(file *ast.File, name string, out map[string]string) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, ident := range vs.Names {
				if ident.Name != name || i >= len(vs.Values) {
					continue
				}
				cl, ok := vs.Values[i].(*ast.CompositeLit)
				if !ok {
					continue
				}
				for _, elt := range cl.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					klit, ok := kv.Key.(*ast.BasicLit)
					if !ok || klit.Kind != token.STRING {
						continue
					}
					key, err := strconv.Unquote(klit.Value)
					if err != nil {
						continue
					}
					if v := stringExpr(kv.Value); v != "" {
						out[key] = v
					}
				}
			}
		}
	}
}

// collectFunctions records, for each top-level function:
//   - the (Name, Description) pairs from any ToolDefinition composite literal
//     in its body (one pair per literal — the configurator's version tracked
//     only Description, but tool discovery callers need both)
//   - the same-package function calls it makes, so factories that delegate to
//     helpers (e.g. HelpjuiceTools → helpjuiceFAQRead) still yield pairs
func collectFunctions(file *ast.File, fnPairs map[string][]toolPair, fnCalls map[string][]string) {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		name := fn.Name.Name
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if cl, ok := n.(*ast.CompositeLit); ok && isToolDefinitionLit(cl) {
				var pair toolPair
				for _, el := range cl.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					kid, ok := kv.Key.(*ast.Ident)
					if !ok {
						continue
					}
					v := stringExpr(kv.Value)
					if v == "" {
						continue
					}
					switch kid.Name {
					case "Name":
						pair.Name = v
					case "Description":
						pair.Description = v
					}
				}
				if pair.Name != "" || pair.Description != "" {
					fnPairs[name] = append(fnPairs[name], pair)
				}
			}
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok {
					fnCalls[name] = append(fnCalls[name], id.Name)
				}
			}
			return true
		})
	}
}

func isToolDefinitionLit(cl *ast.CompositeLit) bool {
	switch t := cl.Type.(type) {
	case *ast.Ident:
		return t.Name == "ToolDefinition"
	case *ast.SelectorExpr:
		return t.Sel.Name == "ToolDefinition"
	}
	return false
}

// stringExpr resolves to a string-literal value if possible. Supports plain
// strings, raw strings, and `"a" + "b"` concatenation. Returns "" when any
// operand is non-literal (e.g. `cfg.ServerName + "__"` — the MCP bridges build
// names dynamically and partial output would surface garbage like `"__"`).
func stringExpr(e ast.Expr) string {
	s, _ := stringExprOK(e)
	return s
}

func stringExprOK(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			s, err := strconv.Unquote(v.Value)
			if err != nil {
				return "", false
			}
			return s, true
		}
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			a, oka := stringExprOK(v.X)
			b, okb := stringExprOK(v.Y)
			if !oka || !okb {
				return "", false
			}
			return a + b, true
		}
	}
	return "", false
}
