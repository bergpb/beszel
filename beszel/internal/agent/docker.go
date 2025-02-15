package agent

import (
	"beszel/internal/entities/container"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Returns stats for all running containers
func (a *Agent) getDockerStats() ([]*container.Stats, error) {
	resp, err := a.dockerClient.Get("http://localhost/containers/json")
	if err != nil {
		a.closeIdleConnections(err)
		return nil, err
	}
	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(&a.apiContainerList); err != nil {
		slog.Error("Error decoding containers", "err", err)
		return nil, err
	}

	containersLength := len(*a.apiContainerList)
	containerStats := make([]*container.Stats, containersLength)

	// store valid ids to clean up old container ids from map
	validIds := make(map[string]struct{}, containersLength)

	var wg sync.WaitGroup

	for i, ctr := range *a.apiContainerList {
		ctr.IdShort = ctr.Id[:12]
		validIds[ctr.IdShort] = struct{}{}
		// check if container is less than 1 minute old (possible restart)
		// note: can't use Created field because it's not updated on restart
		if strings.Contains(ctr.Status, "second") {
			// if so, remove old container data
			a.deleteContainerStatsSync(ctr.IdShort)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			stats, err := a.getContainerStats(ctr)
			if err != nil {
				// close idle connections if error is a network timeout
				isTimeout := a.closeIdleConnections(err)
				// delete container from map if not a timeout
				if !isTimeout {
					a.deleteContainerStatsSync(ctr.IdShort)
				}
				// retry once
				stats, err = a.getContainerStats(ctr)
				if err != nil {
					slog.Error("Error getting container stats", "err", err)
				}
			}
			containerStats[i] = stats
		}()
	}

	wg.Wait()

	// remove old / invalid container stats
	for id := range a.containerStatsMap {
		if _, exists := validIds[id]; !exists {
			delete(a.containerStatsMap, id)
		}
	}

	return containerStats, nil
}

// Returns stats for individual container
func (a *Agent) getContainerStats(ctr container.ApiInfo) (*container.Stats, error) {
	name := ctr.Names[0][1:]

	resp, err := a.dockerClient.Get("http://localhost/containers/" + ctr.IdShort + "/stats?stream=0&one-shot=1")
	if err != nil {
		return &container.Stats{Name: name}, err
	}
	defer resp.Body.Close()

	a.containerStatsMutex.Lock()
	defer a.containerStatsMutex.Unlock()

	// add empty values if they doesn't exist in map
	stats, initialized := a.containerStatsMap[ctr.IdShort]
	if !initialized {
		stats = &container.Stats{Name: name}
		a.containerStatsMap[ctr.IdShort] = stats
	}

	// reset current stats
	stats.Cpu = 0
	stats.Mem = 0
	stats.NetworkSent = 0
	stats.NetworkRecv = 0

	// docker host container stats response
	var res container.ApiStats
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return stats, err
	}

	// check if container has valid data, otherwise may be in restart loop (#103)
	if res.MemoryStats.Usage == 0 {
		return stats, fmt.Errorf("%s - no memory stats - see https://github.com/henrygd/beszel/issues/144", name)
	}

	// memory (https://docs.docker.com/reference/cli/docker/container/stats/)
	memCache := res.MemoryStats.Stats.InactiveFile
	if memCache == 0 {
		memCache = res.MemoryStats.Stats.Cache
	}
	usedMemory := res.MemoryStats.Usage - memCache

	// cpu
	cpuDelta := res.CPUStats.CPUUsage.TotalUsage - stats.PrevCpu[0]
	systemDelta := res.CPUStats.SystemUsage - stats.PrevCpu[1]
	cpuPct := float64(cpuDelta) / float64(systemDelta) * 100
	if cpuPct > 100 {
		return stats, fmt.Errorf("%s cpu pct greater than 100: %+v", name, cpuPct)
	}
	stats.PrevCpu = [2]uint64{res.CPUStats.CPUUsage.TotalUsage, res.CPUStats.SystemUsage}

	// network
	var total_sent, total_recv uint64
	for _, v := range res.Networks {
		total_sent += v.TxBytes
		total_recv += v.RxBytes
	}
	var sent_delta, recv_delta float64
	// prevent first run from sending all prev sent/recv bytes
	if initialized {
		secondsElapsed := time.Since(stats.PrevNet.Time).Seconds()
		sent_delta = float64(total_sent-stats.PrevNet.Sent) / secondsElapsed
		recv_delta = float64(total_recv-stats.PrevNet.Recv) / secondsElapsed
	}
	stats.PrevNet.Sent = total_sent
	stats.PrevNet.Recv = total_recv
	stats.PrevNet.Time = time.Now()

	stats.Cpu = twoDecimals(cpuPct)
	stats.Mem = bytesToMegabytes(float64(usedMemory))
	stats.NetworkSent = bytesToMegabytes(sent_delta)
	stats.NetworkRecv = bytesToMegabytes(recv_delta)

	return stats, nil
}

// Creates a new http client for docker api
func newDockerClient() *http.Client {
	dockerHost := "unix:///var/run/docker.sock"
	if dockerHostEnv, exists := os.LookupEnv("DOCKER_HOST"); exists {
		slog.Info("DOCKER_HOST", "host", dockerHostEnv)
		dockerHost = dockerHostEnv
	}

	parsedURL, err := url.Parse(dockerHost)
	if err != nil {
		slog.Error("Error parsing DOCKER_HOST", "err", err)
		os.Exit(1)
	}

	transport := &http.Transport{
		ForceAttemptHTTP2:   false,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
		MaxConnsPerHost:     10,
		MaxIdleConnsPerHost: 10,
		DisableKeepAlives:   false,
	}

	switch parsedURL.Scheme {
	case "unix":
		transport.DialContext = func(ctx context.Context, proto, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", parsedURL.Path)
		}
	case "tcp", "http", "https":
		transport.DialContext = func(ctx context.Context, proto, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", parsedURL.Host)
		}
	default:
		slog.Error("Invalid DOCKER_HOST", "scheme", parsedURL.Scheme)
		os.Exit(1)
	}

	return &http.Client{
		Timeout:   time.Second,
		Transport: transport,
	}
}

// Closes idle connections on timeouts to prevent reuse of stale connections
func (a *Agent) closeIdleConnections(err error) (isTimeout bool) {
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		slog.Warn("Closing idle connections", "err", err)
		a.dockerClient.Transport.(*http.Transport).CloseIdleConnections()
		return true
	}
	return false
}
