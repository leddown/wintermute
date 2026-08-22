package utilities

import (
	"sync"
	"time"

	"wintermute/internal/hostmetrics"
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
// The /proc reading itself lives in internal/hostmetrics, shared with the node
// agent that reports the same numbers from a remote box. Two implementations of
// "how busy is this machine" would eventually disagree about the same
// condition, which is worse than either being slightly wrong.
//
// Everything is read from /proc, which costs a few file reads per poll and
// needs no privileges. On a system without /proc the fields stay zero rather
// than erroring: a missing gauge shouldn't fail the page.

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

// resourceSampler turns successive /proc readings into rates, averaged over
// sampleWindow. One instance is shared by every caller; the history it keeps
// is what decouples the reported average from how often the UI happens to
// poll.
type resourceSampler struct {
	mu sync.Mutex
	// history is ordered oldest-first and pruned to sampleWindow, so its
	// first entry is the furthest back a rate can be measured from.
	history []hostmetrics.Counters
	last    ResourceSample
}

// Sample returns the current resource rates, averaged over the last
// sampleWindow (or over however much history exists, when the gauges have
// only just started).
func (s *resourceSampler) Sample() ResourceSample {
	s.mu.Lock()
	defer s.mu.Unlock()

	now, err := hostmetrics.ReadCounters()
	if err != nil {
		return s.last // keep showing the last good values rather than blanking
	}

	// Drop readings that have fallen out of the window. A gap in polling
	// (nobody watching the page) clears the history entirely, so the gauges
	// warm up again rather than reporting an average over the idle period.
	cutoff := now.At.Add(-sampleWindow)
	kept := s.history[:0]
	for _, c := range s.history {
		if c.At.After(cutoff) {
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
	elapsed := now.At.Sub(prev.At)
	if elapsed < minSampleInterval {
		return s.last
	}
	s.history = append(s.history, *now)
	secs := elapsed.Seconds()

	out := ResourceSample{
		NetRxBytesPerSec:     hostmetrics.PerSecond(now.NetRx, prev.NetRx, secs),
		NetTxBytesPerSec:     hostmetrics.PerSecond(now.NetTx, prev.NetTx, secs),
		DiskReadBytesPerSec:  hostmetrics.PerSecond(now.DiskRead, prev.DiskRead, secs),
		DiskWriteBytesPerSec: hostmetrics.PerSecond(now.DiskWrite, prev.DiskWrite, secs),
		// CPU is a ratio of jiffies, not a rate per second: the busy share of
		// all the time that passed across every core.
		CPUPercent: hostmetrics.BusyPercent(*now, prev),
	}

	s.last = out
	return out
}
