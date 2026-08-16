package utilities

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Live resource sampling for the Utilities view's activity gauges.
//
// This came from morpheus, where it explained why an ffmpeg rename was taking
// five minutes. The question has the same shape here and is asked about a
// different job: a turn against a local model can sit for minutes, and "the
// machine is thinking" and "the machine is swapping" look identical from a
// browser. These are the three numbers that tell them apart — and on a box
// running inference they are what you want in front of you while a batch runs.
//
// Deliberately not the same thing as internal/models/hardware.go: that reports
// what the machine *is* (cores, memory, GPUs), this reports what it is *doing*.
//
// Everything is read from /proc, which costs a few file reads per poll and
// needs no privileges. On a system without /proc the fields stay zero rather
// than erroring: a missing gauge shouldn't fail the page.

// sectorSize is the unit /proc/diskstats counts transfers in. It is fixed at
// 512 bytes regardless of the device's real sector size.
const sectorSize = 512

// minSampleInterval guards against two polls landing close enough together
// that the deltas are dominated by rounding. Below this the previous result
// is returned again instead of a wild number.
const minSampleInterval = 250 * time.Millisecond

// sampleWindow is how far back a rate is averaged over. A single poll-to-poll
// delta is jumpy — ffmpeg and SMB both work in bursts, so a 2-second reading
// swings between nothing and the peak and is hard to read. Averaging over a
// few seconds gives a number that tracks what the machine is actually doing
// without lagging noticeably behind it.
const sampleWindow = 5 * time.Second

// ResourceSample is a point-in-time view of how hard the machine is working.
// The rates are averages over the interval since the previous sample, which
// is what makes them meaningful — a counter total would just grow forever.
type ResourceSample struct {
	CPUPercent float64 `json:"cpu_percent"`
	// Network and disk are bytes per second over the sampling interval.
	NetRxBytesPerSec     float64 `json:"net_rx_bytes_per_sec"`
	NetTxBytesPerSec     float64 `json:"net_tx_bytes_per_sec"`
	DiskReadBytesPerSec  float64 `json:"disk_read_bytes_per_sec"`
	DiskWriteBytesPerSec float64 `json:"disk_write_bytes_per_sec"`
	// Warming is true for the first sample, which has no predecessor to
	// measure against, so the UI can say so rather than showing a flat zero
	// that looks like an idle machine.
	Warming bool `json:"warming"`
}

// rawCounters are the cumulative values /proc exposes. Rates come from the
// difference between two of these.
type rawCounters struct {
	at        time.Time
	cpuTotal  uint64
	cpuIdle   uint64
	netRx     uint64
	netTx     uint64
	diskRead  uint64
	diskWrite uint64
}

// resourceSampler turns successive /proc readings into rates, averaged over
// sampleWindow. One instance is shared by every caller; the history it keeps
// is what decouples the reported average from how often the UI happens to
// poll.
type resourceSampler struct {
	mu sync.Mutex
	// history is ordered oldest-first and pruned to sampleWindow, so its
	// first entry is the furthest back a rate can be measured from.
	history []rawCounters
	last    ResourceSample
}

// Sample returns the current resource rates, averaged over the last
// sampleWindow (or over however much history exists, when the gauges have
// only just started).
func (s *resourceSampler) Sample() ResourceSample {
	s.mu.Lock()
	defer s.mu.Unlock()

	now, err := readCounters()
	if err != nil {
		return s.last // keep showing the last good values rather than blanking
	}

	// Drop readings that have fallen out of the window. A gap in polling
	// (nobody watching the page) clears the history entirely, so the gauges
	// warm up again rather than reporting an average over the idle period.
	cutoff := now.at.Add(-sampleWindow)
	kept := s.history[:0]
	for _, c := range s.history {
		if c.at.After(cutoff) {
			kept = append(kept, c)
		}
	}
	s.history = kept

	if len(s.history) == 0 {
		s.history = append(s.history, *now)
		s.last = ResourceSample{Warming: true}
		return s.last
	}

	prev := s.history[0]
	elapsed := now.at.Sub(prev.at)
	if elapsed < minSampleInterval {
		return s.last
	}
	s.history = append(s.history, *now)
	secs := elapsed.Seconds()

	out := ResourceSample{
		NetRxBytesPerSec:     perSec(now.netRx, prev.netRx, secs),
		NetTxBytesPerSec:     perSec(now.netTx, prev.netTx, secs),
		DiskReadBytesPerSec:  perSec(now.diskRead, prev.diskRead, secs),
		DiskWriteBytesPerSec: perSec(now.diskWrite, prev.diskWrite, secs),
	}
	// CPU is a ratio of jiffies, not a rate per second: the busy share of all
	// the time that passed across every core.
	if totalDelta := now.cpuTotal - prev.cpuTotal; totalDelta > 0 && now.cpuTotal >= prev.cpuTotal {
		idleDelta := now.cpuIdle - prev.cpuIdle
		if idleDelta > totalDelta {
			idleDelta = totalDelta
		}
		out.CPUPercent = float64(totalDelta-idleDelta) / float64(totalDelta) * 100
	}

	s.last = out
	return out
}

// perSec converts a pair of cumulative counter readings into a rate.
// Counters only climb, so a decrease means a wrap or a device that went
// away; report zero rather than a negative or absurd spike.
func perSec(now, prev uint64, secs float64) float64 {
	if now < prev || secs <= 0 {
		return 0
	}
	return float64(now-prev) / secs
}

// readCounters takes one reading of all three subsystems.
func readCounters() (*rawCounters, error) {
	c := &rawCounters{at: time.Now()}
	var err error
	if c.cpuTotal, c.cpuIdle, err = readCPU(); err != nil {
		return nil, err
	}
	c.netRx, c.netTx = readNet()
	c.diskRead, c.diskWrite = readDisk()
	return c, nil
}

// readCPU sums the aggregate "cpu" line of /proc/stat. idle includes iowait:
// a process blocked on the disk is not consuming CPU, and counting it as
// busy would make every large file copy look CPU-bound.
func readCPU() (total, idle uint64, err error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, fmt.Errorf("utilities: read /proc/stat: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		for i, f := range fields[1:] {
			v, convErr := strconv.ParseUint(f, 10, 64)
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
	return 0, 0, fmt.Errorf("utilities: no cpu line in /proc/stat")
}

// readNet sums received and transmitted bytes across every real interface.
// Loopback is excluded: it carries the app's own database traffic, which
// would otherwise show up as network load that never touches the wire.
func readNet() (rx, tx uint64) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		name, rest, found := strings.Cut(line, ":")
		if !found {
			continue // the two header lines
		}
		name = strings.TrimSpace(name)
		if name == "lo" || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "docker") {
			continue
		}
		fields := strings.Fields(rest)
		// Receive columns come first (bytes is 0), transmit bytes is 8.
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

// readDisk sums sectors read and written across whole disks.
//
// Only whole disks are counted, identified by having their own directory in
// /sys/block. Partitions appear in /proc/diskstats too, and their I/O is
// already included in the parent device's, so counting both would roughly
// double every figure.
func readDisk() (read, written uint64) {
	f, err := os.Open("/proc/diskstats")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		// major minor name reads merged sectors-read ms writes merged sectors-written …
		if len(fields) < 10 {
			continue
		}
		name := fields[2]
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") {
			continue
		}
		if _, err := os.Stat("/sys/block/" + name); err != nil {
			continue // a partition, or not a block device we should count
		}
		if v, err := strconv.ParseUint(fields[5], 10, 64); err == nil {
			read += v * sectorSize
		}
		if v, err := strconv.ParseUint(fields[9], 10, 64); err == nil {
			written += v * sectorSize
		}
	}
	return read, written
}
