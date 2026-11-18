#!/usr/bin/env python3
"""Developer checks of the live cleanup check and the runner check (L-e1 P6), not acceptance proof.

Each check runs a copy of the script in a temporary root. The copy differs
only in its fixed lines: the root, /proc and the tools, which are fakes here.
The scripts clear their environment, so each fake has its state path built
in. Limit: the real process tree, terminate-user, the engine reset and the
runner start with root-owned files are proven only by the rehearsal and the
owner steps.
"""
import os
from pathlib import Path
import pwd
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
UID = os.getuid()
# The subordinate UID range of the live account in the fake /etc/subuid.
SUBUID = 100000
SINKS = ("service-logs container-logs container-records temporal-history postgresql object-store "
         "step-summary test-output host-processes").split()
RECEIPT = "".join(f"sink={name} count=0 complete\n" for name in SINKS) + "complete=true\n"
CLEANUP_FIXED = ["root=\n", "proc=/proc\n", "podman=/usr/bin/podman\n", "python=/usr/bin/python3\n",
                 'home_of() { /usr/bin/getent passwd "$1" | /usr/bin/cut -d: -f6; }\n']
RUNNER_FIXED = CLEANUP_FIXED[:2] + [
    'uid_of() { /usr/bin/id -u "$1"; }\n', CLEANUP_FIXED[4],
    "entry_of() { /usr/bin/stat -c '%u %g %f' -- \"$1\"; }\n",
    'sha256_of() { /usr/bin/sha256sum -- "$1"; }\n',
    'crontab_of() { /usr/bin/crontab -u "$1" -l; }\n',
    "at_jobs() { if [ -x /usr/bin/atq ]; then /usr/bin/atq; fi; }\n"]
FAKE_PODMAN = """import os, subprocess, sys
state, args = STATE, sys.argv[1:]
with open(os.path.join(state, "log"), "a") as log:
    log.write(" ".join(args) + "\\n")
with open(os.path.join(state, "xdg"), "a") as xdg:
    xdg.write(os.environ.get("XDG_RUNTIME_DIR", "unset") + "\\n")
flag = lambda name: os.path.exists(os.path.join(state, name))
if args[0] == "unshare":
    sys.exit(1 if flag("rm-fails") else subprocess.run(args[1:]).returncode)
if args[0] == "system":
    sys.exit(1 if flag("reset-fails") else 0)
if args[0] == "network":
    print("podman" + ("\\nextra" if flag("networks") else ""))
    sys.exit(0)
kind = {"ps": "containers", "pod": "pods", "volume": "volumes", "images": "images"}[args[0]]
if flag(kind):
    print("left-" + kind)
"""
FAKE_ENTRY = """import os, sys
with open(os.path.join(STATE, "reads"), "a") as reads:
    reads.write(sys.argv[1] + "\\n")
status, owner = os.lstat(sys.argv[1]), os.path.join(STATE, "owner")
foreign = os.path.exists(owner) and open(owner).read() == sys.argv[1]
print(status.st_uid + foreign, status.st_gid, format(status.st_mode, "x"))
"""
FAKE_SHA = """import hashlib, os, sys
with open(os.path.join(STATE, "reads"), "a") as reads:
    reads.write(sys.argv[1] + "\\n")
if os.path.exists(os.path.join(STATE, "sha-fails")):
    sys.exit(1)
print(hashlib.sha256(open(sys.argv[1], "rb").read()).hexdigest())
"""


class Host:
    """A temporary root with the live account's folders, fake tools and a fake /proc."""

    def __init__(self, directory):
        self.root = Path(directory).resolve()
        self.state, self.bin, self.proc = self.root / "state", self.root / "bin", self.root / "proc"
        for path in (self.state, self.bin, self.proc, self.root / "hooks"):
            path.mkdir()
        for name, code in (("podman", FAKE_PODMAN), ("entry", FAKE_ENTRY), ("sha", FAKE_SHA)):
            self.tool(name, code.replace("STATE", repr(str(self.state))))
        self.home = self.root / "home/pmlive"
        self.runtime = self.root / f"run/user/{UID}"
        self.tokens = self.runtime / "pm-live"
        self.tokens.mkdir(parents=True, mode=0o700)
        self.run_root = self.runtime / "pm-live-run/77-1"
        self.run_root.mkdir(parents=True)
        (self.run_root / "scan-receipt").write_text(RECEIPT)
        (self.run_root / "fixture").mkdir()
        (self.home / "pm-live-cache/77-1").mkdir(parents=True)
        (self.root / "etc").mkdir()
        self.process(1, 0, "/init.scope")  # a real /proc always holds PID 1
        # One range for pmlive (the runner check) and one for the user of the
        # test (the cleanup check, which runs as that user).
        (self.root / "etc/subuid").write_text(
            f"pmlive:{SUBUID}:65536\n{pwd.getpwuid(UID).pw_name}:{SUBUID}:65536\n")

    def tool(self, name, code):
        path = self.bin / name
        path.write_text(f"#!{sys.executable}\n{code}")
        path.chmod(0o755)

    def script(self, name, fixed, replacements):
        text = (ROOT / "scripts/ci" / name).read_text()
        for line, replacement in zip(fixed, replacements):
            assert text.count(line) == 1, line
            text = text.replace(line, replacement)
        path = self.root / "hooks" / name
        path.write_text(text)
        return path

    def run(self, script, *arguments, env=None):
        return subprocess.run(["/bin/sh", str(script), *arguments], env={"PATH": "/usr/bin:/bin", **(env or {})},
                              capture_output=True, text=True, timeout=60)

    def process(self, pid, uid, cgroup="/user.slice/user-1.slice/user@1.service/app.slice/x.scope"):
        entry = self.proc / str(pid)
        entry.mkdir()
        (entry / "status").write_text(f"Name:\tx\nUid:\t{uid}\t{uid}\t{uid}\t{uid}\n")
        (entry / "cgroup").write_text(f"0::{cgroup}\n")
        return entry

    def flag(self, name, text=""):
        (self.state / name).write_text(text)


class CleanupCheck(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.host = Host(self.directory.name)
        self.script = self.copy(sys.executable)

    def copy(self, python):
        return self.host.script("live-cleanup-check.sh", CLEANUP_FIXED, [
            f"root={self.host.root}\n", f"proc={self.host.proc}\n", f"podman={self.host.bin / 'podman'}\n",
            f"python={python}\n", f'home_of() {{ echo "{self.host.home}"; }}\n'])

    def tearDown(self):
        self.directory.cleanup()

    def check(self, mode="in-job"):
        return self.host.run(self.script, mode)

    def test_the_fixed_lines_are_the_production_values(self):
        for name, fixed in (("live-cleanup-check.sh", CLEANUP_FIXED), ("live-runner-check.sh", RUNNER_FIXED)):
            text = (ROOT / "scripts/ci" / name).read_text()
            self.assertEqual([text.count(line) for line in fixed], [1] * len(fixed), name)
            self.assertIn('exec /usr/bin/env -i PATH=/usr/bin:/bin /bin/sh "$0" --clean-env "$@"', text)

    def test_the_clean_case_passes(self):
        for mode in ("in-job", "post-stop"):
            with self.subTest(mode=mode):
                result = self.check(mode)
                self.assertEqual(result.returncode, 0, result.stdout)
                self.assertFalse((self.host.runtime / "pm-live-run").exists())
                self.assertFalse((self.host.home / "pm-live-cache").exists())
                self.assertEqual(list(self.host.tokens.iterdir()), [])
                self.host.run_root.mkdir(parents=True)

    def test_each_receipt_fault_fails(self):
        receipt = self.host.run_root / "scan-receipt"
        faults = {"missing sink": RECEIPT.replace("sink=postgresql count=0 complete\n", ""),
                  "incomplete": RECEIPT.replace("postgresql count=0 complete", "postgresql count=0 incomplete"),
                  "count above 0": RECEIPT.replace("postgresql count=0", "postgresql count=1"),
                  "no final line": RECEIPT.replace("complete=true\n", ""),
                  "unknown format": RECEIPT + "x\n", "a line after the end": RECEIPT + RECEIPT[:30] + "\n",
                  "unknown sink": "sink=other count=0 complete\n" + RECEIPT,
                  "a sink twice": RECEIPT.splitlines(True)[0] + RECEIPT, "missing": None, "two run roots": RECEIPT}
        for name, text in faults.items():
            with self.subTest(case=name):
                self.host.run_root.mkdir(parents=True, exist_ok=True)
                if text is not None:
                    receipt.write_text(text)
                if name == "two run roots":
                    (self.host.runtime / "pm-live-run/77-2").mkdir()
                    (self.host.runtime / "pm-live-run/77-2/scan-receipt").write_text(RECEIPT)
                result = self.check()
                self.assertEqual(result.returncode, 1, result.stdout)
                self.assertIn("FAILED: scan receipt", result.stdout)

    def test_each_leftover_and_each_removal_failure_fails(self):
        for flag in ("containers", "pods", "volumes", "images", "networks", "reset-fails", "rm-fails"):
            with self.subTest(case=flag):
                self.host.run_root.mkdir(parents=True, exist_ok=True)
                (self.host.run_root / "scan-receipt").write_text(RECEIPT)
                self.host.flag(flag)
                result = self.check()
                self.assertEqual(result.returncode, 1, result.stdout)
                (self.host.state / flag).unlink()
        (self.host.tokens / "other").write_text("")
        self.assertIn("FAILED: token directory is not empty", self.check("post-stop").stdout)

    def test_every_step_runs_and_keeps_its_result(self):
        # The receipt is missing and the reset fails. A leftover token file
        # is still removed, each engine list runs, and the run root goes.
        (self.host.run_root / "scan-receipt").unlink()
        (self.host.tokens / "w2-token").write_text("dummy")
        self.host.flag("reset-fails")
        result = self.check()
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertEqual(list(self.host.tokens.iterdir()), [])
        log = (self.host.state / "log").read_text().splitlines()
        self.assertEqual([line.split()[0] for line in log], ["system", "ps", "pod", "volume", "images", "network",
                                                             "unshare", "unshare"])
        self.assertFalse((self.host.runtime / "pm-live-run").exists())
        self.assertIn("FAILED: in-job cleanup: uncertain", result.stdout)

    def test_podman_gets_the_runtime_folder_of_the_script(self):
        # crrao I3: env -i removes the caller's XDG_RUNTIME_DIR. The script
        # sets its own value, and each podman call gets it.
        result = self.host.run(self.script, "post-stop", env={"XDG_RUNTIME_DIR": "/caller/value"})
        self.assertEqual(result.returncode, 0, result.stdout)
        values = set((self.host.state / "xdg").read_text().splitlines())
        self.assertEqual(values, {str(self.host.runtime)})

    def test_a_token_link_is_removed_as_a_link(self):
        target = self.host.root / "target"
        target.write_text("keep")
        (self.host.tokens / "w2-token").symlink_to(target)
        self.assertEqual(self.check().returncode, 0)
        self.assertEqual((list(self.host.tokens.iterdir()), target.read_text()), ([], "keep"))

    def test_a_replaced_token_directory_is_refused(self):
        # #403 F4: the token directory is renamed and a link to a directory
        # outside takes its name, after the earlier steps and just before the
        # removal (the python seam does it, then runs the helper). The token
        # must not be reported as removed, and the outside file must stay.
        tokens, outside = self.host.tokens, self.host.root / "outside"
        (tokens / "w2-token").write_text("dummy")
        swap = self.host.root / "swap-then-python"
        swap.write_text(f'#!/bin/sh\n/bin/mv "{tokens}" "{tokens}.moved" && /bin/mkdir -m 700 "{outside}" && '
                        f'echo outside > "{outside}/w2-token" && /bin/ln -s "{outside}" "{tokens}"\n'
                        f'exec "{sys.executable}" "$@"\n')
        swap.chmod(0o755)
        self.script = self.copy(swap)
        result = self.check()
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn("token directory: missing, a link", result.stdout)
        self.assertEqual((outside / "w2-token").read_text(), "outside\n")

    def test_a_swap_after_the_open_cannot_redirect_the_removal(self):
        # #403 F4: the swap happens inside the helper, after its open and
        # check. The removal goes through the opened descriptor: the real
        # token goes, and the file behind the new link stays.
        tokens, outside = self.host.tokens, self.host.root / "outside"
        (tokens / "w2-token").write_text("dummy")
        swap = (f"os.rename({str(tokens)!r}, {str(tokens) + '.moved'!r}); os.mkdir({str(outside)!r}, 0o700); "
                f"open({str(outside / 'w2-token')!r}, 'w').write('outside'); os.symlink({str(outside)!r}, {str(tokens)!r})\\n")
        seam = self.host.root / "swap-inside-python"
        seam.write_text(f"#!{sys.executable}\nimport os, sys\nargs = sys.argv[1:]\nindex = args.index('-c') + 1\n"
                        "anchor = 'status = os.fstat(directory)\\n'\n"
                        f"args[index] = args[index].replace(anchor, anchor + \"{swap}\", 1)\n"
                        "os.execv(sys.executable, [sys.executable, *args])\n")
        seam.chmod(0o755)
        self.script = self.copy(seam)
        result = self.check()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual((outside / "w2-token").read_text(), "outside")
        self.assertEqual(list(Path(str(tokens) + ".moved").iterdir()), [])

    def test_a_token_directory_with_a_wide_mode_is_refused(self):
        # #403 F4: the owner's token directory has mode 0700. Another mode fails.
        self.host.tokens.chmod(0o755)
        result = self.check()
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn("token directory: missing, a link", result.stdout)

    def test_the_in_job_process_list_is_a_report_only(self):
        self.host.process(10, UID)
        self.host.process(11, UID, "/system.slice/pm-live-runner.service")
        self.host.process(12, UID + 1)
        self.host.process(13, SUBUID + 7)
        result = self.check()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("report, not a gate: 2 processes of the live UID or its subordinate UIDs outside the runner "
                      "service", result.stdout)


class RunnerCheck(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        host = self.host = Host(self.directory.name)
        state = host.state
        self.script = host.script("live-runner-check.sh", RUNNER_FIXED, [
            f"root={host.root}\n", f"proc={host.proc}\n", f"uid_of() {{ echo {UID}; }}\n",
            f'home_of() {{ echo "{host.home}"; }}\n', f'entry_of() {{ "{host.bin / "entry"}" "$1"; }}\n',
            f'sha256_of() {{ "{host.bin / "sha"}" "$1"; }}\n',
            f'crontab_of() {{ if [ -f "{state}/crontab" ]; then cat "{state}/crontab"; '
            'else echo "no crontab for $1" >&2; return 1; fi; }\n',
            f'at_jobs() {{ cat "{state}/atq" 2> /dev/null; true; }}\n'])
        host.run_root.parent.parent.rename(host.root / "runtime-gone")  # the gate needs no runtime folder
        self.runner = host.root / "opt/pm-live/actions-runner"
        for path, text in (("run.sh", "#!/bin/sh\n"), (".env", "A=1\n"), ("bin/Runner.Listener", "binary\n"),
                           (".runner", '{\n  "disableUpdate": true\n}\n'), ("_diag/log", "x\n")):
            (self.runner / path).parent.mkdir(parents=True, exist_ok=True)
            (self.runner / path).write_text(text)
        self.unit = host.root / "etc/systemd/system/pm-live-runner.service"
        self.unit.parent.mkdir(parents=True, exist_ok=True)
        self.unit.write_text("[Service]\nUser=pmlive\nExecStart=/opt/pm-live/actions-runner/run.sh\n"
                             "EnvironmentFile=/opt/pm-live/actions-runner/.env\n")
        (host.root / "etc/pm-live").mkdir(exist_ok=True)
        (host.home / ".config/containers").mkdir(parents=True)
        (host.home / ".bashrc").write_text("# fixed\n")
        (host.home / ".local/share/containers/storage").mkdir(parents=True)
        (host.root / "var/lib/pm-live/work/job").mkdir(parents=True)
        self.list = host.root / "etc/pm-live/runner-files.sha256"
        result = host.run(self.script, "--record")
        self.assertEqual(result.returncode, 0, result.stdout)
        self.recorded = self.list.read_bytes()

    def tearDown(self):
        if self.host.proc.exists():
            self.host.proc.chmod(0o755)
            for entry in self.host.proc.iterdir():
                (entry / "status").chmod(0o644)
        self.directory.cleanup()

    def check(self):
        return self.host.run(self.script)

    def test_the_clean_case_passes_and_empties_the_folders(self):
        result = self.check()
        self.assertEqual(result.returncode, 0, result.stdout)
        for folder in (self.host.root / "var/lib/pm-live/work", self.runner / "_diag",
                       self.host.home / ".local/share/containers", self.host.home / "pm-live-cache"):
            self.assertEqual(list(folder.iterdir()), [], folder)

    def test_each_change_fails_and_never_records(self):
        home = lambda: self.host.home
        changes = {
            ".env": lambda: (self.runner / ".env").write_text("A=2\n"),
            "unit file": lambda: self.unit.write_text(self.unit.read_text() + "# changed\n"),
            "ExecStart program": lambda: (self.runner / "run.sh").write_text("#!/bin/sh\nexit 0\n"),
            "runner binary": lambda: (self.runner / "bin/Runner.Listener").write_text("other\n"),
            "new home file": lambda: (home() / ".profile").write_text("x\n"),
            "user unit": lambda: ((home() / ".config/systemd/user").mkdir(parents=True),
                                  (home() / ".config/systemd/user/x.timer").write_text("")),
            "ssh folder": lambda: (home() / ".ssh").mkdir(),
            "crontab": lambda: self.host.flag("crontab", "* * * * * true\n"),
            "at job": lambda: self.host.flag("atq", "3\tThu Nov 19 10:00:00 2026 a pmlive\n"),
            "mode": lambda: (self.runner / ".env").chmod(0o664),
            "owner": lambda: self.host.flag("owner", str(self.runner / ".env")),
            "no disableupdate": lambda: (self.runner / ".runner").write_text("{}\n"),
            "user slice": lambda: self.unit.write_text(self.unit.read_text() + "Slice=user.slice\n"),
        }
        for name, change in changes.items():
            with self.subTest(case=name):
                # A fresh recorded tree for each case.
                self.tearDown()
                self.setUp()
                change()
                result = self.check()
                self.assertEqual(result.returncode, 1, result.stdout)
                self.assertEqual(self.list.read_bytes(), self.recorded)

    # crrao m1: a process of a subordinate UID of pmlive, and a range that is
    # missing, unreadable or not one range. Each fails the gate.
    SUBUID_FAULTS = {
        # #403 F3: a process folder that is missing, cannot be listed or lists
        # no process is no proof that no live process exists.
        "process folder missing": lambda self: [(entry / "status").unlink() or (entry / "cgroup").unlink()
                                                or entry.rmdir() for entry in self.host.proc.iterdir()]
        and self.host.proc.rmdir(),
        "process folder unlistable": lambda self: self.host.proc.chmod(0),
        "no process listed": lambda self: [(entry / "status").unlink() or (entry / "cgroup").unlink()
                                           or entry.rmdir() for entry in self.host.proc.iterdir()],
        "subordinate UID process": lambda self: self.host.process(23, SUBUID + 5),
        "subordinate UID range end": lambda self: self.host.process(24, SUBUID + 65535),
        "no range": lambda self: (self.host.root / "etc/subuid").write_text(""),
        "two ranges": lambda self: (self.host.root / "etc/subuid").write_text(
            f"pmlive:{SUBUID}:65536\npmlive:300000:65536\n"),
        "unreadable range": lambda self: (self.host.root / "etc/subuid").chmod(0),
    }

    def test_the_gate(self):
        cases = {"live UID process": lambda: self.host.process(20, UID),
                 "user manager cgroup": lambda: self.host.process(21, UID, f"/user.slice/user-{UID}.slice/user@{UID}.service/init.scope"),
                 "unreadable entry": lambda: (self.host.process(22, UID + 1) / "status").chmod(0),
                 "runtime folder": lambda: self.host.runtime.mkdir(parents=True),
                 **{name: (lambda fault=fault: fault(self)) for name, fault in self.SUBUID_FAULTS.items()}}
        for name, fault in cases.items():
            with self.subTest(case=name):
                self.tearDown()
                self.setUp()
                fault()
                result = self.check()
                self.assertEqual(result.returncode, 1, result.stdout)
                self.assertIn("not emptied, because the gate failed", result.stdout)
                self.assertTrue((self.host.root / "var/lib/pm-live/work/job").exists())

    def test_gate_mode_checks_only_the_processes(self):
        # Owner step 13a: --gate runs only the /proc gate. It empties and
        # compares nothing, also when it fails.
        cases = {"clean": (lambda: None, 0),
                 "live UID process": (lambda: self.host.process(20, UID), 1),
                 "user manager cgroup": (lambda: self.host.process(
                     21, UID, f"/user.slice/user-{UID}.slice/user@{UID}.service/init.scope"), 1),
                 "unreadable entry": (lambda: (self.host.process(22, UID + 1) / "status").chmod(0), 1),
                 "runtime folder": (lambda: self.host.runtime.mkdir(parents=True), 1),
                 **{name: ((lambda fault=fault: fault(self)), 1) for name, fault in self.SUBUID_FAULTS.items()}}
        for name, (fault, expected) in cases.items():
            with self.subTest(case=name):
                self.tearDown()
                self.setUp()
                self.host.process(30, UID + 1)
                (self.runner / ".env").write_text("A=2\n")
                fault()
                result = self.host.run(self.script, "--gate")
                self.assertEqual(result.returncode, expected, result.stdout)
                for folder in (self.host.root / "var/lib/pm-live/work", self.runner / "_diag",
                               self.host.home / ".local/share/containers", self.host.home / "pm-live-cache"):
                    self.assertNotEqual(list(folder.iterdir()), [], folder)
                self.assertNotRegex(result.stdout, "runner, unit|root-held")  # no compare ran
                self.assertEqual(self.list.read_bytes(), self.recorded)

    def test_unit_rules_hold_for_a_recorded_unit(self):
        # crrao m1: the list holds what the owner installed, so these lines are
        # recorded first. Only the unit rules can refuse them.
        for line, expected in (("KillMode=process\n", 1), ("Slice=user.slice\n", 1),
                               ("KillMode=control-group\n", 0)):
            with self.subTest(line=line):
                self.tearDown()
                self.setUp()
                self.unit.write_text(self.unit.read_text() + line)
                self.assertEqual(self.host.run(self.script, "--record").returncode, 0)
                result = self.check()
                self.assertEqual(result.returncode, expected, result.stdout)

    def test_a_failed_gate_reads_no_file(self):
        # crrao I2: with a failed gate, root reads no runner or home file.
        reads = self.host.state / "reads"
        reads.unlink()  # the record of setUp read each file
        self.host.process(20, UID)
        result = self.check()
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertFalse(reads.exists(), reads.read_text() if reads.exists() else "")
        self.assertIn("not compared, because the gate failed", result.stdout)

    def test_a_listing_or_hash_failure_fails_record_and_compare(self):
        # #403 F2: a failed hash or listing gave a list with empty hashes.
        unreadable = self.host.home / ".config/containers"
        for name, fault, undo in (("hash fails", lambda: self.host.flag("sha-fails"), lambda: None),
                                  ("listing fails", lambda: unreadable.chmod(0), lambda: unreadable.chmod(0o755))):
            with self.subTest(case=name):
                fault()
                try:
                    record = self.host.run(self.script, "--record")
                    check = self.check()
                finally:
                    undo()
                self.assertEqual(record.returncode, 1, record.stdout)
                self.assertEqual(self.list.read_bytes(), self.recorded)
                self.assertEqual(check.returncode, 1, check.stdout)
                self.assertIn("cannot be listed, read or hashed", check.stdout)
                (self.host.state / "sha-fails").unlink(missing_ok=True)

    def test_record_refuses_a_forbidden_file(self):
        (self.host.home / ".ssh").mkdir()
        result = self.host.run(self.script, "--record")
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertEqual(self.list.read_bytes(), self.recorded)


if __name__ == "__main__":
    unittest.main()
