package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Traffic served by this node is invisible to the panel unless the node says
// so. Nothing on the master touches these connections, so without this file an
// account moved to a node has an unlimited quota and an expiry nobody enforces
// against usage.
//
// The numbers come from Xray's metrics handler, which publishes the same
// counters the gRPC stats API serves, as expvar JSON on loopback. Using the
// API instead would mean linking the panel's xray package, and that package
// embeds the 160 MB core; a node agent that ships a second copy of Xray to
// report a few integers is not an agent.

const (
	// Matches service.NodeMetricsPort on the master, which is what put the
	// metrics inbound in this node's config in the first place.
	metricsPort    = 62791
	metricsPath    = "/debug/vars"
	metricsTimeout = 10 * time.Second
	metricsMaxBody = 8 << 20

	// The most one account may be reported as having moved in one interval.
	// Past this the number is a misread counter rather than a busy customer,
	// and the panel refuses it anyway - catching it here keeps the rest of the
	// report from being dragged into the argument.
	metricsSanity = int64(1) << 44
)

type coreCounter struct {
	Uplink   int64 `json:"uplink"`
	Downlink int64 `json:"downlink"`
}

type coreStats struct {
	Stats struct {
		Inbound map[string]coreCounter `json:"inbound"`
		User    map[string]coreCounter `json:"user"`
	} `json:"stats"`
}

type trafficSample struct {
	Email string `json:"email"`
	Up    int64  `json:"up"`
	Down  int64  `json:"down"`
}

type tagSample struct {
	Tag  string `json:"tag"`
	Up   int64  `json:"up"`
	Down int64  `json:"down"`
}

type trafficReport struct {
	Users    []trafficSample `json:"users"`
	Inbounds []tagSample     `json:"inbounds"`
}

type trafficReply struct {
	Success  bool   `json:"success"`
	Msg      string `json:"msg"`
	Accepted int    `json:"accepted"`
}

// trafficDelta is one interval's difference plus the absolute readings that
// produced it. The readings become the new baseline only once the panel has
// accepted the difference, which is the whole reason they are carried instead
// of being written straight back.
type trafficDelta struct {
	report   trafficReport
	userUp   map[string]int64
	userDown map[string]int64
	tagUp    map[string]int64
	tagDown  map[string]int64
}

// trafficMeter turns Xray's cumulative counters into deltas the panel can add.
type trafficMeter struct {
	mu       sync.Mutex
	client   *http.Client
	userUp   map[string]int64
	userDown map[string]int64
	tagUp    map[string]int64
	tagDown  map[string]int64
}

var xrayTraffic = &trafficMeter{}

func (m *trafficMeter) http() *http.Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client == nil {
		// Its own client, with no proxy: the panel's may legitimately go through
		// one, and a loopback request that follows HTTP_PROXY out to the internet
		// would be a strange way to read a local counter.
		m.client = &http.Client{
			Timeout:   metricsTimeout,
			Transport: &http.Transport{},
		}
	}
	return m.client
}

func (m *trafficMeter) scrape() (*coreStats, error) {
	endpoint := url.URL{
		Scheme: "http",
		Host:   "127.0.0.1:" + strconv.Itoa(metricsPort),
		Path:   metricsPath,
	}
	resp, err := m.http().Get(endpoint.String())
	if err != nil {
		return nil, fmt.Errorf("reading the core's counters: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, metricsMaxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the core's metrics endpoint answered %s", resp.Status)
	}

	var stats coreStats
	if err := json.Unmarshal(data, &stats); err != nil {
		return nil, fmt.Errorf("the core's counters are not readable: %s", firstLine(string(data)))
	}
	return &stats, nil
}

// measure diffs a reading against the last accepted one.
func (m *trafficMeter) measure(stats *coreStats) trafficDelta {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.userUp == nil {
		m.userUp = map[string]int64{}
		m.userDown = map[string]int64{}
		m.tagUp = map[string]int64{}
		m.tagDown = map[string]int64{}
	}

	delta := trafficDelta{
		userUp:   map[string]int64{},
		userDown: map[string]int64{},
		tagUp:    map[string]int64{},
		tagDown:  map[string]int64{},
	}

	for email, counter := range stats.Stats.User {
		name := strings.TrimSpace(email)
		if name == "" {
			continue
		}
		delta.userUp[name] = counter.Uplink
		delta.userDown[name] = counter.Downlink
		up := metricsStep(m.userUp[name], counter.Uplink)
		down := metricsStep(m.userDown[name], counter.Downlink)
		if up == 0 && down == 0 {
			continue
		}
		if up > metricsSanity || down > metricsSanity {
			continue
		}
		delta.report.Users = append(delta.report.Users, trafficSample{Email: name, Up: up, Down: down})
	}

	for tag, counter := range stats.Stats.Inbound {
		name := strings.TrimSpace(tag)
		// The agent's own two loopback listeners. Their bytes are this agent
		// talking to itself.
		if name == "" || name == "api" || name == "metrics" {
			continue
		}
		delta.tagUp[name] = counter.Uplink
		delta.tagDown[name] = counter.Downlink
		up := metricsStep(m.tagUp[name], counter.Uplink)
		down := metricsStep(m.tagDown[name], counter.Downlink)
		if up == 0 && down == 0 {
			continue
		}
		delta.report.Inbounds = append(delta.report.Inbounds, tagSample{Tag: name, Up: up, Down: down})
	}

	return delta
}

// commit moves the baselines forward. Merged rather than replaced: a counter
// missing from this reading (an inbound that went away with a config change)
// keeps its old baseline, so if it ever comes back the bytes it already
// reported are not reported a second time.
func (m *trafficMeter) commit(delta trafficDelta) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.userUp == nil {
		m.userUp = map[string]int64{}
		m.userDown = map[string]int64{}
		m.tagUp = map[string]int64{}
		m.tagDown = map[string]int64{}
	}
	for name, value := range delta.userUp {
		m.userUp[name] = value
	}
	for name, value := range delta.userDown {
		m.userDown[name] = value
	}
	for name, value := range delta.tagUp {
		m.tagUp[name] = value
	}
	for name, value := range delta.tagDown {
		m.tagDown[name] = value
	}
}

// metricsStep is the difference between two readings of a counter that only
// ever climbs - until the core restarts and it starts again from zero, which
// is what the second branch is for.
func metricsStep(last, current int64) int64 {
	if current <= 0 {
		return 0
	}
	if current < last {
		return current
	}
	return current - last
}

// reportTraffic sends this interval's bytes to the panel.
//
// Failures here are worth a log line and nothing more. The node is still
// serving, the counters keep climbing, and the next tick sends everything that
// did not land this time.
func (a *agent) reportTraffic() error {
	if !xrayCore.Running() {
		return nil
	}
	stats, err := xrayTraffic.scrape()
	if err != nil {
		return err
	}

	delta := xrayTraffic.measure(stats)
	if len(delta.report.Users) == 0 && len(delta.report.Inbounds) == 0 {
		// An idle interval still moves the baselines: the readings are current
		// and identical to the stored ones, so committing costs nothing and keeps
		// the maps in step with a core that may have just restarted into zeros.
		xrayTraffic.commit(delta)
		return nil
	}

	var reply trafficReply
	if err := a.call(http.MethodPost, "node/traffic", delta.report, &reply, true); err != nil {
		return fmt.Errorf("reporting traffic: %w", err)
	}
	if !reply.Success {
		return fmt.Errorf("the panel refused this node's traffic report: %s", firstLine(reply.Msg))
	}
	xrayTraffic.commit(delta)
	return nil
}
