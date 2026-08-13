// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// pp:data-source computed

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/lsconfig"
)

// DefaultTestInstancePort is the throwaway validation port. It sits far above
// the startPort span llama-swap assigns from and outside the reserved
// well-known list, so a validation instance can never collide with a real seat
// or with a service port on this host.
const DefaultTestInstancePort = 18795

// reservedPorts must never be bound by a throwaway instance.
var reservedPorts = map[int]bool{80: true, 443: true, 3000: true, 5000: true, 8000: true, 6443: true}

type testInstanceResult struct {
	SchemaVersion    string   `json:"schema_version"`
	ConfigPath       string   `json:"config_path"`
	ConfigSha        string   `json:"config_sha256"`
	Binary           string   `json:"binary"`
	Port             int      `json:"port"`
	Listen           string   `json:"listen"`
	Healthy          bool     `json:"healthy"`
	BootMillis       int64    `json:"boot_ms"`
	ExpectedModels   int      `json:"expected_models"`
	RegisteredModels int      `json:"registered_models"`
	Missing          []string `json:"missing_models,omitempty"`
	Unexpected       []string `json:"unexpected_models,omitempty"`
	Killed           bool     `json:"killed"`
	ExitConfirmed    bool     `json:"exit_confirmed"`
	ExitStatus       string   `json:"exit_status,omitempty"`
	Stderr           string   `json:"stderr_tail,omitempty"`
	Notes            []string `json:"notes,omitempty"`
}

const testInstanceSchemaVersion = "testinstance/1"

func newConfigTestInstanceCmd(flags *rootFlags) *cobra.Command {
	var (
		port    int
		binary  string
		timeout time.Duration
	)

	cmd := &cobra.Command{
		Use: "testinstance [file]",
		// `config test` is the name the operator ritual uses; it is registered
		// as an alias rather than the primary because a Go source file named
		// config_test.go is a test file, not a command.
		Aliases: []string{"test"},
		Short:   "Boot a throwaway llama-swap on a scratch port, count the models it registers, then kill it.",
		Long: "Validate a config by BOOTING it, in a disposable instance that cannot touch\n" +
			"the live service.\n\n" +
			"Schema validation and lint catch what can be checked statically. Only\n" +
			"llama-swap itself can tell you it will parse the routing block, resolve every\n" +
			"matrix var, and register the roster you expect — which is why the operator\n" +
			"ritual has always been \"boot it on a scratch port first\".\n\n" +
			"The instance binds 127.0.0.1 on a scratch port (default " + strconv.Itoa(DefaultTestInstancePort) + "), runs with a\n" +
			"hidden window so it cannot flash a console over your work, is polled on\n" +
			"/v1/models until healthy, and is then killed with its exit confirmed.\n\n" +
			"It refuses to start if the port is already listening (exit " + fmt.Sprint(ExitPortConflict) + "), if the port is\n" +
			"inside the config's own startPort span, or if it is a reserved well-known\n" +
			"port. Booting a config does NOT load any model: llama-swap registers the\n" +
			"roster and waits.",
		Example: "  llamaswap-pp-cli config testinstance ./candidate.yaml\n" +
			"  llamaswap-pp-cli config test ./candidate.yaml --port 18795 --json\n" +
			"  llamaswap-pp-cli config testinstance --dry-run",
		Annotations: map[string]string{
			// Spawns a process: not read-only.
			"mcp:local-write":     "true",
			"pp:typed-exit-codes": fmt.Sprintf("0,1,2,%d,%d", ExitPortConflict, ExitConfigInvalid),
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			path, err := resolveConfigPath(args, 0)
			if err != nil {
				return err
			}
			f, err := loadConfigFile(path)
			if err != nil {
				return err
			}
			if port == 0 {
				port = DefaultTestInstancePort
			}
			bin := binary
			if strings.TrimSpace(bin) == "" {
				bin = defaultLlamaSwapBinary(path)
			}
			listen := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

			if err := validateScratchPort(port, f); err != nil {
				return err
			}
			plan := []string{
				fmt.Sprintf("spawn (hidden window): %s --config %s --listen %s", bin, path, listen),
				fmt.Sprintf("poll http://%s/v1/models until healthy (timeout %s)", listen, timeout),
				fmt.Sprintf("expect %d registered model(s)", len(f.Models)),
				"kill the process and confirm it exited",
				"the live llama-swap service and its config are never touched",
			}
			if verifyPlan(out, flags, "boot a throwaway llama-swap instance", plan) {
				return nil
			}

			res, err := runTestInstance(cmd.Context(), bin, path, listen, port, f, timeout, cmd.ErrOrStderr())
			if res != nil {
				if wantsJSON(out, flags) {
					if perr := printJSONFiltered(out, res, flags); perr != nil {
						return perr
					}
				} else {
					printTestInstanceHuman(cmd, res)
				}
			}
			if err != nil {
				return err
			}
			if !res.Healthy {
				return errConfigInvalid(fmt.Errorf("throwaway instance never became healthy on %s", listen))
			}
			if res.RegisteredModels != res.ExpectedModels {
				return errConfigInvalid(fmt.Errorf("registered %d model(s), config declares %d", res.RegisteredModels, res.ExpectedModels))
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&port, "port", DefaultTestInstancePort, "scratch port on 127.0.0.1 for the throwaway instance")
	cmd.Flags().StringVar(&binary, "binary", "", "llama-swap executable (default: alongside the config)")
	cmd.Flags().DurationVar(&timeout, "timeout", 25*time.Second, "how long to wait for the throwaway instance to answer /v1/models")
	return cmd
}

// defaultLlamaSwapBinary finds the llama-swap executable to boot a candidate
// with.
//
// Order matters: next to the CANDIDATE first (a self-contained test tree
// brings its own binary), then next to the LIVE config, then $PATH. The
// live-config fallback is the one that makes the command usable for its main
// job — validating a candidate that lives in a scratch directory, far from the
// deployment, where "the binary is next to the config" is false.
func defaultLlamaSwapBinary(configPath string) string {
	name := "llama-swap"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	var dirs []string
	dirs = append(dirs, filepath.Dir(configPath))
	if live, err := lsconfig.DefaultConfigPath(); err == nil {
		dirs = append(dirs, filepath.Dir(live))
	}
	for _, d := range dirs {
		candidate := filepath.Join(d, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return filepath.Join(dirs[0], name)
}

func validateScratchPort(port int, f *lsconfig.File) error {
	if port < 1 || port > 65535 {
		return usageErr(fmt.Errorf("port %d is out of range", port))
	}
	if reservedPorts[port] {
		return errPortConflict(fmt.Errorf("port %d is on the reserved well-known list and must never be bound by a throwaway instance", port))
	}
	if f.StartPort > 0 && port >= f.StartPort && port < f.StartPort+len(f.Models)+16 {
		return errPortConflict(fmt.Errorf("port %d is inside the config's own startPort span (%d..%d); the instance would fight its own seats for the port",
			port, f.StartPort, f.StartPort+len(f.Models)+15))
	}
	if busy, _ := probeLoopbackListener(port); busy {
		return errPortConflict(fmt.Errorf("127.0.0.1:%d already has a listener — refusing to start (pass --port to pick another)", port))
	}
	return nil
}

func probeLoopbackListener(port int) (bool, error) {
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := d.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err == nil {
		_ = conn.Close()
		return true, nil
	}
	return false, nil
}

func runTestInstance(ctx context.Context, bin, configPath, listen string, port int, f *lsconfig.File, timeout time.Duration, progress io.Writer) (*testInstanceResult, error) {
	res := &testInstanceResult{
		SchemaVersion: testInstanceSchemaVersion,
		ConfigPath:    f.Path, ConfigSha: f.Sha256,
		Binary: bin, Port: port, Listen: listen,
		ExpectedModels: len(f.Models),
	}
	if _, err := os.Stat(bin); err != nil {
		return res, fmt.Errorf("llama-swap binary %s not found: %w (pass --binary)", bin, err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	proc := exec.CommandContext(runCtx, bin, "--config", configPath, "--listen", listen)
	hideSpawnedWindow(proc)
	var stderr strings.Builder
	proc.Stdout = io.Discard
	proc.Stderr = &stderr
	start := time.Now()
	if err := proc.Start(); err != nil {
		return res, fmt.Errorf("start throwaway instance: %w", err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- proc.Wait() }()

	// noteExit records a terminal status exactly once. proc.Wait() can only be
	// reaped a single time, so whichever path observes it first owns the
	// result; without this the cleanup path re-reads an already-drained
	// channel, "kills" a dead process, and reports a phantom 5s timeout
	// alongside the exit status the poll loop already captured.
	exitObserved := false
	noteExit := func(err error) {
		if exitObserved {
			return
		}
		exitObserved = true
		res.ExitConfirmed = true
		res.ExitStatus = exitStatusString(err)
	}

	// Always terminate, on every path out of here. A leaked llama-swap holding
	// a scratch port is exactly the mess this command must not create.
	defer func() {
		if !exitObserved {
			if proc.Process != nil {
				_ = proc.Process.Kill()
				res.Killed = true
			}
			select {
			case err := <-waitDone:
				noteExit(err)
			case <-time.After(5 * time.Second):
				res.Notes = append(res.Notes, "process did not report exit within 5s after kill")
			}
		}
		res.Stderr = tailString(stderr.String(), 1200)
	}()

	deadline := time.Now().Add(timeout)
	url := "http://" + listen + "/v1/models"
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		select {
		case err := <-waitDone:
			noteExit(err)
			res.Notes = append(res.Notes, "instance exited before answering — the config was rejected at boot")
			return res, nil
		case <-runCtx.Done():
			return res, runCtx.Err()
		default:
		}
		ids, err := fetchTestInstanceModels(client, url)
		if err == nil {
			res.Healthy = true
			res.BootMillis = time.Since(start).Milliseconds()
			res.RegisteredModels = len(ids)
			res.Missing, res.Unexpected = compareRosters(f, ids)
			return res, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	res.BootMillis = time.Since(start).Milliseconds()
	res.Notes = append(res.Notes, fmt.Sprintf("timed out after %s waiting for %s", timeout, url))
	res.Stderr = tailString(stderr.String(), 1200)
	return res, nil
}

func fetchTestInstanceModels(client *http.Client, url string) ([]string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var env struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(env.Data))
	for _, m := range env.Data {
		out = append(out, m.ID)
	}
	return out, nil
}

// compareRosters names the seats the config declares but the instance did not
// register, and vice versa. A bare count would hide the case where one seat
// silently dropped and another appeared.
func compareRosters(f *lsconfig.File, ids []string) (missing, unexpected []string) {
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	for _, m := range f.Models {
		if !got[m.ID] && !m.Unlisted {
			missing = append(missing, m.ID)
		}
	}
	for _, id := range ids {
		if _, ok := f.ModelIndex[id]; !ok {
			unexpected = append(unexpected, id)
		}
	}
	return missing, unexpected
}

func exitStatusString(err error) string {
	if err == nil {
		return "exit 0"
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Sprintf("exit %d", ee.ExitCode())
	}
	return err.Error()
}

func tailString(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func printTestInstanceHuman(cmd *cobra.Command, r *testInstanceResult) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s\n", bold("config testinstance"))
	fmt.Fprintf(out, "  config  %s (sha %s)\n", r.ConfigPath, r.ConfigSha[:16])
	fmt.Fprintf(out, "  binary  %s\n", r.Binary)
	fmt.Fprintf(out, "  listen  %s (throwaway — NOT the live service)\n\n", r.Listen)
	if r.Healthy {
		fmt.Fprintf(out, "%s booted in %d ms\n", green("HEALTHY"), r.BootMillis)
	} else {
		fmt.Fprintf(out, "%s never answered /v1/models\n", red("UNHEALTHY"))
	}
	fmt.Fprintf(out, "  models  %d registered / %d declared\n", r.RegisteredModels, r.ExpectedModels)
	if len(r.Missing) > 0 {
		fmt.Fprintf(out, "  %s %s\n", red("missing:"), strings.Join(r.Missing, ", "))
	}
	if len(r.Unexpected) > 0 {
		fmt.Fprintf(out, "  %s %s\n", yellow("unexpected:"), strings.Join(r.Unexpected, ", "))
	}
	fmt.Fprintf(out, "  process %s\n", processDisposition(r))
	for _, n := range r.Notes {
		fmt.Fprintf(out, "\n  note: %s\n", n)
	}
	if r.Stderr != "" {
		fprintBlock(out, "INSTANCE STDERR (tail)", r.Stderr)
	}
}

func processDisposition(r *testInstanceResult) string {
	switch {
	case r.Killed && r.ExitConfirmed:
		return "killed, exit confirmed (" + r.ExitStatus + ")"
	case r.ExitConfirmed:
		return "exited on its own (" + r.ExitStatus + ")"
	case r.Killed:
		return "killed, exit NOT confirmed"
	}
	return "not started"
}
