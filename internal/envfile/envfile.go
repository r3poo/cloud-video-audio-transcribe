// Package envfile reads a minimal .env file without pulling in a dependency.
//
// Supported syntax is deliberately small: KEY=VALUE lines, # comments, an
// optional "export " prefix, and single or double quoted values. Real process
// environment variables always win over values loaded from a file.
package envfile

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Values holds the key/value pairs parsed from one or more .env files. When
// several files are loaded, values from earlier files win.
type Values struct {
	values map[string]string
	// Paths lists the files that were actually read, in load order.
	Paths []string
}

// Get resolves key from the process environment first, then from the loaded
// files. It returns "" when the key is not set anywhere.
func (v *Values) Get(key string) string {
	if s := os.Getenv(key); s != "" {
		return s
	}
	if v == nil {
		return ""
	}
	return v.values[key]
}

// Has reports whether key is set by the environment or by a loaded file.
func (v *Values) Has(key string) bool {
	return v.Get(key) != ""
}

// Source returns the file that provided key, or "environment" when the value
// came from the process environment. It returns "" when the key is not set.
func (v *Values) Source(key string) string {
	if os.Getenv(key) != "" {
		return "environment"
	}
	if v == nil {
		return ""
	}
	if _, ok := v.values[key]; !ok {
		return ""
	}
	if len(v.Paths) > 0 {
		return v.Paths[0]
	}
	return ".env"
}

// Parse reads KEY=VALUE pairs from r.
func Parse(r io.Reader) (map[string]string, error) {
	out := make(map[string]string)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 8*1024), 1024*1024)

	for line := 1; sc.Scan(); line++ {
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(s, "export "); ok {
			s = strings.TrimSpace(rest)
		}
		key, raw, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE, got %q", line, s)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", line)
		}
		if !validKey(key) {
			return nil, fmt.Errorf("line %d: invalid key %q (use letters, digits, underscore)", line, key)
		}
		val, err := unquote(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out[key] = val
	}
	return out, sc.Err()
}

func validKey(key string) bool {
	for i, r := range key {
		switch {
		case r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// unquote strips matching single or double quotes and expands the escape
// sequences dotenv users expect. Unknown escapes are left untouched so Windows
// paths such as "C:\Users" survive.
func unquote(s string) (string, error) {
	if len(s) < 2 {
		return s, nil
	}
	switch {
	case s[0] == '\'' && s[len(s)-1] == '\'':
		return s[1 : len(s)-1], nil
	case s[0] == '"' && s[len(s)-1] == '"':
		var b strings.Builder
		body := s[1 : len(s)-1]
		for i := 0; i < len(body); i++ {
			if body[i] != '\\' || i+1 >= len(body) {
				b.WriteByte(body[i])
				continue
			}
			i++
			switch body[i] {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte('\\')
				b.WriteByte(body[i])
			}
		}
		return b.String(), nil
	}
	// Unquoted: drop a trailing " # comment".
	if i := strings.Index(s, " #"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s, nil
}

// CandidatePaths returns every .env location worth checking, most specific
// first: next to the executable, the working directory, then the per-user
// configuration directory.
func CandidatePaths(exeDir, configDir string) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(p string) {
		if p == "" {
			return
		}
		p = filepath.Clean(p)
		if seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	if exeDir != "" {
		add(filepath.Join(exeDir, ".env"))
	}
	if cwd, err := os.Getwd(); err == nil {
		add(filepath.Join(cwd, ".env"))
	}
	if configDir != "" {
		add(filepath.Join(configDir, ".env"))
	}
	return out
}

// Load reads every path that exists and merges the results. Missing files are
// not an error; a malformed file is.
func Load(paths ...string) (*Values, error) {
	v := &Values{values: make(map[string]string)}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return v, fmt.Errorf("open %s: %w", p, err)
		}
		parsed, parseErr := Parse(f)
		f.Close()
		if parseErr != nil {
			return v, fmt.Errorf("%s: %w", p, parseErr)
		}
		for k, val := range parsed {
			if _, exists := v.values[k]; !exists {
				v.values[k] = val
			}
		}
		v.Paths = append(v.Paths, p)
	}
	return v, nil
}
