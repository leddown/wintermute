package models

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Verdict grades whether a model will run on the detected hardware.
type Verdict string

const (
	// VerdictFits means weights, KV cache and overhead sit inside free VRAM
	// with headroom to spare.
	VerdictFits Verdict = "fits"
	// VerdictTight means it fits, but with little margin — a desktop session
	// opening a browser tab could push it over.
	VerdictTight Verdict = "tight"
	// VerdictPartial means some layers must stay on the CPU. It will run, and
	// it will be slow; the estimate says how slow.
	VerdictPartial Verdict = "partial"
	// VerdictNo means it will not run usefully at all.
	VerdictNo Verdict = "no"
	// VerdictUnknown means the hardware that would run the model was not
	// measured, so there is no verdict to give. It is distinct from VerdictNo:
	// "it will not run" and "nobody looked" lead to opposite decisions, and
	// collapsing them into one answer is how a usable model gets ruled out.
	VerdictUnknown Verdict = "unknown"
)

// quantBPW maps a quantization name to its effective bits per weight.
//
// These are measured file-size ratios rather than the nominal bit width: a
// "4-bit" k-quant stores its scales and mins alongside the weights and lands
// nearer 4.85 bits in practice. Using the nominal 4.0 here would under-predict
// VRAM by about 20%, which on an 8GB card is the difference between a model
// that loads and one that does not.
var quantBPW = map[string]float64{
	"Q2_K":    3.35,
	"IQ3_XXS": 3.06,
	"IQ3_XS":  3.3,
	"Q3_K_S":  3.5,
	"Q3_K_M":  3.9,
	"Q3_K_L":  4.27,
	"IQ4_XS":  4.25,
	"IQ4_NL":  4.5,
	"Q4_0":    4.55,
	"Q4_K_S":  4.58,
	"Q4_K_M":  4.85,
	"Q5_0":    5.54,
	"Q5_K_S":  5.52,
	"Q5_K_M":  5.65,
	"Q6_K":    6.56,
	"Q8_0":    8.5,
	"F16":     16,
	"BF16":    16,
	"F32":     32,
}

// quantNames is the recognised set, longest first so inferQuant matches
// "Q4_K_M" before "Q4_0" inside a filename.
var quantNames = func() []string {
	out := make([]string, 0, len(quantBPW))
	for name := range quantBPW {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}()

// DefaultQuant is the recommendation for a memory-constrained GPU: the best
// quality per byte, and on Pascal also the fastest, since it rides the INT8
// dot-product path.
const DefaultQuant = "Q4_K_M"

// kvBytesPerElem maps a KV cache type to bytes per element.
var kvBytesPerElem = map[string]float64{
	"f16":  2,
	"fp16": 2,
	"q8_0": 1,
	"q5_1": 0.75,
	"q4_0": 0.5,
}

// geometry is the attention shape needed to size a KV cache.
type geometry struct {
	layers  int
	kvHeads int
	headDim int
}

// geometryBands approximates the shape of a modern grouped-query-attention
// model at each size. Real geometry is used when a backend reports it; this is
// the fallback, and every estimate derived from it is flagged.
//
// The numbers come from the reference models at each scale (Llama 3.x, Qwen3,
// Gemma 3), all of which use 8 KV heads — GQA is why an 8B model's KV cache
// costs about 1GB at 8K context rather than the 4GB multi-head attention would
// have needed, and therefore why an 8B model fits on this card at all.
var geometryBands = []struct {
	maxParamsB float64
	geo        geometry
}{
	{1.5, geometry{16, 8, 64}},
	{3.5, geometry{28, 8, 128}},
	{5, geometry{36, 8, 128}},
	{9, geometry{32, 8, 128}},
	{15, geometry{40, 8, 128}},
	{28, geometry{48, 8, 128}},
	{40, geometry{64, 8, 128}},
	{math.MaxFloat64, geometry{80, 8, 128}},
}

func inferGeometry(paramsB float64) geometry {
	for _, band := range geometryBands {
		if paramsB <= band.maxParamsB {
			return band.geo
		}
	}
	return geometry{80, 8, 128}
}

// FitInput describes the model and runtime settings to evaluate.
type FitInput struct {
	ParamsB       float64 `json:"params_b"`
	Quant         string  `json:"quant"`
	ContextTokens int     `json:"context_tokens"`
	// KVCacheType is "f16", "q8_0" or "q4_0". Empty means q8_0, which is the
	// recommended default: it roughly halves the cost of context for a quality
	// loss that is not observable in practice.
	KVCacheType string `json:"kv_cache_type,omitempty"`
	// Layers, KVHeads and HeadDim override the inferred geometry when known.
	Layers  int `json:"layers,omitempty"`
	KVHeads int `json:"kv_heads,omitempty"`
	HeadDim int `json:"head_dim,omitempty"`
	// ActiveParamsB is set for mixture-of-experts models, where throughput is
	// governed by the active parameters even though memory is governed by the
	// total. Zero means dense.
	ActiveParamsB float64 `json:"active_params_b,omitempty"`
}

// Fit is the outcome of evaluating a FitInput against some hardware.
type Fit struct {
	WeightsMB  float64 `json:"weights_mb"`
	KVCacheMB  float64 `json:"kv_cache_mb"`
	OverheadMB float64 `json:"overhead_mb"`
	TotalMB    float64 `json:"total_mb"`

	FreeVRAMMB  float64 `json:"free_vram_mb"`
	TotalVRAMMB float64 `json:"total_vram_mb"`

	Verdict Verdict `json:"verdict"`
	// Host names the machine this verdict is about, copied from the hardware
	// it was graded against. Empty means this server. On a fleet the verdict
	// is meaningless without it: "fits" is a statement about one machine, and
	// which one is the thing the reader is deciding.
	Host string `json:"host,omitempty"`
	// GPULayers is how many of the model's layers fit, when the verdict is
	// partial. It is what you would pass to --n-gpu-layers.
	GPULayers   int `json:"gpu_layers,omitempty"`
	TotalLayers int `json:"total_layers,omitempty"`

	// TokensPerSec is a first-order estimate, not a benchmark. Generation is
	// memory-bandwidth-bound, so it is derived from bandwidth over model size.
	TokensPerSec float64 `json:"tokens_per_sec"`
	// Estimated marks a result that relied on inferred geometry or an inferred
	// parameter count. The UI says so rather than presenting a guess as fact.
	Estimated bool     `json:"estimated"`
	Notes     []string `json:"notes,omitempty"`
}

// Headroom reports the free VRAM left over, in MB. Negative means it does not
// fit.
func (f Fit) Headroom() float64 { return f.FreeVRAMMB - f.TotalMB }

// EstimateFit computes the memory footprint and expected throughput of running
// a model on the given hardware.
//
// A GPU-less hw is calculated against system RAM, with the verdict reflecting
// CPU-only inference. A nil hw, or one whose RunsInference is false, yields
// VerdictUnknown with the footprint still filled in: the host being described
// is not the host that would run the model, and a confident answer computed
// from the wrong machine is worse than no answer.
func EstimateFit(in FitInput, hw *Hardware) Fit {
	fit := Fit{Estimated: false}
	if hw != nil {
		fit.Host = hw.Host
	}

	quant := strings.ToUpper(strings.TrimSpace(in.Quant))
	bpw, known := quantBPW[quant]
	if !known {
		bpw = quantBPW[DefaultQuant]
		fit.Notes = append(fit.Notes,
			fmt.Sprintf("Unknown quantization %q; assuming %s.", in.Quant, DefaultQuant))
		fit.Estimated = true
	}

	if in.ParamsB <= 0 {
		fit.Verdict = VerdictNo
		fit.Notes = append(fit.Notes, "Parameter count unknown, so the footprint cannot be estimated.")
		fit.Estimated = true
		return fit
	}

	// Weights. 1e9 params * bits / 8 bits-per-byte / 1MB.
	fit.WeightsMB = in.ParamsB * 1e9 * bpw / 8 / (1 << 20)

	geo := geometry{layers: in.Layers, kvHeads: in.KVHeads, headDim: in.HeadDim}
	if geo.layers == 0 || geo.kvHeads == 0 || geo.headDim == 0 {
		geo = inferGeometry(in.ParamsB)
		fit.Estimated = true
	}
	fit.TotalLayers = geo.layers

	ctx := in.ContextTokens
	if ctx <= 0 {
		ctx = 8192
	}
	kvType := strings.ToLower(in.KVCacheType)
	if kvType == "" {
		kvType = "q8_0"
	}
	elem, ok := kvBytesPerElem[kvType]
	if !ok {
		elem = 1
		fit.Notes = append(fit.Notes, fmt.Sprintf("Unknown KV cache type %q; assuming q8_0.", in.KVCacheType))
	}

	// Two caches (K and V), one per layer, sized by the GQA head count.
	fit.KVCacheMB = 2 * float64(geo.layers) * float64(geo.kvHeads) * float64(geo.headDim) *
		float64(ctx) * elem / (1 << 20)

	// CUDA context plus compute buffers. The context allocation is roughly
	// fixed; the compute buffer grows with the sequence length being processed.
	fit.OverheadMB = 320 + float64(ctx)/1024*32
	fit.TotalMB = fit.WeightsMB + fit.KVCacheMB + fit.OverheadMB

	// The footprint above is a property of the model and is worth keeping. What
	// follows is not: verdict, free VRAM and throughput are all statements about
	// a particular machine, and this host is not it.
	if hw == nil || !hw.RunsInference || hw.Stale(time.Now()) {
		fit.Verdict = VerdictUnknown
		fit.Estimated = true
		fit.Notes = append(fit.Notes, unknownHostNote(hw))
		return fit
	}

	gpu := hw.PrimaryGPU()
	if gpu == nil {
		return finishCPUOnly(fit, in, hw)
	}

	fit.FreeVRAMMB = float64(gpu.FreeMB)
	fit.TotalVRAMMB = float64(gpu.TotalMB)

	bandwidth := gpu.BandwidthGBs
	if bandwidth <= 0 {
		bandwidth = defaultBandwidthGBs
	}

	// Throughput is governed by how many bytes must be read per token. For a
	// mixture-of-experts model only the active experts are read, so memory and
	// speed decouple — which is what makes a 30B A3B usable with CPU offload
	// when a dense 30B is not.
	readMB := fit.WeightsMB
	if in.ActiveParamsB > 0 && in.ActiveParamsB < in.ParamsB {
		readMB = in.ActiveParamsB * 1e9 * bpw / 8 / (1 << 20)
		fit.Notes = append(fit.Notes,
			fmt.Sprintf("Mixture-of-experts: %.0fB total parameters but only ~%.0fB active per token, so it runs far faster than its size suggests.",
				in.ParamsB, in.ActiveParamsB))
	}

	switch {
	case fit.TotalMB <= fit.FreeVRAMMB*0.90:
		fit.Verdict = VerdictFits
		fit.GPULayers = geo.layers
		fit.TokensPerSec = throughput(bandwidth, gpuEfficiency(gpu), readMB)

	case fit.TotalMB <= fit.FreeVRAMMB:
		fit.Verdict = VerdictTight
		fit.GPULayers = geo.layers
		fit.TokensPerSec = throughput(bandwidth, gpuEfficiency(gpu), readMB)
		fit.Notes = append(fit.Notes,
			"Fits with under 10% headroom. Anything else that claims VRAM — a desktop session, a browser — could push this over. Reduce context if it fails to load.")

	default:
		fit.Verdict = VerdictPartial
		perLayerMB := fit.WeightsMB / float64(geo.layers)
		usable := fit.FreeVRAMMB - fit.OverheadMB - fit.KVCacheMB
		if usable < 0 {
			usable = 0
		}
		fit.GPULayers = int(usable / perLayerMB)
		if fit.GPULayers > geo.layers {
			fit.GPULayers = geo.layers
		}

		gpuFrac := float64(fit.GPULayers) / float64(geo.layers)
		ramBandwidth := float64(defaultRAMBandwidthGBs)
		if hw != nil && hw.RAMBandwidthGBs > 0 {
			ramBandwidth = hw.RAMBandwidthGBs
		}
		// Harmonic mean: the slow tier dominates. Even a 50/50 split lands
		// close to RAM speed, which is why partial offload disappoints people
		// who expect it to scale linearly.
		effective := 1 / (gpuFrac/bandwidth + (1-gpuFrac)/ramBandwidth)
		fit.TokensPerSec = throughput(effective, gpuEfficiency(gpu), readMB)

		if fit.GPULayers <= 0 {
			fit.Verdict = VerdictNo
			fit.Notes = append(fit.Notes,
				"Not even the KV cache and overhead fit in free VRAM. Reduce context, use a smaller quantization, or pick a smaller model.")
		} else {
			fit.Notes = append(fit.Notes, fmt.Sprintf(
				"Needs CPU offload: about %d of %d layers fit on the GPU (--n-gpu-layers %d). Expect a large slowdown.",
				fit.GPULayers, geo.layers, fit.GPULayers))
		}
	}

	addArchNotes(&fit, gpu, quant)
	return fit
}

// finishCPUOnly grades a machine with no usable GPU.
func unknownHostNote(hw *Hardware) string {
	if hw == nil {
		return "No hardware profile was supplied, so whether this fits cannot be judged. " +
			"The memory footprint above is a property of the model and holds anywhere."
	}
	if hw.Stale(time.Now()) {
		return fmt.Sprintf("%s last reported %s ago, so its free memory is not known well "+
			"enough to judge by. The footprint above holds anywhere; start the agent to get "+
			"a verdict back.", orHere(hw.Host), roundDuration(time.Since(hw.ReportedAt)))
	}
	if hw.Host != "" {
		return fmt.Sprintf("No backend is declared to run on %s, so it is not treated as a "+
			"machine that would run this model. Set \"node\": %q on the backend serving it.",
			hw.Host, hw.Host)
	}
	return "This server runs no local backend, so the models run on another machine " +
		"whose GPU and memory are not visible from here. The memory footprint above " +
		"is a property of the model and holds anywhere; whether it fits is unknown. " +
		"A fleet node can be graded instead by declaring which node a backend runs on."
}

// orHere names a host for a sentence, for the local machine as well as a node.
func orHere(host string) string {
	if host == "" {
		return "This server"
	}
	return host
}

// roundDuration renders an age the way someone reads it off a screen, not to
// the nanosecond.
func roundDuration(d time.Duration) time.Duration {
	if d < time.Minute {
		return d.Round(time.Second)
	}
	return d.Round(time.Minute)
}

func finishCPUOnly(fit Fit, in FitInput, hw *Hardware) Fit {
	ramMB := 0.0
	bandwidth := float64(defaultRAMBandwidthGBs)
	if hw != nil {
		ramMB = float64(hw.RAMAvailableMB)
		if hw.RAMBandwidthGBs > 0 {
			bandwidth = hw.RAMBandwidthGBs
		}
	}
	fit.GPULayers = 0
	readMB := fit.WeightsMB
	if in.ActiveParamsB > 0 && in.ActiveParamsB < in.ParamsB {
		readMB *= in.ActiveParamsB / in.ParamsB
	}
	fit.TokensPerSec = throughput(bandwidth, 0.6, readMB)

	if ramMB > 0 && fit.TotalMB > ramMB {
		fit.Verdict = VerdictNo
		fit.Notes = append(fit.Notes, "Exceeds available system RAM.")
		return fit
	}
	fit.Verdict = VerdictPartial
	fit.Notes = append(fit.Notes, "No GPU detected — this runs on the CPU, typically 10-30x slower than a GPU that fits the model.")
	return fit
}

// gpuEfficiency is the fraction of theoretical bandwidth a real decode loop
// achieves. Older architectures with no tensor cores sit lower.
func gpuEfficiency(g *GPU) float64 {
	switch g.Arch {
	case "Pascal":
		return 0.70
	case "Turing":
		return 0.75
	default:
		return 0.80
	}
}

// throughput converts bandwidth and model size into tokens per second.
// Generating one token reads the whole set of active weights once, so this is
// simply how many times per second that read can happen.
func throughput(bandwidthGBs, efficiency, readMB float64) float64 {
	if readMB <= 0 {
		return 0
	}
	readGB := readMB / 1024
	return bandwidthGBs * efficiency / readGB
}

func addArchNotes(fit *Fit, gpu *GPU, quant string) {
	if gpu == nil {
		return
	}
	if gpu.Arch == "Pascal" {
		switch quant {
		case "F16", "BF16", "F32":
			fit.Notes = append(fit.Notes,
				"This card runs FP16 at 1/64 of FP32. Use a k-quant such as Q4_K_M instead — it is both smaller and substantially faster here.")
		case "IQ4_XS", "IQ3_XS", "IQ3_XXS", "IQ4_NL":
			fit.Notes = append(fit.Notes,
				"i-quant kernels are less optimized on Pascal than k-quants. Q4_K_M is usually faster at a similar size; benchmark before committing.")
		}
	}
}

/* ---- grading against more than one machine ----

   A verdict was a single answer while there was a single machine to give it
   about. On a fleet it is one answer per host, and the question a reader
   actually has — "does anything I own run this?" — is answered by the best of
   them together with the name of the machine that earned it. */

// verdictRank orders verdicts from worst to best, so the most favourable one
// can be picked without spelling the comparison out at each call site.
//
// VerdictNo outranks VerdictUnknown deliberately. "This machine was measured
// and will not run it" is a fact the reader can act on; "nobody looked" is
// not, and letting an unmeasured host outrank a measured one would replace the
// only real answer available with silence.
func verdictRank(v Verdict) int {
	switch v {
	case VerdictFits:
		return 4
	case VerdictTight:
		return 3
	case VerdictPartial:
		return 2
	case VerdictNo:
		return 1
	default:
		return 0
	}
}

// EstimateFleetFit grades one model against every candidate machine, in the
// order given.
//
// With no candidates it still returns one result: the footprint is a property
// of the model and is worth having even when there is nothing to judge it
// against, and a caller that got an empty slice back would have to special-case
// the very situation the note inside already explains.
func EstimateFleetFit(in FitInput, hosts []*Hardware) []Fit {
	if len(hosts) == 0 {
		return []Fit{EstimateFit(in, nil)}
	}
	out := make([]Fit, 0, len(hosts))
	for _, hw := range hosts {
		out = append(out, EstimateFit(in, hw))
	}
	return out
}

// BestFit picks the most favourable verdict from a graded set, breaking ties on
// expected throughput — between two machines that both fit it, the faster one
// is the one worth naming.
func BestFit(fits []Fit) Fit {
	var best Fit
	for i, f := range fits {
		if i == 0 {
			best = f
			continue
		}
		switch {
		case verdictRank(f.Verdict) > verdictRank(best.Verdict):
			best = f
		case verdictRank(f.Verdict) == verdictRank(best.Verdict) && f.TokensPerSec > best.TokensPerSec:
			best = f
		}
	}
	return best
}
