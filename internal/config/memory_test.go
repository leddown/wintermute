package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMemory(t *testing.T) {
	ok := map[string]int64{
		"7GB":    7 * 1000 * 1000 * 1000,
		"7 GB":   7 * 1000 * 1000 * 1000,
		"8GiB":   8 * 1024 * 1024 * 1024,
		"3500MB": 3500 * 1000 * 1000,
		"0.5GB":  500 * 1000 * 1000,
		"1024":   1024,
		"20gb":   20 * 1000 * 1000 * 1000,
	}
	for in, want := range ok {
		got, err := ParseMemory(in)
		if err != nil {
			t.Errorf("ParseMemory(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseMemory(%q) = %d, want %d", in, got, want)
		}
	}
	for _, in := range []string{"big", "7 potatoes", "", "-4GB"} {
		if _, err := ParseMemory(in); err == nil {
			t.Errorf("ParseMemory(%q) should have failed", in)
		}
	}
}

// The example file has to actually load: it is decoded with
// DisallowUnknownFields, so a stray key in it is a broken starting point.
func TestExampleBackendsFileLoads(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "backends.example.json"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	var file BackendsFile
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		t.Fatalf("example backends.json does not decode: %v", err)
	}
	var withMemory int
	for _, b := range file.Backends {
		if b.Memory == "" {
			continue
		}
		withMemory++
		if _, err := ParseMemory(b.Memory); err != nil {
			t.Errorf("backend %q memory %q: %v", b.Name, b.Memory, err)
		}
	}
	if withMemory == 0 {
		t.Error("example declares no memory sizes, so it documents nothing")
	}
}
