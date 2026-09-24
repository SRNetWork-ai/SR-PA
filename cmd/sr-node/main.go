// Command sr-node is the agent half of SR-UI's node fleet.
//
// It runs on a serving machine, enrolls once against the panel with a
// single-use join token, then reports in every 30 seconds and pulls its
// assignment whenever the panel answers with a config hash it is not already
// running.
//
// What it deliberately does NOT do yet is turn that assignment into a running
// Xray config; that is the next piece of work. Until it exists the agent
// refuses to claim a config is applied: it stores the bundle, reports the
// reason it is out of sync, and runs an apply hook if the operator supplies
// one. A fleet page that says "in sync" while nothing is serving traffic is
// worse than one that admits it does not know.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	agentVersion   = "0.1.0"
	defaultTick    = 30 * time.Second
	minTick        = 5 * time.Second
	httpTimeout    = 20 * time.Second
	hookTimeout    = 2 * time.Minute
	stateFileName  = "agent.json"
	bundleFileName = "bundle.json"
)

type options struct {
	url      string
	token    string
	dir      string
	hook     string
	corePath string
	interval int
	insecure bool
	once     bool
	version  bool
}

// state is everything the agent must survive a restart with. It is written with
// 0600 because nodeToken is this machine's credential on the panel.
type state struct {
	PanelURL    string `json:"panelUrl"`
	NodeToken   string `json:"nodeToken"`
	NodeId      int    `json:"nodeId"`
	NodeName    string `json:"nodeName"`
	Role        string `json:"role"`
	AppliedHash string `json:"appliedHash"`
}

type enrollResponse struct {
	Success   bool   `json:"success"`
	Msg       string `json:"msg"`
	NodeToken string `json:"nodeToken"`
	Node      struct {
		Id   int    `json:"id"`
		Name string `json:"name"`
		Role string `json:"role"`
	} `json:"node"`
	HeartbeatSeconds int `json:"heartbeatSeconds"`
}

type heartbeatRequest struct {
	AgentVersion string  `json:"agentVersion"`
	CoreVersion  string  `json:"coreVersion"`
	AppliedHash  string  `json:"appliedHash"`
	Uptime       int64   `json:"uptime"`
	CpuPct       float64 `json:"cpuPct"`
	MemPct       float64 `json:"memPct"`
	DiskPct      float64 `json:"diskPct"`
	Up           int64   `json:"up"`
	Down         int64   `json:"down"`
	LatencyMs    int64   `json:"latencyMs"`
	Error        string  `json:"error"`
}

type heartbeatResponse struct {
	Success    bool   `json:"success"`
	Msg        string `json:"msg"`
	ConfigHash string `json:"configHash"`
	InSync     bool   `json:"inSync"`
	ServerTime int64  `json:"serverTime"`
}

// bundle mirrors only the fields the agent acts on. The file it writes keeps the
// panel's full response, so a future apply step reads the whole assignment even
// though this version understands part of it.
type bundle struct {
	NodeId int    `json:"nodeId"`
	Node   string `json:"node"`
	Role   string `json:"role"`
	Hash   string `json:"hash"`
}

type agent struct {
	opts     options
	st       state
	client   *http.Client
	tick     time.Duration
	lastErr  string
	latency  int64
	cpuIdle  uint64
	cpuTotal uint64
}

func main() {
	var opts options
	flag.StringVar(&opts.url, "url", "", "panel base url, including the panel's secret path")
	flag.StringVar(&opts.token, "token", "", "single-use join token minted in the panel; enrolls or re-enrolls this node")
	flag.StringVar(&opts.dir, "dir", "/etc/sr-node", "state directory")
	flag.StringVar(&opts.hook, "apply", "", "program run after a new config is stored (default: apply.sh in the state directory when present)")
	flag.StringVar(&opts.corePath, "core", "", "path to the xray binary, used only to report its version")
	flag.IntVar(&opts.interval, "interval", 0, "seconds between heartbeats; overrides what the panel asked for")
	flag.BoolVar(&opts.insecure, "insecure", false, "accept the panel's certificate without verifying it (self-signed panels)")
	flag.BoolVar(&opts.once, "once", false, "run a single cycle and exit; used by the installer to prove the channel works")
	flag.BoolVar(&opts.version, "version", false, "print the agent version and exit")
	flag.Parse()

	if opts.version {
		fmt.Println("sr-node " + agentVersion)
		return
	}
	log.SetFlags(log.LstdFlags)
	if err := run(opts); err != nil {
		log.Fatalf("sr-node: %v", err)
	}
}

func run(opts options) error {
	if err := os.MkdirAll(opts.dir, 0o700); err != nil {
		return fmt.Errorf("state directory %s: %w", opts.dir, err)
	}

	a := &agent{opts: opts, tick: defaultTick}
	a.client = &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{InsecureSkipVerify: opts.insecure},
		},
	}

	if err := a.loadState(); err != nil {
		return err
	}
	if u := strings.TrimSpace(opts.url); u != "" {
		a.st.PanelURL = u
	}
	if a.st.PanelURL == "" {
		return errors.New("no panel url on record: pass -url the first time")
	}

	// A join token on the command line always enrolls, even when this node
	// already has one. That is the rotation path: a rebuilt machine or a token
	// believed leaked is fixed by minting a new one, not by editing a file.
	if token := strings.TrimSpace(opts.token); token != "" {
		if err := a.enroll(token); err != nil {
			return err
		}
	} else if a.st.NodeToken == "" {
		return errors.New("this node has never enrolled: mint a join token in the panel and pass it with -token")
	}

	if opts.interval > 0 {
		a.tick = time.Duration(opts.interval) * time.Second
	}
	if a.tick < minTick {
		a.tick = minTick
	}

	// Prime the CPU counters so the first reading is a real interval rather than
	// an average since boot, which on a server that was busy at 3am reads as
	// permanently busy.
	a.cpuIdle, a.cpuTotal, _ = cpuSample()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("sr-node %s reporting as node %d (%s) every %s", agentVersion, a.st.NodeId, a.st.NodeName, a.tick)
	a.cycle()
	if opts.once {
		return nil
	}

	ticker := time.NewTicker(a.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Print("sr-node: stopping")
			return nil
		case <-ticker.C:
			a.cycle()
		}
	}
}

// cycle is one heartbeat and, if the panel says this node is behind, one sync.
//
// Nothing in here is fatal. A panel that is restarting, a network that drops for
// a minute or a config that fails to apply are all things the next tick should
// retry; exiting would turn a transient failure into an operator's afternoon.
func (a *agent) cycle() {
	start := time.Now()
	var resp heartbeatResponse
	if err := a.call(http.MethodPost, "node/heartbeat", a.collect(), &resp, true); err != nil {
		log.Printf("sr-node: heartbeat failed: %v", err)
		return
	}
	a.latency = time.Since(start).Milliseconds()

	if resp.ConfigHash == "" || resp.ConfigHash == a.st.AppliedHash {
		if resp.ConfigHash != "" {
			a.lastErr = ""
		}
		return
	}
	if err := a.sync(resp.ConfigHash); err != nil {
		// Kept and sent with the next heartbeat: the reason a node is out of sync
		// belongs on the panel where the operator is looking, not only in a log on
		// the machine they would have to guess to open.
		a.lastErr = err.Error()
		log.Printf("sr-node: %v", err)
	}
}

func (a *agent) collect() heartbeatRequest {
	up, down := netTotals()
	return heartbeatRequest{
		AgentVersion: agentVersion,
		CoreVersion:  a.coreVersion(),
		AppliedHash:  a.st.AppliedHash,
		Uptime:       hostUptime(),
		// Rounded to whole numbers: one decimal place of CPU is noise on a fleet
		// page, and an integer decodes cleanly whatever numeric type the panel
		// keeps these in.
		CpuPct:    math.Round(a.cpuPct()),
		MemPct:    math.Round(memPct()),
		DiskPct:   math.Round(diskPct(a.opts.dir)),
		Up:        up,
		Down:      down,
		LatencyMs: a.latency,
		Error:     a.lastErr,
	}
}

func (a *agent) enroll(token string) error {
	var resp enrollResponse
	body := map[string]string{"token": token, "agentVersion": agentVersion}
	if err := a.call(http.MethodPost, "node/enroll", body, &resp, false); err != nil {
		return fmt.Errorf("enrollment failed: %w", err)
	}
	if !resp.Success || resp.NodeToken == "" {
		return fmt.Errorf("enrollment refused: %s", strings.TrimSpace(resp.Msg))
	}

	a.st.NodeToken = resp.NodeToken
	a.st.NodeId = resp.Node.Id
	a.st.NodeName = resp.Node.Name
	a.st.Role = resp.Node.Role
	// A fresh enrollment says nothing about what this machine is running. Keeping
	// the old hash would show the node as in sync with a config it may never have
	// received.
	a.st.AppliedHash = ""
	if resp.HeartbeatSeconds > 0 {
		a.tick = time.Duration(resp.HeartbeatSeconds) * time.Second
	}
	if err := a.saveState(); err != nil {
		return err
	}
	log.Printf("sr-node: enrolled as node %d (%s), role %s", a.st.NodeId, a.st.NodeName, a.st.Role)
	return nil
}

// sync stores the assignment and applies it if an apply hook exists.
func (a *agent) sync(hash string) error {
	raw, err := a.request(http.MethodGet, "node/config", nil, true)
	if err != nil {
		return fmt.Errorf("fetching config %s: %w", short(hash), err)
	}
	var b bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return fmt.Errorf("config %s is not readable: %w", short(hash), err)
	}
	if b.Hash == "" {
		return errors.New("the panel returned a config with no hash")
	}

	path := filepath.Join(a.opts.dir, bundleFileName)
	if err := writeAtomic(path, raw, 0o600); err != nil {
		return fmt.Errorf("storing config %s: %w", short(b.Hash), err)
	}

	hook := a.hookPath()
	if hook == "" {
		// Not an error in the sense of something broken, but the node is genuinely
		// not serving this config, and the panel must not be told otherwise.
		return fmt.Errorf("config %s stored at %s, but no apply hook is configured, so this node is not serving it", short(b.Hash), path)
	}
	if err := a.runHook(hook, path, &b); err != nil {
		return err
	}

	a.st.AppliedHash = b.Hash
	a.lastErr = ""
	if err := a.saveState(); err != nil {
		return err
	}
	log.Printf("sr-node: applied config %s", short(b.Hash))
	return nil
}

// hookPath resolves the apply hook, preferring the flag and falling back to a
// conventional apply.sh so an operator can drop a script in place without
// editing the unit file.
func (a *agent) hookPath() string {
	candidates := []string{strings.TrimSpace(a.opts.hook), filepath.Join(a.opts.dir, "apply.sh")}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		return candidate
	}
	return ""
}

func (a *agent) runHook(hook, bundlePath string, b *bundle) error {
	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, hook, bundlePath)
	cmd.Env = append(os.Environ(),
		"SR_NODE_BUNDLE="+bundlePath,
		"SR_NODE_HASH="+b.Hash,
		"SR_NODE_ID="+strconv.Itoa(b.NodeId),
		"SR_NODE_NAME="+b.Node,
		"SR_NODE_ROLE="+b.Role,
	)
	out, err := cmd.CombinedOutput()
	if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
		log.Printf("sr-node: apply hook said: %s", firstLine(trimmed))
	}
	if err != nil {
		return fmt.Errorf("apply hook %s failed: %w", hook, err)
	}
	return nil
}

func (a *agent) call(method, route string, body any, out any, auth bool) error {
	raw, err := a.request(method, route, body, auth)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("unreadable answer from %s: %s", route, firstLine(string(raw)))
	}
	return nil
}

func (a *agent) request(method, route string, body any, auth bool) ([]byte, error) {
	endpoint, err := joinURL(a.st.PanelURL, route)
	if err != nil {
		return nil, err
	}

	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequest(method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "sr-node/"+agentVersion)
	if auth {
		req.Header.Set("Authorization", "Bearer "+a.st.NodeToken)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		// Spelled out because it is the one failure an operator cannot read off a
		// status code: the node exists, its token does not.
		return nil, fmt.Errorf("the panel rejected this node's token (%s); mint a new join token and re-run with -token", firstLine(string(data)))
	case resp.StatusCode == http.StatusNotFound:
		// The agent routes live behind the panel's secret base path, so a 404 here
		// almost always means the url is missing that path rather than that the
		// panel is down.
		return nil, fmt.Errorf("%s not found at this address; check that the url includes the panel's secret path", route)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("%s %s: %s %s", method, route, resp.Status, firstLine(string(data)))
	}
	return data, nil
}

func (a *agent) loadState() error {
	data, err := os.ReadFile(filepath.Join(a.opts.dir, stateFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &a.st)
}

func (a *agent) saveState() error {
	data, err := json.MarshalIndent(a.st, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(a.opts.dir, stateFileName), data, 0o600)
}

// coreVersion is best effort and stays empty when no core path was given: an
// unknown version is better than a wrong one on the fleet page.
func (a *agent) coreVersion() string {
	path := strings.TrimSpace(a.opts.corePath)
	if path == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "-version").Output()
	if err != nil {
		return ""
	}
	line := firstLine(string(out))
	if fields := strings.Fields(line); len(fields) >= 2 {
		return fields[1]
	}
	return line
}

func (a *agent) cpuPct() float64 {
	idle, total, err := cpuSample()
	if err != nil {
		return 0
	}
	defer func() {
		a.cpuIdle, a.cpuTotal = idle, total
	}()
	if total <= a.cpuTotal {
		return 0
	}
	deltaTotal := float64(total - a.cpuTotal)
	deltaIdle := float64(idle - a.cpuIdle)
	busy := (deltaTotal - deltaIdle) / deltaTotal * 100
	return clampPct(busy)
}

func cpuSample() (uint64, uint64, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		var idle, total uint64
		for i, raw := range strings.Fields(line)[1:] {
			v, convErr := strconv.ParseUint(raw, 10, 64)
			if convErr != nil {
				continue
			}
			total += v
			// idle and iowait: a box waiting on disk is not a box doing work.
			if i == 3 || i == 4 {
				idle += v
			}
		}
		return idle, total, nil
	}
	return 0, 0, errors.New("no cpu line in /proc/stat")
}

func memPct() float64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	var total, available float64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		value, convErr := strconv.ParseFloat(fields[1], 64)
		if convErr != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = value
		case "MemAvailable:":
			available = value
		}
	}
	if total <= 0 {
		return 0
	}
	// MemAvailable, not MemFree: cache is memory the kernel will hand back on
	// demand, and counting it as used makes every healthy Linux box look full.
	return clampPct((total - available) / total * 100)
}

func diskPct(path string) float64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	total := float64(st.Blocks) * float64(st.Bsize)
	free := float64(st.Bavail) * float64(st.Bsize)
	if total <= 0 {
		return 0
	}
	// Bavail, not Bfree: the blocks reserved for root are not space this node can
	// use for logs or a core download.
	return clampPct((total - free) / total * 100)
}

// netTotals sums the machine's real interfaces. Loopback and container bridges
// are skipped: counting them would report traffic that never left the box.
func netTotals() (int64, int64) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	var up, down int64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		colon := strings.Index(line, ":")
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(line[:colon])
		if name == "lo" || strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "br-") {
			continue
		}
		fields := strings.Fields(line[colon+1:])
		if len(fields) < 9 {
			continue
		}
		if rx, convErr := strconv.ParseInt(fields[0], 10, 64); convErr == nil {
			down += rx
		}
		if tx, convErr := strconv.ParseInt(fields[8], 10, 64); convErr == nil {
			up += tx
		}
	}
	return up, down
}

func hostUptime() int64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return int64(seconds)
}

// joinURL appends an agent route to whatever base the operator was given,
// including the panel's secret path.
func joinURL(base, route string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", fmt.Errorf("panel url %q is not a url: %w", base, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("panel url %q needs a scheme (http or https)", base)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("panel url %q has no host", base)
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/" + strings.TrimPrefix(route, "/")
	return parsed.String(), nil
}

// writeAtomic never leaves a half-written credential or config behind, which a
// crash or a full disk during a plain write would.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func clampPct(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// firstLine keeps log lines and error strings to one readable line: an HTML
// error page from a reverse proxy is otherwise pasted whole into the journal.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
