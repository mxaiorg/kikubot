package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// validateNpxPackages walks the tools package for `Command: "npx"` literals
// reachable from the factory function of each used tool, then confirms each
// referenced npm package is pre-installed in the Dockerfile. A missing
// pre-install is a warning (the agent will still work — it'll just pay the
// npx download cost on every cold start).
func validateNpxPackages(root string, af *AgentsFile, localAccounts map[string]bool, factoryNames map[string]string, r *Report) {
	sec := r.Section("npx-backed tools are pre-installed in Dockerfile")

	if af == nil || len(factoryNames) == 0 {
		sec.Warn("nothing to check (no agents or no parsed registry)")
		return
	}

	// Tool keys actually used by locally-deployed agents.
	used := map[string]bool{}
	for _, a := range af.Agents {
		if !localAccounts[emailAccount(a.Email)] {
			continue
		}
		for _, t := range a.Tools {
			if _, ok := factoryNames[t]; ok {
				used[t] = true
			}
		}
	}
	if len(used) == 0 {
		sec.Pass("no locally-used tools resolve through the registry")
		return
	}

	pkgInfo, err := loadToolsPackage(root)
	if err != nil {
		sec.Fail("cannot parse internal/tools package: %v", err)
		return
	}

	// For each used tool key, gather npx packages reachable from its factory.
	toolPkgs := map[string][]string{}
	allPkgs := map[string]bool{}
	for key := range used {
		fn := factoryNames[key]
		if fn == "" {
			continue
		}
		pkgs := reachableNpxPackages(pkgInfo, fn)
		if len(pkgs) == 0 {
			continue
		}
		toolPkgs[key] = pkgs
		for _, p := range pkgs {
			allPkgs[p] = true
		}
	}

	if len(toolPkgs) == 0 {
		sec.Pass("no npx-backed tools used by locally-deployed agents")
		return
	}

	dockerPath := filepath.Join(root, "Dockerfile")
	installed, dockerErr := dockerfileGlobalNpmPackages(dockerPath)
	if dockerErr != nil {
		sec.Fail("cannot read Dockerfile: %v", dockerErr)
		return
	}

	missing := 0
	keys := make([]string, 0, len(toolPkgs))
	for k := range toolPkgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		pkgs := toolPkgs[key]
		sort.Strings(pkgs)
		for _, pkg := range pkgs {
			if installed[pkg] {
				sec.Pass("tool %q npx package %s is pre-installed", key, pkg)
				continue
			}
			sec.Warn("tool %q uses npx package %s which is not pre-installed in Dockerfile — add: RUN npm install --global %s",
				key, pkg, pkg)
			missing++
		}
	}
	_ = missing
}

// toolsPackage is a collection of parsed *.go files from internal/tools/.
type toolsPackage struct {
	funcs map[string]*ast.FuncDecl
}

func loadToolsPackage(root string) (*toolsPackage, error) {
	dir := filepath.Join(root, "internal", "tools")
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	tp := &toolsPackage{funcs: map[string]*ast.FuncDecl{}}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil {
					tp.funcs[fd.Name.Name] = fd
				}
			}
		}
	}
	return tp, nil
}

// reachableNpxPackages returns npm package names extracted from any
// `Command: "npx"` composite literal in `start` or in functions transitively
// called from `start` (within the tools package). Cycles and unknown callees
// are skipped.
func reachableNpxPackages(tp *toolsPackage, start string) []string {
	visited := map[string]bool{}
	pkgs := map[string]bool{}
	var visit func(name string)
	visit = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		fd, ok := tp.funcs[name]
		if !ok || fd.Body == nil {
			return
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				if pkg, found := npxPackageFromCompositeLit(x); found {
					pkgs[pkg] = true
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok {
					if _, known := tp.funcs[id.Name]; known {
						visit(id.Name)
					}
				}
			}
			return true
		})
	}
	visit(start)

	out := make([]string, 0, len(pkgs))
	for p := range pkgs {
		out = append(out, p)
	}
	return out
}

// npxPackageFromCompositeLit returns the npm package name if cl assigns
// Command:"npx" alongside a BaseArgs/Args slice whose first non-flag entry
// is the package. The first entry that doesn't begin with "-" is the package.
func npxPackageFromCompositeLit(cl *ast.CompositeLit) (string, bool) {
	var commandIsNpx bool
	var argsLit *ast.CompositeLit
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "Command":
			if v, ok := stringLitValue(kv.Value); ok && v == "npx" {
				commandIsNpx = true
			}
		case "BaseArgs", "Args":
			if cl2, ok := kv.Value.(*ast.CompositeLit); ok {
				argsLit = cl2
			}
		}
	}
	if !commandIsNpx || argsLit == nil {
		return "", false
	}
	for _, e := range argsLit.Elts {
		s, ok := stringLitValue(e)
		if !ok {
			continue
		}
		if strings.HasPrefix(s, "-") {
			continue
		}
		// strip a trailing @version (e.g. @xeroapi/xero-mcp-server@latest).
		// We compare against the form used in `npm install --global <pkg>`,
		// which usually omits the version tag.
		base := s
		if idx := strings.LastIndex(s, "@"); idx > 0 {
			base = s[:idx]
		}
		return base, true
	}
	return "", false
}

func stringLitValue(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return v, true
}

// dockerfileGlobalNpmPackages returns the set of packages installed via
// `npm install -g|--global <pkg>` (or `npm i -g <pkg>`) on uncommented RUN
// lines of the Dockerfile. Versions/tags are stripped so comparisons work
// regardless of whether the source spec includes an @version tag.
func dockerfileGlobalNpmPackages(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Tokenise the rest of the line; require "npm install" or "npm i"
		// AND a global flag.
		fields := strings.Fields(line)
		idx := -1
		for i, f := range fields {
			if f == "npm" && i+1 < len(fields) && (fields[i+1] == "install" || fields[i+1] == "i") {
				idx = i + 2
				break
			}
		}
		if idx < 0 {
			continue
		}
		hasGlobal := false
		var pkgs []string
		for j := idx; j < len(fields); j++ {
			tok := fields[j]
			switch {
			case tok == "--global" || tok == "-g":
				hasGlobal = true
			case strings.HasPrefix(tok, "-"):
				// other npm flag, ignore
			case tok == "&&" || tok == "\\":
				// shell continuation, stop parsing this segment
				goto done
			default:
				pkgs = append(pkgs, tok)
			}
		}
	done:
		if !hasGlobal {
			continue
		}
		for _, p := range pkgs {
			base := p
			if idx := strings.LastIndex(p, "@"); idx > 0 {
				base = p[:idx]
			}
			out[base] = true
		}
	}
	return out, nil
}
