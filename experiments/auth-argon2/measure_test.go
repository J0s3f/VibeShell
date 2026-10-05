package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
)

// Measurements (calibration, timing gap, burst throughput) are gated behind
// VIBESHELL_AUTH_MEASURE so the routine test suite stays fast; receipts record
// the gated runs. They run without -race because instrumentation would
// dominate the wall-clock numbers being measured.
const measureEnv = "VIBESHELL_AUTH_MEASURE"

// recommendedParams is the candidate measured for the timing-gap and burst
// reports. It matches the recommendation TestCalibrationReport derives from
// this container (PLAN 4.2); the spike doc records the calibration table that
// justifies it.
var recommendedParams = Params{MemoryKiB: 131072, Iterations: 3, Parallelism: 4}

func requireMeasure(t *testing.T) {
	t.Helper()
	if os.Getenv(measureEnv) == "" {
		t.Skipf("set %s=1 to run measurements", measureEnv)
	}
}

type durationStats struct {
	N      int
	Min    time.Duration
	Median time.Duration
	Mean   time.Duration
	Max    time.Duration
}

func summarize(samples []time.Duration) durationStats {
	if len(samples) == 0 {
		return durationStats{}
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	return durationStats{
		N:      len(sorted),
		Min:    sorted[0],
		Median: sorted[len(sorted)/2],
		Mean:   total / time.Duration(len(sorted)),
		Max:    sorted[len(sorted)-1],
	}
}

func (s durationStats) report() string {
	return fmt.Sprintf("n=%d min=%.3fms median=%.3fms mean=%.3fms max=%.3fms",
		s.N, ms(s.Min), ms(s.Median), ms(s.Mean), ms(s.Max))
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// TestCalibrationReport measures candidate Argon2id parameter sets and
// reports which land in the 100-250 ms target for a login.
func TestCalibrationReport(t *testing.T) {
	requireMeasure(t)

	candidates := []Params{
		{MemoryKiB: 16384, Iterations: 2, Parallelism: 1},  // 16 MiB
		{MemoryKiB: 19456, Iterations: 2, Parallelism: 1},  // 19 MiB, OWASP-style minimum
		{MemoryKiB: 32768, Iterations: 3, Parallelism: 2},  // 32 MiB
		{MemoryKiB: 65536, Iterations: 3, Parallelism: 3},  // 64 MiB
		{MemoryKiB: 65536, Iterations: 4, Parallelism: 4},  // 64 MiB, more passes
		{MemoryKiB: 131072, Iterations: 3, Parallelism: 4}, // 128 MiB
		{MemoryKiB: 131072, Iterations: 4, Parallelism: 4}, // 128 MiB, more passes
		{MemoryKiB: 196608, Iterations: 3, Parallelism: 4}, // 192 MiB
		{MemoryKiB: 262144, Iterations: 3, Parallelism: 4}, // 256 MiB
	}
	const (
		warmups = 2
		samples = 7
	)
	password := []byte(fakePassword)
	salt := testSalt
	out := make([]byte, DefaultHashBytes)

	t.Logf("host: NumCPU=%d GOMAXPROCS=%d", runtime.NumCPU(), runtime.GOMAXPROCS(0))
	t.Logf("target: median duration in [100ms, 250ms]")
	t.Logf("%-32s %-12s %s", "parameters", "hash size", "duration")

	recommended := Params{}
	for _, candidate := range candidates {
		for i := 0; i < warmups; i++ {
			argon2.IDKey(password, salt, candidate.Iterations, candidate.MemoryKiB, candidate.Parallelism, uint32(len(out)))
		}
		durations := make([]time.Duration, 0, samples)
		for i := 0; i < samples; i++ {
			start := time.Now()
			argon2.IDKey(password, salt, candidate.Iterations, candidate.MemoryKiB, candidate.Parallelism, uint32(len(out)))
			durations = append(durations, time.Since(start))
		}
		stats := summarize(durations)
		label := fmt.Sprintf("m=%d,t=%d,p=%d", candidate.MemoryKiB, candidate.Iterations, candidate.Parallelism)
		mark := ""
		if stats.Median >= 100*time.Millisecond && stats.Median <= 250*time.Millisecond {
			mark = "  <- in target"
			if recommended == (Params{}) {
				recommended = candidate
			}
		}
		t.Logf("%-32s %-12s %s%s", label, fmt.Sprintf("%d MiB", candidate.MemoryKiB/1024), stats.report(), mark)
	}
	if recommended == (Params{}) {
		t.Fatal("no candidate landed in the 100-250ms target")
	}
	t.Logf("recommended default: m=%d,t=%d,p=%d", recommended.MemoryKiB, recommended.Iterations, recommended.Parallelism)
}

// TestTimingGapReport measures how well dummy verification hides the
// unknown-user case: known user (correct and wrong password) versus unknown
// user (dummy hash), plus the naive lookup-miss path for comparison.
func TestTimingGapReport(t *testing.T) {
	requireMeasure(t)

	const rounds = 25
	params := recommendedParams
	salt := testSalt

	verifier, err := NewVerifier(params, salt)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	hash := mustEncodeWith(t, fakePassword, params, salt)
	disabled := mustEncodeWith(t, fakePassword, params, salt)

	store := &FileStore{}
	file := PasswordFile{
		Schema:  PasswordFileSchema,
		Version: PasswordFileVersion,
		Users: []UserEntry{
			{Username: "alice", Hash: hash.String(), Enabled: true, IdentityRef: "sec-identity-alice"},
			{Username: "bob", Hash: disabled.String(), Enabled: false, IdentityRef: "sec-identity-bob"},
		},
	}
	if err := store.Load(mustEncodeFile(t, file)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	admission, err := NewAdmission(AdmissionConfig{MaxConcurrent: 4, MaxWaiting: 8})
	if err != nil {
		t.Fatalf("NewAdmission: %v", err)
	}
	abuse, err := NewAbuseGuard(AbuseConfig{
		Window: time.Hour, Threshold: 1 << 20, Penalty: time.Minute, MaxTrackedSources: 8,
	}, time.Now)
	if err != nil {
		t.Fatalf("NewAbuseGuard: %v", err)
	}
	auth := NewAuthenticator(store, verifier, admission, abuse)
	limiter, err := NewConnectionLimiter(ConnectionConfig{MaxAttempts: 1 << 20}, time.Now)
	if err != nil {
		t.Fatalf("NewConnectionLimiter: %v", err)
	}
	ctx := context.Background()

	correct := make([]time.Duration, 0, rounds)
	wrong := make([]time.Duration, 0, rounds)
	unknown := make([]time.Duration, 0, rounds)
	naive := make([]time.Duration, 0, rounds)

	// Rounds are interleaved so host load drift affects all conditions
	// equally.
	for round := 0; round < rounds; round++ {
		start := time.Now()
		if err := auth.Authenticate(ctx, "198.51.100.1:4000", limiter, "alice", []byte(fakePassword)); err != nil {
			t.Fatalf("correct password rejected: %v", err)
		}
		correct = append(correct, time.Since(start))

		start = time.Now()
		if err := auth.Authenticate(ctx, "198.51.100.2:4001", limiter, "alice", []byte("wrong-password-fake")); err == nil {
			t.Fatal("wrong password accepted")
		}
		wrong = append(wrong, time.Since(start))

		start = time.Now()
		if err := auth.Authenticate(ctx, "198.51.100.3:4002", limiter, "ghost", []byte(fakePassword)); err == nil {
			t.Fatal("unknown user accepted")
		}
		unknown = append(unknown, time.Since(start))

		start = time.Now()
		if _, found := file.Find("ghost"); found {
			t.Fatal("ghost found")
		}
		naive = append(naive, time.Since(start))
	}

	correctStats := summarize(correct)
	wrongStats := summarize(wrong)
	unknownStats := summarize(unknown)
	naiveStats := summarize(naive)

	t.Logf("params: m=%d,t=%d,p=%d (%d rounds, interleaved)", params.MemoryKiB, params.Iterations, params.Parallelism, rounds)
	t.Logf("known user, correct password : %s", correctStats.report())
	t.Logf("known user, wrong password   : %s", wrongStats.report())
	t.Logf("unknown user (dummy verify)  : %s", unknownStats.report())
	t.Logf("naive lookup miss (no dummy) : %s", naiveStats.report())

	dummyGap := absDuration(correctStats.Mean - unknownStats.Mean)
	wrongGap := absDuration(wrongStats.Mean - unknownStats.Mean)
	naiveGap := absDuration(correctStats.Mean - naiveStats.Mean)
	t.Logf("timing gap, unknown vs correct (dummy path) : %.3fms", ms(dummyGap))
	t.Logf("timing gap, unknown vs wrong   (dummy path) : %.3fms", ms(wrongGap))
	t.Logf("timing gap, unknown vs correct (naive path) : %.3fms", ms(naiveGap))

	// The dummy path must absorb most of the Argon2id cost; a gap above half
	// the real verification means the dummy work is missing or mispriced.
	limit := correctStats.Mean / 2
	if dummyGap > limit {
		t.Errorf("dummy-verify timing gap %.3fms exceeds half the real verification (%.3fms)",
			ms(dummyGap), ms(limit))
	}
	if naiveStats.Mean > correctStats.Mean/4 {
		t.Errorf("naive lookup-miss path unexpectedly slow: %s", naiveStats.report())
	}
}

// TestBurstThroughputReport measures throughput, peak concurrency, and memory
// under a bounded burst of authentication attempts at two admission bounds,
// and the cost of the abusive-rate fast rejection.
func TestBurstThroughputReport(t *testing.T) {
	requireMeasure(t)

	params := recommendedParams
	t.Logf("params: m=%d,t=%d,p=%d", params.MemoryKiB, params.Iterations, params.Parallelism)
	// Two bounds show how much concurrent Argon2id work the 4-CPU reference
	// shape actually absorbs: each hash already runs p=4 lanes, so
	// maxConcurrent=4 means 16 runnable lanes on 4 CPUs.
	for _, maxConcurrent := range []int{4, 2} {
		result := runBurst(t, maxConcurrent)
		t.Logf("maxConcurrent=%d: %d attempts (%d correct, %d wrong) wall=%.0fms throughput=%.1f auth/s",
			maxConcurrent, result.attempts, result.correct, result.wrong,
			ms(result.wall), float64(result.attempts)/result.wall.Seconds())
		t.Logf("maxConcurrent=%d: peak in-flight=%d heap before=%dKiB peak=%dKiB after=%dKiB sys=%dKiB",
			maxConcurrent, result.peakInFlight,
			result.heapBefore/1024, result.heapPeak/1024, result.heapAfter/1024, result.sysAfter/1024)
	}
	measureAbuseFastPath(t)
}

type burstResult struct {
	attempts     int
	correct      int
	wrong        int
	wall         time.Duration
	peakInFlight uint64
	heapBefore   uint64
	heapPeak     uint64
	heapAfter    uint64
	sysAfter     uint64
}

func runBurst(t *testing.T, maxConcurrent int) burstResult {
	t.Helper()
	const (
		attempts     = 64
		correctShare = 32
	)
	params := recommendedParams

	verifier, err := NewVerifier(params, testSalt)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	store := &FileStore{}
	if err := store.Load(mustEncodeFile(t, fixtureFileWithParams(t, params, "alice"))); err != nil {
		t.Fatalf("Load: %v", err)
	}
	admission, err := NewAdmission(AdmissionConfig{MaxConcurrent: maxConcurrent, MaxWaiting: attempts})
	if err != nil {
		t.Fatalf("NewAdmission: %v", err)
	}
	abuse, err := NewAbuseGuard(DefaultAbuseConfig(), time.Now)
	if err != nil {
		t.Fatalf("NewAbuseGuard: %v", err)
	}
	auth := NewAuthenticator(store, verifier, admission, abuse)
	ctx := context.Background()

	// Warm the code paths and page in the binaries.
	warmLimiter, err := NewConnectionLimiter(ConnectionConfig{MaxAttempts: 6}, time.Now)
	if err != nil {
		t.Fatalf("NewConnectionLimiter: %v", err)
	}
	if err := auth.Authenticate(ctx, "198.51.100.9:9", warmLimiter, "alice", []byte(fakePassword)); err != nil {
		t.Fatalf("warmup: %v", err)
	}

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	var peakHeap, peakInFlight atomic.Uint64
	peakHeap.Store(before.HeapAlloc)
	samplerDone := make(chan struct{})
	samplerStopped := make(chan struct{})
	go func() {
		defer close(samplerStopped)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-samplerDone:
				return
			case <-ticker.C:
				var sample runtime.MemStats
				runtime.ReadMemStats(&sample)
				bumpAtomic(&peakHeap, sample.HeapAlloc)
				bumpAtomic(&peakInFlight, uint64(admission.inWork.Load()))
			}
		}
	}()

	start := time.Now()
	var wg sync.WaitGroup
	var accepted, authFailed atomic.Int64
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			limiter, err := NewConnectionLimiter(ConnectionConfig{MaxAttempts: 6}, time.Now)
			if err != nil {
				t.Errorf("limiter: %v", err)
				return
			}
			password := []byte(fakePassword)
			if i >= correctShare {
				password = []byte("wrong-password-fake")
			}
			source := fmt.Sprintf("198.51.100.%d:5000", i)
			err = auth.Authenticate(ctx, source, limiter, "alice", password)
			switch {
			case i < correctShare && err == nil:
				accepted.Add(1)
			case i >= correctShare && errors.Is(err, ErrAuthFailed):
				authFailed.Add(1)
			default:
				t.Errorf("attempt %d: unexpected error %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	wall := time.Since(start)
	close(samplerDone)
	<-samplerStopped

	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)

	if accepted.Load() != correctShare || authFailed.Load() != int64(attempts-correctShare) {
		t.Fatalf("accepted=%d authFailed=%d", accepted.Load(), authFailed.Load())
	}
	if got := peakInFlight.Load(); got > uint64(maxConcurrent) {
		t.Errorf("peak in-flight = %d, want <= %d", got, maxConcurrent)
	}
	return burstResult{
		attempts:     attempts,
		correct:      int(accepted.Load()),
		wrong:        int(authFailed.Load()),
		wall:         wall,
		peakInFlight: peakInFlight.Load(),
		heapBefore:   before.HeapAlloc,
		heapPeak:     peakHeap.Load(),
		heapAfter:    after.HeapAlloc,
		sysAfter:     after.Sys,
	}
}

// measureAbuseFastPath reaches the penalty threshold and then measures how
// fast further attempts are rejected without Argon2id work.
func measureAbuseFastPath(t *testing.T) {
	t.Helper()
	verifier, err := NewVerifier(recommendedParams, testSalt)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	store := &FileStore{}
	if err := store.Load(mustEncodeFile(t, fixtureFileWithParams(t, recommendedParams, "alice"))); err != nil {
		t.Fatalf("Load: %v", err)
	}
	admission, err := NewAdmission(AdmissionConfig{MaxConcurrent: 4, MaxWaiting: 8})
	if err != nil {
		t.Fatalf("NewAdmission: %v", err)
	}
	abuse, err := NewAbuseGuard(DefaultAbuseConfig(), time.Now)
	if err != nil {
		t.Fatalf("NewAbuseGuard: %v", err)
	}
	auth := NewAuthenticator(store, verifier, admission, abuse)
	ctx := context.Background()

	hammerLimiter, err := NewConnectionLimiter(ConnectionConfig{MaxAttempts: 1 << 20}, time.Now)
	if err != nil {
		t.Fatalf("NewConnectionLimiter: %v", err)
	}
	const abusiveSource = "198.51.100.200:6000"
	for i := 0; i < DefaultAbuseConfig().Threshold; i++ {
		err := auth.Authenticate(ctx, abusiveSource, hammerLimiter, "alice", []byte("wrong-password-fake"))
		if err == nil {
			t.Fatal("wrong password accepted while reaching penalty")
		}
	}
	const rejects = 1000
	rejectStart := time.Now()
	for i := 0; i < rejects; i++ {
		err := auth.Authenticate(ctx, abusiveSource, hammerLimiter, "alice", []byte(fakePassword))
		if !errors.Is(err, ErrAbusive) {
			t.Fatalf("reject %d: error = %v, want ErrAbusive", i, err)
		}
	}
	rejectWall := time.Since(rejectStart)
	t.Logf("abusive-rate fast path: %d rejections in %.3fms, mean %.6fms per rejection",
		rejects, ms(rejectWall), ms(rejectWall)/float64(rejects))
}

func bumpAtomic(target *atomic.Uint64, value uint64) {
	for {
		current := target.Load()
		if value <= current || target.CompareAndSwap(current, value) {
			return
		}
	}
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func mustEncodeWith(t *testing.T, password string, params Params, salt []byte) EncodedHash {
	t.Helper()
	encoded, err := NewEncodedHash([]byte(password), params, salt)
	if err != nil {
		t.Fatalf("NewEncodedHash: %v", err)
	}
	return encoded
}

func fixtureFileWithParams(t *testing.T, params Params, username string) PasswordFile {
	t.Helper()
	return fixtureFile(t, UserEntry{
		Username:    username,
		Hash:        mustEncodeWith(t, fakePassword, params, testSalt).String(),
		Enabled:     true,
		IdentityRef: "sec-identity-" + username,
	})
}
