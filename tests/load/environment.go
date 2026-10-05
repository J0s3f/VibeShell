package load

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Environment records the machine a receipt was produced on. PLAN 13 requires
// actual CPU/RAM to travel with the results, because a latency number without
// its host is not a benchmark.
type Environment struct {
	GoVersion     string `json:"go_version"`
	NumCPU        int    `json:"num_cpu"`
	GOMAXPROCS    int    `json:"gomaxprocs"`
	MemTotalBytes int64  `json:"mem_total_bytes"`
	CgroupCPUMax  string `json:"cgroup_cpu_max,omitempty"`
	CgroupMemMax  string `json:"cgroup_memory_max,omitempty"`
	Kernel        string `json:"kernel"`
	Arch          string `json:"arch"`
}

// ReadEnvironment collects the host facts the receipt must carry. Every probe
// is best effort: a missing cgroup file is recorded as an empty bound rather
// than guessed, so a missing measurement never becomes a fabricated one.
func ReadEnvironment() Environment {
	env := Environment{
		GoVersion:  runtime.Version(),
		NumCPU:     runtime.NumCPU(),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
		Arch:       runtime.GOARCH,
		Kernel:     kernelRelease(),
	}
	env.MemTotalBytes = memTotalBytes()
	env.CgroupCPUMax = readTrimmed("/sys/fs/cgroup/cpu.max")
	env.CgroupMemMax = readTrimmed("/sys/fs/cgroup/memory.max")
	return env
}

// kernelRelease reports the running kernel version, or "unknown".
func kernelRelease() string {
	var uts syscall.Utsname
	if err := syscall.Uname(&uts); err != nil {
		return "unknown"
	}
	return charsToString(uts.Release[:])
}

// charsToString converts a fixed-size syscall byte array into a Go string,
// stopping at the first NUL the C API uses as a terminator. The array element
// type follows the platform's C char, so the bytes are narrowed explicitly.
func charsToString[T ~int8 | ~byte](chars []T) string {
	out := make([]byte, 0, len(chars))
	for _, c := range chars {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}

// memTotalBytes reads MemTotal from /proc/meminfo, or 0 when unavailable.
func memTotalBytes() int64 {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0
			}
			return kb * 1024
		}
	}
	return 0
}

// readTrimmed returns the trimmed contents of path, or "" when it is absent.
func readTrimmed(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// sampleSink collects resource samples from several goroutines. Scenarios
// sample from inside their workers, so the collector owns the lock that keeps
// the receipt's sample list race-free.
type sampleSink struct {
	mu      sync.Mutex
	sampler *Sampler
	samples []ResourceSample
}

// newSampleSink returns a sink driven by sampler.
func newSampleSink(sampler *Sampler) *sampleSink {
	return &sampleSink{sampler: sampler}
}

// Add captures one sample under the given label.
func (s *sampleSink) Add(label string) {
	sample := s.sampler.Sample(label)
	s.mu.Lock()
	s.samples = append(s.samples, sample)
	s.mu.Unlock()
}

// Snapshot returns the collected samples in capture order.
func (s *sampleSink) Snapshot() []ResourceSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ResourceSample(nil), s.samples...)
}

// ResourceSample is one point-in-time resource observation. The harness takes
// these at scenario boundaries so growth between phases stays visible instead
// of being averaged away.
type ResourceSample struct {
	Label           string  `json:"label"`
	WallMillis      int64   `json:"wall_ms"`
	CPUMillis       int64   `json:"cpu_ms"`
	RSSBytes        int64   `json:"rss_bytes"`
	HeapAllocBytes  uint64  `json:"heap_alloc_bytes"`
	HeapInuseBytes  uint64  `json:"heap_inuse_bytes"`
	SysBytes        uint64  `json:"sys_bytes"`
	Goroutines      int     `json:"goroutines"`
	GCRuns          uint32  `json:"gc_runs"`
	GCPauseMillis   float64 `json:"gc_pause_ms"`
	NumGC           uint32  `json:"num_gc"`
	OpenConnections int     `json:"open_fds"`
}

// Sampler observes process resources. One sampler serves the whole run so the
// scenario measurements share a baseline.
type Sampler struct {
	started time.Time
}

// NewSampler starts the resource clock. All reported durations are relative to
// this call, so scenarios compose without hidden offsets.
func NewSampler() *Sampler { return &Sampler{started: time.Now()} }

// Sample captures the current process resources.
func (s *Sampler) Sample(label string) ResourceSample {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	var uts syscall.Rusage
	cpuNanos := int64(0)
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &uts); err == nil {
		cpuNanos = timevalNanos(uts.Utime) + timevalNanos(uts.Stime)
	}
	return ResourceSample{
		Label:           label,
		WallMillis:      time.Since(s.started).Milliseconds(),
		CPUMillis:       cpuNanos / int64(time.Millisecond),
		RSSBytes:        residentBytes(),
		HeapAllocBytes:  mem.HeapAlloc,
		HeapInuseBytes:  mem.HeapInuse,
		SysBytes:        mem.Sys,
		Goroutines:      runtime.NumGoroutine(),
		GCRuns:          mem.NumGC,
		NumGC:           mem.NumGC,
		GCPauseMillis:   float64(mem.PauseTotalNs) / float64(time.Millisecond),
		OpenConnections: openFileDescriptors(),
	}
}

// CPUMillis reports process CPU time consumed so far, for computing CPU cost
// per unit of work independently of wall clock.
func (s *Sampler) CPUMillis() int64 {
	var uts syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &uts); err != nil {
		return 0
	}
	return (timevalNanos(uts.Utime) + timevalNanos(uts.Stime)) / int64(time.Millisecond)
}

func timevalNanos(tv syscall.Timeval) int64 {
	return tv.Sec*int64(time.Second) + tv.Usec*int64(time.Microsecond)
}

// residentBytes reads VmRSS from /proc/self/status, or 0 when unavailable.
func residentBytes() int64 {
	file, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

// openFileDescriptors counts entries in /proc/self/fd, or 0 when unavailable.
// It is the harness's leak signal for file handles: a session that leaks a
// socket shows up here even when memory looks flat.
func openFileDescriptors() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	return len(entries)
}

// HumanBytes renders a byte count with a stable unit for receipt summaries. A
// negative value is signed rather than wrapped, because a shrinking measurement
// is a result, not an error.
func HumanBytes(n int64) string {
	const unit = 1024
	sign := ""
	if n < 0 {
		sign = "-"
		n = -n
	}
	if n < unit {
		return fmt.Sprintf("%s%d B", sign, n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%s%.1f %ciB", sign, float64(n)/float64(div), "KMGTP"[exp])
}
