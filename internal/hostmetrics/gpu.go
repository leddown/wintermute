package hostmetrics

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// GPU telemetry, read by shelling out to nvidia-smi.
//
// Parsing a command's CSV output is not the tidiest way to get these numbers,
// and the alternative is worse for this program: NVML means cgo, which is the
// one thing the whole build deliberately avoids, and DCGM is a datacentre agent
// aimed at Kubernetes clusters. For consumer and prosumer cards on a home
// network — which is what this fleet is — nvidia-smi is what the established
// exporters use too, for the same reason.
//
// The fragility is real and bounded: the field list is requested explicitly, so
// a future nvidia-smi adding or reordering columns cannot silently shift the
// meaning of a value. A field that cannot be parsed stays zero rather than
// failing the reading, and a host with no NVIDIA driver simply reports no GPUs
// instead of erroring on every sample.

// GPU is one card's live state.
type GPU struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	// UtilPercent is the share of the last sampling period the card spent
	// executing. It is not the same as memory being full: a card can be at 0%
	// utilisation with 20 GB of weights resident, which is exactly what an
	// idle loaded model looks like.
	UtilPercent   float64 `json:"util_percent"`
	MemUsedBytes  int64   `json:"mem_used_bytes"`
	MemTotalBytes int64   `json:"mem_total_bytes"`
	TempC         float64 `json:"temp_c"`
	PowerWatts    float64 `json:"power_watts"`
}

// gpuQueryTimeout bounds the call. nvidia-smi normally answers in milliseconds
// but can block on a wedged driver, and a metrics agent must never hang on it.
const gpuQueryTimeout = 5 * time.Second

// ReadGPUs returns every NVIDIA card's live state.
//
// A machine with no NVIDIA driver — which is most of them — returns nothing and
// no error. That is not a failure worth reporting once every fifteen seconds
// for the life of the process.
func ReadGPUs(ctx context.Context) []GPU {
	ctx, cancel := context.WithTimeout(ctx, gpuQueryTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=index,name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw",
		"--format=csv,noheader,nounits")
	out, err := cmd.Output()
	if err != nil {
		return nil
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
		g := GPU{Name: fields[1]}
		g.Index, _ = strconv.Atoi(fields[0])
		g.UtilPercent = parseFloatOrZero(fields[2])
		// nvidia-smi reports memory in mebibytes under -nounits.
		g.MemUsedBytes = int64(parseFloatOrZero(fields[3])) * 1024 * 1024
		g.MemTotalBytes = int64(parseFloatOrZero(fields[4])) * 1024 * 1024
		g.TempC = parseFloatOrZero(fields[5])
		g.PowerWatts = parseFloatOrZero(fields[6])
		gpus = append(gpus, g)
	}
	return gpus
}

// parseFloatOrZero tolerates the placeholders nvidia-smi uses for values a card
// does not report — "[N/A]" and "[Not Supported]" are both common on consumer
// hardware, particularly for power draw.
func parseFloatOrZero(raw string) float64 {
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0
	}
	return v
}

// SummariseGPUs reduces a set of cards to the figures worth keeping in a time
// series.
//
// Utilisation and temperature are taken as maxima rather than means: with two
// cards where one is saturated and the other idle, the average says the machine
// is half busy, which is true of nothing and hides the card that is the actual
// constraint. Memory and power are summed, because those genuinely are totals
// for the box.
func SummariseGPUs(gpus []GPU) (utilMax, tempMax, powerSum float64, memUsed, memTotal int64) {
	for _, g := range gpus {
		if g.UtilPercent > utilMax {
			utilMax = g.UtilPercent
		}
		if g.TempC > tempMax {
			tempMax = g.TempC
		}
		powerSum += g.PowerWatts
		memUsed += g.MemUsedBytes
		memTotal += g.MemTotalBytes
	}
	return utilMax, tempMax, powerSum, memUsed, memTotal
}
