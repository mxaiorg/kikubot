package main

import (
	"fmt"
	"os"
)

const (
	colReset  = "\033[0m"
	colGreen  = "\033[32m"
	colRed    = "\033[31m"
	colYellow = "\033[33m"
	colBold   = "\033[1m"
	colDim    = "\033[2m"
)

type Report struct {
	color    bool
	failures int
	warnings int
	passes   int
}

func NewReport(noColor bool) *Report {
	color := !noColor
	if os.Getenv("NO_COLOR") != "" {
		color = false
	}
	return &Report{color: color}
}

func (r *Report) c(code, s string) string {
	if !r.color {
		return s
	}
	return code + s + colReset
}

func (r *Report) Header(root string) {
	fmt.Println(r.c(colBold, "kikudoctor"))
	fmt.Printf("  root: %s\n\n", r.c(colDim, root))
}

type section struct {
	r     *Report
	title string
	shown bool
}

func (r *Report) Section(title string) *section {
	return &section{r: r, title: title}
}

func (s *section) ensureHeader() {
	if s.shown {
		return
	}
	fmt.Printf("%s %s\n", s.r.c(colBold, "▸"), s.title)
	s.shown = true
}

func (s *section) Pass(format string, args ...any) {
	s.ensureHeader()
	fmt.Printf("    %s %s\n", s.r.c(colGreen, "✓"), fmt.Sprintf(format, args...))
	s.r.passes++
}

func (s *section) Fail(format string, args ...any) {
	s.ensureHeader()
	fmt.Printf("    %s %s\n", s.r.c(colRed, "✗"), fmt.Sprintf(format, args...))
	s.r.failures++
}

func (s *section) Warn(format string, args ...any) {
	s.ensureHeader()
	fmt.Printf("    %s %s\n", s.r.c(colYellow, "!"), fmt.Sprintf(format, args...))
	s.r.warnings++
}

func (r *Report) Summary() {
	fmt.Println()
	switch {
	case r.failures > 0:
		fmt.Printf("%s  %d passed, %d warning(s), %d failure(s)\n",
			r.c(colRed, "FAIL"), r.passes, r.warnings, r.failures)
	case r.warnings > 0:
		fmt.Printf("%s  %d passed, %d warning(s)\n",
			r.c(colYellow, "OK  "), r.passes, r.warnings)
	default:
		fmt.Printf("%s  %d checks passed\n", r.c(colGreen, "OK  "), r.passes)
	}
}

func (r *Report) HasFailures() bool { return r.failures > 0 }
