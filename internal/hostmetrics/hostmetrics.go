// Package hostmetrics reads what a Linux machine is doing, from /proc.
//
// It exists to be used from two places that must not disagree. The server
// reports on its own box for the Utilities gauges; the node agent reports on a
// remote box for the fleet view. Both answer the same question — "how hard is
// this machine working" — and two implementations of it would eventually
// produce two different numbers for the same condition, which is worse than
// either being slightly wrong.
//
// Everything here is a file read with no privileges and no dependencies. That
// is deliberate: the node agent is a binary an operator will copy onto machines
// they care about, and the fewer things it links against, the less there is to
// think about before doing so. It is also why this reads /proc directly rather
// than pulling in a cross-platform metrics library — the hosts are Linux, the
// portability would be unused, and the parsing is a hundred lines.
//
// A field that cannot be read stays zero rather than failing the sample. A
// missing gauge is a missing gauge; a sample that errors because one counter
// was unavailable loses the other six as well.
package hostmetrics

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

// SectorSize is the unit /proc/diskstats counts transfers in. Fixed at 512
// bytes regardless of the device's real sector size.
const SectorSize = 512

// Counters are the cumulative values /proc exposes. Rates come from the
// difference between two readings.
type Counters struct {
	At        time.Time
	CPUTotal  uint64
	CPUIdle   uint64
	NetRx     uint64
	NetTx     uint64
	DiskRead  uint64
	DiskWrite uint64
}

// ReadCounters takes one reading of every cumulative counter.
func ReadCounters() (*Counters, error) {
	total, idle, err := ReadCPU()
	if err != nil {
		return nil, err
	}
	rx, tx := ReadNet()
	read, written := ReadDisk()
	return &Counters{
		At: time.Now(), CPUTotal: total, CPUIdle: idle,
		NetRx: rx, NetTx: tx, DiskRead: read, DiskWrite: written,
	}, nil
}

// ReadCPU returns the total and idle jiffy counts from /proc/stat.
//
// Idle includes iowait: a core waiting on a disk is not doing work, and
// counting it as busy would make a machine that is stalled look like one that
// is loaded — the exact confusion these numbers exist to resolve.
func ReadCPU() (total, idle uint64, err error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		for i, raw := range fields[1:] {
			v, convErr := strconv.ParseUint(raw, 10, 64)
			if convErr != nil {
				continue
			}
			total += v
			// Fields after "cpu" are user, nice, system, idle, iowait, …
			if i == 3 || i == 4 {
				idle += v
			}
		}
		return total, idle, nil
	}
	return 0, 0, scanner.Err()
}

// ReadNet returns cumulative received and transmitted bytes across every real
// interface.
//
// Loopback is excluded: on a box running inference it carries the traffic
// between the server and a local model, which would otherwise dominate the
// figure and say nothing about the network.
func ReadNet() (rx, tx uint64) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "lo" || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "docker") {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		if v, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
			rx += v
		}
		if v, err := strconv.ParseUint(fields[8], 10, 64); err == nil {
			tx += v
		}
	}
	return rx, tx
}

// ReadDisk returns cumulative bytes read and written across every physical
// device.
//
// Partitions and virtual devices are skipped so the same bytes are not counted
// twice — /proc/diskstats lists sda and sda1 side by side, and summing both
// doubles everything on that disk.
func ReadDisk() (read, written uint64) {
	f, err := os.Open("/proc/diskstats")
	if err != nil {
		return 0, 0
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 {
			continue
		}
		name := fields[2]
		if !physicalDevice(name) {
			continue
		}
		if v, err := strconv.ParseUint(fields[5], 10, 64); err == nil {
			read += v * SectorSize
		}
		if v, err := strconv.ParseUint(fields[9], 10, 64); err == nil {
			written += v * SectorSize
		}
	}
	return read, written
}

// physicalDevice reports whether a /proc/diskstats name is a whole device
// worth counting, as opposed to a partition or something virtual.
func physicalDevice(name string) bool {
	switch {
	case strings.HasPrefix(name, "loop"), strings.HasPrefix(name, "ram"),
		strings.HasPrefix(name, "dm-"), strings.HasPrefix(name, "zram"):
		return false
	}
	// A trailing digit on an sd/vd/hd name is a partition; nvme partitions are
	// spelled nvme0n1p1, so the "p" is what distinguishes them from the
	// namespace itself.
	switch {
	case strings.HasPrefix(name, "nvme"):
		return !strings.Contains(name[4:], "p")
	case strings.HasPrefix(name, "sd"), strings.HasPrefix(name, "vd"), strings.HasPrefix(name, "hd"):
		last := name[len(name)-1]
		return last < '0' || last > '9'
	}
	return true
}

// PerSecond converts a pair of cumulative readings into a rate.
//
// Counters only climb, so a decrease means a wrap or a device that went away.
// Report zero rather than a negative or an absurd spike.
func PerSecond(now, prev uint64, secs float64) float64 {
	if now < prev || secs <= 0 {
		return 0
	}
	return float64(now-prev) / secs
}

// BusyPercent turns two CPU readings into the busy share of elapsed time
// across every core.
func BusyPercent(now, prev Counters) float64 {
	totalDelta := now.CPUTotal - prev.CPUTotal
	if now.CPUTotal < prev.CPUTotal || totalDelta == 0 {
		return 0
	}
	idleDelta := now.CPUIdle - prev.CPUIdle
	if idleDelta > totalDelta {
		idleDelta = totalDelta
	}
	return float64(totalDelta-idleDelta) / float64(totalDelta) * 100
}

// Memory is what the machine's RAM is doing, in bytes.
//
// Used is derived from MemAvailable rather than from MemFree. Free counts only
// genuinely untouched pages and reads near zero on any machine that has been up
// a while, because Linux uses the rest as cache; MemAvailable is the kernel's
// own estimate of what a new allocation could actually get, which is the number
// that answers "is this box about to swap".
type Memory struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	UsedBytes      uint64 `json:"used_bytes"`
	SwapTotalBytes uint64 `json:"swap_total_bytes"`
	SwapUsedBytes  uint64 `json:"swap_used_bytes"`
}

// ReadMemory reads /proc/meminfo.
func ReadMemory() Memory {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return Memory{}
	}
	defer func() { _ = f.Close() }()

	values := map[string]uint64{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, rest, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		// meminfo is in kibibytes unless it says otherwise.
		values[key] = v * 1024
	}

	mem := Memory{
		TotalBytes:     values["MemTotal"],
		AvailableBytes: values["MemAvailable"],
		SwapTotalBytes: values["SwapTotal"],
	}
	if mem.TotalBytes >= mem.AvailableBytes {
		mem.UsedBytes = mem.TotalBytes - mem.AvailableBytes
	}
	if free := values["SwapFree"]; mem.SwapTotalBytes >= free {
		mem.SwapUsedBytes = mem.SwapTotalBytes - free
	}
	return mem
}

// LoadAverage is the kernel's run-queue average over one, five and fifteen
// minutes.
//
// Worth reporting beside CPU percentage rather than instead of it: a box at
// 100% with a load of 1 is working, and a box at 100% with a load of 40 is
// thrashing, and the percentage alone cannot tell them apart.
type LoadAverage struct {
	One     float64 `json:"one"`
	Five    float64 `json:"five"`
	Fifteen float64 `json:"fifteen"`
}

// ReadLoadAverage reads /proc/loadavg.
func ReadLoadAverage() LoadAverage {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return LoadAverage{}
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 3 {
		return LoadAverage{}
	}
	var out LoadAverage
	out.One, _ = strconv.ParseFloat(fields[0], 64)
	out.Five, _ = strconv.ParseFloat(fields[1], 64)
	out.Fifteen, _ = strconv.ParseFloat(fields[2], 64)
	return out
}

// ReadUptime returns how long the machine has been up.
func ReadUptime() time.Duration {
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}
