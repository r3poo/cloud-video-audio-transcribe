package envfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    map[string]string
		wantErr bool
	}{
		{
			name:  "plain pairs",
			input: "A=1\nB=two\n",
			want:  map[string]string{"A": "1", "B": "two"},
		},
		{
			name:  "comments and blank lines",
			input: "# note\n\nA=1\n   # indented note\nB=2\n",
			want:  map[string]string{"A": "1", "B": "2"},
		},
		{
			name:  "export prefix",
			input: "export A=1\nexport B = 2\n",
			want:  map[string]string{"A": "1", "B": "2"},
		},
		{
			name:  "single quotes are literal",
			input: "A=' spaced # not a comment '\n",
			want:  map[string]string{"A": " spaced # not a comment "},
		},
		{
			name:  "double quotes expand escapes",
			input: "A=\"line1\\nline2\\t\\\"q\\\"\"\n",
			want:  map[string]string{"A": "line1\nline2\t\"q\""},
		},
		{
			name:  "windows path keeps unknown escapes",
			input: `A="C:\Users\me\bin"` + "\n",
			want:  map[string]string{"A": `C:\Users\me\bin`},
		},
		{
			name:  "trailing comment trimmed for bare values",
			input: "A=value  # comment\n",
			want:  map[string]string{"A": "value"},
		},
		{
			name:  "empty value",
			input: "A=\n",
			want:  map[string]string{"A": ""},
		},
		{
			name:    "line without equals",
			input:   "A\n",
			wantErr: true,
		},
		{
			name:    "invalid key",
			input:   "A-B=1\n",
			wantErr: true,
		},
		{
			name:    "key starting with digit",
			input:   "1A=1\n",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(strings.NewReader(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = %v, want error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", tc.input, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Parse(%q) = %v, want %v", tc.input, got, tc.want)
			}
			for k, want := range tc.want {
				if got[k] != want {
					t.Errorf("key %q = %q, want %q", k, got[k], want)
				}
			}
		})
	}
}

func TestLoadPrecedence(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.env")
	second := filepath.Join(dir, "second.env")
	missing := filepath.Join(dir, "absent.env")

	if err := os.WriteFile(first, []byte("A=first\nB=first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("B=second\nC=second\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	v, err := Load(missing, first, second)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := v.Get("A"); got != "first" {
		t.Errorf("A = %q, want first", got)
	}
	if got := v.Get("B"); got != "first" {
		t.Errorf("B = %q, want the earlier file to win, got %q", got, "first")
	}
	if got := v.Get("C"); got != "second" {
		t.Errorf("C = %q, want second", got)
	}
	if got := v.Get("Z"); got != "" {
		t.Errorf("Z = %q, want empty", got)
	}
	if len(v.Paths) != 2 || v.Paths[0] != filepath.Clean(first) {
		t.Errorf("Paths = %v, want the existing files in load order", v.Paths)
	}
}

func TestLoadReportsMalformedFile(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.env")
	if err := os.WriteFile(bad, []byte("not a pair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := Load(bad)
	if err == nil {
		t.Fatal("Load accepted a malformed file")
	}
	if v == nil {
		t.Fatal("Load returned no Values alongside the error")
	}
}

func TestEnvironmentWinsOverFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("TRANSCRIBE_TEST_KEY=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Get("TRANSCRIBE_TEST_KEY"); got != "from-file" {
		t.Fatalf("before: got %q", got)
	}

	t.Setenv("TRANSCRIBE_TEST_KEY", "from-env")
	if got := v.Get("TRANSCRIBE_TEST_KEY"); got != "from-env" {
		t.Errorf("Get = %q, want the process environment to win", got)
	}
	if src := v.Source("TRANSCRIBE_TEST_KEY"); src != "environment" {
		t.Errorf("Source = %q, want environment", src)
	}
}

func TestCandidatePathsKeepsOrder(t *testing.T) {
	exeDir := filepath.Join("C:", "app")
	configDir := filepath.Join("C:", "Users", "me", "AppData", "Roaming", "cloud-transcribe")
	paths := CandidatePaths(exeDir, configDir)
	if len(paths) < 2 {
		t.Fatalf("CandidatePaths = %v, want at least two entries", paths)
	}
	if filepath.Base(paths[0]) != ".env" {
		t.Errorf("first candidate = %q, want a .env file", paths[0])
	}
	if !strings.HasPrefix(paths[0], filepath.Clean(exeDir)) {
		t.Errorf("first candidate = %q, want it inside the executable directory", paths[0])
	}
	if paths[len(paths)-1] != filepath.Join(configDir, ".env") {
		t.Errorf("last candidate = %q, want the config directory last", paths[len(paths)-1])
	}
}

func TestCandidatePathsDeduplicates(t *testing.T) {
	paths := CandidatePaths("", "")
	if len(paths) > 1 {
		t.Fatalf("CandidatePaths(\"\", \"\") = %v, want at most the working directory copy", paths)
	}
}

func TestSetKeyCreatesAndUpdates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", ".env")

	if err := SetKey(path, "OPENROUTER_MODEL", "openai/whisper-large-v3"); err != nil {
		t.Fatalf("SetKey create: %v", err)
	}
	v, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Get("OPENROUTER_MODEL"); got != "openai/whisper-large-v3" {
		t.Fatalf("after create: got %q", got)
	}

	// A second key is appended without disturbing the first.
	if err := SetKey(path, "TRANSCRIBE_OUTPUT_DIR", `C:\out`); err != nil {
		t.Fatalf("SetKey append: %v", err)
	}
	// An existing key is replaced in place.
	if err := SetKey(path, "OPENROUTER_MODEL", "openai/whisper-1"); err != nil {
		t.Fatalf("SetKey replace: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Count(text, "OPENROUTER_MODEL=") != 1 {
		t.Fatalf("model key appears %d times in:\n%s", strings.Count(text, "OPENROUTER_MODEL="), text)
	}
	if !strings.Contains(text, "OPENROUTER_MODEL=openai/whisper-1") {
		t.Errorf("model key not updated:\n%s", text)
	}
	if !strings.Contains(text, `TRANSCRIBE_OUTPUT_DIR=C:\out`) {
		t.Errorf("second key not written:\n%s", text)
	}
}

func TestSetKeyPreservesComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	original := "# keep me\nOPENROUTER_API_KEY=abc\n# and me too\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SetKey(path, "OPENROUTER_API_KEY", "def"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"# keep me", "# and me too", "OPENROUTER_API_KEY=def"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "abc") {
		t.Errorf("old value still present:\n%s", text)
	}
}

func TestSetQuotesAwkwardValues(t *testing.T) {
	v := &Values{}
	v.Set("A", "plain")
	// Set does not touch the disk; values are read back through Get.
	t.Setenv("A", "")
	if got := v.Get("A"); got != "plain" {
		t.Errorf("Get = %q, want plain", got)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	value := `C:\dir with space\file #1`
	if err := SetKey(path, "TRANSCRIBE_OUTPUT_DIR", value); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Get("TRANSCRIBE_OUTPUT_DIR"); got != value {
		t.Errorf("round trip = %q, want %q", got, value)
	}
}
