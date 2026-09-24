package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The node's Xray lives here: resolved, configured, started, watched and
// restarted by the agent itself.
//
// The obvious alternative was a systemd unit the installer drops next to the
// agent's own. It was rejected because the two would then disagree about who
// owns the core: the agent writes a config and asks systemd to reload, systemd
// restarts the core on its own schedule after a crash, and the panel is told a
// hash is applied by a process that cannot see whether the core took it. Owning
// the child process means "applied" can mean what it says - the core parsed
// this config, started, and was still alive a moment later.

const (
	configFileName = "config.json"

	coreTestTimeout = 15 * time.Second
	coreStopTimeout = 8 * time.Second

	// How long to watch a freshly started core before believing it. A config
	// Xray parses but cannot bind - a port something else on the machine already
	// holds - dies within a second, and reporting that as applied is the exact
	// lie the fleet page exists to avoid.
	coreSettleWindow = 2 * time.Second

	// masterToken is what the panel writes as a relay's egress address when the
	// egress is the panel's own server. The master cannot know which address a
	// node reaches it on; the agent does, because it is already talking to it.
	masterToken = "@master"
)

var xrayCore = &coreProcess{}

type coreProcess struct {
	mu      sync.Mutex
	path    string
	dir     string
	cmd     *exec.Cmd
	lastLog string
	exit    string
	version string
}

// Configure resolves the core binary once, at startup. A missing core is not a
// startup failure: the agent still enrolls and still reports in, and the panel
// gets told why nothing is serving, which is far more useful than an agent that
// refused to start on a machine nobody is logged into.
func (c *coreProcess) Configure(dir, explicit string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dir = dir
	c.path = resolveCore(explicit)
	if c.path == "" {
		log.Print("sr-node: no xray binary found yet; pass -core once one is installed")
		return
	}
	log.Printf("sr-node: core binary at %s", c.path)
}

func (c *coreProcess) Path() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path
}

func (c *coreProcess) ConfigPath() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dir == "" {
		return ""
	}
	return filepath.Join(c.dir, configFileName)
}

func (c *coreProcess) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cmd != nil && c.cmd.Process != nil
}

func (c *coreProcess) LastLog() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastLog != "" {
		return c.lastLog
	}
	return c.exit
}

// Version is cached: the fleet page wants it every heartbeat, and forking the
// core twice a minute to ask it something that changes on upgrades only is a
// waste on the small machines relays tend to be.
func (c *coreProcess) Version() string {
	c.mu.Lock()
	cached, path := c.version, c.path
	c.mu.Unlock()
	if cached != "" || path == "" {
		return cached
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "-version").Output()
	if err != nil {
		return ""
	}
	version := firstLine(string(out))
	if fields := strings.Fields(version); len(fields) >= 2 {
		version = fields[1]
	}
	c.mu.Lock()
	c.version = version
	c.mu.Unlock()
	return version
}

// Apply installs a config and brings the core up on it.
//
// The staging dance matters: the config is written beside the live one, tested,
// and only then renamed over it. A node that is serving traffic on a config
// that works must not lose it because the panel sent one that does not.
func (c *coreProcess) Apply(config []byte, panelURL string) error {
	if len(config) == 0 {
		return errors.New("the panel sent no config for this node")
	}
	if c.Path() == "" {
		return errors.New("no xray binary on this node: install one, or point the agent at it with -core")
	}
	path := c.ConfigPath()
	if path == "" {
		return errors.New("the agent has no state directory to keep the config in")
	}

	prepared, err := prepareConfig(config, panelURL)
	if err != nil {
		return err
	}

	staged := path + ".new"
	if err := writeAtomic(staged, prepared, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", staged, err)
	}
	if err := c.test(staged); err != nil {
		_ = os.Remove(staged)
		return err
	}
	if err := os.Rename(staged, path); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("installing %s: %w", path, err)
	}
	return c.restart()
}

// Ensure restarts a core that died on its own - out of memory, killed, crashed.
// Nothing else on the machine is watching, and a node whose core is dead looks
// perfectly healthy from the outside: the agent still reports, the panel still
// shows it online, and not one client connects.
func (c *coreProcess) Ensure() error {
	if c.Running() {
		return nil
	}
	path := c.ConfigPath()
	if path == "" || c.Path() == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		// Nothing has been applied yet. Not a fault, just a node waiting for work.
		return nil
	}
	log.Print("sr-node: core is not running; starting it")
	return c.restart()
}

func (c *coreProcess) Stop() {
	c.mu.Lock()
	cmd := c.cmd
	c.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}

	_ = cmd.Process.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(coreStopTimeout)
	for time.Now().Before(deadline) {
		if !c.Running() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	// A core that ignores SIGTERM still holds the ports the new one needs.
	_ = cmd.Process.Kill()
	time.Sleep(300 * time.Millisecond)
}

func (c *coreProcess) restart() error {
	c.Stop()
	if err := c.start(); err != nil {
		return err
	}
	time.Sleep(coreSettleWindow)
	if c.Running() {
		return nil
	}
	if reason := c.LastLog(); reason != "" {
		return fmt.Errorf("the core started and stopped again: %s", reason)
	}
	return errors.New("the core started and stopped again without saying why")
}

func (c *coreProcess) start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil {
		return nil
	}
	if c.path == "" {
		return errors.New("no xray binary on this node")
	}
	configPath := filepath.Join(c.dir, configFileName)
	if _, err := os.Stat(configPath); err != nil {
		return fmt.Errorf("no config to run at %s", configPath)
	}

	cmd := exec.Command(c.path, "run", "-c", configPath)
	cmd.Dir = filepath.Dir(c.path)
	cmd.Env = coreEnv(c.path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the core: %w", err)
	}

	c.cmd = cmd
	c.exit = ""
	go c.drain(stdout)
	go c.drain(stderr)
	go func() {
		waitErr := cmd.Wait()
		c.mu.Lock()
		if c.cmd == cmd {
			c.cmd = nil
		}
		if waitErr != nil {
			c.exit = waitErr.Error()
		}
		c.mu.Unlock()
	}()
	log.Printf("sr-node: core started on %s", configPath)
	return nil
}

// test asks the core whether it would accept the config, before the config is
// allowed anywhere near the running one.
func (c *coreProcess) test(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), coreTestTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, c.Path(), "run", "-test", "-c", path).CombinedOutput()
	if err == nil {
		return nil
	}
	text := strings.TrimSpace(string(out))
	lower := strings.ToLower(text)
	// Not every core build has -test. Not being able to check is not a reason to
	// refuse work: the supervised start finds out for real a moment later.
	if strings.Contains(lower, "not defined") || strings.Contains(lower, "unknown flag") || strings.Contains(lower, "invalid flag") {
		return nil
	}
	if text == "" {
		text = err.Error()
	}
	return fmt.Errorf("the core refused this config: %s", firstLine(text))
}

// drain keeps the core's output where an operator will actually look - the
// agent's journal - and keeps the last line for the panel, because the reason a
// node is broken belongs on the page the operator is staring at.
func (c *coreProcess) drain(r io.ReadCloser) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := firstLine(scanner.Text())
		if line == "" {
			continue
		}
		c.mu.Lock()
		c.lastLog = line
		c.mu.Unlock()
		log.Printf("sr-node: core: %s", line)
	}
	_ = r.Close()
}

// prepareConfig turns the master's rendering into something this machine can
// run, which for a relay means filling in the one address the master could not
// know: its own.
func prepareConfig(config []byte, panelURL string) ([]byte, error) {
	text := string(config)
	if strings.Contains(text, masterToken) {
		host, err := masterHost(panelURL)
		if err != nil {
			return nil, err
		}
		text = strings.ReplaceAll(text, quoted(masterToken), quoted(host))
	}
	if !json.Valid([]byte(text)) {
		return nil, errors.New("the panel sent a config that is not valid json")
	}
	return []byte(text), nil
}

func quoted(value string) string {
	mark := string(rune(34))
	return mark + value + mark
}

// masterHost is the address this node reaches the panel on, which is the
// address a relay forwards to. It is taken from the url the node enrolled with
// rather than from anything the panel says about itself, because that url is
// the one address already proven to work from here.
func masterHost(panelURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(panelURL))
	if err != nil || parsed.Hostname() == "" {
		return "", fmt.Errorf("cannot tell the panel's address from %q, and this node relays to it", panelURL)
	}
	host := parsed.Hostname()
	for _, r := range host {
		plain := r == '.' || r == '-' || r == '_' || r == ':' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !plain {
			return "", fmt.Errorf("the panel address %q is not a plain host name", host)
		}
	}
	return host, nil
}

// resolveCore finds an xray binary without being told where one is. The order
// is deliberate: what the operator said, then what the node installer puts in
// place, then the usual system locations, then the path.
func resolveCore(explicit string) string {
	candidates := []string{
		strings.TrimSpace(explicit),
		strings.TrimSpace(os.Getenv("SR_NODE_CORE")),
	}
	archName := "xray-linux-" + runtime.GOARCH
	for _, dir := range []string{"/etc/sr-node", "/etc/sr-node/bin", "/usr/local/bin", "/usr/bin", "/opt/sr-node", "/opt/sr-ui/bin"} {
		candidates = append(candidates, filepath.Join(dir, "xray"), filepath.Join(dir, archName))
	}

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
	if found, err := exec.LookPath("xray"); err == nil {
		return found
	}
	return ""
}

// coreEnv points the core at its geo data when it sits next to the binary,
// which is where the node installer puts it. Without this a routing rule that
// names a geosite quietly matches nothing.
func coreEnv(path string) []string {
	env := os.Environ()
	dir := filepath.Dir(path)
	if _, err := os.Stat(filepath.Join(dir, "geoip.dat")); err == nil {
		env = append(env, "XRAY_LOCATION_ASSET="+dir)
	}
	return env
}
