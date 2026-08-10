package models

import (
	"math"
	"testing"
)

// gtx1070 is the reference machine this package was calibrated against: 8GB,
// Pascal, 256 GB/s, with a little VRAM already taken by a desktop session.
func gtx1070() *Hardware {
	return &Hardware{
		GPUs: []GPU{{
			Name: "NVIDIA GeForce GTX 1070", TotalMB: 8192, UsedMB: 500, FreeMB: 7692,
			ComputeCap: "6.1", BandwidthGBs: 256, Arch: "Pascal",
		}},
		RAMTotalMB: 32768, RAMAvailableMB: 24576, RAMBandwidthGBs: 40,
	}
}

func TestWeightsMatchRealFileSizes(t *testing.T) {
	// Known-good figures: these are the sizes the GGUF files actually are, and
	// the whole calculator is worthless if it does not reproduce them.
	tests := []struct {
		name    string
		paramsB float64
		quant   string
		wantGB  float64
	}{
		{"8B Q4_K_M", 8.0, "Q4_K_M", 4.52},
		{"8B Q5_K_M", 8.0, "Q5_K_M", 5.26},
		{"4B Q4_K_M", 4.0, "Q4_K_M", 2.26},
		{"12B Q4_K_M", 12.2, "Q4_K_M", 6.89},
		{"8B Q8_0", 8.0, "Q8_0", 7.92},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fit := EstimateFit(FitInput{ParamsB: tt.paramsB, Quant: tt.quant, ContextTokens: 8192}, gtx1070())
			gotGB := fit.WeightsMB / 1024
			if math.Abs(gotGB-tt.wantGB) > 0.15 {
				t.Errorf("weights = %.2fGB, want about %.2fGB", gotGB, tt.wantGB)
			}
		})
	}
}

func TestKVCacheScalesWithContextAndType(t *testing.T) {
	// An 8B model with 32 layers and 8 KV heads of dim 128 needs exactly 1GB
	// of fp16 KV cache at 8192 tokens. That is the number that decides whether
	// a long context is affordable, so it is pinned here.
	in := FitInput{
		ParamsB: 8.0, Quant: "Q4_K_M", ContextTokens: 8192,
		Layers: 32, KVHeads: 8, HeadDim: 128, KVCacheType: "f16",
	}
	fit := EstimateFit(in, gtx1070())
	if got := fit.KVCacheMB; math.Abs(got-1024) > 1 {
		t.Errorf("fp16 KV cache = %.0fMB, want 1024MB", got)
	}

	in.KVCacheType = "q8_0"
	if got := EstimateFit(in, gtx1070()).KVCacheMB; math.Abs(got-512) > 1 {
		t.Errorf("q8_0 KV cache = %.0fMB, want 512MB", got)
	}

	in.KVCacheType = "f16"
	in.ContextTokens = 16384
	if got := EstimateFit(in, gtx1070()).KVCacheMB; math.Abs(got-2048) > 1 {
		t.Errorf("KV cache at 16K = %.0fMB, want 2048MB", got)
	}
}

func TestVerdicts(t *testing.T) {
	tests := []struct {
		name    string
		in      FitInput
		want    Verdict
	}{
		{
			"8B Q4_K_M at 8K fits",
			FitInput{ParamsB: 8.0, Quant: "Q4_K_M", ContextTokens: 8192},
			VerdictFits,
		},
		{
			"14B Q4_K_M does not fit in 8GB",
			FitInput{ParamsB: 14.8, Quant: "Q4_K_M", ContextTokens: 8192},
			VerdictPartial,
		},
		{
			"8B unquantized is hopeless",
			FitInput{ParamsB: 8.0, Quant: "F16", ContextTokens: 8192},
			VerdictPartial,
		},
		{
			"4B Q4_K_M fits with room to spare",
			FitInput{ParamsB: 4.0, Quant: "Q4_K_M", ContextTokens: 16384},
			VerdictFits,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EstimateFit(tt.in, gtx1070()).Verdict; got != tt.want {
				t.Errorf("verdict = %q, want %q", got, tt.want)
			}
		})
	}
}

// A long enough context turns a model that fits into one that does not. This
// is the trade-off the Planner exists to make visible.
func TestContextLengthCanPushAModelOver(t *testing.T) {
	in := FitInput{ParamsB: 8.0, Quant: "Q4_K_M", ContextTokens: 8192}
	if got := EstimateFit(in, gtx1070()).Verdict; got != VerdictFits {
		t.Fatalf("at 8K: verdict = %q, want %q", got, VerdictFits)
	}

	in.ContextTokens = 131072
	if got := EstimateFit(in, gtx1070()).Verdict; got == VerdictFits {
		t.Error("at 128K the same model should no longer fit")
	}
}

func TestThroughputEstimateIsCalibrated(t *testing.T) {
	// Measured reality on this card is roughly 30-45 tok/s for an 8B at
	// Q4_K_M. An estimate outside that band means the bandwidth model drifted.
	fit := EstimateFit(FitInput{ParamsB: 8.0, Quant: "Q4_K_M", ContextTokens: 8192}, gtx1070())
	if fit.TokensPerSec < 30 || fit.TokensPerSec > 50 {
		t.Errorf("tokens/sec = %.1f, want roughly 30-50 for an 8B Q4_K_M on a GTX 1070", fit.TokensPerSec)
	}
}

// Partial offload must be modelled as a cliff, not a gentle slope: a model
// half on the CPU runs near RAM speed, not half GPU speed.
func TestPartialOffloadIsPunishing(t *testing.T) {
	full := EstimateFit(FitInput{ParamsB: 8.0, Quant: "Q4_K_M", ContextTokens: 8192}, gtx1070())
	partial := EstimateFit(FitInput{ParamsB: 24, Quant: "Q4_K_M", ContextTokens: 8192}, gtx1070())

	if partial.Verdict != VerdictPartial {
		t.Fatalf("verdict = %q, want %q", partial.Verdict, VerdictPartial)
	}
	if partial.GPULayers >= partial.TotalLayers {
		t.Errorf("GPULayers = %d of %d; a 24B should not fit entirely", partial.GPULayers, partial.TotalLayers)
	}
	if partial.TokensPerSec > full.TokensPerSec/3 {
		t.Errorf("partial offload = %.1f tok/s against %.1f fully resident; the penalty is under-modelled",
			partial.TokensPerSec, full.TokensPerSec)
	}
}

// A mixture-of-experts model reads only its active parameters per token, so it
// must be predicted as far faster than its total size implies.
func TestMoEIsFasterThanItsSize(t *testing.T) {
	dense := EstimateFit(FitInput{ParamsB: 30.5, Quant: "Q4_K_M", ContextTokens: 8192}, gtx1070())
	moe := EstimateFit(FitInput{ParamsB: 30.5, ActiveParamsB: 3.3, Quant: "Q4_K_M", ContextTokens: 8192}, gtx1070())

	if moe.TotalMB != dense.TotalMB {
		t.Errorf("memory should be identical: MoE %.0fMB against dense %.0fMB", moe.TotalMB, dense.TotalMB)
	}
	if moe.TokensPerSec <= dense.TokensPerSec*2 {
		t.Errorf("MoE = %.1f tok/s against dense %.1f; should be several times faster",
			moe.TokensPerSec, dense.TokensPerSec)
	}
}

func TestPascalWarnsAboutHalfPrecision(t *testing.T) {
	fit := EstimateFit(FitInput{ParamsB: 4.0, Quant: "F16", ContextTokens: 4096}, gtx1070())
	if !containsSubstring(fit.Notes, "1/64") {
		t.Errorf("expected a Pascal FP16 warning, got notes: %v", fit.Notes)
	}
}

func TestNoGPUFallsBackToCPU(t *testing.T) {
	hw := &Hardware{RAMTotalMB: 16384, RAMAvailableMB: 12288, RAMBandwidthGBs: 40}
	fit := EstimateFit(FitInput{ParamsB: 8.0, Quant: "Q4_K_M", ContextTokens: 4096}, hw)

	if fit.Verdict != VerdictPartial {
		t.Errorf("verdict = %q, want %q for a CPU-only host", fit.Verdict, VerdictPartial)
	}
	if fit.TokensPerSec > 15 {
		t.Errorf("CPU throughput = %.1f tok/s, which is implausibly high", fit.TokensPerSec)
	}
}

func TestUnknownParamsIsReportedNotGuessed(t *testing.T) {
	fit := EstimateFit(FitInput{ParamsB: 0, Quant: "Q4_K_M"}, gtx1070())
	if fit.Verdict != VerdictNo {
		t.Errorf("verdict = %q, want %q when the size is unknown", fit.Verdict, VerdictNo)
	}
	if !fit.Estimated {
		t.Error("a result derived from an unknown parameter count must be marked estimated")
	}
}

func TestInferParams(t *testing.T) {
	tests := []struct {
		in   string
		want float64
	}{
		{"qwen3:8b", 8},
		{"Llama-3.1-8B-Instruct", 8},
		{"gemma-3-4b-it", 4},
		{"Qwen3-Coder-30B-A3B", 30},
		{"1.5B", 1.5},
		{"7.2b", 7.2},
		{"model-with-no-size", 0},
		{"base-model", 0},
		{"Q4_K_M", 0},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := inferParams(tt.in); got != tt.want {
				t.Errorf("inferParams(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestInferQuant(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Qwen_Qwen3-8B-Q4_K_M.gguf", "Q4_K_M"},
		{"model-q8_0.gguf", "Q8_0"},
		{"model-IQ4_XS.gguf", "IQ4_XS"},
		{"model-f16.gguf", "F16"},
		{"model.gguf", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := inferQuant(tt.in); got != tt.want {
				t.Errorf("inferQuant(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func containsSubstring(list []string, want string) bool {
	for _, s := range list {
		for i := 0; i+len(want) <= len(s); i++ {
			if s[i:i+len(want)] == want {
				return true
			}
		}
	}
	return false
}
