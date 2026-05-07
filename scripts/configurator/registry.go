package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
)

// toolInfo is one entry in the tool registry as exposed to the dashboard.
type toolInfo struct {
	Key         string // key in registry map (e.g. "report")
	Description string // joined Description from all ToolDefinitions returned
}

// loadToolRegistry parses internal/tools/*.go on demand and returns the
// ordered list of registry keys plus their descriptions. Reading the source
// at request time means descriptions stay in sync with the codebase.
//
// Limitations: factories that build their Description from a non-literal
// expression (e.g. `Description: desc`) emit an empty description; the chip
// still works, just with no tooltip.
func loadToolRegistry(root string) ([]toolInfo, error) {
	dir := filepath.Join(root, "internal", "tools")
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", dir, err)
	}

	var (
		orderedKeys []string                // declared order in the registry literal
		keyFn       = map[string]string{}   // registry key -> factory function name
		keyOverride = map[string]string{}   // registry key -> author-supplied summary
		fnLits      = map[string][]string{} // function name -> Description literals found in body
		fnCalls     = map[string][]string{} // function name -> functions it calls (same package)
		fnDoc       = map[string]string{}   // function doc comment fallback
	)

	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			collectRegistry(file, &orderedKeys, keyFn)
			collectStringMap(file, "registryDescriptions", keyOverride)
			collectFunctions(file, fnLits, fnCalls, fnDoc)
		}
	}

	infos := make([]toolInfo, 0, len(orderedKeys))
	for _, k := range orderedKeys {
		fn := keyFn[k]
		var desc string
		switch {
		case keyOverride[k] != "":
			// 1. Author-supplied summary in registryDescriptions — preferred
			//    when present; concise and tool-agnostic.
			desc = keyOverride[k]
		default:
			descs := resolveDescriptions(fn, fnLits, fnCalls, map[string]bool{})
			if len(descs) > 0 {
				// 2. Description literals inside the factory body — verbatim
				//    from the runtime tool definitions.
				desc = strings.Join(descs, " | ")
			} else {
				// 3. Factory function doc comment — last-resort hint.
				desc = fnDoc[fn]
			}
		}
		infos = append(infos, toolInfo{Key: k, Description: desc})
	}
	return infos, nil
}

// collectStringMap finds `var <name> = map[string]string{...}` in a file and
// loads the literal entries into `out`. Non-literal values are skipped.
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

// resolveDescriptions collects Description literals from `fn` and from every
// same-package function it transitively calls. Call cycles are bounded by `seen`.
func resolveDescriptions(fn string, fnLits, fnCalls map[string][]string, seen map[string]bool) []string {
	if fn == "" || seen[fn] {
		return nil
	}
	seen[fn] = true
	out := append([]string(nil), fnLits[fn]...)
	for _, called := range fnCalls[fn] {
		out = append(out, resolveDescriptions(called, fnLits, fnCalls, seen)...)
	}
	return out
}

// collectRegistry walks a file looking for `var registry = map[string]toolFactory{...}`
// and records each entry in declaration order.
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

// factoryName returns the factory function name behind a registry value,
// unwrapping `wrap(Fn)` if present.
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

// collectFunctions walks every top-level function in `file`, recording:
//   - the Description: "…" literals inside the body (`fnLits`)
//   - same-package function calls (so we can resolve transitive descriptions
//     when a factory delegates to helpers, e.g. HelpjuiceTools → helpjuiceFAQRead)
//   - the doc comment as a fallback hint (`fnDoc`)
func collectFunctions(file *ast.File, fnLits, fnCalls map[string][]string, fnDoc map[string]string) {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		name := fn.Name.Name
		if fn.Doc != nil {
			fnDoc[name] = strings.TrimSpace(fn.Doc.Text())
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if cl, ok := n.(*ast.CompositeLit); ok && isToolDefinitionLit(cl) {
				for _, el := range cl.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					kid, ok := kv.Key.(*ast.Ident)
					if !ok || kid.Name != "Description" {
						continue
					}
					if v := stringExpr(kv.Value); v != "" {
						fnLits[name] = append(fnLits[name], v)
					}
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

// stringExpr resolves an expression to its string-literal value if possible.
// Supports plain string literals, raw strings, and `"a" + "b"` concatenation.
// Returns "" when the value isn't a literal (e.g. variable reference).
func stringExpr(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			s, err := strconv.Unquote(v.Value)
			if err != nil {
				return ""
			}
			return s
		}
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			return stringExpr(v.X) + stringExpr(v.Y)
		}
	}
	return ""
}
