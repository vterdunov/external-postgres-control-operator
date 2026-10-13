"""Offline checks for the bounded installed-application migration fixture."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time
import unittest


SCRIPT = Path(__file__).resolve().parent / "fixtures" / "migration.sh"
SQL = "CREATE TABLE app.orders(value integer); INSERT INTO app.orders VALUES (42)"


class MigrationFixtureTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="epco-migration-fixture-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.connection = self.root / "connection"
        self.connection.mkdir()
        for name, value in {"host": "first-host", "port": "5432", "dbname": "application",
                            "user": "application", "sslmode": "disable", "pgpass": "first-pass"}.items():
            (self.connection / name).write_text(value)
        gate = self.root / "migration-gate"
        gate.mkdir()
        (gate / "ready").write_text("yes")
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.calls = self.root / "calls.jsonl"
        self.count = self.root / "migration-count"
        self.timeout_args = self.root / "timeout-args.json"
        self.late_marker = self.root / "late-marker"
        self.real_timeout = shutil.which("timeout")
        self.assertIsNotNone(self.real_timeout, "GNU timeout is required by the fixture")
        self.write_fake("timeout", """#!/usr/bin/env python3
import json, os, subprocess, sys
from pathlib import Path
args = sys.argv[1:]
Path(os.environ['EP_TIMEOUT_ARGS']).write_text(json.dumps(args))
if args[:2] != ['--kill-after=1s', '60s']:
    sys.exit(96)
result = subprocess.run([os.environ['EP_REAL_TIMEOUT'], '--kill-after=1s', '3s', *args[2:]],
                        timeout=5, check=False)
sys.exit(result.returncode)
""")
        self.write_fake("psql", """#!/usr/bin/env python3
import hashlib, json, os, sys, time
from pathlib import Path
args = sys.argv[1:]
sql = args[args.index('-Atc') + 1] if '-Atc' in args else ''
pgpass = Path(os.environ.get('PGPASSFILE', ''))
record = {'args': args, 'sql': sql, 'host': os.environ.get('PGHOST'),
          'pgpass_path': str(pgpass),
          'pgpass_exists': pgpass.is_file(),
          'pgpass_sha': hashlib.sha256(pgpass.read_bytes()).hexdigest() if pgpass.is_file() else ''}
with Path(os.environ['EP_CALLS']).open('a') as log:
    log.write(json.dumps(record) + '\\n')
if sql == 'SELECT 1':
    sys.exit(0)
if sql != os.environ['EP_SQL']:
    sys.exit(95)
count_file = Path(os.environ['EP_COUNT'])
count = int(count_file.read_text()) + 1 if count_file.exists() else 1
count_file.write_text(str(count))
if os.environ['EP_MODE'] == 'transient' and count == 1:
    Path(os.environ['EP_HOST_FILE']).write_text('second-host')
    Path(os.environ['EP_PGPASS_SOURCE']).write_text('second-pass')
    sys.exit(1)
if os.environ['EP_MODE'] == 'permanent':
    sys.exit(1)
if os.environ['EP_MODE'] == 'hang':
    time.sleep(10)
    Path(os.environ['EP_LATE_MARKER']).write_text('escaped timeout')
sys.exit(0)
""")
        self.write_fake("sleep", "#!/bin/sh\nexec /bin/sleep 0.05\n")

    def write_fake(self, name, content):
        path = self.bin / name
        path.write_text(content)
        path.chmod(0o755)

    def invoke(self, mode):
        self.assertTrue(SCRIPT.is_file(), "The migration fixture script is required")
        script = SCRIPT.read_text()
        script = script.replace("/connection/", str(self.connection) + "/")
        script = script.replace("/migration-gate/", str(self.root / "migration-gate") + "/")
        env = {**os.environ, "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
               "EP_REAL_TIMEOUT": self.real_timeout, "EP_TIMEOUT_ARGS": str(self.timeout_args),
               "EP_CALLS": str(self.calls), "EP_COUNT": str(self.count), "EP_SQL": SQL,
               "EP_MODE": mode, "EP_HOST_FILE": str(self.connection / "host"),
               "EP_PGPASS_SOURCE": str(self.connection / "pgpass"),
               "EP_LATE_MARKER": str(self.late_marker)}
        return subprocess.run(["sh", "-ceu", script], env=env, text=True,
                              capture_output=True, timeout=7)

    def records(self):
        return [json.loads(line) for line in self.calls.read_text().splitlines()]

    def test_transient_sql_failure_reloads_connection_and_completes_once(self):
        result = self.invoke("transient")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(json.loads(self.timeout_args.read_text())[:2], ["--kill-after=1s", "60s"])
        calls = self.records()
        migrations = [call for call in calls if call["sql"] == SQL]
        self.assertEqual(len(migrations), 2, calls)
        self.assertEqual([call["host"] for call in migrations], ["first-host", "second-host"])
        self.assertNotEqual(migrations[0]["pgpass_sha"], migrations[1]["pgpass_sha"])
        self.assertTrue(all(call["pgpass_exists"] for call in calls))
        self.assertTrue(all(not Path(call["pgpass_path"]).exists() for call in calls))
        self.assertTrue(all(call["args"].count("-Atc") == 1 for call in migrations))
        self.assertNotIn("IF NOT EXISTS", json.dumps(calls))
        self.assertIn("Application connection established", result.stdout)
        self.assertIn("Migration gate observed", result.stdout)
        self.assertEqual(result.stdout.count("Migration completed"), 1)

    def test_permanent_sql_failure_exits_without_completion(self):
        result = self.invoke("permanent")
        self.assertNotEqual(result.returncode, 0)
        self.assertGreaterEqual(int(self.count.read_text()), 2)
        self.assertNotIn("Migration completed", result.stdout + result.stderr)
        self.assertTrue(self.timeout_args.is_file())
        self.assertTrue(all(not Path(call["pgpass_path"]).exists() for call in self.records()))

    def test_hung_sql_is_killed_by_outer_timeout(self):
        result = self.invoke("hang")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("Migration completed", result.stdout + result.stderr)
        time.sleep(0.2)
        self.assertFalse(self.late_marker.exists(), "A hung SQL attempt escaped the bounded process group")
        self.assertTrue(all(not Path(call["pgpass_path"]).exists() for call in self.records()))


if __name__ == "__main__":
    unittest.main()
