#!/usr/bin/env python3
"""Run a precompiled suite with signal forwarding and owned process cleanup."""

import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time

from processes import cleanup_owned


def main():
    root = Path(__file__).resolve().parents[2]
    artifacts = Path(os.environ.get("EPCO_ARTIFACTS_DIR", root / "artifacts" / "envtest")).resolve()
    artifacts.mkdir(parents=True, exist_ok=True)
    seed = os.environ.get("EPCO_TEST_SEED", str(time.time_ns() % 2_147_483_647))
    (artifacts / "seed.txt").write_text(seed + "\n")
    print(f"envtest random seed: {seed}; artifacts: {artifacts}", flush=True)

    child = None
    interrupted = None
    cleanup_verified = True
    run_id = f'epco-envtest-{os.getpid()}-{time.time_ns()}'
    child_environment = {**os.environ, 'EPCO_TEST_RUN_ID': run_id}

    def interrupt(signum, _frame):
        nonlocal interrupted
        if interrupted is not None:
            return
        interrupted = signum
        if child is not None and child.poll() is None:
            # Ginkgo handles SIGINT and completes diagnostics and suite cleanup.
            child.send_signal(signal.SIGINT)

    signal.signal(signal.SIGINT, interrupt)
    signal.signal(signal.SIGTERM, interrupt)

    def execute(arguments, label):
        nonlocal child, cleanup_verified
        log_path = artifacts / f"{label}.log"
        with log_path.open("w") as log:
            working_directory = root / "test" / "envtest" if label == "suite" else root
            child = subprocess.Popen(arguments, cwd=working_directory, env=child_environment,
                                     stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
            return_code = 1
            try:
                shutdown_deadline = None
                while child.poll() is None:
                    if interrupted is not None:
                        if shutdown_deadline is None:
                            shutdown_deadline = time.monotonic() + 45
                        elif time.monotonic() >= shutdown_deadline:
                            os.killpg(child.pid, signal.SIGKILL)
                            break
                    time.sleep(0.1)
                return_code = child.wait()
            finally:
                # Reap ordinary compiler descendants, then separately identify
                # run-owned envtest assets, which create their own groups.
                try:
                    os.killpg(child.pid, signal.SIGTERM)
                    time.sleep(0.2)
                    os.killpg(child.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                child = None
                try:
                    cleaned = cleanup_owned(run_id, artifacts / f'{label}-processes.json')
                except (OSError, RuntimeError, ValueError, subprocess.SubprocessError):
                    print('Could not verify cleanup of run-owned processes', file=sys.stderr)
                    cleaned = False
                cleanup_verified = cleanup_verified and cleaned
                if not cleaned and return_code == 0:
                    return_code = 1
        print(log_path.read_text(), end="", flush=True)
        return return_code

    temporary_directory = tempfile.TemporaryDirectory(prefix="epco-envtest-", delete=False)
    with temporary_directory as temporary:
        child_environment['TMPDIR'] = temporary
        try:
            executable = str(Path(temporary) / "envtest.test")
            code = execute([
                "mise", "exec", "--", "go", "test", "-mod=readonly", "-c", "-race",
                "-tags", "envtest", "-o", executable, "./test/envtest",
            ], "compile")
            if code == 0 and interrupted is None:
                code = execute([
                    executable, "-test.v", "-test.timeout=5m",
                    "-ginkgo.randomize-all", f"-ginkgo.seed={seed}",
                    f"-ginkgo.junit-report={artifacts / 'junit.xml'}",
                    *sys.argv[1:],
                ], "suite")
        finally:
            if cleanup_verified:
                temporary_directory.cleanup()
            else:
                print('Retained run-owned temporary directory after unverified process cleanup: '
                      f'{temporary}', file=sys.stderr)
    if interrupted is not None:
        return 128 + interrupted
    return code if code >= 0 else 128 - code


if __name__ == "__main__":
    sys.exit(main())
