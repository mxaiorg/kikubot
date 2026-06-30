package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"kikubot/internal/config"
)

// toolInfo is one entry in the tool registry as exposed to the dashboard.
type toolInfo struct {
	Key         string // key in registry map (e.g. "report")
	Description string // joined Description from all ToolDefinitions returned
	Private     bool   // registered dynamically from internal/tools_priv (present only when private source is deployed)
	MCP         bool   // declared in configs/mcp_servers.yaml (remote MCP server, registered at runtime — not in the static registry literal)
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

	// Private tools (internal/tools_priv) aren't in the static registry literal —
	// they self-register at runtime via tools.Register(...) from an init(). Read
	// those calls statically too so the picker can offer them, flagged private.
	seen := make(map[string]bool, len(infos))
	for _, i := range infos {
		seen[i.Key] = true
	}
	privKeys, privDesc := collectPrivateRegistrations(filepath.Join(root, "internal", "tools_priv"))
	for _, k := range privKeys {
		if seen[k] {
			continue
		}
		seen[k] = true
		infos = append(infos, toolInfo{Key: k, Description: privDesc[k], Private: true})
	}

	// Remote MCP servers (configs/mcp_servers.yaml) aren't in the static
	// registry literal either — they're registered at runtime from the
	// declarative catalog. Surface them so the picker can offer them, flagged
	// MCP. Assigning one still requires its mcp_servers.yaml entry plus
	// credentials (and, for oauth2, a seeded token file); the badge signals
	// that extra setup. A missing/unreadable file just yields no MCP tools.
	mcpServers, mcpErr := config.LoadMCPServers(filepath.Join(root, "configs", "mcp_servers.yaml"))
	if mcpErr != nil {
		log.Printf("configurator: cannot read mcp_servers.yaml: %v", mcpErr)
	}
	for _, s := range mcpServers {
		k := strings.TrimSpace(s.Key)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		infos = append(infos, toolInfo{Key: k, Description: s.Description, MCP: true})
	}
	return infos, nil
}

// collectPrivateRegistrations statically scans a directory for
// `tools.Register("key", factory, "description")` calls (typically inside an
// init() in package toolspriv) and returns the keys in sorted order plus any
// literal descriptions. A missing directory yields nothing — a public checkout
// simply has no private tools to surface.
//
// Each .go file is parsed independently (test files are ignored): a single
// unparseable file is logged and skipped rather than wiping out discovery for
// the whole directory. The final tally is logged so a deployment that finds no
// private tools — despite files being present — is visible in the logs instead
// of failing silently.
func collectPrivateRegistrations(dir string) (keys []string, desc map[string]string) {
	desc = map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("configurator: cannot read private tools dir %s: %v", dir, err)
		}
		return nil, desc
	}

	fset := token.NewFileSet()
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// Don't let one broken file blank the entire private list.
			log.Printf("configurator: skipping unparseable private tool file %s: %v", path, perr)
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Register" {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "tools" {
				return true
			}
			if len(call.Args) < 1 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			key, uErr := strconv.Unquote(lit.Value)
			if uErr != nil || key == "" {
				return true
			}
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
			if len(call.Args) >= 3 {
				if d := stringExpr(call.Args[2]); d != "" {
					desc[key] = d
				}
			}
			return true
		})
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		log.Printf("configurator: no private tool registrations found in %s", dir)
	} else {
		log.Printf("configurator: discovered %d private tool(s) in %s: %v", len(keys), dir, keys)
	}
	return keys, desc
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
