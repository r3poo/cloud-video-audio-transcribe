package envfile

import (
	"os"
	"path/filepath"
	"strings"
)

// Set records a value in this Values instance without touching the disk, so a
// choice made in the UI is visible for the rest of the session.
func (v *Values) Set(key, value string) {
	if v.values == nil {
		v.values = make(map[string]string)
	}
	v.values[key] = value
}

// SetKey writes key=value into path, replacing an existing assignment and
// leaving every other line (comments included) untouched. The file is written
// atomically so a crash cannot leave a truncated .env behind.
func SetKey(path, key, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	var lines []string
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		text := strings.ReplaceAll(string(data), "\r\n", "\n")
		text = strings.TrimSuffix(text, "\n")
		if text != "" {
			lines = strings.Split(text, "\n")
		}
	case os.IsNotExist(err):
		// Start a fresh file.
	default:
		return err
	}

	replaced := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "export ") {
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "export "))
		}
		existing, _, ok := strings.Cut(trimmed, "=")
		if !ok || strings.TrimSpace(existing) != key {
			continue
		}
		lines[i] = key + "=" + quoteValue(value)
		replaced = true
		break
	}
	if !replaced {
		lines = append(lines, key+"="+quoteValue(value))
	}

	content := strings.Join(lines, "\r\n") + "\r\n"
	tmp, err := os.CreateTemp(filepath.Dir(path), ".env-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		// Not fatal on filesystems without POSIX permissions.
		_ = err
	}
	return os.Rename(tmpName, path)
}

// quoteValue adds double quotes only when a bare value would parse differently.
func quoteValue(value string) string {
	if value == "" {
		return ""
	}
	if !strings.ContainsAny(value, " \"#'") {
		return value
	}
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}
