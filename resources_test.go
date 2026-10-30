package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

// The GUI cannot start without these resources, and the failure is cryptic
// ("TTM_ADDTOOL failed") rather than obvious, so the build inputs are pinned
// here: walk needs Common Controls 6.0, which only the manifest provides.

func TestManifestDeclaresCommonControlsSix(t *testing.T) {
	manifest, err := os.ReadFile("app.manifest")
	if err != nil {
		t.Fatalf("reading app.manifest: %v", err)
	}

	for _, want := range []string{
		`<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">`,
		`name="Microsoft.Windows.Common-Controls" version="6.0.0.0"`,
		`<dpiAware`,
		`requestedExecutionLevel level="asInvoker"`,
	} {
		if !bytes.Contains(manifest, []byte(want)) {
			t.Errorf("app.manifest is missing %s", want)
		}
	}
}

// TestManifestIsCompiledIntoTheSysoFiles guards the generated resource objects:
// go build links them silently, and deleting one produces an exe that fails at
// startup on a user's machine rather than here.
func TestManifestIsCompiledIntoTheSysoFiles(t *testing.T) {
	manifest, err := os.ReadFile("app.manifest")
	if err != nil {
		t.Fatalf("reading app.manifest: %v", err)
	}

	cases := map[string]uint16{
		"app_windows_amd64.syso": 0x8664, // IMAGE_FILE_MACHINE_AMD64
		"app_windows_arm64.syso": 0xAA64, // IMAGE_FILE_MACHINE_ARM64
	}

	for name, wantMachine := range cases {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("%s is missing: go build would produce an exe without a manifest (%v)", name, err)
		}
		machine, sections := readCOFF(t, name, raw)
		if machine != wantMachine {
			t.Errorf("%s targets machine %#04x, want %#04x", name, machine, wantMachine)
		}

		rsrc, ok := sections[".rsrc"]
		if !ok {
			t.Fatalf("%s has no .rsrc section (sections: %v)", name, keys(sections))
		}
		if !bytes.Contains(raw[rsrc.offset:rsrc.offset+rsrc.size], bytes.TrimSpace(manifest)) {
			t.Errorf("%s does not contain app.manifest verbatim", name)
		}
		if !bytes.Contains(raw, []byte("Common-Controls")) {
			t.Errorf("%s does not mention Common Controls", name)
		}
	}
}

type section struct {
	offset int
	size   int
}

// readCOFF parses the object header and section table of a COFF object file.
func readCOFF(t *testing.T, name string, raw []byte) (machine uint16, sections map[string]section) {
	t.Helper()
	if len(raw) < 20 {
		t.Fatalf("%s is too short to be a COFF object", name)
	}

	machine = binary.LittleEndian.Uint16(raw[0:2])
	count := int(binary.LittleEndian.Uint16(raw[2:4]))
	optionalHeaderSize := int(binary.LittleEndian.Uint16(raw[16:18]))

	offset := 20 + optionalHeaderSize
	sections = make(map[string]section, count)
	for i := 0; i < count; i++ {
		header := raw[offset+i*40:]
		if len(header) < 40 {
			t.Fatalf("%s: section table is truncated", name)
		}
		sectionName := string(bytes.TrimRight(header[0:8], "\x00"))
		size := int(binary.LittleEndian.Uint32(header[16:20]))
		dataOffset := int(binary.LittleEndian.Uint32(header[20:24]))
		if dataOffset+size > len(raw) {
			t.Fatalf("%s: section %s points past the end of the file", name, sectionName)
		}
		sections[sectionName] = section{offset: dataOffset, size: size}
	}
	return machine, sections
}

func keys[S ~map[string]V, V any](m S) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
