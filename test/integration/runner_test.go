//go:build !integration

package integration

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type runnerScratchState struct {
	Root    string `json:"root"`
	Scratch string `json:"scratch"`
}

var _ = Describe("Integration runner lifecycle", func() {
	DescribeTable("preserves failure status and cleanup ordering", func(mode string, signal syscall.Signal, want int) {
		dir := GinkgoT().TempDir()
		logPath := filepath.Join(dir, "calls")
		pidPath := filepath.Join(dir, "processes")
		realSleep, err := exec.LookPath("sleep")
		Expect(err).NotTo(HaveOccurred())
		realPython, err := exec.LookPath("python3")
		Expect(err).NotTo(HaveOccurred())
		mise := `#!/bin/bash
echo $$ >> "$FAKE_PIDS"
if [[ "$*" == *setup-envtest* ]]; then
  if [[ "$*" != *'use 1.37.0 -p path'* ]]; then exit 12; fi
  echo assets >> "$FAKE_LOG"
  if [[ "$FAKE_MODE" == assets-failure ]]; then exit 11; fi
  if [[ "$FAKE_MODE" == assets-signal ]]; then
    echo ready >> "$FAKE_LOG"
    trap 'exit 143' TERM
    while :; do sleep .05; done
  fi
  echo "$FAKE_ASSETS"
  exit 0
fi
if [[ "$FAKE_MODE" == compile-failure ]]; then echo 'compiler failed with status 9' >&2; exit 9; fi
if [[ "$FAKE_MODE" == compile-signal ]]; then
  sleep 300 &
  while [[ ! -s "$FAKE_DESCENDANT" ]]; do "$FAKE_REAL_SLEEP" .01; done
  echo ready >> "$FAKE_LOG"
  trap 'exit 143' TERM
  while :; do sleep .05; done
fi
while [[ $# -gt 0 ]]; do
 if [[ "$1" == -o ]]; then shift; binary="$1"; break; fi
 shift
done
cat > "$binary" <<'BINARY'
#!/bin/bash
echo $$ >> "$FAKE_PIDS"
if [[ -n "${EPCO_TEST_DIAGNOSTICS_ID:-}" ]]; then echo diagnostics >> "$FAKE_LOG"; exit 0; fi
if [[ "$FAKE_MODE" == assets && "$KUBEBUILDER_ASSETS" != "$FAKE_ASSETS" ]]; then exit 12; fi
if [[ "$FAKE_MODE" == controlplane-* ]]; then
  "$FAKE_REAL_PYTHON" "$FAKE_CONTROLPLANE_SPAWNER"
fi
if [[ "$FAKE_MODE" == scratch-* ]]; then
  "$FAKE_REAL_PYTHON" "$FAKE_SCRATCH_CREATOR"
  "$FAKE_REAL_PYTHON" "$FAKE_CONTROLPLANE_SPAWNER"
fi
if [[ "$FAKE_MODE" == signal ]]; then
  sleep 300 &
  while [[ ! -s "$FAKE_DESCENDANT" ]]; do "$FAKE_REAL_SLEEP" .01; done
fi
echo ready >> "$FAKE_LOG"
case "$FAKE_MODE" in
 failure|controlplane-failure|scratch-failure|scratch-failure-cleanup-failure) exit 7 ;;
 signal|controlplane-signal) trap '' TERM; while :; do sleep .05; done ;;
esac
exit 0
BINARY
chmod +x "$binary"
`
		docker := `#!/bin/bash
echo $$ >> "$FAKE_PIDS"
case "$1" in
 ps) echo owned-fixture ;;
 rm) echo cleanup >> "$FAKE_LOG" ;;
esac
`
		sleep := `#!/bin/bash
echo $$ >> "$FAKE_PIDS"
if [[ "$1" == 300 ]]; then echo $$ > "$FAKE_DESCENDANT"; fi
exec "$FAKE_REAL_SLEEP" "$@"
`
		// Managed Python exposes its environment on macOS; restricted system sleep does not.
		controlPlane := `#!/bin/bash
echo $$ >> "$FAKE_PIDS"
echo $$ > "$FAKE_DESCENDANT"
exec "$FAKE_REAL_PYTHON" -c 'import time; time.sleep(300)'
`
		controlPlaneSpawner := `import os
from pathlib import Path
import subprocess
import time

# controller-runtime v0.25.0 starts control-plane children in separate groups.
subprocess.Popen(
    [str(Path(os.environ["KUBEBUILDER_ASSETS"]) / "kube-apiserver")],
    stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    start_new_session=True,
)
deadline = time.monotonic() + 3
while not Path(os.environ["FAKE_DESCENDANT"]).exists():
    if time.monotonic() >= deadline:
        raise RuntimeError("owned control-plane stub did not start")
    time.sleep(0.01)
`
		scratchCreator := `import json
import os
from pathlib import Path
import tempfile

scratch = Path(tempfile.mkdtemp(prefix="k8s_test_framework_"))
(scratch / "fixture-state").write_text("owned synthetic fixture data")
Path(os.environ["FAKE_SCRATCH_STATE"]).write_text(json.dumps({
    "root": os.environ["EPCO_TEST_TMPDIR"], "scratch": str(scratch),
}))
`
		python := `#!/bin/bash
if [[ "$FAKE_MODE" == scratch-*-cleanup-failure && "$1" == */test/envtest/processes.py ]]; then
  exit 1
fi
exec "$FAKE_REAL_PYTHON" "$@"
`
		Expect(os.WriteFile(filepath.Join(dir, "mise"), []byte(mise), 0700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "sleep"), []byte(sleep), 0700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "kube-apiserver"), []byte(controlPlane), 0700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "spawn-controlplane.py"), []byte(controlPlaneSpawner), 0600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "create-scratch.py"), []byte(scratchCreator), 0600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "python3"), []byte(python), 0700)).To(Succeed())
		_, source, _, ok := runtime.Caller(0)
		Expect(ok).To(BeTrue())
		runner := filepath.Join(filepath.Dir(source), "run.sh")
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/bash", runner)
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		cmd.WaitDelay = time.Second
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "FAKE_MODE="+mode, "FAKE_LOG="+logPath,
			"FAKE_PIDS="+pidPath, "FAKE_REAL_SLEEP="+realSleep, "FAKE_DESCENDANT="+filepath.Join(dir, "descendant"),
			"FAKE_REAL_PYTHON="+realPython, "FAKE_CONTROLPLANE_SPAWNER="+filepath.Join(dir, "spawn-controlplane.py"),
			"FAKE_SCRATCH_CREATOR="+filepath.Join(dir, "create-scratch.py"),
			"FAKE_SCRATCH_STATE="+filepath.Join(dir, "scratch.json"),
			"EPCO_ARTIFACTS_DIR="+filepath.Join(dir, "artifacts"), "TMPDIR="+dir,
			"FAKE_ASSETS="+filepath.Join(dir, "pinned-assets"))
		assets := dir
		if strings.HasPrefix(mode, "assets") {
			assets = ""
		}
		cmd.Env = append(cmd.Env, "KUBEBUILDER_ASSETS="+assets)
		// Ensure a failing regression does not leave its runner or recorded descendants behind.
		defer func() {
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			data, _ := os.ReadFile(pidPath)
			for value := range strings.FieldsSeq(string(data)) {
				pid, err := strconv.Atoi(value)
				if err == nil {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
			var state runnerScratchState
			if data, err := os.ReadFile(filepath.Join(dir, "scratch.json")); err == nil && json.Unmarshal(data, &state) == nil {
				for _, path := range []string{state.Scratch, state.Root} {
					if relative, err := filepath.Rel(dir, path); err == nil && relative != "." &&
						relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
						_ = os.RemoveAll(path)
					}
				}
			}
		}()
		var unrelated *exec.Cmd
		if strings.HasPrefix(mode, "controlplane-") {
			unrelated = exec.Command(filepath.Join(dir, "kube-apiserver"))
			unrelated.Env = append(os.Environ(), "FAKE_REAL_SLEEP="+realSleep, "FAKE_REAL_PYTHON="+realPython,
				"FAKE_PIDS="+filepath.Join(dir, "unrelated-pids"),
				"FAKE_DESCENDANT="+filepath.Join(dir, "unrelated-descendant"), "EPCO_TEST_RUN_ID=unrelated-runner-check")
			unrelated.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			Expect(unrelated.Start()).To(Succeed())
			defer func() {
				_ = unrelated.Process.Kill()
				_ = unrelated.Wait()
			}()
		}
		started := time.Now()
		Expect(cmd.Start()).To(Succeed())
		if signal != 0 {
			readLog := func() string { data, _ := os.ReadFile(logPath); return string(data) }
			Eventually(readLog, 3*time.Second, 20*time.Millisecond).Should(ContainSubstring("ready"))
			Expect(cmd.Process.Signal(signal)).To(Succeed())
		}
		err = cmd.Wait()
		elapsed := time.Since(started)
		calls, readErr := os.ReadFile(logPath)
		diagnostic := fmt.Sprintf(
			"mode=%s wait=%v context=%v elapsed=%s calls=%q output=%q",
			mode, err, ctx.Err(), elapsed, calls, output.String(),
		)
		Expect(ctx.Err()).NotTo(HaveOccurred(), diagnostic)
		// Signal escalation has a five-second grace period; cleanup must not wait for its watchdog.
		Expect(elapsed).To(BeNumerically("<", 8*time.Second), diagnostic)
		if want == 0 {
			Expect(err).NotTo(HaveOccurred(), diagnostic)
		} else {
			Expect(err).To(HaveOccurred(), diagnostic)
			Expect(cmd.ProcessState.ExitCode()).To(Equal(want), diagnostic)
		}
		Expect(readErr).NotTo(HaveOccurred(), diagnostic)
		Expect(string(calls)).To(HaveSuffix("cleanup\n"))
		if want != 0 && mode != "compile-failure" && mode != "compile-signal" &&
			mode != "scratch-success-cleanup-failure" && !strings.HasPrefix(mode, "assets") {
			Expect(string(calls)).To(ContainSubstring("diagnostics\ncleanup"))
		}
		if strings.HasPrefix(mode, "assets") {
			Expect(string(calls)).To(HavePrefix("assets\n"))
		} else {
			Expect(string(calls)).NotTo(ContainSubstring("assets\n"))
		}
		files, err := filepath.Glob(filepath.Join(dir, "epco-integration-bin.*"))
		Expect(err).NotTo(HaveOccurred())
		cleanupFailed := strings.HasSuffix(mode, "-cleanup-failure")
		if strings.HasPrefix(mode, "scratch-") {
			var state runnerScratchState
			data, err := os.ReadFile(filepath.Join(dir, "scratch.json"))
			Expect(err).NotTo(HaveOccurred())
			Expect(json.Unmarshal(data, &state)).To(Succeed())
			if cleanupFailed {
				Expect(files).To(ConsistOf(state.Root), "unverified process cleanup must retain the exact owned root")
				Expect(state.Scratch).To(BeADirectory(), "unverified cleanup must retain fixture recovery data")
				child, err := os.ReadFile(filepath.Join(dir, "descendant"))
				Expect(err).NotTo(HaveOccurred())
				status, err := runnerProcessState(strings.TrimSpace(string(child)))
				Expect(err).NotTo(HaveOccurred())
				Expect(status).NotTo(Or(BeEmpty(), HavePrefix("Z")), "fault fixture must retain a live unverified owned child")
			} else {
				relative, err := filepath.Rel(state.Root, state.Scratch)
				Expect(err).NotTo(HaveOccurred())
				Expect(relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator))).To(BeFalse(),
					"default temporary fixture directories must remain inside the owned runner root")
				Expect(state.Scratch).NotTo(BeAnExistingFile(), "verified cleanup must remove nested fixture scratch")
			}
		}
		if !cleanupFailed {
			Expect(files).To(BeEmpty())
		}
		Expect(strings.Count(string(calls), "cleanup")).To(Equal(1), fmt.Sprintf("calls: %s", calls))
		if unrelated != nil {
			state, stateErr := runnerProcessState(strconv.Itoa(unrelated.Process.Pid))
			Expect(stateErr).NotTo(HaveOccurred())
			Expect(state).NotTo(Or(BeEmpty(), HavePrefix("Z")), "runner must preserve an unrelated control-plane process")
		}
		pids, err := os.ReadFile(pidPath)
		Expect(err).NotTo(HaveOccurred(), diagnostic)
		for pid := range strings.FieldsSeq(string(pids)) {
			if cleanupFailed {
				continue
			}
			Eventually(func() (string, error) {
				return runnerProcessState(pid)
			}, time.Second, 20*time.Millisecond).Should(
				Or(BeEmpty(), HavePrefix("Z")), "runner leaked PID %s; %s", pid, diagnostic,
			)
		}
	},
		Entry("success", "success", syscall.Signal(0), 0),
		Entry("test failure", "failure", syscall.Signal(0), 7),
		Entry("success with isolated control-plane descendant", "controlplane-success", syscall.Signal(0), 0),
		Entry("failure with isolated control-plane descendant", "controlplane-failure", syscall.Signal(0), 7),
		Entry("SIGTERM with isolated control-plane descendant", "controlplane-signal", syscall.SIGTERM, 143),
		Entry("success removes nested default fixture scratch", "scratch-success", syscall.Signal(0), 0),
		Entry("test failure removes nested default fixture scratch", "scratch-failure", syscall.Signal(0), 7),
		Entry("unverified cleanup retains successful suite scratch", "scratch-success-cleanup-failure", syscall.Signal(0), 1),
		Entry("unverified cleanup preserves test failure and scratch",
			"scratch-failure-cleanup-failure", syscall.Signal(0), 7),
		Entry("compilation failure", "compile-failure", syscall.Signal(0), 9),
		Entry("SIGINT", "signal", syscall.SIGINT, 130),
		Entry("SIGTERM", "signal", syscall.SIGTERM, 143),
		Entry("SIGTERM while compiling", "compile-signal", syscall.SIGTERM, 143),
		Entry("pinned envtest assets", "assets", syscall.Signal(0), 0),
		Entry("envtest assets failure", "assets-failure", syscall.Signal(0), 11),
		Entry("SIGTERM during envtest setup", "assets-signal", syscall.SIGTERM, 143),
	)
})

func runnerProcessState(pid string) (string, error) {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
		if os.IsNotExist(err) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		// The parenthesized command name may itself contain spaces and parentheses.
		fields := strings.Fields(string(data)[strings.LastIndex(string(data), ")")+1:])
		if len(fields) == 0 {
			return "", fmt.Errorf("missing process state in /proc/%s/stat", pid)
		}
		return fields[0], nil
	}
	data, err := exec.Command("ps", "-o", "stat=", "-p", pid).Output()
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return "", nil
	}
	return strings.TrimSpace(string(data)), err
}
