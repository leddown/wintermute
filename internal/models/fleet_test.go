package models

import (
	"testing"
	"time"

	"wintermute/internal/node"
)

// A node's reported state must become the same kind of evidence a local probe
// produces, or the fleet is a dashboard rather than an input to any decision.
func TestHardwareFromNodeCarriesTheCardAndItsFreeMemory(t *testing.T) {
	now := time.Now().UTC()
	n := node.Node{
		Name:       "tycho",
		Cores:      12,
		LastSeenAt: now,
		GPUs: []node.GPUCard{
			{Index: 0, Name: "NVIDIA GeForce RTX 3090", MemTotalBytes: 24 * 1 << 30},
		},
		Latest: &node.Sample{
			At:          now,
			MemTotal:    64 * 1 << 30,
			MemUsed:     16 * 1 << 30,
			GPUMemUsed:  4 * 1 << 30,
			GPUMemTotal: 24 * 1 << 30,
		},
	}

	hw := HardwareFromNode(n)
	if hw.Host != "tycho" {
		t.Errorf("host = %q, want %q", hw.Host, "tycho")
	}
	gpu := hw.PrimaryGPU()
	if gpu == nil {
		t.Fatal("the reported card did not survive")
	}
	// 24GB total, 4GB in use: the sample is the whole truth for a single card.
	if gpu.TotalMB != 24576 || gpu.FreeMB != 20480 {
		t.Errorf("card = %dMB total / %dMB free, want 24576 / 20480", gpu.TotalMB, gpu.FreeMB)
	}
	// The name is looked up exactly as a local probe's is, which is what makes
	// a throughput estimate possible for a machine this server cannot see.
	if gpu.BandwidthGBs != 936 || gpu.Arch != "Ampere" {
		t.Errorf("spec = %.0f GB/s %q, want 936 \"Ampere\"", gpu.BandwidthGBs, gpu.Arch)
	}
	if hw.RAMAvailableMB != 49152 {
		t.Errorf("available RAM = %dMB, want 49152", hw.RAMAvailableMB)
	}

	// And the whole point: a model this server could not touch is judged by the
	// machine that would run it.
	fit := EstimateFit(FitInput{ParamsB: 8, Quant: "Q4_K_M", ContextTokens: 4096}, hw)
	if fit.Verdict != VerdictFits {
		t.Errorf("verdict = %q, want %q on a 3090 with 20GB free", fit.Verdict, VerdictFits)
	}
	if fit.Host != "tycho" {
		t.Errorf("fit.Host = %q, want the machine it was graded against", fit.Host)
	}
}

// Free VRAM crosses the wire as a total across cards. With more than one, the
// split is not knowable, and the estimate must lean pessimistic rather than
// invent a distribution: a model reported as fitting and then failing to load
// is the failure worth avoiding.
func TestMultiGPUNodeChargesUsedMemoryToTheLargestCard(t *testing.T) {
	now := time.Now().UTC()
	n := node.Node{
		Name:       "erebus",
		LastSeenAt: now,
		GPUs: []node.GPUCard{
			{Index: 0, Name: "NVIDIA GeForce RTX 3090", MemTotalBytes: 24 * 1 << 30},
			{Index: 1, Name: "NVIDIA GeForce GTX 1080", MemTotalBytes: 8 * 1 << 30},
		},
		Latest: &node.Sample{At: now, GPUMemUsed: 6 * 1 << 30, GPUMemTotal: 32 * 1 << 30},
	}

	hw := HardwareFromNode(n)
	primary := hw.PrimaryGPU()
	if primary.TotalMB != 24576 {
		t.Fatalf("primary card = %dMB, want the 24GB one", primary.TotalMB)
	}
	if primary.FreeMB != 24576-6144 {
		t.Errorf("free = %dMB, want the whole 6144MB of reported use charged here", primary.FreeMB)
	}
	// The second card keeps its memory, which is what "unattributed" honestly
	// means — not that it is known to be idle, but that nothing said otherwise.
	if hw.GPUs[1].FreeMB != 8192 {
		t.Errorf("second card free = %dMB, want 8192", hw.GPUs[1].FreeMB)
	}
	// And it says so, rather than presenting the assumption as a measurement.
	if !containsSubstring(primary.Notes, "charged to this one") {
		t.Errorf("the attribution was not disclosed: %v", primary.Notes)
	}
}

// A machine that stopped reporting still has a plausible profile in the
// database. Grading against it would answer confidently for a box that is
// switched off.
func TestStaleNodeIsNotGraded(t *testing.T) {
	now := time.Now().UTC()
	n := node.Node{
		Name:       "tycho",
		LastSeenAt: now.Add(-2 * time.Hour),
		GPUs:       []node.GPUCard{{Name: "NVIDIA GeForce RTX 3090", MemTotalBytes: 24 * 1 << 30}},
		Latest:     &node.Sample{At: now.Add(-2 * time.Hour), GPUMemTotal: 24 * 1 << 30},
	}

	hw := HardwareFromNode(n)
	if !hw.Stale(now) {
		t.Fatal("a two-hour-old reading must not count as current")
	}
	fit := EstimateFit(FitInput{ParamsB: 8, Quant: "Q4_K_M"}, hw)
	if fit.Verdict != VerdictUnknown {
		t.Errorf("verdict = %q, want %q for a node that stopped reporting", fit.Verdict, VerdictUnknown)
	}
	// Still named, so the page can say which machine went quiet.
	if fit.Host != "tycho" {
		t.Errorf("fit.Host = %q, want the stale machine named", fit.Host)
	}
	// The footprint is a property of the model and survives, as it does
	// everywhere else a verdict cannot be given.
	if fit.TotalMB <= 0 {
		t.Error("the footprint was discarded along with the verdict")
	}
	// A reading from a moment ago is fine.
	fresh := HardwareFromNode(node.Node{
		Name: "tycho", LastSeenAt: now,
		GPUs:   []node.GPUCard{{Name: "NVIDIA GeForce RTX 3090", MemTotalBytes: 24 * 1 << 30}},
		Latest: &node.Sample{At: now.Add(-30 * time.Second), GPUMemTotal: 24 * 1 << 30},
	})
	if fresh.Stale(now) {
		t.Error("a thirty-second-old reading must still count")
	}
}

// The best verdict is what a reader acts on, and it is worthless without the
// name of the machine that earned it.
func TestBestFitPrefersTheMachineThatRunsIt(t *testing.T) {
	now := time.Now().UTC()
	small := &Hardware{
		Host: "pi", RunsInference: true, RAMTotalMB: 8192, RAMAvailableMB: 6144,
		RAMBandwidthGBs: 20, DetectedAt: now,
	}
	big := HardwareFromNode(node.Node{
		Name: "tycho", LastSeenAt: now,
		GPUs:   []node.GPUCard{{Name: "NVIDIA GeForce RTX 3090", MemTotalBytes: 24 * 1 << 30}},
		Latest: &node.Sample{At: now, MemTotal: 64 * 1 << 30, MemUsed: 8 * 1 << 30, GPUMemTotal: 24 * 1 << 30},
	})

	in := FitInput{ParamsB: 8, Quant: "Q4_K_M", ContextTokens: 4096}
	graded := EstimateFleetFit(in, []*Hardware{small, big})
	if len(graded) != 2 {
		t.Fatalf("graded %d machines, want 2", len(graded))
	}
	best := BestFit(graded)
	if best.Verdict != VerdictFits || best.Host != "tycho" {
		t.Errorf("best = %q on %q, want \"fits\" on \"tycho\"", best.Verdict, best.Host)
	}

	// With nothing to grade against, the footprint still comes back — a caller
	// that got an empty slice would have to special-case exactly the case the
	// note inside already explains.
	none := EstimateFleetFit(in, nil)
	if len(none) != 1 || none[0].Verdict != VerdictUnknown || none[0].TotalMB <= 0 {
		t.Errorf("with no hosts: %d results, verdict %q, %.0fMB", len(none), none[0].Verdict, none[0].TotalMB)
	}
}

// "It was measured and will not run" is actionable; "nobody looked" is not.
// Letting an unmeasured machine outrank a measured one would replace the only
// real answer available with silence.
func TestNoOutranksUnknown(t *testing.T) {
	measured := Fit{Verdict: VerdictNo, Host: "pi"}
	unmeasured := Fit{Verdict: VerdictUnknown, Host: "tycho"}
	if got := BestFit([]Fit{unmeasured, measured}); got.Host != "pi" {
		t.Errorf("best = %q/%q, want the machine that was actually measured", got.Verdict, got.Host)
	}
}

// The declared link is the whole security and correctness story: nothing infers
// a machine from a URL, so a node is a candidate only when a backend says so.
func TestOnlyDeclaredNodesAreInferenceHosts(t *testing.T) {
	backends := []Backend{
		{Name: "local", BaseURL: "http://127.0.0.1:8080/v1"},
		{Name: "big", BaseURL: "http://192.168.1.40:8080/v1", Node: "tycho"},
		{Name: "claude", Cloud: true, Node: "tycho"},
	}
	got := inferenceNodes(backends)
	if !got["tycho"] {
		t.Error("a backend declared on a node did not make it a candidate")
	}
	if len(got) != 1 {
		t.Errorf("candidates = %v, want tycho alone", got)
	}

	// A backend on a node's address but with nothing declared stays this
	// server's problem, because an address is not a machine.
	undeclared := inferenceNodes([]Backend{{Name: "big", BaseURL: "http://tycho.lan:8080/v1"}})
	if len(undeclared) != 0 {
		t.Errorf("candidates = %v, want none inferred from a hostname", undeclared)
	}
}

// The planner names one machine, and on a fleet it must not be the API server.
func TestSortHostsPutsTheBestEquippedFirst(t *testing.T) {
	pi := &Hardware{Host: "pi", RAMTotalMB: 8192}
	mid := &Hardware{Host: "mid", GPUs: []GPU{{TotalMB: 8192}}, RAMTotalMB: 32768}
	big := &Hardware{Host: "tycho", GPUs: []GPU{{TotalMB: 24576}}, RAMTotalMB: 65536}

	hosts := []*Hardware{pi, mid, big}
	sortHosts(hosts)
	if hosts[0].Host != "tycho" || hosts[1].Host != "mid" || hosts[2].Host != "pi" {
		t.Errorf("order = %q, %q, %q; want tycho, mid, pi",
			hosts[0].Host, hosts[1].Host, hosts[2].Host)
	}
}
