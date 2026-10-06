#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline checks for lib.sh's transient-retry, upload and tier-order helpers."""

import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import textwrap
import unittest

LIB = Path(__file__).with_name("lib.sh")
RESET = "Unable to connect to the server: dial tcp 1.2.3.4:443: connect: socket is not connected"


class LibShell(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)
        (self.dir / "bin").mkdir()

    def fake_kubectl(self, *responses):
        """Each response is (rc, stdout, stderr), consumed one per call."""
        for i, (rc, out, err) in enumerate(responses, 1):
            (self.dir / f"r{i}").write_text(f"{rc}\n{out}\n{err}\n")
        script = self.dir / "bin" / "kubectl"
        script.write_text(textwrap.dedent(f"""\
            #!/bin/sh
            n=$(( $(cat {self.dir}/calls 2>/dev/null || echo 0) + 1 ))
            echo "$n" > {self.dir}/calls
            echo "$*" >> {self.dir}/argv
            f={self.dir}/r$n
            [ -f "$f" ] || f=$(ls {self.dir}/r* | sort -V | tail -1)
            rc=$(sed -n 1p "$f"); out=$(sed -n 2p "$f"); err=$(sed -n 3p "$f")
            [ -n "$out" ] && echo "$out"
            [ -n "$err" ] && echo "$err" >&2
            exit "$rc"
            """))
        script.chmod(0o755)

    def calls(self):
        path = self.dir / "calls"
        return int(path.read_text()) if path.exists() else 0

    def bash(self, body, env=None):
        script = f'set -uo pipefail\nE2E_DIR="{LIB.parent}"\nsource "{LIB}"\n{body}\n'
        full_env = {**os.environ, "PATH": f"{self.dir / 'bin'}:{os.environ['PATH']}",
                    "E2E_RETRY_BACKOFF": "0", "E2E_RETRIES": "4", "TMPDIR": str(self.dir),
                    **(env or {})}
        return subprocess.run(["bash", "-c", script], text=True, capture_output=True, env=full_env)


class RetryTest(LibShell):
    def test_transient_errors_are_retried_and_only_final_output_kept(self):
        self.fake_kubectl((1, "partial", "read: connection reset by peer"),
                          (1, "", "error: unexpected EOF"),
                          (0, "deleted", ""))
        result = self.bash("kubectl_retry delete ns x --ignore-not-found")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "deleted\n")
        self.assertEqual(self.calls(), 3)
        self.assertIn("transient API error (attempt 1/4", result.stderr)

    def test_each_listed_transport_error_is_transient(self):
        for err in ("read: connection reset by peer", "error: EOF", "unexpected EOF",
                    RESET, "net/http: TLS handshake timeout"):
            with self.subTest(err=err):
                (self.dir / "calls").unlink(missing_ok=True)
                self.fake_kubectl((1, "", err), (0, "ok", ""))
                result = self.bash("kubectl_retry get nodes")
                self.assertEqual((result.returncode, self.calls()), (0, 2), result.stderr)

    def test_real_errors_fail_immediately_with_their_status(self):
        self.fake_kubectl((3, "", 'Error from server (NotFound): pods "x" not found'))
        result = self.bash("kubectl_retry get pod x")
        self.assertEqual(result.returncode, 3)
        self.assertEqual(self.calls(), 1)
        self.assertIn("NotFound", result.stderr)

    def test_retries_are_bounded(self):
        self.fake_kubectl((1, "", RESET))
        result = self.bash("kubectl_retry get nodes")
        self.assertEqual(result.returncode, 1)
        self.assertEqual(self.calls(), 4)
        self.assertIn(RESET, result.stderr)

    def test_released_recheck_prevents_false_leak(self):
        fixtures = self.dir / "e2e"
        fixtures.mkdir()
        (fixtures / "nodeprofile-fixtures.py").write_text(textwrap.dedent("""\
            import sys
            print(sys.argv[1])
            sys.exit({"teardown": 1, "released": int(sys.argv[2] != "gone")}[sys.argv[1]])
            """))
        gone = self.bash(f'E2E_DIR="{fixtures}"; abort_unstarted_profile_fixture gone batch uid img')
        self.assertEqual(gone.returncode, 0, gone.stdout + gone.stderr)
        self.assertIn("no leak", gone.stdout)
        kept = self.bash(f'E2E_DIR="{fixtures}"; abort_unstarted_profile_fixture kept batch uid img')
        self.assertEqual(kept.returncode, 1)


class UploadTest(LibShell):
    """_node_upload over a fake kubectl transport whose "node" is a local dir."""

    def upload(self, failures):
        root = self.dir / "node"
        (root / "dst").mkdir(parents=True)
        src = self.dir / "src.bin"
        src.write_bytes(os.urandom(10_000))
        src.chmod(0o640)
        # node_exec runs the command locally; chunk writes listed in FAILURES
        # complete but then report a dropped stream, or fail before writing.
        body = textwrap.dedent(f"""\
            E2E_NODE_ACCESS=kubectl
            E2E_UPLOAD_CHUNK=4k
            writes=0
            node_exec() {{
              local i=0
              [[ "$1" == "-i" ]] && {{ i=1; shift; }}
              shift
              if (( i )); then
                writes=$(( $(cat "{self.dir}/writes" 2>/dev/null || echo 0) + 1 ))
                echo "$writes" > "{self.dir}/writes"
                case " {failures} " in
                  *" after:$writes "*) "$@"; echo "{RESET}" >&2; return 1 ;;
                  *" before:$writes "*) cat >/dev/null; echo "{RESET}" >&2; return 1 ;;
                esac
              fi
              "$@"
            }}
            _node_upload node "{src}" "{root}/dst" 640
            """)
        result = self.bash(body)
        return result, src, root / "dst" / "src.bin"

    def assert_uploaded(self, result, src, dst):
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(hashlib.sha256(dst.read_bytes()).digest(),
                         hashlib.sha256(src.read_bytes()).digest())
        self.assertEqual(oct(dst.stat().st_mode & 0o777), "0o640")
        self.assertEqual(sorted(p.name for p in dst.parent.iterdir()), ["src.bin"])

    def test_clean_upload(self):
        result, src, dst = self.upload("")
        self.assert_uploaded(result, src, dst)
        self.assertEqual(int((self.dir / "writes").read_text()), 3)

    def test_chunk_written_before_reset_is_resumed_not_resent(self):
        result, src, dst = self.upload("after:2")
        self.assert_uploaded(result, src, dst)
        self.assertIn("chunk 2 already on node, resuming", result.stderr)
        self.assertEqual(int((self.dir / "writes").read_text()), 3)

    def test_lost_chunk_is_resent(self):
        result, src, dst = self.upload("before:1 before:2")
        self.assert_uploaded(result, src, dst)
        self.assertEqual(int((self.dir / "writes").read_text()), 5)

    def test_persistent_failure_is_bounded_and_cleans_up(self):
        result, _, dst = self.upload(" ".join(f"before:{i}" for i in range(1, 20)))
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(dst.exists())
        self.assertEqual(list(dst.parent.iterdir()), [])
        self.assertEqual(int((self.dir / "writes").read_text()), 5)


class TierOrderTest(LibShell):
    def order(self, *tiers):
        result = self.bash("e2e_order_tiers " + " ".join(map(str, tiers)))
        self.assertEqual(result.returncode, 0, result.stderr)
        return [int(t) for t in result.stdout.split()]

    def test_tier17_runs_before_kubernetes_tiers(self):
        self.assertEqual(self.order(*range(1, 20)),
                         [1, 2, 3, 17, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 18, 19])
        self.assertEqual(self.order(4, 13, 17), [17, 4, 13])
        self.assertEqual(self.order(12, 1, 17), [17, 12, 1])

    def test_order_is_unchanged_without_tier17_or_alone(self):
        self.assertEqual(self.order(17), [17])
        self.assertEqual(self.order(8, 4, 12), [8, 4, 12])
        self.assertEqual(self.order(1, 2, 17), [1, 2, 17])


class ProgressWaitTest(LibShell):
    """wait_while_progressing with a fake clock-free progress counter."""

    PRELUDE = textwrap.dedent("""\
        n=0
        progress() { cat "$TMPDIR/progress" 2>/dev/null; }
        tick() { n=$((n+1)); echo "$n" > "$TMPDIR/progress"; }
        ready_after() { tick; [ "$n" -ge "$1" ]; }
        """)

    def wait(self, body):
        return self.bash(self.PRELUDE + body, env={"E2E_PROGRESS_POLL": "0.2"})

    def test_waits_past_idle_window_while_progress_advances(self):
        # Ready after ~3s of polls, longer than the 1s idle window, but every
        # poll advances the fingerprint.
        result = self.wait('wait_while_progressing 1 30 progress ready_after 15 && echo ok')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "ok\n")

    def test_fails_when_progress_stalls(self):
        result = self.wait('wait_while_progressing 1 30 progress false; '
                           'echo "rc=$? $E2E_WAIT_STOP"')
        self.assertRegex(result.stdout, r"^rc=1 no progress for 1s after \ds\n$")

    def test_overall_limit_bounds_endless_progress(self):
        result = self.wait('wait_while_progressing 30 2 progress ready_after 1000000; '
                           'echo "rc=$? $E2E_WAIT_STOP"')
        self.assertEqual(result.stdout, "rc=1 still progressing at the 2s limit\n")

    def test_positive_int_validation(self):
        for value, rc in (("900", 0), ("0", 1), ("-1", 1), ("15m", 1), ("", 1)):
            with self.subTest(value=value):
                result = self.bash(f'e2e_positive_int E2E_X "{value}"')
                self.assertEqual(result.returncode, rc, result.stderr)


if __name__ == "__main__":
    unittest.main()
