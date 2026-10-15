package monitor

import (
	"math"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/cedar2025/xboard-node/internal/nlog"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
)

var startTime = time.Now()

func init() {
	// Seed the CPU baseline so the first Collect() returns real values.
	// (Previously a blocking 500ms cpu.Percent sample at startup.)
	collectCPU()

	// Seed the network baseline so the first Collect() can compute rates.
	collectNetSpeed()
}

// Status holds system resource metrics
type Status struct {
	Uptime     uint64
	CPU        float64
	CPUPerCore []float64
	Load1      float64
	Load5      float64
	Load15     float64
	MemTotal   uint64
	MemUsed    uint64
	SwapTotal  uint64
	SwapUsed   uint64
	DiskTotal  uint64
	DiskUsed   uint64
	Goroutines int

	// Network speed (bytes/sec), -1 means unavailable (first sample).
	NetInSpeed  float64
	NetOutSpeed float64

	// GC metrics (process-wide)
	NumGC       uint32
	LastPauseMS float64
}

// netBaseline tracks the previous network counters for rate calculation.
var (
	netMu       sync.Mutex
	netPrevRecv uint64
	netPrevSent uint64
	netPrevTime time.Time
	netHasBase  bool
)

// skipInterface returns true for loopback and common virtual interfaces.
func skipInterface(name string) bool {
	lower := strings.ToLower(name)
	for _, prefix := range []string{"lo", "docker", "veth", "br-", "virbr", "vnet", "tun", "tap"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// collectNetSpeed calculates network in/out bytes per second since last call.
// Returns -1, -1 on first call or if counters decreased (reboot).
func collectNetSpeed() (inSpeed, outSpeed float64) {
	counters, err := net.IOCounters(true) // per-interface
	if err != nil {
		nlog.Core().Debug("failed to get network counters", "error", err)
		return -1, -1
	}

	var totalRecv, totalSent uint64
	for _, c := range counters {
		if skipInterface(c.Name) {
			continue
		}
		totalRecv += c.BytesRecv
		totalSent += c.BytesSent
	}

	now := time.Now()

	netMu.Lock()
	defer netMu.Unlock()

	if !netHasBase {
		netPrevRecv, netPrevSent, netPrevTime, netHasBase = totalRecv, totalSent, now, true
		return -1, -1
	}

	elapsed := now.Sub(netPrevTime).Seconds()
	if elapsed <= 0 {
		return -1, -1
	}

	// Counter decreased → system reboot or interface reset; reset baseline.
	if totalRecv < netPrevRecv || totalSent < netPrevSent {
		netPrevRecv, netPrevSent, netPrevTime = totalRecv, totalSent, now
		return -1, -1
	}

	inSpeed = float64(totalRecv-netPrevRecv) / elapsed
	outSpeed = float64(totalSent-netPrevSent) / elapsed

	netPrevRecv, netPrevSent, netPrevTime = totalRecv, totalSent, now
	return inSpeed, outSpeed
}

// gcCache caches the last GC metrics to avoid calling ReadMemStats (STW) on
// every Collect() invocation. We only refresh when NumGC actually increases.
var (
	gcMu          sync.Mutex
	gcLastNumGC   uint32
	gcLastPauseMS float64
)

// readGCMetrics returns GC count and last pause duration.
// Uses debug.ReadGCStats (cheaper than runtime.ReadMemStats, no STW) to check
// if NumGC changed, and only calls the full STW ReadMemStats when it has.
func readGCMetrics() (numGC uint32, lastPauseMS float64) {
	var stats debug.GCStats
	debug.ReadGCStats(&stats) // no STW, reads from already-maintained GC stats

	currentNumGC := uint32(stats.NumGC)

	gcMu.Lock()
	defer gcMu.Unlock()

	if currentNumGC == gcLastNumGC {
		return gcLastNumGC, gcLastPauseMS
	}

	// NumGC changed — read the pause duration from GCStats (already in stats)
	if len(stats.Pause) > 0 {
		gcLastPauseMS = float64(stats.Pause[0]) / float64(time.Millisecond)
	}
	gcLastNumGC = currentNumGC
	return gcLastNumGC, gcLastPauseMS
}

// cpuMu guards the per-core baseline used by collectCPU.
var (
	cpuMu   sync.Mutex
	cpuPrev []cpu.TimesStat
)

// collectCPU reads /proc/stat once (per-core) and derives both the overall
// and per-core usage since the previous call, instead of two cpu.Percent
// calls that each parse /proc/stat.
func collectCPU() (total float64, perCore []float64) {
	times, err := cpu.Times(true)
	if err != nil || len(times) == 0 {
		if err != nil {
			nlog.Core().Debug("failed to get CPU usage", "error", err)
		}
		return 0, nil
	}

	cpuMu.Lock()
	prev := cpuPrev
	cpuPrev = times
	cpuMu.Unlock()

	if len(prev) != len(times) {
		return 0, nil
	}

	perCore = make([]float64, len(times))
	var busySum, allSum float64
	for i := range times {
		b1, a1 := cpuBusyAll(prev[i])
		b2, a2 := cpuBusyAll(times[i])
		db, da := b2-b1, a2-a1
		if da > 0 && db >= 0 {
			perCore[i] = math.Min(100, db/da*100)
			busySum += db
			allSum += da
		}
	}
	if allSum > 0 {
		total = math.Min(100, busySum/allSum*100)
	}
	return total, perCore
}

// cpuBusyAll mirrors gopsutil's busy/total accounting (guest time is already
// included in user/nice on Linux).
func cpuBusyAll(t cpu.TimesStat) (busy, all float64) {
	all = t.User + t.System + t.Idle + t.Nice + t.Iowait + t.Irq + t.Softirq + t.Steal
	busy = all - t.Idle - t.Iowait
	return busy, all
}

// Collect gathers current system metrics
func Collect() Status {
	var s Status

	s.Uptime = uint64(time.Since(startTime).Seconds())

	s.CPU, s.CPUPerCore = collectCPU()

	if loadAvg, err := load.Avg(); err == nil {
		s.Load1 = loadAvg.Load1
		s.Load5 = loadAvg.Load5
		s.Load15 = loadAvg.Load15
	}

	if vmStat, err := mem.VirtualMemory(); err == nil {
		s.MemTotal = vmStat.Total
		s.MemUsed = vmStat.Used
	}

	if swapStat, err := mem.SwapMemory(); err == nil {
		s.SwapTotal = swapStat.Total
		s.SwapUsed = swapStat.Used
	}

	if diskStat, err := disk.Usage("/"); err == nil {
		s.DiskTotal = diskStat.Total
		s.DiskUsed = diskStat.Used
	}

	s.NetInSpeed, s.NetOutSpeed = collectNetSpeed()

	// GC metrics — only triggers STW when NumGC has changed (cheap otherwise)
	s.Goroutines = runtime.NumGoroutine()
	s.NumGC, s.LastPauseMS = readGCMetrics()

	return s
}
