package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// envEntry represents one logical entry in a .env file:
//   - a comment / blank line (Key == ""), preserved verbatim via Lines
//   - a key=value pair (Key != ""), possibly commented out, possibly spanning
//     multiple physical lines (quoted value with embedded newlines).
type envEntry struct {
	Lines     []string
	Key       string
	Value     string
	Quoted    bool
	Commented bool
}

// envFile is an ordered, comment-preserving model of a .env file.
type envFile struct {
	Entries []envEntry
}

func loadEnvFile(path string) (*envFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseEnv(string(b)), nil
}

// loadEnvWithFallback reads `path`; if missing, reads `fallback`. Returns an
// empty envFile if neither exists (no error).
func loadEnvWithFallback(path, fallback string) (*envFile, error) {
	if _, err := os.Stat(path); err == nil {
		return loadEnvFile(path)
	}
	if fallback != "" {
		if _, err := os.Stat(fallback); err == nil {
			return loadEnvFile(fallback)
		}
	}
	return &envFile{}, nil
}

func parseEnv(s string) *envFile {
	f := &envFile{}
	lines := strings.Split(s, "\n")
	// strip a trailing empty line that comes from a final \n
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			f.Entries = append(f.Entries, envEntry{Lines: []string{line}})
			i++
			continue
		}

		commented := false
		scan := trimmed
		if strings.HasPrefix(trimmed, "#") {
			after := strings.TrimSpace(trimmed[1:])
			if eq := strings.Index(after, "="); eq > 0 {
				k := strings.TrimSpace(after[:eq])
				if isValidKey(k) {
					commented = true
					scan = after
				}
			}
			if !commented {
				f.Entries = append(f.Entries, envEntry{Lines: []string{line}})
				i++
				continue
			}
		}

		eq := strings.Index(scan, "=")
		if eq <= 0 {
			f.Entries = append(f.Entries, envEntry{Lines: []string{line}})
			i++
			continue
		}
		key := strings.TrimSpace(scan[:eq])
		rest := scan[eq+1:]

		ent := envEntry{Key: key, Commented: commented, Lines: []string{line}}
		if strings.HasPrefix(rest, "\"") {
			ent.Quoted = true
			body := rest[1:]
			if idx := closingDQuote(body); idx >= 0 {
				ent.Value = unescapeDQ(body[:idx])
			} else {
				// multi-line quoted value
				parts := []string{body}
				i++
				for i < len(lines) {
					nx := lines[i]
					ent.Lines = append(ent.Lines, nx)
					if idx := closingDQuote(nx); idx >= 0 {
						parts = append(parts, nx[:idx])
						break
					}
					parts = append(parts, nx)
					i++
				}
				ent.Value = unescapeDQ(strings.Join(parts, "\n"))
			}
		} else if strings.HasPrefix(rest, "'") {
			ent.Quoted = true
			body := rest[1:]
			if idx := strings.Index(body, "'"); idx >= 0 {
				ent.Value = body[:idx]
			} else {
				ent.Value = body
			}
		} else {
			// strip trailing inline comment? .env files don't reliably
			// support trailing comments; treat the whole rest as the value.
			ent.Value = strings.TrimSpace(rest)
		}
		f.Entries = append(f.Entries, ent)
		i++
	}
	return f
}

func closingDQuote(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == '"' {
			return i
		}
	}
	return -1
}

func unescapeDQ(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte(s[i+1])
			}
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isValidKey(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

// Get returns the live (non-commented) value for key, or "" if missing/commented.
func (f *envFile) Get(key string) (string, bool) {
	for _, e := range f.Entries {
		if e.Key == key && !e.Commented {
			return e.Value, true
		}
	}
	return "", false
}

// GetAny returns the value for key whether commented or not.
func (f *envFile) GetAny(key string) (value string, commented bool, ok bool) {
	for _, e := range f.Entries {
		if e.Key == key {
			return e.Value, e.Commented, true
		}
	}
	return "", false, false
}

func (f *envFile) Has(key string) bool {
	for _, e := range f.Entries {
		if e.Key == key {
			return true
		}
	}
	return false
}

// Set writes key=value into the file, uncommenting if the existing entry was
// commented. If key is missing it is appended.
func (f *envFile) Set(key, value string) {
	for i := range f.Entries {
		if f.Entries[i].Key == key {
			f.Entries[i].Value = value
			f.Entries[i].Commented = false
			f.Entries[i].Lines = []string{formatVar(key, value, false)}
			return
		}
	}
	f.Entries = append(f.Entries, envEntry{
		Key:   key,
		Value: value,
		Lines: []string{formatVar(key, value, false)},
	})
}

// Delete removes a key entirely.
func (f *envFile) Delete(key string) {
	out := f.Entries[:0]
	for _, e := range f.Entries {
		if e.Key == key {
			continue
		}
		out = append(out, e)
	}
	f.Entries = out
}

// Comment converts an existing live entry into a commented one. No-op if
// missing or already commented.
func (f *envFile) Comment(key string) {
	for i := range f.Entries {
		if f.Entries[i].Key == key && !f.Entries[i].Commented {
			f.Entries[i].Commented = true
			f.Entries[i].Lines = []string{formatVar(key, f.Entries[i].Value, true)}
			return
		}
	}
}

// Render emits the file back to text.
func (f *envFile) Render() string {
	var b strings.Builder
	for i, e := range f.Entries {
		if i > 0 {
			b.WriteByte('\n')
		}
		for j, ln := range e.Lines {
			if j > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(ln)
		}
	}
	b.WriteByte('\n')
	return b.String()
}

func (f *envFile) Save(path string) error {
	return os.WriteFile(path, []byte(f.Render()), 0o644)
}

// Keys returns the set of declared (live or commented) keys, in declaration order.
func (f *envFile) Keys() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range f.Entries {
		if e.Key == "" || seen[e.Key] {
			continue
		}
		seen[e.Key] = true
		out = append(out, e.Key)
	}
	return out
}

// SortedKeys returns keys alphabetically.
func (f *envFile) SortedKeys() []string {
	k := f.Keys()
	sort.Strings(k)
	return k
}

func formatVar(key, value string, commented bool) string {
	prefix := ""
	if commented {
		prefix = "#"
	}
	if needsQuoting(value) {
		escaped := strings.ReplaceAll(value, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		// Allow literal newlines (do NOT escape \n) — kikubot env loader
		// supports them, and SYSTEM_PROMPT relies on this.
		return fmt.Sprintf("%s%s=\"%s\"", prefix, key, escaped)
	}
	return fmt.Sprintf("%s%s=%s", prefix, key, value)
}

func needsQuoting(s string) bool {
	if s == "" {
		return false
	}
	return strings.ContainsAny(s, " \t#\"'\n=,")
}
