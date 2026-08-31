package hostmetrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The message exists for one case: the tool is on the machine and not on this
// process's PATH. A host with no NVIDIA driver at all is most hosts, and a
// startup line about it on every ARM board in a fleet would be noise.
func TestGPUToolStatusOnlySpeaksWhenThereIsAFix(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	// The real candidate list would find this machine's own nvidia-smi, which
	// is a fact about the machine running the test rather than about the code.
	installed := filepath.Join(dir, "elsewhere", "nvidia-smi")
	old := gpuToolCandidates
	gpuToolCandidates = []string{installed}
	t.Cleanup(func() { gpuToolCandidates = old })

	if got := GPUToolStatus(); got != "" {
		t.Errorf("with nvidia-smi nowhere on the machine, want silence, got %q", got)
	}

	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := GPUToolStatus()
	if !strings.Contains(got, installed) {
		t.Errorf("the message must name where it found it, got %q", got)
	}
	if !strings.Contains(got, "PATH") {
		t.Errorf("the message must name the fix, got %q", got)
	}

	// And says nothing again once the tool is reachable, whatever else is true.
	t.Setenv("PATH", filepath.Dir(installed))
	if got := GPUToolStatus(); got != "" {
		t.Errorf("with nvidia-smi on PATH, want silence, got %q", got)
	}
}
