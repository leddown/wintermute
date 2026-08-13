package models

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GPU is one detected graphics device.
type GPU struct {
	Index      int    `json:"index"`
	Name       string `json:"name"`
	TotalMB    int    `json:"total_mb"`
	UsedMB     int    `json:"used_mb"`
	FreeMB     int    `json:"free_mb"`
	ComputeCap string `json:"compute_cap,omitempty"`
	Driver     string `json:"driver,omitempty"`
	// BandwidthGBs is memory bandwidth, looked up from the table below. Token
	// generation is memory-bound, so this is the single number that predicts
	// throughput — not TFLOPS, and not core count.
	BandwidthGBs float64 `json:"bandwidth_gbs,omitempty"`
	Arch         string  `json:"arch,omitempty"`
	// Notes carries architecture-specific advice worth surfacing, e.g. that
	// Pascal's FP16 rate makes half-precision models a trap.
	Notes []string `json:"notes,omitempty"`
}

// NPU is a detected neural accelerator that is not a GPU.
type NPU struct {
	Vendor string `json:"vendor"`
	Device string `json:"device"`
	// LLMCapable distinguishes a part that can run language models from a
	// vision-only inference accelerator. Getting this wrong would be actively
	// misleading: a Hailo-8 is a fine object detector and cannot run an LLM at
	// all.
	LLMCapable bool   `json:"llm_capable"`
	Note       string `json:"note,omitempty"`
}

// Hardware is the host's inference-relevant capability.
type Hardware struct {
	GPUs             []GPU     `json:"gpus"`
	NPUs             []NPU     `json:"npus,omitempty"`
	CPUModel         string    `json:"cpu_model,omitempty"`
	CPUCores         int       `json:"cpu_cores,omitempty"`
	RAMTotalMB       int       `json:"ram_total_mb,omitempty"`
	RAMAvailableMB   int       `json:"ram_available_mb,omitempty"`
	RAMBandwidthGBs  float64   `json:"ram_bandwidth_gbs,omitempty"`
	DetectedAt       time.Time `json:"detected_at"`
	Warnings         []string  `json:"warnings,omitempty"`
	NvidiaSMIPresent bool      `json:"nvidia_smi_present"`

	// RunsInference reports whether any configured non-cloud backend actually
	// runs on this host. When it is false everything above is still true about
	// the machine serving this API, and says nothing about the machine running
	// the models — so no fit estimate may be computed from it.
	//
	// DetectHardware cannot know this; it is set by the Catalog, which is the
	// only thing that sees both the host and the backend list.
	RunsInference bool `json:"runs_inference"`
}

// PrimaryGPU returns the GPU with the most total memory, or nil.
func (h *Hardware) PrimaryGPU() *GPU {
	var best *GPU
	for i := range h.GPUs {
		if best == nil || h.GPUs[i].TotalMB > best.TotalMB {
			best = &h.GPUs[i]
		}
	}
	return best
}

// gpuSpec is the static data nvidia-smi does not report.
type gpuSpec struct {
	bandwidthGBs float64
	arch         string
	notes        []string
}

// pascalNotes is attached to every Pascal card, because the two facts in it
// invalidate most generic local-LLM advice.
var pascalNotes = []string{
	"Pascal runs FP16 at 1/64 of FP32 — never run an unquantized F16 model on this card.",
	"Pascal has fast INT8 (dp4a), which llama.cpp's MMQ kernels use: k-quants like Q4_K_M are both the smallest and the fastest option.",
	"The NVIDIA 580 driver branch is the last that supports this card, and CUDA 13 cannot compile for it — build llama.cpp against CUDA 12.x with -DCMAKE_CUDA_ARCHITECTURES=61.",
}

// gpuSpecs maps a substring of the reported device name to its specification.
// Longest match wins, so "1070 ti" beats "1070".
var gpuSpecs = map[string]gpuSpec{
	"gtx 1060":    {192, "Pascal", pascalNotes},
	"gtx 1070 ti": {256, "Pascal", pascalNotes},
	"gtx 1070":    {256, "Pascal", pascalNotes},
	"gtx 1080 ti": {484, "Pascal", pascalNotes},
	"gtx 1080":    {320, "Pascal", pascalNotes},
	"titan xp":    {548, "Pascal", pascalNotes},
	"gtx 1660":    {192, "Turing", nil},
	"rtx 2060":    {336, "Turing", nil},
	"rtx 2070":    {448, "Turing", nil},
	"rtx 2080 ti": {616, "Turing", nil},
	"rtx 3060":    {360, "Ampere", nil},
	"rtx 3070":    {448, "Ampere", nil},
	"rtx 3080":    {760, "Ampere", nil},
	"rtx 3090":    {936, "Ampere", nil},
	"rtx 4060":    {272, "Ada", nil},
	"rtx 4070":    {504, "Ada", nil},
	"rtx 4080":    {717, "Ada", nil},
	"rtx 4090":    {1008, "Ada", nil},
	"rtx 5070":    {672, "Blackwell", nil},
	"rtx 5080":    {960, "Blackwell", nil},
	"rtx 5090":    {1792, "Blackwell", nil},
	"a100":        {1555, "Ampere", nil},
	"h100":        {3350, "Hopper", nil},
	"l4":          {300, "Ada", nil},
	"tesla p40":   {346, "Pascal", pascalNotes},
}

// defaultBandwidthGBs is used for an unrecognised card. It is deliberately
// conservative so throughput estimates under-promise rather than over-promise.
const defaultBandwidthGBs = 200

// defaultRAMBandwidthGBs approximates dual-channel DDR4, which is what governs
// throughput for any layer that spills off the GPU.
const defaultRAMBandwidthGBs = 40

func lookupGPUSpec(name string) gpuSpec {
	lower := strings.ToLower(name)
	best := gpuSpec{bandwidthGBs: defaultBandwidthGBs}
	bestLen := 0
	for key, spec := range gpuSpecs {
		if strings.Contains(lower, key) && len(key) > bestLen {
			best, bestLen = spec, len(key)
		}
	}
	return best
}

// DetectHardware inspects the host. Every probe is best-effort: a missing tool
// or an unreadable file becomes a warning, never an error, because the server
// must start on a machine with no GPU at all.
func DetectHardware(ctx context.Context) *Hardware {
	hw := &Hardware{
		DetectedAt:      time.Now().UTC(),
		RAMBandwidthGBs: defaultRAMBandwidthGBs,
	}

	if gpus, err := detectNvidia(ctx); err != nil {
		hw.Warnings = append(hw.Warnings, "no NVIDIA GPU detected: "+err.Error())
	} else {
		hw.NvidiaSMIPresent = true
		hw.GPUs = gpus
	}

	hw.NPUs = detectNPUs(ctx)
	readMemInfo(hw)
	readCPUInfo(hw)

	if len(hw.GPUs) == 0 {
		hw.Warnings = append(hw.Warnings,
			"no GPU found — models will run on CPU, which is typically 10-30x slower")
	}
	return hw
}

// detectNvidia shells out to nvidia-smi. Parsing its CSV output is the only
// dependency-free way to get free VRAM, and free VRAM is what the fit
// calculator needs — total VRAM overstates what is actually available once a
// desktop session has taken its share.
func detectNvidia(ctx context.Context) ([]GPU, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=index,name,memory.total,memory.used,memory.free,compute_cap,driver_version",
		"--format=csv,noheader,nounits")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var gpus []GPU
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 7 {
			continue
		}
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		g := GPU{
			Index:      atoi(fields[0]),
			Name:       fields[1],
			TotalMB:    atoi(fields[2]),
			UsedMB:     atoi(fields[3]),
			FreeMB:     atoi(fields[4]),
			ComputeCap: fields[5],
			Driver:     fields[6],
		}
		spec := lookupGPUSpec(g.Name)
		g.BandwidthGBs = spec.bandwidthGBs
		g.Arch = spec.arch
		g.Notes = spec.notes
		gpus = append(gpus, g)
	}
	return gpus, nil
}

// detectNPUs looks for non-GPU accelerators. Only Hailo is handled today.
func detectNPUs(ctx context.Context) []NPU {
	devices, _ := filepath.Glob("/dev/hailo*")
	if len(devices) == 0 {
		return nil
	}

	npu := NPU{Vendor: "Hailo", Device: strings.Join(devices, ", ")}

	// hailortcli names the part, which decides everything: only the 10H can
	// run language models. Absent the tool we stay honest and say we cannot
	// tell rather than assuming the capable one.
	if id := hailoIdentify(ctx); id != "" {
		npu.Device = id
		lower := strings.ToLower(id)
		switch {
		case strings.Contains(lower, "hailo10") || strings.Contains(lower, "hailo-10"):
			npu.LLMCapable = true
			npu.Note = "Hailo-10H: runs small LLMs/VLMs (~2B class) at roughly 10 tok/s in a 2.5W envelope."
		case strings.Contains(lower, "hailo8") || strings.Contains(lower, "hailo-8"):
			npu.Note = "Hailo-8/8L is a vision inference accelerator and cannot run language models. Excellent as a Frigate object detector — see docs/vision-monitoring.md."
		default:
			npu.Note = "Hailo device present; model unrecognised."
		}
	} else {
		npu.Note = "Hailo device node present but hailortcli is not installed, so the part could not be identified."
	}
	return []NPU{npu}
}

func hailoIdentify(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "hailortcli", "fw-control", "identify").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(strings.ToLower(line), "device architecture") {
			if _, value, ok := strings.Cut(line, ":"); ok {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func readMemInfo(hw *Hardware) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		hw.Warnings = append(hw.Warnings, "could not read /proc/meminfo: "+err.Error())
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		kb := atoi(strings.TrimSuffix(strings.TrimSpace(value), " kB"))
		switch key {
		case "MemTotal":
			hw.RAMTotalMB = kb / 1024
		case "MemAvailable":
			hw.RAMAvailableMB = kb / 1024
		}
	}
}

func readCPUInfo(hw *Hardware) {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "model name":
			if hw.CPUModel == "" {
				hw.CPUModel = strings.TrimSpace(value)
			}
		case "processor":
			hw.CPUCores++
		}
	}
}

func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}
