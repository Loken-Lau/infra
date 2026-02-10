// run with something like:
//
// sudo `which go` test -benchtime=15s -bench=. -v
// sudo modprobe nbd
// echo 1024 | sudo tee /proc/sys/vm/nr_hugepages
package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/proxy"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox"
	blockmetrics "github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block/metrics"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/fc"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/nbd"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/network"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/template"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/tcpfirewall"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build"
	buildconfig "github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/config"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/build/metrics"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/metadata"
	artifactsregistry "github.com/e2b-dev/infra/packages/shared/pkg/artifacts-registry"
	"github.com/e2b-dev/infra/packages/shared/pkg/dockerhub"
	featureflags "github.com/e2b-dev/infra/packages/shared/pkg/feature-flags"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/limit"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	sbxlogger "github.com/e2b-dev/infra/packages/shared/pkg/logger/sandbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/orchestrator")

func BenchmarkBaseImageLaunch(b *testing.B) {
	if os.Geteuid() != 0 {
		b.Skip("skipping benchmark because not running as root")
	}

	// test configuration
	const (
		testType        = startPauseResume
		baseImage       = "e2bdev/base"
		kernelVersion   = "vmlinux-6.1.102"
		fcVersion       = "v1.12.1_717921c"
		templateID      = "fcb33d09-3141-42c4-8d3b-c2df411681db"
		buildID         = "ba6aae36-74f7-487a-b6f7-74fd7c94e479"
		useHugePages    = false
		templateVersion = "v2.0.0"
		rootfsPath      = "/home/ubuntu/ljh/testspace/infra/packages/rootfs/rootfs.ext4"
	)

	sbxNetwork := &orchestrator.SandboxNetworkConfig{}

	// cache paths, to speed up test runs. these paths aren't wiped between tests
	persistenceDir := getPersistenceDir()
	kernelsDir := filepath.Join(persistenceDir, "kernels")
	sandboxDir := filepath.Join(persistenceDir, "sandbox")
	err := os.MkdirAll(kernelsDir, 0o755)
	require.NoError(b, err)

	// ephemeral data
	tempDir := b.TempDir()

	abs := func(s string) string {
		return utils.Must(filepath.Abs(s))
	}

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint != "" {
		spanExporter, err := telemetry.NewSpanExporter(b.Context(),
			otlptracegrpc.WithEndpoint(endpoint),
		)
		b.Cleanup(func() {
			ctx := context.WithoutCancel(b.Context())
			err := spanExporter.Shutdown(ctx)
			assert.NoError(b, err)
		})

		require.NoError(b, err)
		resource, err := telemetry.GetResource(b.Context(), "node-id", "BenchmarkBaseImageLaunch", "service-commit", "service-version", "service-instance-id")
		require.NoError(b, err)
		tracerProvider := telemetry.NewTracerProvider(spanExporter, resource)
		otel.SetTracerProvider(tracerProvider)
	}

	linuxKernelURL, err := url.JoinPath("https://storage.googleapis.com/e2b-prod-public-builds/kernels/", kernelVersion, "vmlinux.bin")
	require.NoError(b, err)
	linuxKernelFilename := filepath.Join(kernelsDir, kernelVersion, "vmlinux.bin")

	downloadKernel(b, linuxKernelFilename, linuxKernelURL)

	// hacks, these should go away
	b.Setenv("ARTIFACTS_REGISTRY_PROVIDER", "Local")
	b.Setenv("FIRECRACKER_VERSIONS_DIR", abs(filepath.Join("..", "fc-versions", "builds")))
	b.Setenv("HOST_ENVD_PATH", abs(filepath.Join("..", "envd", "bin", "envd")))
	b.Setenv("HOST_KERNELS_DIR", abs(kernelsDir))
	b.Setenv("LOCAL_TEMPLATE_STORAGE_BASE_PATH", abs(filepath.Dir(rootfsPath)))
	b.Setenv("ORCHESTRATOR_BASE_PATH", tempDir)
	b.Setenv("SANDBOX_DIR", abs(sandboxDir))
	b.Setenv("SNAPSHOT_CACHE_DIR", abs(filepath.Join(tempDir, "snapshot-cache")))
	b.Setenv("STORAGE_PROVIDER", "Local")
	b.Setenv("USE_LOCAL_NAMESPACE_STORAGE", "true")

	config, err := cfg.Parse()
	require.NoError(b, err)

	// prep directories
	for _, subdir := range []string{"build", "build-templates" /*"fc-vm",*/, "sandbox", "snapshot-cache", "template"} {
		fullDirName := filepath.Join(tempDir, subdir)
		err := os.MkdirAll(fullDirName, 0o755)
		require.NoError(b, err)
	}

	l, err := logger.NewDevelopmentLogger()
	require.NoError(b, err)

	sbxlogger.SetSandboxLoggerInternal(l)
	// sbxlogger.SetSandboxLoggerExternal(logger)

	slotStorage, err := network.NewStorageLocal(b.Context(), config.NetworkConfig)
	require.NoError(b, err)
	networkPool := network.NewPool(8, 8, slotStorage, config.NetworkConfig)
	go func() {
		networkPool.Populate(b.Context())
		l.Info(b.Context(), "network pool populated")
	}()
	b.Cleanup(func() {
		ctx := context.WithoutCancel(b.Context())
		err := networkPool.Close(ctx)
		assert.NoError(b, err)
	})

	devicePool, err := nbd.NewDevicePool()
	require.NoError(b, err, "do you have the nbd kernel module installed?")
	go func() {
		devicePool.Populate(b.Context())
		l.Info(b.Context(), "device pool populated")
	}()
	b.Cleanup(func() {
		ctx := context.WithoutCancel(b.Context())
		err := devicePool.Close(ctx)
		assert.NoError(b, err)
	})

	featureFlags, err := featureflags.NewClient()
	require.NoError(b, err)
	b.Cleanup(func() {
		ctx := context.WithoutCancel(b.Context())
		err := featureFlags.Close(ctx)
		assert.NoError(b, err)
	})

	limiter, err := limit.New(b.Context(), featureFlags)
	require.NoError(b, err)

	persistence, err := storage.GetTemplateStorageProvider(b.Context(), limiter)
	require.NoError(b, err)

	blockMetrics, err := blockmetrics.NewMetrics(&noop.MeterProvider{})
	require.NoError(b, err)

	c, err := cfg.Parse()
	if err != nil {
		b.Fatalf("error parsing config: %v", err)
	}

	templateCache, err := template.NewCache(c, featureFlags, persistence, blockMetrics)
	require.NoError(b, err)
	templateCache.Start(b.Context())
	b.Cleanup(templateCache.Stop)

	sandboxFactory := sandbox.NewFactory(config.BuilderConfig, networkPool, devicePool, featureFlags)

	dockerhubRepository, err := dockerhub.GetRemoteRepository(b.Context())
	require.NoError(b, err)
	b.Cleanup(func() {
		err := dockerhubRepository.Close()
		assert.NoError(b, err)
	})

	accessToken := "access-token"
	sandboxConfig := sandbox.Config{
		BaseTemplateID:  templateID,
		Vcpu:            2,
		RamMB:           512,
		TotalDiskSizeMB: 2 * 1024,
		HugePages:       useHugePages,
		Network:         sbxNetwork,
		Envd: sandbox.EnvdMetadata{
			Vars:        map[string]string{"HELLO": "WORLD"},
			AccessToken: &accessToken,
			Version:     "1.2.3",
		},
		FirecrackerConfig: fc.Config{
			KernelVersion:      kernelVersion,
			FirecrackerVersion: fcVersion,
		},
	}

	runtime := sandbox.RuntimeMetadata{
		TemplateID:  templateID,
		SandboxID:   "sandbox-id",
		ExecutionID: "execution-id",
		TeamID:      "team-id",
	}

	artifactRegistry, err := artifactsregistry.GetArtifactsRegistryProvider(b.Context())
	require.NoError(b, err)

	persistenceTemplate, err := storage.GetTemplateStorageProvider(b.Context(), nil)
	require.NoError(b, err)

	persistenceBuild, err := storage.GetBuildCacheStorageProvider(b.Context(), nil)
	require.NoError(b, err)

	var proxyPort uint16 = 5007

	sandboxes := sandbox.NewSandboxesMap()

	tcpFirewall := tcpfirewall.New(
		l,
		config.NetworkConfig,
		sandboxes,
		noop.NewMeterProvider(),
		featureFlags,
	)
	go func() {
		err := tcpFirewall.Start(b.Context())
		assert.NoError(b, err)
	}()
	b.Cleanup(func() {
		ctx := context.WithoutCancel(b.Context())
		err := tcpFirewall.Close(ctx)
		assert.NoError(b, err)
	})

	sandboxProxy, err := proxy.NewSandboxProxy(noop.MeterProvider{}, proxyPort, sandboxes)
	require.NoError(b, err)
	go func() {
		err := sandboxProxy.Start(b.Context())
		assert.ErrorIs(b, http.ErrServerClosed, err)
	}()
	b.Cleanup(func() {
		ctx := context.WithoutCancel(b.Context())
		err := sandboxProxy.Close(ctx)
		assert.NoError(b, err)
	})

	buildMetrics, err := metrics.NewBuildMetrics(noop.MeterProvider{})
	require.NoError(b, err)

	builder := build.NewBuilder(
		config.BuilderConfig,
		l,
		featureFlags,
		sandboxFactory,
		persistenceTemplate,
		persistenceBuild,
		artifactRegistry,
		dockerhubRepository,
		sandboxProxy,
		sandboxes,
		templateCache,
		buildMetrics,
	)

	buildPath := filepath.Join(os.Getenv("LOCAL_TEMPLATE_STORAGE_BASE_PATH"), buildID, "rootfs.ext4")
	ensureRootfsAtBuildPath(b, rootfsPath, buildPath)
	if _, err := os.Stat(buildPath); os.IsNotExist(err) {
		// build template
		force := true
		templateConfig := buildconfig.TemplateConfig{
			Version:            templateVersion,
			TemplateID:         templateID,
			FromImage:          baseImage,
			Force:              &force,
			VCpuCount:          sandboxConfig.Vcpu,
			MemoryMB:           sandboxConfig.RamMB,
			StartCmd:           "echo 'start cmd debug' && sleep .1 && echo 'done starting command debug'",
			DiskSizeMB:         sandboxConfig.TotalDiskSizeMB,
			HugePages:          sandboxConfig.HugePages,
			KernelVersion:      kernelVersion,
			FirecrackerVersion: fcVersion,
		}

		metadata := storage.TemplateFiles{
			BuildID: buildID,
		}
		_, err = builder.Build(b.Context(), metadata, templateConfig, l.Detach(b.Context()).Core())
		require.NoError(b, err)
	}

	// retrieve template
	tmpl, err := templateCache.GetTemplate(
		b.Context(),
		buildID,
		false,
		false,
	)
	require.NoError(b, err)

	tc := testContainer{
		sandboxFactory: sandboxFactory,
		testType:       testType,
		tmpl:           tmpl,
		sandboxConfig:  sandboxConfig,
		runtime:        runtime,
	}

	stats := newBenchStats(testType)
	defer stats.Report(b)

	for b.Loop() {
		tc.testOneItem(b, buildID, kernelVersion, fcVersion, stats)
	}
}

func getPersistenceDir() string {
	home := os.Getenv("HOME")
	if home != "" {
		return filepath.Join(home, ".cache", "e2b-orchestrator-benchmark")
	}

	return filepath.Join(os.TempDir(), "e2b-orchestrator-benchmark")
}

type testCycle string

const (
	onlyStart        testCycle = "only-start"
	startAndPause    testCycle = "start-and-pause"
	startPauseResume testCycle = "start-pause-resume"
)

type testContainer struct {
	testType       testCycle
	sandboxFactory *sandbox.Factory
	tmpl           template.Template
	sandboxConfig  sandbox.Config
	runtime        sandbox.RuntimeMetadata
}

func (tc *testContainer) testOneItem(b *testing.B, buildID, kernelVersion, fcVersion string, stats *benchStats) {
	b.Helper()

	ctx, span := tracer.Start(b.Context(), "testOneItem")
	defer span.End()

	var totalMeasured time.Duration

	startResumeAt := time.Now()
	sbx, err := tc.sandboxFactory.ResumeSandbox(
		ctx,
		tc.tmpl,
		tc.sandboxConfig,
		tc.runtime,
		time.Now(),
		time.Now().Add(time.Second*15),
		nil,
	)
	require.NoError(b, err)
	resumeDur := time.Since(startResumeAt)
	stats.AddResume(resumeDur)
	totalMeasured += resumeDur

	if tc.testType == onlyStart {
		b.StopTimer()
		closeAt := time.Now()
		err = sbx.Close(ctx)
		require.NoError(b, err)
		stats.AddClose(time.Since(closeAt))
		b.StartTimer()

		stats.AddTotal(totalMeasured)
		return
	}

	meta, err := sbx.Template.Metadata()
	require.NoError(b, err)

	templateMetadata := meta.SameVersionTemplate(metadata.TemplateMetadata{
		BuildID:            buildID,
		KernelVersion:      kernelVersion,
		FirecrackerVersion: fcVersion,
	})
	pauseAt := time.Now()
	snap, err := sbx.Pause(ctx, templateMetadata)
	require.NoError(b, err)
	require.NotNil(b, snap)
	pauseDur := time.Since(pauseAt)
	stats.AddPause(pauseDur)
	totalMeasured += pauseDur

	stats.AddSnapshotSizes(snap)

	if tc.testType == startAndPause {
		b.StopTimer()
		closeAt := time.Now()
		err = sbx.Close(ctx)
		require.NoError(b, err)
		stats.AddClose(time.Since(closeAt))
		b.StartTimer()

		stats.AddTotal(totalMeasured)
	}

	// resume sandbox
	resume2At := time.Now()
	sbx, err = tc.sandboxFactory.ResumeSandbox(ctx, tc.tmpl, tc.sandboxConfig, tc.runtime, time.Now(), time.Now().Add(time.Second*15), nil)
	require.NoError(b, err)
	resume2Dur := time.Since(resume2At)
	stats.AddResume2(resume2Dur)
	totalMeasured += resume2Dur

	// close sandbox
	closeAt := time.Now()
	err = sbx.Close(ctx)
	require.NoError(b, err)
	stats.AddClose(time.Since(closeAt))

	stats.AddTotal(totalMeasured)
}

func downloadKernel(b *testing.B, filename, url string) {
	b.Helper()

	dirname := filepath.Dir(filename)
	err := os.MkdirAll(dirname, 0o755)
	require.NoError(b, err)

	// kernel already exists
	if _, err := os.Stat(filename); err == nil {
		return
	}

	client := &http.Client{}
	req, err := http.NewRequestWithContext(b.Context(), http.MethodGet, url, nil)
	require.NoError(b, err)
	response, err := client.Do(req)
	require.NoError(b, err)
	require.Equal(b, http.StatusOK, response.StatusCode)
	defer response.Body.Close()

	file, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY, 0o644)
	require.NoError(b, err)
	defer file.Close()

	_, err = file.ReadFrom(response.Body)
	require.NoError(b, err)
}

func ensureRootfsAtBuildPath(b *testing.B, rootfsPath, buildPath string) {
	b.Helper()

	if rootfsPath == "" || buildPath == "" {
		return
	}

	if _, err := os.Stat(buildPath); err == nil {
		return
	}

	if _, err := os.Stat(rootfsPath); os.IsNotExist(err) {
		return
	}

	err := os.MkdirAll(filepath.Dir(buildPath), 0o755)
	require.NoError(b, err)

	if err := os.Symlink(rootfsPath, buildPath); err == nil {
		return
	}

	src, err := os.Open(rootfsPath)
	require.NoError(b, err)
	defer src.Close()

	dst, err := os.OpenFile(buildPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	require.NoError(b, err)
	defer dst.Close()

	_, err = io.Copy(dst, src)
	require.NoError(b, err)
}

type durSummary struct {
	Count int
	Avg   time.Duration
	P50   time.Duration
	P95   time.Duration
	Max   time.Duration
	Sum   time.Duration
}

func summarizeDurations(durs []time.Duration) durSummary {
	if len(durs) == 0 {
		return durSummary{}
	}

	cp := make([]time.Duration, len(durs))
	copy(cp, durs)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })

	var sum time.Duration
	for _, d := range cp {
		sum += d
	}

	p50 := cp[(len(cp)-1)*50/100]
	p95 := cp[(len(cp)-1)*95/100]

	return durSummary{
		Count: len(cp),
		Avg:   sum / time.Duration(len(cp)),
		P50:   p50,
		P95:   p95,
		Max:   cp[len(cp)-1],
		Sum:   sum,
	}
}

type benchStats struct {
	testType testCycle

	resume  []time.Duration
	pause   []time.Duration
	resume2 []time.Duration
	close   []time.Duration
	total   []time.Duration

	snapfileBytes    []int64
	memfileDiffBytes []int64
	rootfsDiffBytes  []int64
}

func newBenchStats(testType testCycle) *benchStats {
	return &benchStats{testType: testType}
}

func (s *benchStats) AddResume(d time.Duration)  { s.resume = append(s.resume, d) }
func (s *benchStats) AddPause(d time.Duration)   { s.pause = append(s.pause, d) }
func (s *benchStats) AddResume2(d time.Duration) { s.resume2 = append(s.resume2, d) }
func (s *benchStats) AddClose(d time.Duration)   { s.close = append(s.close, d) }
func (s *benchStats) AddTotal(d time.Duration)   { s.total = append(s.total, d) }

func (s *benchStats) AddSnapshotSizes(snap *sandbox.Snapshot) {
	if snap == nil {
		return
	}

	if snap.Snapfile != nil {
		if st, err := os.Stat(snap.Snapfile.Path()); err == nil {
			s.snapfileBytes = append(s.snapfileBytes, st.Size())
		}
	}

	// switch d := snap.MemfileDiff.(type) {
	// if size, err := snap.MemfileDiff.FileSize(); err == nil{
	// 	s.memfileDiffBytes = append(s.memfileDiffBytes, 0)
	// }
	// }

	// switch d := snap.RootfsDiff.(type) {
	// if size, err := snap.RootfsDiff.FileSize(); err == nil{
	// 	s.rootfsDiffBytes = append(s.rootfsDiffBytes, 0)
	// }
	// }
}

func summarizeBytes(sizes []int64) (count int, avg int64, p50 int64, p95 int64, max int64) {
	if len(sizes) == 0 {
		return 0, 0, 0, 0, 0
	}
	cp := make([]int64, len(sizes))
	copy(cp, sizes)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	var sum int64
	for _, v := range cp {
		sum += v
	}
	p50 = cp[(len(cp)-1)*50/100]
	p95 = cp[(len(cp)-1)*95/100]
	max = cp[len(cp)-1]
	return len(cp), sum / int64(len(cp)), p50, p95, max
}

func (s *benchStats) Report(b *testing.B) {
	b.StopTimer()

	resume := summarizeDurations(s.resume)
	pause := summarizeDurations(s.pause)
	resume2 := summarizeDurations(s.resume2)
	closeDur := summarizeDurations(s.close)
	total := summarizeDurations(s.total)

	b.Logf("timing summary (testType=%s, n=%d):", s.testType, total.Count)
	if resume.Count > 0 {
		b.Logf("  resume1: avg=%s p50=%s p95=%s max=%s", resume.Avg, resume.P50, resume.P95, resume.Max)
		b.ReportMetric(float64(resume.Avg.Milliseconds()), "resume1_avg_ms")
		b.ReportMetric(float64(resume.P95.Milliseconds()), "resume1_p95_ms")
	}
	if pause.Count > 0 {
		b.Logf("  pause:   avg=%s p50=%s p95=%s max=%s", pause.Avg, pause.P50, pause.P95, pause.Max)
		b.ReportMetric(float64(pause.Avg.Milliseconds()), "pause_avg_ms")
		b.ReportMetric(float64(pause.P95.Milliseconds()), "pause_p95_ms")
	}
	if resume2.Count > 0 {
		b.Logf("  resume2: avg=%s p50=%s p95=%s max=%s", resume2.Avg, resume2.P50, resume2.P95, resume2.Max)
		b.ReportMetric(float64(resume2.Avg.Milliseconds()), "resume2_avg_ms")
		b.ReportMetric(float64(resume2.P95.Milliseconds()), "resume2_p95_ms")
	}
	if closeDur.Count > 0 {
		b.Logf("  close:   avg=%s p50=%s p95=%s max=%s", closeDur.Avg, closeDur.P50, closeDur.P95, closeDur.Max)
		b.ReportMetric(float64(closeDur.Avg.Milliseconds()), "close_avg_ms")
	}
	if total.Count > 0 {
		b.Logf("  total(measured): avg=%s p50=%s p95=%s max=%s", total.Avg, total.P50, total.P95, total.Max)
		b.ReportMetric(float64(total.Avg.Milliseconds()), "total_avg_ms")
		b.ReportMetric(float64(total.P95.Milliseconds()), "total_p95_ms")
	}

	if len(s.snapfileBytes) > 0 || len(s.memfileDiffBytes) > 0 || len(s.rootfsDiffBytes) > 0 {
		c, avg, p50, p95, max := summarizeBytes(s.snapfileBytes)
		if c > 0 {
			b.Logf("  snapfile bytes:   avg=%d p50=%d p95=%d max=%d", avg, p50, p95, max)
		}
		c, avg, p50, p95, max = summarizeBytes(s.memfileDiffBytes)
		if c > 0 {
			b.Logf("  memdiff bytes:    avg=%d p50=%d p95=%d max=%d", avg, p50, p95, max)
		}
		c, avg, p50, p95, max = summarizeBytes(s.rootfsDiffBytes)
		if c > 0 {
			b.Logf("  rootfsdiff bytes: avg=%d p50=%d p95=%d max=%d", avg, p50, p95, max)
		}
	}
}
