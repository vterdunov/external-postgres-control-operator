//go:build !integration

package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Integration runner lifecycle", func() {
	DescribeTable("preserves failure status and cleanup ordering", func(mode string, signal syscall.Signal, want int) {
		dir := GinkgoT().TempDir()
		logPath := filepath.Join(dir, "calls")
		mise := `#!/bin/bash
if [[ "$FAKE_MODE" == compile-failure ]]; then exit 9; fi
if [[ "$FAKE_MODE" == compile-signal ]]; then
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
if [[ -n "${EPCO_TEST_DIAGNOSTICS_ID:-}" ]]; then echo diagnostics >> "$FAKE_LOG"; exit 0; fi
echo ready >> "$FAKE_LOG"
case "$FAKE_MODE" in
 failure) exit 7 ;;
 signal) trap '' TERM; while :; do sleep .05; done ;;
esac
exit 0
BINARY
chmod +x "$binary"
`
		docker := `#!/bin/bash
case "$1" in
 ps) echo owned-fixture ;;
 rm) echo cleanup >> "$FAKE_LOG" ;;
esac
`
		Expect(os.WriteFile(filepath.Join(dir, "mise"), []byte(mise), 0700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0700)).To(Succeed())
		_, source, _, ok := runtime.Caller(0)
		Expect(ok).To(BeTrue())
		runner := filepath.Join(filepath.Dir(source), "run.sh")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/bash", runner)
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "FAKE_MODE="+mode, "FAKE_LOG="+logPath,
			"EPCO_ARTIFACTS_DIR="+filepath.Join(dir, "artifacts"), "TMPDIR="+dir)
		Expect(cmd.Start()).To(Succeed())
		if signal != 0 {
			readLog := func() string { data, _ := os.ReadFile(logPath); return string(data) }
			Eventually(readLog, 3*time.Second, 20*time.Millisecond).Should(ContainSubstring("ready"))
			Expect(cmd.Process.Signal(signal)).To(Succeed())
		}
		err := cmd.Wait()
		if want == 0 {
			Expect(err).NotTo(HaveOccurred())
		} else {
			Expect(err).To(HaveOccurred())
			Expect(cmd.ProcessState.ExitCode()).To(Equal(want))
		}
		calls, err := os.ReadFile(logPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(calls)).To(HaveSuffix("cleanup\n"))
		if want != 0 && mode != "compile-failure" && mode != "compile-signal" {
			Expect(string(calls)).To(ContainSubstring("diagnostics\ncleanup"))
		}
		files, err := filepath.Glob(filepath.Join(dir, "epco-integration-bin.*"))
		Expect(err).NotTo(HaveOccurred())
		Expect(files).To(BeEmpty())
		Expect(strings.Count(string(calls), "cleanup")).To(Equal(1), fmt.Sprintf("calls: %s", calls))
	},
		Entry("success", "success", syscall.Signal(0), 0),
		Entry("test failure", "failure", syscall.Signal(0), 7),
		Entry("compilation failure", "compile-failure", syscall.Signal(0), 9),
		Entry("SIGINT", "signal", syscall.SIGINT, 130),
		Entry("SIGTERM", "signal", syscall.SIGTERM, 143),
		Entry("SIGTERM while compiling", "compile-signal", syscall.SIGTERM, 143),
	)
})
