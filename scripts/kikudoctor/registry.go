package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
)

// loadRegistry parses internal/tools/registry.go and returns the set of
// string keys, plus a key → factory function name map (peeling `wrap(X)` to X).
func loadRegistry(root string, r *Report) (map[string]bool, map[string]string, bool) {
	path := filepath.Join(root, "internal", "tools", "registry.go")
	sec := r.Section(fmt.Sprintf("tool registry (%s)", relPath(root, path)))

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		sec.Fail("cannot parse: %v", err)
		return nil, nil, false
	}

	keys := map[string]bool{}
	factories := map[string]string{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			var match bool
			for _, n := range vs.Names {
				if n.Name == "registry" {
					match = true
					break
				}
			}
			if !match {
				continue
			}
			for _, val := range vs.Values {
				cl, ok := val.(*ast.CompositeLit)
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
					unquoted, err := strconv.Unquote(lit.Value)
					if err != nil {
						continue
					}
					keys[unquoted] = true
					if name := factoryName(kv.Value); name != "" {
						factories[unquoted] = name
					}
				}
			}
		}
	}

	if len(keys) == 0 {
		sec.Fail("no keys found in registry map")
		return keys, factories, false
	}
	sec.Pass("found %d registered tool key(s): %s", len(keys), formatSortedSet(keys))
	return keys, factories, true
}

// factoryName extracts the leaf factory function name from a registry value.
// Handles bare identifiers (`MxMCP`) and `wrap(X)` calls — both common in the
// existing registry. Anything more elaborate is reported as "" and skipped by
// downstream checks.
func factoryName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.CallExpr:
		if id, ok := e.Fun.(*ast.Ident); ok && id.Name == "wrap" && len(e.Args) == 1 {
			return factoryName(e.Args[0])
		}
	}
	return ""
}

// validateAgentTools verifies that each tool listed by a locally-deployed
// agent maps to a key in internal/tools/registry.go. Agents not deployed on
// this machine (no matching service in docker-compose.yml) are skipped, since
// they may run binaries on other machines with different tool sets.
func validateAgentTools(af *AgentsFile, keys, localAccounts map[string]bool, r *Report) {
	sec := r.Section("agent tools resolve to registered tools (locally-deployed agents)")
	if af == nil || len(af.Agents) == 0 {
		sec.Fail("no agents to validate")
		return
	}
	if len(keys) == 0 {
		sec.Fail("no registered tools available to validate against")
		return
	}

	checked := 0
	skipped := 0
	bad := 0
	for _, a := range af.Agents {
		acct := emailAccount(a.Email)
		if !localAccounts[acct] {
			skipped++
			continue
		}
		checked++
		for _, t := range a.Tools {
			if !keys[t] {
				sec.Fail("agent %q references unknown tool %q", a.Name, t)
				bad++
			}
		}
	}

	if checked == 0 {
		sec.Warn("no agents from agents.yaml are deployed locally — nothing to validate")
		return
	}
	if bad == 0 {
		if skipped > 0 {
			sec.Pass("all tools resolve for %d locally-deployed agent(s); skipped %d remote agent(s)", checked, skipped)
		} else {
			sec.Pass("all tools resolve for %d locally-deployed agent(s)", checked)
		}
	}
}

func formatSortedSet(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	s := ""
	for i, k := range out {
		if i > 0 {
			s += ", "
		}
		s += k
	}
	return s
}
