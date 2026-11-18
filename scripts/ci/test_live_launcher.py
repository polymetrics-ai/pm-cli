#!/usr/bin/env python3
"""Developer checks of the live mode of in-ci-image.sh (L-e1 P5), not acceptance proof.

The checks run a copy of the launcher in a temporary root. The copy differs
only in the four fixed lines of the live mode: the runtime root, the
/etc/pm-live path, and the tmpfs and owner probes. A fake engine records its
argument list and its environment. Limit: the real engine, tmpfs and the
process view are proven only by the rehearsal.
"""
import hashlib
import json
import os
from pathlib import Path
import socket
import stat
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
LAUNCHER = ROOT / "scripts/ci/in-ci-image.sh"
FIXED = ["live_runtime=/run/user\n", "live_etc=/etc/pm-live\n",
         'live_fs_type() { stat -f -c %T "$1"; }\n', "live_owner_mode() { stat -c '%u %a' \"$1\"; }\n"]
DUMMY = "dummy-value-" + hashlib.sha256(b"live launcher").hexdigest()[:28]
UID = os.getuid()
FAKE_ENGINE = """import json, os, sys
with open(os.environ["FAKE_ENGINE_LOG"], "a") as log:
    log.write(json.dumps({"argv": sys.argv[1:], "env": dict(os.environ)}) + "\\n")
if sys.argv[1:3] == ["image", "inspect"]:
    print("sha256:" + "f" * 64)
"""
# The names that the run mode passes from the host with a bare --env.
RUN_NAMES = {"CI", "GITHUB_ACTIONS", "GITHUB_WORKSPACE", "GITHUB_REPOSITORY", "GITHUB_SHA", "GITHUB_REF",
             "GITHUB_EVENT_NAME", "GITHUB_EVENT_PATH", "GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT", "RUNNER_TEMP",
             "RUNNER_NAME", "GITHUB_STEP_SUMMARY"}


class Workspace:
    """A temporary checkout, runtime root, token channel, fake engine and socket."""

    def __init__(self, directory):
        self.root = Path(directory).resolve()
        script = LAUNCHER.read_text()
        owner = (f'live_owner_mode() {{ "{sys.executable}" -I -c \'import os, sys; s = os.lstat(sys.argv[1]); '
                 f'print(s.st_uid + (sys.argv[1] == os.environ.get("FAKE_OWNER_PATH")), '
                 f'format(s.st_mode & 0o777, "o"))\' "$1"; }}\n')
        fixed = [f"live_runtime={self.root}/run/user\n", f"live_etc={self.root}/etc/pm-live\n",
                 'live_fs_type() { case "$1" in "$FAKE_TMPFS"/*) echo tmpfs ;; *) echo ext4 ;; esac; }\n', owner]
        for line, replacement in zip(FIXED, fixed):
            assert script.count(line) == 1, line
            script = script.replace(line, replacement)
        (self.root / "scripts/ci").mkdir(parents=True)
        (self.root / "scripts/ci/in-ci-image.sh").write_text(script)
        (self.root / "build/ci").mkdir(parents=True)
        for name in ("Dockerfile", "requirements.txt"):
            (self.root / "build/ci" / name).write_text(name + "\n")
        (self.root / "bin").mkdir()
        engine = self.root / "bin/podman"
        engine.write_text(f"#!{sys.executable}\n" + FAKE_ENGINE)
        engine.chmod(0o755)
        self.socket = socket.socket(socket.AF_UNIX)
        self.socket.bind(str(self.root / "engine.sock"))
        for name in ("home", "runner-temp"):
            (self.root / name).mkdir()
        (self.root / "runner-temp/summary").write_text("")
        self.tokens = self.root / "run/user" / str(UID) / "pm-live"
        self.tokens.mkdir(parents=True, mode=0o700)
        self.tokens.chmod(0o700)
        self.token = self.tokens / "w2-token"
        self.write_token(DUMMY)
        self.log = self.root / "engine.log"

    def write_token(self, value, mode=0o600):
        self.token.write_text(value)
        self.token.chmod(mode)

    def env(self, **changes):
        env = {"PATH": "/usr/bin:/bin", "HOME": str(self.root / "home"), "GITHUB_WORKSPACE": str(self.root),
               "RUNNER_TEMP": str(self.root / "runner-temp"), "GITHUB_RUN_ID": "77", "GITHUB_RUN_ATTEMPT": "1",
               "GITHUB_STEP_SUMMARY": str(self.root / "runner-temp/summary"),
               "PMCLI_CONTAINER_ENGINE": str(self.root / "bin/podman"),
               "PMCLI_ENGINE_SOCKET": str(self.root / "engine.sock"), "FAKE_ENGINE_LOG": str(self.log),
               "FAKE_TMPFS": str(self.root / "run/user"), "PMCLI_LIVE_TOKEN_FILE": str(self.token)}
        env.update(changes)
        return env

    def run(self, *arguments, **changes):
        result = subprocess.run(["/bin/sh", str(self.root / "scripts/ci/in-ci-image.sh"), *arguments],
                                env=self.env(**changes), capture_output=True, text=True, timeout=60)
        calls = [json.loads(line) for line in self.log.read_text().splitlines()] if self.log.exists() else []
        return result, calls

    def close(self):
        self.socket.close()


def options(argv, name):
    return [argv[index + 1] for index, word in enumerate(argv) if word == name]


class LiveLauncher(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.space = Workspace(self.directory.name)

    def tearDown(self):
        self.space.close()
        self.directory.cleanup()

    def live(self, **changes):
        return self.space.run("live", "live-w2", "--", "sh", "scripts/ci/check-live.sh", **changes)

    def test_the_fixed_lines_are_the_production_values(self):
        script = LAUNCHER.read_text()
        self.assertEqual([script.count(line) for line in FIXED], [1, 1, 1, 1])

    def test_live_arguments_and_environment(self):
        result, calls = self.live()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([call["argv"][:2] for call in calls], [["build", "--platform"], ["image", "inspect"],
                                                                ["run", "--rm"]])
        self.assertNotIn(DUMMY, json.dumps(calls) + result.stdout + result.stderr)
        argv = calls[-1]["argv"]
        run_root = self.space.root / f"run/user/{UID}/pm-live-run/77-1"
        cache = self.space.root / "home/pm-live-cache/77-1"
        self.assertEqual(stat.S_IMODE(run_root.stat().st_mode), 0o700)
        self.assertEqual(sorted(path.name for path in cache.iterdir()), ["home"])
        self.assertIn("--pid", argv)
        self.assertEqual(options(argv, "--pid"), ["host"])
        self.assertEqual([word for word in argv if word.startswith(("--privileged", "--cap-add", "--device"))], [])
        values = {}
        for item in options(argv, "--env"):
            name, _, value = item.partition("=")
            if "=" in item:
                values[name] = value
            else:
                self.assertIn(name, RUN_NAMES - {"RUNNER_TEMP", "GITHUB_EVENT_PATH"})
        self.assertEqual(values, {"PMCLI_CACHE_DIR": str(cache), "HOME": f"{cache}/home",
                                  "PM_TOOLS_CACHE": f"{cache}/pm-tools", "RUNNER_TEMP": str(run_root),
                                  "PMCLI_LIVE_TOKEN_FILE": "/run/pm-live-token/w2-token"})
        self.assertEqual(options(argv, "--volume"), [
            f"{self.space.root}:{self.space.root}", f"{run_root}:{run_root}", f"{cache}:{cache}",
            f"{self.space.tokens}:/run/pm-live-token",
            f"{self.space.root}/etc/pm-live/run-kind:/run/pm-live-run-kind:ro",
            f"{self.space.root}/runner-temp/summary:{self.space.root}/runner-temp/summary",
            f"{self.space.root}/engine.sock:/var/run/docker.sock"])
        self.assertEqual(argv[-3:], ["localhost/pmcli-live:77-1", "sh", "scripts/ci/check-live.sh"])

    def test_each_call_builds_a_fresh_image(self):
        # The fake reports that each image exists. The live mode still builds.
        for attempt in ("1", "2"):
            result, calls = self.live(GITHUB_RUN_ATTEMPT=attempt)
            self.assertEqual(result.returncode, 0, result.stderr)
        builds = [call["argv"] for call in calls if call["argv"][0] == "build"]
        self.assertEqual([options(argv, "--tag") for argv in builds],
                         [["localhost/pmcli-live:77-1"], ["localhost/pmcli-live:77-2"]])

    def test_a_reused_run_root_is_refused(self):
        self.assertEqual(self.live()[0].returncode, 0)
        result, calls = self.live()
        self.assertEqual(result.returncode, 1)
        self.assertIn("the run root exists", result.stderr)
        self.assertEqual(len([call for call in calls if call["argv"][0] == "build"]), 1)

    def test_token_channel_refusals_stop_before_the_build(self):
        space = self.space
        real = space.tokens / "real"

        def link():
            real.write_text(DUMMY)
            space.token.unlink()
            space.token.symlink_to(real)

        def fifo():
            space.token.unlink()
            os.mkfifo(space.token, 0o600)

        cases = {
            "link": (link, {}), "FIFO": (fifo, {}),
            "mode 0640": (lambda: space.token.chmod(0o640), {}),
            "wrong owner": (lambda: None, {"FAKE_OWNER_PATH": str(space.token)}),
            "wrong directory": (lambda: (space.root / "w2-token").write_text(DUMMY),
                                {"PMCLI_LIVE_TOKEN_FILE": str(space.root / "w2-token")}),
            "not tmpfs": (lambda: None, {"FAKE_TMPFS": str(space.root / "other")}),
            "directory mode 0750": (lambda: space.tokens.chmod(0o750), {}),
            "empty": (lambda: space.write_token(""), {}),
            "19 bytes": (lambda: space.write_token("x" * 19), {}),
            "4097 bytes": (lambda: space.write_token("x" * 4097), {}),
            "unset": (lambda: None, {"PMCLI_LIVE_TOKEN_FILE": ""}),
        }
        for name, (fault, changes) in cases.items():
            with self.subTest(case=name):
                fault()
                result, calls = self.live(**changes)
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn("in-ci-image.sh live: ", result.stderr)
                self.assertEqual(calls, [])
                self.assertFalse((space.root / f"run/user/{UID}/pm-live-run").exists())
                space.tokens.chmod(0o700)
                for path in (space.token, real):
                    if path.is_symlink() or path.exists():
                        path.unlink()
                space.write_token(DUMMY)
        for size in (20, 4096):
            with self.subTest(size=size):
                space.write_token("x" * size)
                result, _ = self.live(GITHUB_RUN_ID=str(size))
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_run_mode_is_unchanged(self):
        result, calls = self.space.run("run", "bdd", "--", "sh", "check.sh")
        self.assertEqual(result.returncode, 0, result.stderr)
        root, cache = self.space.root, self.space.root / "home/.cache/pmcli"
        argv = calls[-1]["argv"]
        image = argv[-3]
        self.assertTrue(image.startswith("localhost/pmcli-ci:"), image)
        self.assertEqual(argv, [
            "run", "--rm", "--init", "--pull", "never", "--name", "pmcli-77-1-bdd",
            "--userns", "keep-id", "--user", f"{UID}:{os.getgid()}", "--network", "host",
            "--memory", "8g", "--cpus", "4", "--pids-limit", "4096",
            "--volume", f"{root}:{root}", "--volume", f"{root}/runner-temp:{root}/runner-temp",
            "--volume", f"{cache}:{cache}", "--volume", f"{root}/engine.sock:/var/run/docker.sock",
            "--workdir", str(root), "--env", f"PMCLI_CACHE_DIR={cache}", "--env", f"HOME={cache}/home",
            "--env", f"PM_TOOLS_CACHE={cache}/pm-tools",
            "--env", "CI", "--env", "GITHUB_ACTIONS", "--env", "GITHUB_WORKSPACE", "--env", "GITHUB_REPOSITORY",
            "--env", "GITHUB_SHA", "--env", "GITHUB_REF", "--env", "GITHUB_EVENT_NAME", "--env", "GITHUB_EVENT_PATH",
            "--env", "GITHUB_RUN_ID", "--env", "GITHUB_RUN_ATTEMPT", "--env", "RUNNER_TEMP", "--env", "RUNNER_NAME",
            "--env", "GITHUB_STEP_SUMMARY", image, "sh", "check.sh"])
        self.assertNotIn("build", [call["argv"][0] for call in calls])


if __name__ == "__main__":
    unittest.main()
