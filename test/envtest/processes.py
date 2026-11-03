"""Bounded cleanup of exact run-owned processes, including separate groups.

Process arguments and environment values are never written to diagnostics.
Only an exact inherited environment entry proves ownership; an executable name
or a marker appearing in argv or another environment value does not.
"""

import argparse
import ctypes
import json
import os
from pathlib import Path
import re
import signal
import struct
import subprocess
import sys
import time
from typing import NamedTuple


class Process(NamedTuple):
    pid: int
    parent: int
    birth: str
    state: str


def linux_process(pid):
    try:
        path = Path('/proc') / str(pid) / 'stat'
        if path.stat().st_uid != os.getuid():
            return None
        data = path.read_text()
        fields = data[data.rfind(')') + 1:].split()
        return Process(pid, int(fields[1]), fields[19], fields[0])
    except (FileNotFoundError, PermissionError, ProcessLookupError):
        return None


def snapshot(pid=None):
    if sys.platform.startswith('linux'):
        candidates = [str(pid)] if pid is not None else os.listdir('/proc')
        return {p.pid: p for candidate in candidates if candidate.isdecimal()
                if (p := linux_process(int(candidate))) is not None}
    if sys.platform != 'darwin':
        raise RuntimeError('owned process cleanup supports Linux and Darwin only')
    selection = ['-p', str(pid)] if pid is not None else ['-ax']
    result = subprocess.run(
        ['ps', *selection, '-o', 'pid=,ppid=,uid=,lstart=,stat='],
        capture_output=True, text=True, timeout=2, check=False,
    )
    if result.returncode not in (0, 1) or (pid is None and result.returncode != 0):
        raise RuntimeError('could not inspect process identities')
    processes = {}
    for line in result.stdout.splitlines():
        fields = line.split()
        if len(fields) == 9 and int(fields[2]) == os.getuid():
            process = Process(int(fields[0]), int(fields[1]), ' '.join(fields[3:8]), fields[8])
            processes[process.pid] = process
    return processes


def darwin_environment(pid):
    # XNU KERN_PROCARGS2: argc, executable path + padding, argc argv strings,
    # then NUL-separated environment entries. Skip argv before testing ownership.
    libc = ctypes.CDLL(None, use_errno=True)
    sysctl = libc.sysctl
    sysctl.argtypes = [ctypes.POINTER(ctypes.c_int), ctypes.c_uint, ctypes.c_void_p,
                       ctypes.POINTER(ctypes.c_size_t), ctypes.c_void_p, ctypes.c_size_t]
    sysctl.restype = ctypes.c_int
    mib = (ctypes.c_int * 3)(1, 49, pid)
    length = ctypes.c_size_t()
    if sysctl(mib, 3, None, ctypes.byref(length), None, 0) != 0:
        return None
    if not 4 <= length.value <= 8 * 1024 * 1024:
        return None
    buffer = ctypes.create_string_buffer(length.value)
    if sysctl(mib, 3, buffer, ctypes.byref(length), None, 0) != 0:
        return None
    data = buffer.raw[:length.value]
    argc = struct.unpack_from('=i', data)[0]
    if argc < 0:
        return None
    position = data.find(b'\0', 4)
    if position < 0:
        return None
    while position < len(data) and data[position] == 0:
        position += 1
    for _ in range(argc):
        position = data.find(b'\0', position)
        if position < 0:
            return None
        position += 1
    return data[position:].split(b'\0')


def environment(pid):
    if sys.platform == 'darwin':
        return darwin_environment(pid)
    if sys.platform.startswith('linux'):
        try:
            return (Path('/proc') / str(pid) / 'environ').read_bytes().split(b'\0')
        except (FileNotFoundError, PermissionError, ProcessLookupError):
            return None
    raise RuntimeError('owned process cleanup supports Linux and Darwin only')


def ancestors(processes):
    excluded = {0, 1, os.getpid()}
    parent = os.getppid()
    while parent not in excluded:
        excluded.add(parent)
        current = processes.get(parent)
        if current is None:
            break
        parent = current.parent
    return excluded


def owned_processes(run_id, excluded):
    marker = os.fsencode('EPCO_TEST_RUN_ID=' + run_id)
    return [process for process in snapshot().values()
            if process.pid not in excluded and not process.state.startswith('Z')
            if (entries := environment(process.pid)) is not None and marker in entries]


def signal_owned(process, run_id, signum, excluded):
    if process.pid in excluded or process.pid <= 1:
        return False
    current = snapshot(process.pid).get(process.pid)
    if current is None or current.birth != process.birth or current.state.startswith('Z'):
        return False
    entries = environment(process.pid)
    if entries is None:
        raise RuntimeError('owned process environment became unreadable')
    if os.fsencode('EPCO_TEST_RUN_ID=' + run_id) not in entries:
        return False
    # Recheck birth immediately before signaling, after the environment read.
    current = snapshot(process.pid).get(process.pid)
    if current is None or current.birth != process.birth or current.state.startswith('Z'):
        return False
    try:
        os.kill(process.pid, signum)
    except ProcessLookupError:
        return False
    return True


def cleanup_owned(run_id, report_path):
    if not re.fullmatch(r'[A-Za-z0-9_.-]+', run_id):
        raise ValueError('a nonempty run identifier is required')
    excluded = ancestors(snapshot())
    pending = owned_processes(run_id, excluded)
    report = {'before': [p._asdict() for p in pending], 'signals': [], 'errors': []}
    for signum in (signal.SIGTERM, signal.SIGKILL):
        deadline = time.monotonic() + 1
        signaled = set()
        while pending and time.monotonic() < deadline:
            for process in pending:
                identity = (process.pid, process.birth)
                if identity in signaled:
                    continue
                signaled.add(identity)
                try:
                    if signal_owned(process, run_id, signum, excluded):
                        report['signals'].append({'pid': process.pid, 'birth': process.birth, 'signal': int(signum)})
                except (OSError, RuntimeError):
                    report['errors'].append({'pid': process.pid, 'reason': 'could not verify or signal owned identity'})
            time.sleep(0.05)
            pending = owned_processes(run_id, excluded)
    report['remaining'] = [p._asdict() for p in pending]
    report_path = Path(report_path)
    report_path.write_text(json.dumps(report, indent=2) + '\n')
    report_path.chmod(0o600)
    return not pending and not report['errors']


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run-id', required=True)
    parser.add_argument('--report', required=True)
    args = parser.parse_args()
    try:
        return 0 if cleanup_owned(args.run_id, args.report) else 1
    except (OSError, RuntimeError, ValueError, subprocess.SubprocessError):
        print('Could not verify bounded cleanup of run-owned processes', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
