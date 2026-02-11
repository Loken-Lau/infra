// Benchmark tool for E2B microVM startup and resume timing
//
// Usage:
//   sudo go run ./cmd/benchmark -build <build-id> -storage <path> -iterations 10
//
// Examples:
//   # Benchmark resume (warm start)
//   sudo go run ./cmd/benchmark -build ba6aae36-74f7-487a-b6f7-74fd7c94e479 -storage .local-build -iterations 10
//
//   # Benchmark resume (cold start - drops page cache between runs)
//   sudo go run ./cmd/benchmark -build ba6aae36-74f7-487a-b6f7-74fd7c94e479 -storage .local-build -iterations 10 -cold
//
//   # Benchmark with JSON output
//   sudo go run ./cmd/benchmark -build ba6aae36-74f7-487a-b6f7-74fd7c94e479 -storage .local-build -iterations 10 -json
//
//   # Benchmark pause (resume + snapshot)
//   sudo go run ./cmd/benchmark -build ba6aae36-74f7-487a-b6f7-74fd7c94e479 -storage .local-build -iterations 5 -pause
//
//   # Benchmark concurrent resume (5 sandboxes in parallel)
//   sudo go run ./cmd/benchmark -build ba6aae36-74f7-487a-b6f7-74fd7c94e479 -storage .local-build -iterations 5 -concurrent -concurrency 5

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/launchdarkly/go-sdk-common/v3/ldlog"
	"go.opentelemetry.io/otel/metric/noop"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/orchestrator/cmd/internal/cmdutil"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox"
	blockmetrics "github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block/metrics"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/fc"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/nbd"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/network"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/template"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/tcpfirewall"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/metadata"
	featureflags "github.com/e2b-dev/infra/packages/shared/pkg/feature-flags"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	sbxlogger "github.com/e2b-dev/infra/packages/shared/pkg/logger/sandbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

func main() {
	buildID := flag.String("build", "", "build ID (UUID) to benchmark (required)")
	storagePath := flag.String("storage", ".local-build", "storage: local path or gs://bucket")
	iterations := flag.Int("iterations", 10, "number of iterations")
	coldStart := flag.Bool("cold", false, "drop page cache between iterations (cold start)")
	noPrefetch := flag.Bool("no-prefetch", false, "disable memory prefetching")
	verbose := flag.Bool("v", false, "verbose logging")
	jsonOutput := flag.Bool("json", false, "output results as JSON")
	pauseMode := flag.Bool("pause", false, "benchmark pause (resume + snapshot) instead of just resume")
	concurrentMode := flag.Bool("concurrent", false, "benchmark concurrent resume (multiple sandboxes in parallel)")
	concurrency := flag.Int("concurrency", 5, "number of concurrent sandboxes for -concurrent mode")
	warmup := flag.Int("warmup", 1, "number of warmup iterations (not counted)")

	flag.Parse()

	if *buildID == "" {
		log.Fatal("-build required")
	}

	if os.Geteuid() != 0 {
		log.Fatal("run as root")
	}

	if *iterations < 1 {
		log.Fatal("-iterations must be >= 1")
	}

	if err := setupEnv(*storagePath); err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		if !*jsonOutput {
			fmt.Println("\n🛑 Stopping...")
		}
		cancel()
	}()

	opts := benchmarkOptions{
		buildID:        *buildID,
		storagePath:    *storagePath,
		iterations:     *iterations,
		coldStart:      *coldStart,
		noPrefetch:     *noPrefetch,
		verbose:        *verbose,
		jsonOutput:     *jsonOutput,
		pauseMode:      *pauseMode,
		concurrentMode: *concurrentMode,
		concurrency:    *concurrency,
		warmup:         *warmup,
	}

	result, err := runBenchmark(ctx, opts)
	cancel()

	if err != nil {
		if *jsonOutput {
			errResult := BenchmarkResult{Error: err.Error()}
			json.NewEncoder(os.Stdout).Encode(errResult)
		} else {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		os.Exit(1)
	}

	if *jsonOutput {
		json.NewEncoder(os.Stdout).Encode(result)
	}
}

type benchmarkOptions struct {
	buildID        string
	storagePath    string
	iterations     int
	coldStart      bool
	noPrefetch     bool
	verbose        bool
	jsonOutput     bool
	pauseMode      bool
	concurrentMode bool
	concurrency    int
	warmup         int
}

// BenchmarkResult holds the complete benchmark results
type BenchmarkResult struct {
	BuildID        string        `json:"build_id"`
	Iterations     int           `json:"iterations"`
	ColdStart      bool          `json:"cold_start"`
	NoPrefetch     bool          `json:"no_prefetch"`
	PauseMode      bool          `json:"pause_mode"`
	ConcurrentMode bool          `json:"concurrent_mode,omitempty"`
	Concurrency    int           `json:"concurrency,omitempty"`
	Runs           []RunResult   `json:"runs"`
	Summary        SummaryStats  `json:"summary"`
	Error          string        `json:"error,omitempty"`
	Timestamp      time.Time     `json:"timestamp"`
	Duration       time.Duration `json:"total_duration"`
}

// RunResult holds timing for a single run
type RunResult struct {
	Iteration int           `json:"iteration"`
	ResumeMs  float64       `json:"resume_ms"`
	PauseMs   float64       `json:"pause_ms,omitempty"`
	TotalMs   float64       `json:"total_ms"`
	Error     string        `json:"error,omitempty"`
}

// SummaryStats holds statistical summary
type SummaryStats struct {
	Resume ResumeStats `json:"resume"`
	Pause  *PauseStats `json:"pause,omitempty"`
	Total  TotalStats  `json:"total"`
}

type ResumeStats struct {
	AvgMs    float64 `json:"avg_ms"`
	MinMs    float64 `json:"min_ms"`
	MaxMs    float64 `json:"max_ms"`
	StdDevMs float64 `json:"stddev_ms"`
	P50Ms    float64 `json:"p50_ms"`
	P95Ms    float64 `json:"p95_ms"`
	P99Ms    float64 `json:"p99_ms"`
}

type PauseStats struct {
	AvgMs    float64 `json:"avg_ms"`
	MinMs    float64 `json:"min_ms"`
	MaxMs    float64 `json:"max_ms"`
	StdDevMs float64 `json:"stddev_ms"`
	P50Ms    float64 `json:"p50_ms"`
	P95Ms    float64 `json:"p95_ms"`
	P99Ms    float64 `json:"p99_ms"`
}

type TotalStats struct {
	AvgMs    float64 `json:"avg_ms"`
	MinMs    float64 `json:"min_ms"`
	MaxMs    float64 `json:"max_ms"`
	StdDevMs float64 `json:"stddev_ms"`
	P50Ms    float64 `json:"p50_ms"`
	P95Ms    float64 `json:"p95_ms"`
	P99Ms    float64 `json:"p99_ms"`
}

func setupEnv(from string) error {
	abs := func(s string) string { return utils.Must(filepath.Abs(s)) }

	var dataDir string
	if strings.HasPrefix(from, "gs://") {
		dataDir = ".local-build"
	} else {
		dataDir = from
	}

	for _, d := range []string{"kernels", "templates", "sandbox", "orchestrator", "snapshot-cache", "fc-versions", "envd"} {
		if err := os.MkdirAll(filepath.Join(dataDir, d), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	for _, d := range []string{"build", "build-templates", "sandbox", "snapshot-cache", "template"} {
		if err := os.MkdirAll(filepath.Join(dataDir, "orchestrator", d), 0o755); err != nil {
			return fmt.Errorf("mkdir orchestrator/%s: %w", d, err)
		}
	}

	env := map[string]string{
		"ARTIFACTS_REGISTRY_PROVIDER": "Local",
		"FIRECRACKER_VERSIONS_DIR":    abs(filepath.Join(dataDir, "fc-versions")),
		"HOST_ENVD_PATH":              abs(filepath.Join(dataDir, "envd", "envd")),
		"HOST_KERNELS_DIR":            abs(filepath.Join(dataDir, "kernels")),
		"ORCHESTRATOR_BASE_PATH":      abs(filepath.Join(dataDir, "orchestrator")),
		"SANDBOX_DIR":                 abs(filepath.Join(dataDir, "sandbox")),
		"SNAPSHOT_CACHE_DIR":          abs(filepath.Join(dataDir, "snapshot-cache")),
		"USE_LOCAL_NAMESPACE_STORAGE": "true",
	}

	if strings.HasPrefix(from, "gs://") {
		env["STORAGE_PROVIDER"] = "GCPBucket"
		env["TEMPLATE_BUCKET_NAME"] = strings.TrimPrefix(from, "gs://")
	} else {
		env["STORAGE_PROVIDER"] = "Local"
		env["LOCAL_TEMPLATE_STORAGE_BASE_PATH"] = abs(filepath.Join(dataDir, "templates"))
	}

	for k, v := range env {
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}

	return nil
}

type benchmarker struct {
	factory    *sandbox.Factory
	sandboxes  *sandbox.Map
	tmpl       template.Template
	sbxConfig  sandbox.Config
	buildID    string
	cache      *template.Cache
	coldStart  bool
	noPrefetch bool
	storage    storage.StorageProvider
	opts       benchmarkOptions
}

func (b *benchmarker) resumeOnce(ctx context.Context, iter int) (resumeDur time.Duration, err error) {
	runtime := sandbox.RuntimeMetadata{
		TemplateID:  b.buildID,
		TeamID:      "local",
		SandboxID:   fmt.Sprintf("sbx-%d-%d", time.Now().UnixNano(), iter),
		ExecutionID: fmt.Sprintf("exec-%d-%d", time.Now().UnixNano(), iter),
	}

	t0 := time.Now()
	sbx, err := b.factory.ResumeSandbox(ctx, b.tmpl, b.sbxConfig, runtime, t0, t0.Add(24*time.Hour), nil)
	resumeDur = time.Since(t0)

	if sbx != nil {
		sbx.Close(context.WithoutCancel(ctx))
	}

	return resumeDur, err
}

func (b *benchmarker) pauseOnce(ctx context.Context, iter int) (resumeDur, pauseDur time.Duration, err error) {
	runtime := sandbox.RuntimeMetadata{
		TemplateID:  b.buildID,
		TeamID:      "local",
		SandboxID:   fmt.Sprintf("sbx-%d-%d", time.Now().UnixNano(), iter),
		ExecutionID: fmt.Sprintf("exec-%d-%d", time.Now().UnixNano(), iter),
	}

	t0 := time.Now()
	sbx, err := b.factory.ResumeSandbox(ctx, b.tmpl, b.sbxConfig, runtime, t0, t0.Add(24*time.Hour), nil)
	resumeDur = time.Since(t0)

	if err != nil {
		if sbx != nil {
			sbx.Close(context.WithoutCancel(ctx))
		}
		return resumeDur, 0, err
	}

	// Register sandbox
	b.sandboxes.Insert(sbx)
	defer b.sandboxes.Remove(runtime.SandboxID)

	// Get metadata for pause
	origMeta, err := b.tmpl.Metadata()
	if err != nil {
		sbx.Close(context.WithoutCancel(ctx))
		return resumeDur, 0, fmt.Errorf("failed to get metadata: %w", err)
	}

	newMeta := origMeta
	newMeta.Template.BuildID = uuid.New().String()

	// Pause and create snapshot
	pauseStart := time.Now()
	snapshot, err := sbx.Pause(ctx, newMeta)
	pauseDur = time.Since(pauseStart)

	if snapshot != nil {
		snapshot.Close(context.WithoutCancel(ctx))
	}
	sbx.Close(context.WithoutCancel(ctx))

	return resumeDur, pauseDur, err
}

// concurrentResumeOnce runs multiple resume operations concurrently and measures total time
func (b *benchmarker) concurrentResumeOnce(ctx context.Context, iter int, concurrency int) (totalDur time.Duration, individualDurs []time.Duration, err error) {
	type result struct {
		sbx      *sandbox.Sandbox
		duration time.Duration
		err      error
	}

	results := make(chan result, concurrency)

	t0 := time.Now()

	// Launch all resumes concurrently
	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			runtime := sandbox.RuntimeMetadata{
				TemplateID:  b.buildID,
				TeamID:      "local",
				SandboxID:   fmt.Sprintf("sbx-%d-%d-%d", time.Now().UnixNano(), iter, idx),
				ExecutionID: fmt.Sprintf("exec-%d-%d-%d", time.Now().UnixNano(), iter, idx),
			}

			start := time.Now()
			sbx, err := b.factory.ResumeSandbox(ctx, b.tmpl, b.sbxConfig, runtime, start, start.Add(24*time.Hour), nil)
			dur := time.Since(start)

			results <- result{sbx: sbx, duration: dur, err: err}
		}(i)
	}

	// Collect results
	var sandboxes []*sandbox.Sandbox
	individualDurs = make([]time.Duration, 0, concurrency)
	var firstErr error

	for i := 0; i < concurrency; i++ {
		r := <-results
		individualDurs = append(individualDurs, r.duration)
		if r.sbx != nil {
			sandboxes = append(sandboxes, r.sbx)
		}
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
	}

	totalDur = time.Since(t0)

	// Cleanup all sandboxes (outside of timing)
	for _, sbx := range sandboxes {
		sbx.Close(context.WithoutCancel(ctx))
	}

	return totalDur, individualDurs, firstErr
}

func (b *benchmarker) clearCaches(ctx context.Context) error {
	b.cache.InvalidateAll()
	if err := dropPageCache(); err != nil {
		return fmt.Errorf("drop page cache: %w", err)
	}
	tmpl, err := b.cache.GetTemplate(ctx, b.buildID, false, false)
	if err != nil {
		return fmt.Errorf("reload template: %w", err)
	}
	if b.noPrefetch {
		tmpl = &noPrefetchTemplate{tmpl}
	}
	b.tmpl = tmpl
	return nil
}

func runBenchmark(ctx context.Context, opts benchmarkOptions) (*BenchmarkResult, error) {
	startTime := time.Now()

	// Suppress logs
	if !opts.verbose {
		cmdutil.SuppressNoisyLogs()
	}
	sbxlogger.SetSandboxLoggerInternal(logger.NewNopLogger())

	config, err := cfg.Parse()
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	slotStorage, err := network.NewStorageLocal(ctx, config.NetworkConfig)
	if err != nil {
		return nil, fmt.Errorf("network storage: %w", err)
	}

	// Scale pool sizes based on concurrency for concurrent benchmarks
	poolSize := 8
	if opts.concurrentMode && opts.concurrency > poolSize {
		poolSize = opts.concurrency + 4 // Add buffer for safety
	}

	networkPool := network.NewPool(poolSize, poolSize, slotStorage, config.NetworkConfig)
	go networkPool.Populate(ctx)
	defer networkPool.Close(context.WithoutCancel(ctx))

	devicePool, err := nbd.NewDevicePool()
	if err != nil {
		return nil, fmt.Errorf("nbd pool: %w", err)
	}
	go devicePool.Populate(ctx)
	defer devicePool.Close(context.WithoutCancel(ctx))

	// Wait for pools to be populated before starting benchmark
	if opts.concurrentMode {
		time.Sleep(time.Duration(poolSize*100) * time.Millisecond)
	}

	flags, _ := featureflags.NewClientWithLogLevel(ldlog.Error)

	persistence, err := storage.GetTemplateStorageProvider(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("storage provider: %w", err)
	}

	blockMetrics, _ := blockmetrics.NewMetrics(&noop.MeterProvider{})

	cache, err := template.NewCache(config, flags, persistence, blockMetrics)
	if err != nil {
		return nil, fmt.Errorf("template cache: %w", err)
	}
	cache.Start(ctx)
	defer cache.Stop()

	sandboxes := sandbox.NewSandboxesMap()
	factory := sandbox.NewFactory(config.BuilderConfig, networkPool, devicePool, flags, nil)

	l := logger.NewNopLogger()
	tcpFw := tcpfirewall.New(l, config.NetworkConfig, sandboxes, noop.NewMeterProvider(), flags)
	go tcpFw.Start(ctx)
	defer tcpFw.Close(context.WithoutCancel(ctx))

	if !opts.jsonOutput {
		fmt.Printf("📦 Loading build %s...\n", opts.buildID)
	}

	tmpl, err := cache.GetTemplate(ctx, opts.buildID, false, false)
	if err != nil {
		return nil, err
	}

	meta, err := tmpl.Metadata()
	if err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}

	if !opts.jsonOutput {
		printTemplateInfo(ctx, tmpl, meta)
	}

	if opts.noPrefetch {
		tmpl = &noPrefetchTemplate{tmpl}
		if !opts.jsonOutput {
			fmt.Println("   Prefetch: disabled")
		}
	}

	token := "local"
	b := &benchmarker{
		factory:    factory,
		sandboxes:  sandboxes,
		tmpl:       tmpl,
		buildID:    opts.buildID,
		cache:      cache,
		coldStart:  opts.coldStart,
		noPrefetch: opts.noPrefetch,
		storage:    persistence,
		opts:       opts,
		sbxConfig: sandbox.Config{
			BaseTemplateID: opts.buildID,
			Vcpu:           1,
			RamMB:          512,
			Network:        &orchestrator.SandboxNetworkConfig{},
			Envd:           sandbox.EnvdMetadata{Vars: map[string]string{}, AccessToken: &token, Version: "1.0.0"},
			FirecrackerConfig: fc.Config{
				KernelVersion:      meta.Template.KernelVersion,
				FirecrackerVersion: meta.Template.FirecrackerVersion,
			},
		},
	}

	// Warmup runs
	if opts.warmup > 0 && !opts.jsonOutput {
		fmt.Printf("\n🔥 Warmup (%d iterations)...\n", opts.warmup)
	}
	for i := 0; i < opts.warmup; i++ {
		if ctx.Err() != nil {
			break
		}
		if opts.concurrentMode {
			_, _, _ = b.concurrentResumeOnce(ctx, -i-1, opts.concurrency)
		} else if opts.pauseMode {
			_, _, _ = b.pauseOnce(ctx, -i-1)
		} else {
			_, _ = b.resumeOnce(ctx, -i-1)
		}
	}

	// Actual benchmark
	if !opts.jsonOutput {
		mode := "resume"
		if opts.pauseMode {
			mode = "pause (resume + snapshot)"
		} else if opts.concurrentMode {
			mode = fmt.Sprintf("concurrent resume (%d sandboxes)", opts.concurrency)
		}
		startType := "warm"
		if opts.coldStart {
			startType = "cold"
		}
		fmt.Printf("\n📊 Benchmarking %s (%s start, %d iterations)...\n\n", mode, startType, opts.iterations)
	}

	runs := make([]RunResult, 0, opts.iterations)

	for i := 0; i < opts.iterations; i++ {
		if ctx.Err() != nil {
			break
		}

		// Clear caches for cold start
		if opts.coldStart && i > 0 {
			if err := b.clearCaches(ctx); err != nil {
				return nil, err
			}
		}

		if !opts.jsonOutput {
			fmt.Printf("\r[%d/%d] Running...    ", i+1, opts.iterations)
		}

		var run RunResult
		run.Iteration = i + 1

		if opts.concurrentMode {
			totalDur, individualDurs, err := b.concurrentResumeOnce(ctx, i, opts.concurrency)
			run.ResumeMs = float64(totalDur) / float64(time.Millisecond)
			run.TotalMs = run.ResumeMs
			if err != nil {
				run.Error = err.Error()
			}
			// Print individual durations for debugging
			if !opts.jsonOutput {
				var durs []float64
				for _, d := range individualDurs {
					durs = append(durs, float64(d)/float64(time.Millisecond))
				}
				fmt.Printf("\n   Individual: %v ms\n", durs)
			}
		} else if opts.pauseMode {
			resumeDur, pauseDur, err := b.pauseOnce(ctx, i)
			run.ResumeMs = float64(resumeDur) / float64(time.Millisecond)
			run.PauseMs = float64(pauseDur) / float64(time.Millisecond)
			run.TotalMs = run.ResumeMs + run.PauseMs
			if err != nil {
				run.Error = err.Error()
			}
		} else {
			resumeDur, err := b.resumeOnce(ctx, i)
			run.ResumeMs = float64(resumeDur) / float64(time.Millisecond)
			run.TotalMs = run.ResumeMs
			if err != nil {
				run.Error = err.Error()
			}
		}

		runs = append(runs, run)

		if run.Error != "" && !opts.jsonOutput {
			fmt.Printf("\r[%d/%d] ❌ Failed: %s\n", i+1, opts.iterations, run.Error)
			break
		}
	}

	if !opts.jsonOutput {
		fmt.Print("\r                         \r")
	}

	// Calculate summary
	summary := calculateSummary(runs, opts.pauseMode)

	result := &BenchmarkResult{
		BuildID:        opts.buildID,
		Iterations:     len(runs),
		ColdStart:      opts.coldStart,
		NoPrefetch:     opts.noPrefetch,
		PauseMode:      opts.pauseMode,
		ConcurrentMode: opts.concurrentMode,
		Concurrency:    opts.concurrency,
		Runs:           runs,
		Summary:        summary,
		Timestamp:      startTime,
		Duration:       time.Since(startTime),
	}

	if !opts.jsonOutput {
		printResults(runs, opts.pauseMode, summary)
	}

	return result, nil
}

func calculateSummary(runs []RunResult, pauseMode bool) SummaryStats {
	var successful []RunResult
	for _, r := range runs {
		if r.Error == "" {
			successful = append(successful, r)
		}
	}

	if len(successful) == 0 {
		return SummaryStats{}
	}

	// Extract durations
	resumeDurs := make([]float64, len(successful))
	pauseDurs := make([]float64, len(successful))
	totalDurs := make([]float64, len(successful))

	for i, r := range successful {
		resumeDurs[i] = r.ResumeMs
		pauseDurs[i] = r.PauseMs
		totalDurs[i] = r.TotalMs
	}

	summary := SummaryStats{
		Resume: calcStats(resumeDurs),
		Total:  TotalStats(calcStats(totalDurs)),
	}

	if pauseMode {
		pauseStats := PauseStats(calcStats(pauseDurs))
		summary.Pause = &pauseStats
	}

	return summary
}

func calcStats(values []float64) ResumeStats {
	if len(values) == 0 {
		return ResumeStats{}
	}

	sorted := slices.Clone(values)
	slices.Sort(sorted)

	n := len(sorted)
	var sum float64
	for _, v := range sorted {
		sum += v
	}
	avg := sum / float64(n)

	// Standard deviation
	var variance float64
	for _, v := range sorted {
		diff := v - avg
		variance += diff * diff
	}
	variance /= float64(n)
	stdDev := math.Sqrt(variance)

	return ResumeStats{
		AvgMs:    avg,
		MinMs:    sorted[0],
		MaxMs:    sorted[n-1],
		StdDevMs: stdDev,
		P50Ms:    sorted[(n-1)*50/100],
		P95Ms:    sorted[(n-1)*95/100],
		P99Ms:    sorted[(n-1)*99/100],
	}
}

func printTemplateInfo(ctx context.Context, tmpl template.Template, meta metadata.Template) {
	fmt.Printf("   Kernel: %s, Firecracker: %s\n", meta.Template.KernelVersion, meta.Template.FirecrackerVersion)

	if memfile, err := tmpl.Memfile(ctx); err == nil {
		if size, err := memfile.Size(ctx); err == nil {
			blockSize := memfile.BlockSize()
			fmt.Printf("   Memfile: %d MB (%d KB blocks)\n", size>>20, blockSize>>10)
		}
	}

	if rootfs, err := tmpl.Rootfs(); err == nil {
		if size, err := rootfs.Size(ctx); err == nil {
			fmt.Printf("   Rootfs: %d MB (%d KB blocks)\n", size>>20, rootfs.BlockSize()>>10)
		}
	}

	if meta.Prefetch != nil && meta.Prefetch.Memory != nil {
		fmt.Printf("   Prefetch: %d blocks\n", meta.Prefetch.Memory.Count())
	}
}

func printResults(runs []RunResult, pauseMode bool, summary SummaryStats) {
	if len(runs) == 0 {
		return
	}

	// Count successful
	var successCount int
	for _, r := range runs {
		if r.Error == "" {
			successCount++
		}
	}

	if successCount == 0 {
		fmt.Println("\n❌ All runs failed")
		return
	}

	// Print individual results
	if pauseMode {
		fmt.Println("📋 Run times (resume / pause / total):")
	} else {
		fmt.Println("📋 Run times:")
	}

	for _, r := range runs {
		if r.Error != "" {
			fmt.Printf("   [%2d] ❌ Failed: %s\n", r.Iteration, r.Error)
			continue
		}

		resumeDiff := (r.ResumeMs - summary.Resume.AvgMs) / summary.Resume.AvgMs * 100

		if pauseMode {
			pauseDiff := (r.PauseMs - summary.Pause.AvgMs) / summary.Pause.AvgMs * 100
			fmt.Printf("   [%2d] %.1fms / %.1fms / %.1fms  (resume: %s%+.1f%%%s, pause: %s%+.1f%%%s)\n",
				r.Iteration,
				r.ResumeMs, r.PauseMs, r.TotalMs,
				colorForDiff(resumeDiff), resumeDiff, colorReset,
				colorForDiff(pauseDiff), pauseDiff, colorReset)
		} else {
			fmt.Printf("   [%2d] %.1fms  %s%+.1f%%%s\n",
				r.Iteration, r.ResumeMs,
				colorForDiff(resumeDiff), resumeDiff, colorReset)
		}
	}

	// Print summary
	fmt.Printf("\n📊 Summary (%d runs):\n", successCount)

	if pauseMode {
		fmt.Printf("   Resume: Avg %.1fms | Min %.1fms | Max %.1fms | StdDev %.1fms\n",
			summary.Resume.AvgMs, summary.Resume.MinMs, summary.Resume.MaxMs, summary.Resume.StdDevMs)
		fmt.Printf("   Pause:  Avg %.1fms | Min %.1fms | Max %.1fms | StdDev %.1fms\n",
			summary.Pause.AvgMs, summary.Pause.MinMs, summary.Pause.MaxMs, summary.Pause.StdDevMs)
		fmt.Printf("   Total:  Avg %.1fms | Min %.1fms | Max %.1fms | StdDev %.1fms\n",
			summary.Total.AvgMs, summary.Total.MinMs, summary.Total.MaxMs, summary.Total.StdDevMs)
	} else {
		fmt.Printf("   Avg: %.1fms | Min: %.1fms | Max: %.1fms | StdDev: %.1fms\n",
			summary.Resume.AvgMs, summary.Resume.MinMs, summary.Resume.MaxMs, summary.Resume.StdDevMs)
	}

	if successCount > 1 {
		fmt.Printf("   P50: %.1fms | P95: %.1fms | P99: %.1fms\n",
			summary.Resume.P50Ms, summary.Resume.P95Ms, summary.Resume.P99Ms)
	}

	// Print visualization
	printVisualization(runs, pauseMode, summary)
}

func printVisualization(runs []RunResult, pauseMode bool, summary SummaryStats) {
	// Collect successful runs
	var successful []RunResult
	for _, r := range runs {
		if r.Error == "" {
			successful = append(successful, r)
		}
	}

	if len(successful) < 2 {
		return
	}

	const chartWidth = 50
	const chartHeight = 12

	// Print bar chart for resume times
	fmt.Println("\n📈 Resume Time Distribution:")
	printBarChart(successful, func(r RunResult) float64 { return r.ResumeMs }, summary.Resume.AvgMs, chartWidth)

	// Print timeline chart
	fmt.Println("\n📉 Timeline (iteration vs time):")
	printTimelineChart(successful, func(r RunResult) float64 { return r.ResumeMs }, summary.Resume, chartWidth, chartHeight)

	if pauseMode && summary.Pause != nil {
		fmt.Println("\n📈 Pause Time Distribution:")
		printBarChart(successful, func(r RunResult) float64 { return r.PauseMs }, summary.Pause.AvgMs, chartWidth)
	}

	// Print histogram
	fmt.Println("\n📊 Histogram:")
	printHistogram(successful, func(r RunResult) float64 { return r.ResumeMs }, chartWidth)
}

func printBarChart(runs []RunResult, getValue func(RunResult) float64, avg float64, width int) {
	// Find max value for scaling
	var maxVal float64
	for _, r := range runs {
		if v := getValue(r); v > maxVal {
			maxVal = v
		}
	}

	if maxVal == 0 {
		return
	}

	for _, r := range runs {
		val := getValue(r)
		barLen := int(val / maxVal * float64(width))
		if barLen < 1 {
			barLen = 1
		}

		// Color based on comparison to average
		diff := (val - avg) / avg * 100
		color := colorForDiff(diff)

		bar := strings.Repeat("█", barLen)
		fmt.Printf("   [%2d] %s%s%s %6.1fms\n", r.Iteration, color, bar, colorReset, val)
	}

	// Print scale
	fmt.Printf("   %s0%s%.0fms\n", strings.Repeat(" ", 5), strings.Repeat("─", width-5), maxVal)
}

func printTimelineChart(runs []RunResult, getValue func(RunResult) float64, stats ResumeStats, width, height int) {
	if len(runs) == 0 {
		return
	}

	// Get values
	values := make([]float64, len(runs))
	for i, r := range runs {
		values[i] = getValue(r)
	}

	minVal := stats.MinMs
	maxVal := stats.MaxMs
	valRange := maxVal - minVal

	if valRange == 0 {
		valRange = 1
	}

	// Create chart grid
	grid := make([][]rune, height)
	for i := range grid {
		grid[i] = make([]rune, width)
		for j := range grid[i] {
			grid[i][j] = ' '
		}
	}

	// Plot points
	for i, val := range values {
		x := i * (width - 1) / (len(values) - 1)
		y := int((val - minVal) / valRange * float64(height-1))
		if y >= height {
			y = height - 1
		}
		if y < 0 {
			y = 0
		}
		// Invert y for display (0 at bottom)
		grid[height-1-y][x] = '●'
	}

	// Draw average line
	avgY := int((stats.AvgMs - minVal) / valRange * float64(height-1))
	if avgY >= 0 && avgY < height {
		for x := 0; x < width; x++ {
			if grid[height-1-avgY][x] == ' ' {
				grid[height-1-avgY][x] = '─'
			}
		}
	}

	// Print chart
	for y := 0; y < height; y++ {
		var label string
		if y == 0 {
			label = fmt.Sprintf("%6.0f│", maxVal)
		} else if y == height-1 {
			label = fmt.Sprintf("%6.0f│", minVal)
		} else if y == height-1-avgY {
			label = fmt.Sprintf("%6.0f│", stats.AvgMs)
		} else {
			label = "      │"
		}
		fmt.Printf("   %s%s\n", label, string(grid[y]))
	}

	// X-axis
	fmt.Printf("   %s└%s\n", strings.Repeat(" ", 6), strings.Repeat("─", width))
	fmt.Printf("   %s 1%s%d\n", strings.Repeat(" ", 6), strings.Repeat(" ", width-2), len(runs))
}

func printHistogram(runs []RunResult, getValue func(RunResult) float64, width int) {
	if len(runs) < 3 {
		return
	}

	// Get values
	values := make([]float64, len(runs))
	var minVal, maxVal float64 = math.MaxFloat64, 0
	for i, r := range runs {
		v := getValue(r)
		values[i] = v
		if v < minVal {
			minVal = v
		}
		if v > maxVal {
			maxVal = v
		}
	}

	valRange := maxVal - minVal
	if valRange == 0 {
		return
	}

	// Create buckets (5-10 buckets depending on data)
	numBuckets := len(runs) / 2
	if numBuckets < 5 {
		numBuckets = 5
	}
	if numBuckets > 10 {
		numBuckets = 10
	}

	buckets := make([]int, numBuckets)
	bucketSize := valRange / float64(numBuckets)

	for _, v := range values {
		idx := int((v - minVal) / bucketSize)
		if idx >= numBuckets {
			idx = numBuckets - 1
		}
		buckets[idx]++
	}

	// Find max bucket count for scaling
	var maxCount int
	for _, c := range buckets {
		if c > maxCount {
			maxCount = c
		}
	}

	if maxCount == 0 {
		return
	}

	// Print histogram
	barWidth := width - 20
	for i, count := range buckets {
		rangeStart := minVal + float64(i)*bucketSize
		rangeEnd := rangeStart + bucketSize

		barLen := count * barWidth / maxCount
		if barLen < 1 && count > 0 {
			barLen = 1
		}

		bar := strings.Repeat("▓", barLen)
		fmt.Printf("   %5.0f-%5.0f │%s %d\n", rangeStart, rangeEnd, bar, count)
	}
}

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
)

func colorForDiff(diff float64) string {
	switch {
	case diff < -5:
		return colorGreen
	case diff > 5:
		return colorRed
	default:
		return colorYellow
	}
}

func dropPageCache() error {
	unix.Sync()
	return os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0o644)
}

// noPrefetchTemplate wraps a template to disable prefetching
type noPrefetchTemplate struct {
	template.Template
}

func (t *noPrefetchTemplate) Metadata() (metadata.Template, error) {
	meta, err := t.Template.Metadata()
	if err != nil {
		return meta, err
	}
	meta.Prefetch = nil
	return meta, nil
}
