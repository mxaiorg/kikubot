// kikudoctor validates a Kikubot deployment by checking agents.yaml,
// configs/env/, the tools registry, and docker-compose.yml.
//
// Run from the kikubot project root:
//
//	go run ./scripts/kikudoctor
//
// Or point at any deployment:
//
//	go run ./scripts/kikudoctor -root /path/to/kikubot
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	root := flag.String("root", ".", "path to the kikubot project root")
	noColor := flag.Bool("no-color", false, "disable ANSI colour in output")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "kikudoctor — validate a Kikubot deployment\n\nUsage:\n  %s [flags]\n\nFlags:\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
	flag.Parse()

	abs, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error resolving root path: %v\n", err)
		os.Exit(2)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "root path %q is not a directory\n", abs)
		os.Exit(2)
	}

	r := NewReport(*noColor)
	r.Header(abs)

	cf := parseCompose(abs)
	localAccounts := localAgentAccounts(cf)

	af, _ := loadAgents(abs, r)
	keys, _ := loadRegistryKeys(abs, r)
	if af != nil && keys != nil {
		validateAgentTools(af, keys, localAccounts, r)
	}
	validateEnvFiles(abs, af, r)
	validateCompose(abs, cf, r)

	r.Summary()
	if r.HasFailures() {
		os.Exit(1)
	}
}

// relPath returns p relative to root if possible, else p unchanged.
func relPath(root, p string) string {
	if rel, err := filepath.Rel(root, p); err == nil {
		return rel
	}
	return p
}
