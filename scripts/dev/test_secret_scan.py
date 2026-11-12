#!/usr/bin/env python3
"""Developer checks with real Git repositories and the pinned Gitleaks binary."""

import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import tomllib
import unittest


SOURCE = Path(__file__).resolve().parent
ZERO = "0" * 40
# Build an invalid test token at runtime so the test source is not a finding.
SENTINEL = "gh" + "p_" + "q7Jm2Zr9Bk4Wx8Nt3Lv6Df1Hs5Ca0Yu2Pe9Ra"


class SecretScanChecks(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        result = subprocess.run(
            [sys.executable, "-I", "-B", str(SOURCE / "secret_scan.py"), "binary"],
            capture_output=True, text=True, check=True,
        )
        cls.binary = result.stdout.strip()

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="pm-secret-check-")
        self.addCleanup(self.temporary.cleanup)
        self.repo = Path(self.temporary.name) / "repo"
        self.repo.mkdir()
        self.env = dict(os.environ, PMCLI_GITLEAKS=self.binary, PMCLI_IN_IMAGE="1")
        for key in ("GIT_INDEX_FILE", "GIT_DIR", "GIT_WORK_TREE"):
            self.env.pop(key, None)
        self.run_git("init", "-q")
        self.run_git("config", "user.email", "fixture@example.invalid")
        self.run_git("config", "user.name", "Secret scan fixture")
        shutil.copytree(SOURCE, self.repo / "scripts/dev")
        self.run_git("config", "core.hooksPath", "scripts/dev/git-hooks")

    def command(self, *command, input=None, env=None):
        return subprocess.run(command, cwd=self.repo, env=env or self.env, input=input,
                              capture_output=True, text=True, check=False)

    def run_git(self, *args, input=None, env=None):
        result = self.command("git", *args, input=input, env=env)
        self.assertEqual(result.returncode, 0, "Git fixture setup failed")
        return result.stdout.strip()

    def stage(self, text):
        (self.repo / "note.txt").write_text(text, encoding="utf-8")
        self.run_git("add", "note.txt")

    def seed_commit(self, text):
        """Construct test history without invoking the hooks under test."""
        self.stage(text)
        tree = self.run_git("write-tree")
        parent = self.command("git", "rev-parse", "--verify", "HEAD")
        args = ["commit-tree", tree]
        if parent.returncode == 0:
            args.extend(["-p", parent.stdout.strip()])
        commit = self.run_git(*args, input="test: construct fixture\n")
        self.run_git("update-ref", "HEAD", commit)
        return commit

    def assert_result(self, result, allowed):
        self.assertEqual(result.returncode == 0, allowed, "unexpected scanner or hook status")
        self.assertNotIn(SENTINEL, result.stdout + result.stderr, "scan output exposed the test token")

    def scan_push(self, local_sha, remote_sha=ZERO, remote_ref="refs/heads/fixture", env=None):
        update = f"refs/heads/fixture {local_sha} {remote_ref} {remote_sha}\n"
        return self.command(sys.executable, "-I", "-B", "scripts/dev/secret_scan.py", "push",
                            input=update, env=env)

    def test_clean_commit_with_real_hooks(self):
        self.stage("clean\n")
        self.assert_result(self.command("git", "commit", "-m", "test: clean fixture"), True)

    def test_staged_secret_blocks_even_when_working_file_is_clean(self):
        self.stage(SENTINEL + "\n")
        (self.repo / "note.txt").write_text("clean\n", encoding="utf-8")
        self.assert_result(self.command("git", "commit", "-m", "test: staged fixture"), False)

    def test_unstaged_secret_does_not_replace_clean_index(self):
        self.stage("clean\n")
        (self.repo / "note.txt").write_text(SENTINEL + "\n", encoding="utf-8")
        self.assert_result(self.command("git", "commit", "-m", "test: clean index"), True)
        self.assertEqual(self.run_git("show", "HEAD:note.txt"), "clean")

    def test_commit_all_scans_git_temporary_index(self):
        self.seed_commit("clean\n")
        (self.repo / "note.txt").write_text(SENTINEL + "\n", encoding="utf-8")
        self.assert_result(self.command("git", "commit", "-am", "test: temporary index"), False)

    def test_explicit_alternate_index_is_scanned(self):
        self.seed_commit("clean\n")
        index = self.repo / ".git/alternate-index"
        shutil.copyfile(self.repo / ".git/index", index)
        env = dict(self.env, GIT_INDEX_FILE=str(index))
        (self.repo / "note.txt").write_text(SENTINEL + "\n", encoding="utf-8")
        self.run_git("add", "note.txt", env=env)
        self.assert_result(self.command("sh", "scripts/dev/hook-pre-commit.sh", env=env), False)

    def test_missing_scanner_blocks_even_with_image_flag(self):
        self.stage("clean\n")
        env = dict(self.env, PMCLI_GITLEAKS=str(self.repo / "absent"))
        self.assert_result(self.command("git", "commit", "-m", "test: missing scanner", env=env), False)

    def test_changed_scanner_blocks(self):
        self.stage("clean\n")
        wrong = self.repo / "wrong-scanner"
        wrong.write_text("invalid executable\n", encoding="utf-8")
        wrong.chmod(0o755)
        env = dict(self.env, PMCLI_GITLEAKS=str(wrong))
        self.assert_result(self.command("sh", "scripts/dev/hook-pre-commit.sh", env=env), False)

    def test_git_error_blocks(self):
        self.stage("clean\n")
        (self.repo / ".git/index").write_bytes(b"invalid index")
        self.assert_result(self.command("sh", "scripts/dev/hook-pre-commit.sh"), False)

    def test_local_allow_comments_and_config_cannot_skip(self):
        self.stage(SENTINEL + " # gitleaks:allow\n")
        config = "[extend]\nuseDefault = true\n[[allowlists]]\nregexes = ['.*']\n"
        (self.repo / ".gitleaks.toml").write_text(config, encoding="utf-8")
        env = dict(self.env, GITLEAKS_CONFIG_TOML=config)
        self.assert_result(self.command("sh", "scripts/dev/hook-pre-commit.sh", env=env), False)

    def test_new_branch_checks_deleted_secret_in_history(self):
        self.seed_commit(SENTINEL + "\n")
        tip = self.seed_commit("clean\n")
        self.assert_result(self.scan_push(tip), False)
        self.assert_result(self.scan_push(tip, "1" * 40), False)

    def test_existing_branch_scans_only_outgoing_range(self):
        self.seed_commit(SENTINEL + "\n")
        remote = self.seed_commit("clean\n")
        tip = self.seed_commit("another clean change\n")
        self.assert_result(self.scan_push(tip, remote), True)
        self.seed_commit(SENTINEL + "\n")
        tip = self.seed_commit("clean again\n")
        self.assert_result(self.scan_push(tip, remote), False)

    def test_delete_and_main_policy(self):
        tip = self.seed_commit("clean\n")
        self.assert_result(self.scan_push(ZERO, tip), True)
        self.assert_result(self.scan_push(tip, remote_ref="refs/heads/main"), False)
        self.assert_result(self.scan_push(tip), True)

    def test_linked_worktree_uses_its_index(self):
        self.seed_commit("clean\n")
        linked = Path(self.temporary.name) / "linked"
        self.run_git("worktree", "add", "-q", "-b", "linked", str(linked))
        shutil.copytree(SOURCE, linked / "scripts/dev")
        self.repo = linked
        self.stage(SENTINEL + "\n")
        self.assert_result(self.command("git", "commit", "-m", "test: linked index"), False)

    def test_shallow_history_blocks_push(self):
        self.seed_commit("first\n")
        tip = self.seed_commit("second\n")
        shallow = Path(self.temporary.name) / "shallow"
        self.run_git("clone", "-q", "--depth=1", self.repo.as_uri(), str(shallow))
        shutil.copytree(SOURCE, shallow / "scripts/dev")
        self.repo = shallow
        self.assert_result(self.scan_push(tip), False)

    def test_merge_resolution_is_scanned(self):
        base = self.seed_commit("base\n")
        main = self.seed_commit("main\n")
        self.stage("side\n")
        side = self.run_git("commit-tree", self.run_git("write-tree"), "-p", base,
                            input="test: side fixture\n")
        self.stage(SENTINEL + "\n")
        merge = self.run_git("commit-tree", self.run_git("write-tree"), "-p", main, "-p", side,
                             input="test: merge fixture\n")
        self.run_git("update-ref", "HEAD", merge)
        self.assert_result(self.scan_push(merge, main), False)

    def test_reviewed_files_pass_but_new_secrets_in_each_file_fail(self):
        policy = tomllib.loads((SOURCE / "gitleaks.toml").read_text())
        paths = {
            re.sub(r"\\(.)", r"\1", entry["paths"][0][1:-1])
            for rule in policy["rules"] for entry in rule["allowlists"]
        }
        for path in paths:
            destination = self.repo / path
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(SOURCE.parents[1] / path, destination)
        self.run_git("add", *sorted(paths))
        self.assert_result(self.command(sys.executable, "-I", "-B", "scripts/dev/secret_scan.py", "staged"), True)
        for path in sorted(paths):
            destination = self.repo / path
            original = destination.read_bytes()
            destination.write_bytes(original + ("\napi_key = '" + SENTINEL + "'\n").encode())
            self.run_git("add", path)
            self.assert_result(self.command(sys.executable, "-I", "-B", "scripts/dev/secret_scan.py", "staged"), False)
            destination.write_bytes(original)
            self.run_git("add", path)

    def test_diff_attributes_and_textconv_cannot_hide_content(self):
        for attribute in ("-diff", "diff=redact"):
            with self.subTest(attribute=attribute):
                self.seed_commit("clean\n")
                (self.repo / ".gitattributes").write_text(f"note.txt {attribute}\n", encoding="utf-8")
                self.run_git("config", "diff.redact.textconv", "true")
                self.run_git("add", ".gitattributes")
                self.stage(SENTINEL + "\n")
                self.assert_result(self.command("sh", "scripts/dev/hook-pre-commit.sh"), False)
                bad = self.seed_commit(SENTINEL + "\n")
                self.assert_result(self.scan_push(bad), False)

    def test_replacement_refs_cannot_hide_pushed_objects(self):
        bad = self.seed_commit(SENTINEL + "\n")
        tip = self.seed_commit("clean\n")
        replacement = self.run_git("commit-tree", self.run_git("write-tree"), input="test: replacement fixture\n")
        self.run_git("replace", bad, replacement)
        self.assert_result(self.scan_push(tip), False)

    def test_grafted_history_blocks_push(self):
        self.seed_commit(SENTINEL + "\n")
        tip = self.seed_commit("clean\n")
        grafts = self.repo / ".git/info/grafts"
        grafts.write_text(tip + "\n", encoding="utf-8")
        self.assert_result(self.scan_push(tip), False)

    def test_type_change_from_symlink_scans_new_blob(self):
        note = self.repo / "note.txt"
        note.symlink_to("clean-target")
        self.run_git("add", "note.txt")
        commit = self.run_git("commit-tree", self.run_git("write-tree"), input="test: symlink fixture\n")
        self.run_git("update-ref", "HEAD", commit)
        note.unlink()
        self.stage(SENTINEL + "\n")
        self.assert_result(self.command("sh", "scripts/dev/hook-pre-commit.sh"), False)


if __name__ == "__main__":
    unittest.main(verbosity=2)
