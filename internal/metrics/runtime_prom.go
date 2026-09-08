package metrics

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/codeswhat/portwing/internal/docker"
)

// DockerMetricsClient is the Docker API subset required for Prometheus
// per-container metrics.
type DockerMetricsClient interface {
	ListContainers(ctx context.Context, all bool) ([]docker.ContainerJSON, error)
	ContainerStats(ctx context.Context, id string) (*docker.ContainerStatsResponse, error)
}

// WriteHostPrometheus appends host resource metrics when collection succeeds.
//
// A host with no procfs cannot produce these numbers, so the resource series
// are omitted there rather than written as zeros, which a scraper could not
// tell apart from a real reading of a completely idle machine.
// portwing_host_metrics_supported carries that distinction explicitly, so a
// dashboard can show "unsupported" instead of a flat zero line.
func WriteHostPrometheus(b *strings.Builder, collector *Collector) {
	if collector == nil {
		return
	}
	host, err := collector.Collect()
	fmt.Fprintf(b, "# HELP portwing_host_metrics_supported Whether host resource metrics are available on this platform (1) or not (0).\n")
	fmt.Fprintf(b, "# TYPE portwing_host_metrics_supported gauge\n")
	if err != nil || host == nil {
		fmt.Fprintf(b, "portwing_host_metrics_supported 0\n")
		return
	}
	fmt.Fprintf(b, "portwing_host_metrics_supported 1\n")

	fmt.Fprintf(b, "# HELP portwing_host_cpu_usage_percent Host CPU usage percentage.\n")
	fmt.Fprintf(b, "# TYPE portwing_host_cpu_usage_percent gauge\n")
	fmt.Fprintf(b, "portwing_host_cpu_usage_percent %g\n", host.CPUUsage)
	fmt.Fprintf(b, "# HELP portwing_host_memory_total_bytes Host total memory in bytes.\n")
	fmt.Fprintf(b, "# TYPE portwing_host_memory_total_bytes gauge\n")
	fmt.Fprintf(b, "portwing_host_memory_total_bytes %d\n", host.MemoryTotal)
	fmt.Fprintf(b, "# HELP portwing_host_memory_used_bytes Host used memory in bytes.\n")
	fmt.Fprintf(b, "# TYPE portwing_host_memory_used_bytes gauge\n")
	fmt.Fprintf(b, "portwing_host_memory_used_bytes %d\n", host.MemoryUsed)
	writeHostDiskPrometheus(b, host)
	fmt.Fprintf(b, "# HELP portwing_host_network_receive_bytes_total Host network bytes received (all non-lo interfaces).\n")
	fmt.Fprintf(b, "# TYPE portwing_host_network_receive_bytes_total counter\n")
	fmt.Fprintf(b, "portwing_host_network_receive_bytes_total %d\n", host.NetworkRxBytes)
	fmt.Fprintf(b, "# HELP portwing_host_network_transmit_bytes_total Host network bytes transmitted (all non-lo interfaces).\n")
	fmt.Fprintf(b, "# TYPE portwing_host_network_transmit_bytes_total counter\n")
	fmt.Fprintf(b, "portwing_host_network_transmit_bytes_total %d\n", host.NetworkTxBytes)
}

// writeHostDiskPrometheus appends the disk series, guarded by its own
// availability gauge.
//
// Disk needs no procfs but does need a readable Docker data root, so it fails
// independently of everything portwing_host_metrics_supported covers and needs
// its own signal. When the statfs fails the byte series are omitted rather than
// written as zeros, which a scraper cannot tell apart from an empty disk; the
// gauge is what stays behind so their absence is legible instead of silent.
func writeHostDiskPrometheus(b *strings.Builder, host *HostMetrics) {
	fmt.Fprintf(b, "# HELP portwing_host_disk_metrics_available Whether host disk usage could be read from the Docker data root (1) or not (0).\n")
	fmt.Fprintf(b, "# TYPE portwing_host_disk_metrics_available gauge\n")
	if !host.DiskMetricsAvailable {
		fmt.Fprintf(b, "portwing_host_disk_metrics_available 0\n")
		return
	}
	fmt.Fprintf(b, "portwing_host_disk_metrics_available 1\n")

	fmt.Fprintf(b, "# HELP portwing_host_disk_total_bytes Host total disk space in bytes.\n")
	fmt.Fprintf(b, "# TYPE portwing_host_disk_total_bytes gauge\n")
	fmt.Fprintf(b, "portwing_host_disk_total_bytes %d\n", host.DiskTotal)
	fmt.Fprintf(b, "# HELP portwing_host_disk_used_bytes Host used disk space in bytes.\n")
	fmt.Fprintf(b, "# TYPE portwing_host_disk_used_bytes gauge\n")
	fmt.Fprintf(b, "portwing_host_disk_used_bytes %d\n", host.DiskUsed)
}

// ContainerCollector samples per-container Docker metrics for one Docker
// client and shares a single in-flight collection between overlapping scrapes.
//
// Collection used to be per-call, so every scrape started its own eight-worker
// stats pool: N scrapes landing together cost Docker N ListContainers calls
// and N stats requests per container. Two Prometheus jobs, or a retry fired
// while a slow scrape is still running, hit exactly that. Callers keep one
// collector per client for the life of the process — building a fresh one per
// scrape brings the per-scrape pool straight back.
type ContainerCollector struct {
	client DockerMetricsClient

	mu     sync.Mutex
	flight *containerFlight
}

// containerFlight is one collection in progress. done is closed once results
// is set, and waiters counts the scrapes still interested in it.
type containerFlight struct {
	done    chan struct{}
	results []containerResult
	waiters int
	cancel  context.CancelFunc
}

// containerResult is one container's sampled metrics.
type containerResult struct {
	id    string
	name  string
	image string
	cpu   float64
	memU  uint64
	memL  uint64
	rxB   uint64
	txB   uint64
}

// NewContainerCollector returns a collector that samples client.
func NewContainerCollector(client DockerMetricsClient) *ContainerCollector {
	return &ContainerCollector{client: client}
}

// collect returns the sampled containers, joining the collection already in
// flight when there is one rather than starting a second pool against the same
// Docker daemon.
func (c *ContainerCollector) collect(ctx context.Context) []containerResult {
	c.mu.Lock()
	flight := c.flight
	if flight == nil {
		// The collection runs on a context of its own, cancelled when the
		// last waiter leaves rather than when the scrape that started it
		// does: otherwise one scraper hanging up would abort a collection the
		// others are still waiting on. A collection nobody waits on is still
		// cancelled immediately, exactly as a per-scrape one was.
		flightCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		flight = &containerFlight{done: make(chan struct{}), waiters: 1, cancel: cancel}
		c.flight = flight
		c.mu.Unlock()
		go c.run(flightCtx, flight)
	} else {
		flight.waiters++
		c.mu.Unlock()
	}

	select {
	case <-flight.done:
		c.leave(flight)
		return flight.results
	case <-ctx.Done():
		c.leave(flight)
		return nil
	}
}

// run samples every container and publishes the results to the waiters. It
// clears the flight before publishing so the next scrape starts a fresh
// collection instead of reading these results back as an untimed cache.
func (c *ContainerCollector) run(ctx context.Context, flight *containerFlight) {
	results := collectContainers(ctx, c.client)

	c.mu.Lock()
	flight.results = results
	if c.flight == flight {
		c.flight = nil
	}
	c.mu.Unlock()

	close(flight.done)
}

// leave drops one waiter from flight and cancels the collection when it was
// the last one still waiting.
func (c *ContainerCollector) leave(flight *containerFlight) {
	c.mu.Lock()
	flight.waiters--
	last := flight.waiters == 0
	if last && c.flight == flight {
		c.flight = nil
	}
	c.mu.Unlock()

	if last {
		flight.cancel()
	}
}

// collectContainers samples every container with a worker pool bounded to
// eight concurrent stats requests, each with a ten-second timeout, and returns
// only the containers whose stats call succeeded.
func collectContainers(ctx context.Context, client DockerMetricsClient) []containerResult {
	containers, err := client.ListContainers(ctx, false)
	if err != nil || len(containers) == 0 {
		return nil
	}

	const maxWorkers = 8
	results := make([]containerResult, len(containers))
	workerCount := min(maxWorkers, len(containers))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workerCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				container := containers[idx]
				name := container.ID
				if len(container.Names) > 0 {
					name = strings.TrimPrefix(container.Names[0], "/")
				}
				statsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				stats, err := client.ContainerStats(statsCtx, container.ID)
				cancel()
				if err != nil {
					continue
				}

				var rxBytes, txBytes uint64
				for _, network := range stats.Networks {
					rxBytes += network.RxBytes
					txBytes += network.TxBytes
				}
				results[idx] = containerResult{
					id:    container.ID,
					name:  name,
					image: container.Image,
					cpu:   float64(stats.CPUStats.CPUUsage.TotalUsage) / 1e9,
					memU:  stats.MemoryStats.Usage,
					memL:  stats.MemoryStats.Limit,
					rxB:   rxBytes,
					txB:   txBytes,
				}
			}
		}()
	}
	for i := range containers {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	valid := make([]containerResult, 0, len(results))
	for _, result := range results {
		if result.id != "" {
			valid = append(valid, result)
		}
	}
	return valid
}

// WriteContainerPrometheus appends per-container Docker metrics sampled by
// collector. Collection is bounded to eight concurrent stats requests, each
// with a ten-second timeout, and concurrent scrapes share one collection
// instead of each starting a pool of their own.
func WriteContainerPrometheus(
	ctx context.Context,
	b *strings.Builder,
	collector *ContainerCollector,
	escapeLabel func(string) string,
) {
	if collector == nil || collector.client == nil {
		return
	}
	valid := collector.collect(ctx)
	if len(valid) == 0 {
		return
	}

	fmt.Fprintf(b, "# HELP container_cpu_usage_seconds_total Cumulative CPU time consumed by the container in seconds.\n")
	fmt.Fprintf(b, "# TYPE container_cpu_usage_seconds_total counter\n")
	for _, result := range valid {
		fmt.Fprintf(b, "container_cpu_usage_seconds_total{id=\"%s\",name=\"%s\",image=\"%s\"} %g\n",
			escapeLabel(result.id), escapeLabel(result.name), escapeLabel(result.image), result.cpu)
	}
	fmt.Fprintf(b, "# HELP container_memory_usage_bytes Current memory usage of the container in bytes.\n")
	fmt.Fprintf(b, "# TYPE container_memory_usage_bytes gauge\n")
	for _, result := range valid {
		fmt.Fprintf(b, "container_memory_usage_bytes{id=\"%s\",name=\"%s\",image=\"%s\"} %d\n",
			escapeLabel(result.id), escapeLabel(result.name), escapeLabel(result.image), result.memU)
	}
	fmt.Fprintf(b, "# HELP container_spec_memory_limit_bytes Memory limit configured for the container in bytes.\n")
	fmt.Fprintf(b, "# TYPE container_spec_memory_limit_bytes gauge\n")
	for _, result := range valid {
		if result.memL == 0 {
			continue
		}
		fmt.Fprintf(b, "container_spec_memory_limit_bytes{id=\"%s\",name=\"%s\",image=\"%s\"} %d\n",
			escapeLabel(result.id), escapeLabel(result.name), escapeLabel(result.image), result.memL)
	}
	fmt.Fprintf(b, "# HELP container_network_receive_bytes_total Cumulative bytes received by the container across all network interfaces.\n")
	fmt.Fprintf(b, "# TYPE container_network_receive_bytes_total counter\n")
	for _, result := range valid {
		fmt.Fprintf(b, "container_network_receive_bytes_total{id=\"%s\",name=\"%s\",image=\"%s\"} %d\n",
			escapeLabel(result.id), escapeLabel(result.name), escapeLabel(result.image), result.rxB)
	}
	fmt.Fprintf(b, "# HELP container_network_transmit_bytes_total Cumulative bytes transmitted by the container across all network interfaces.\n")
	fmt.Fprintf(b, "# TYPE container_network_transmit_bytes_total counter\n")
	for _, result := range valid {
		fmt.Fprintf(b, "container_network_transmit_bytes_total{id=\"%s\",name=\"%s\",image=\"%s\"} %d\n",
			escapeLabel(result.id), escapeLabel(result.name), escapeLabel(result.image), result.txB)
	}
}
