#!/usr/bin/env python3
"""Exercise the public setup checker against isolated, real file inventories."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
CHECKER = ROOT / "scripts/ci/check-setup.py"
POLICY = ROOT / "scripts/ci/setup-policy.json"
TRACKER = "docs/agents/issue-tracker.md"
PROVENANCE = "docs/development/skill-provenance.json"
WORKFLOW = ".github/workflows/check.yml"
SKILL = ".agents/skills/pm-go/SKILL.md"


class SetupContracts(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        result = subprocess.run(
            ["git", "-C", str(ROOT), "ls-files", "-z"], check=True,
            capture_output=True, text=True, timeout=15,
        )
        cls.inputs = set(filter(None, result.stdout.split("\0")))
        cls.contents = {name: (ROOT / name).read_bytes() for name in cls.inputs}

    def fixture(self, directory):
        root = Path(directory) / "checkout"
        for name, content in self.contents.items():
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(content)
        inventory = Path(directory) / "paths.json"
        inventory.write_text(json.dumps(sorted(self.inputs)))
        return root, inventory

    def check(self, root, inventory, env=None):
        return subprocess.run(
            [sys.executable, "-I", "-B", str(CHECKER), "--root", str(root),
             "--policy", str(POLICY), "--paths-file", str(inventory)],
            capture_output=True, text=True, timeout=90, env=env,
        )

    def test_current_setup_is_accepted(self):
        with tempfile.TemporaryDirectory() as directory:
            root, inventory = self.fixture(directory)
            result = self.check(root, inventory)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            report = json.loads(result.stdout)
            self.assertEqual(set(report["provenance"]["project_skills"]), {
                ".agents/skills/pm-go/SKILL.md",
                ".agents/skills/pm-connector-contracts/SKILL.md",
                ".agents/skills/pm-durable-effects/SKILL.md",
                ".agents/skills/pm-release-recovery/SKILL.md",
            })

    def test_local_heading_fragments_are_resolved(self):
        with tempfile.TemporaryDirectory() as directory:
            root, inventory = self.fixture(directory)
            target = root / "docs/agents/domain.md"
            target.write_text(target.read_text() + "\n## Repeated `heading`\n\nFirst.\n\n## Repeated `heading`\n\nSecond.\n")
            tracker = root / TRACKER
            original = tracker.read_text()
            for fragment, accepted in (("repeated-heading", True),
                                       ("repeated-heading-1", True),
                                       ("repeated-heading-2", False)):
                with self.subTest(fragment=fragment):
                    tracker.write_text(original + f"\n[local section](domain.md#{fragment})\n")
                    result = self.check(root, inventory)
                    self.assertEqual(result.returncode == 0, accepted,
                                     result.stdout + result.stderr)

    def test_invalid_setup_is_refused(self):
        def text(root, path, transform):
            target = root / path
            target.write_text(transform(target.read_text()))

        def json_change(root, path, change):
            value = json.loads((root / path).read_text())
            change(value)
            (root / path).write_text(json.dumps(value))

        def section(content, empty=False):
            before, rest = content.split("## Resolve the exact issue", 1)
            _, after = rest.split("\n## ", 1)
            return before + ("## Resolve the exact issue\n\n" if empty else "") + "## " + after

        def broken_tracker_link(root, _):
            text(root, TRACKER, lambda value: value + "\n[broken](missing-required-context.md)\n")

        def missing_membership(root, inventory):
            names = json.loads(inventory.read_text())
            names.remove(TRACKER)
            inventory.write_text(json.dumps(names))

        def symlink(root, _):
            path = root / "docs/agents/domain.md"
            path.unlink()
            path.symlink_to(root / TRACKER)

        def missing_provenance(root, _):
            json_change(root, PROVENANCE,
                        lambda value: value["project_skills"][0]["references"].remove(TRACKER))

        cases = {
            "blank tracker": lambda r, _: (r / TRACKER).write_text(""),
            "wrong document kind": lambda r, _: text(r, TRACKER, lambda s: s.replace(
                "# Issue and specification tracker", "# Unrelated document", 1)),
            "missing required section": lambda r, _: text(r, TRACKER, section),
            "empty required section": lambda r, _: text(r, TRACKER, lambda s: section(s, True)),
            "missing routing edge": lambda r, _: text(r, "AGENTS.md", lambda s: s.replace(
                "[issue/spec lookup](docs/agents/issue-tracker.md)", "issue/spec lookup")),
            "missing provenance reference": missing_provenance,
            "broken tracker link": broken_tracker_link,
            "missing disk file": lambda r, _: (r / TRACKER).unlink(),
            "missing tracked membership": missing_membership,
            "missing provenance": lambda r, _: (r / PROVENANCE).unlink(),
            "stale skill digest": lambda r, _: text(r, SKILL, lambda s: s + "\nChanged skill.\n"),
            "missing frontmatter": lambda r, _: text(r, SKILL, lambda s: s.split("---\n", 2)[-1]),
            "duplicate frontmatter": lambda r, _: text(r, SKILL, lambda s: s.replace(
                "name: pm-go", "name: pm-go\nname: conflicting", 1)),
            "duplicate JSON key": lambda r, _: text(r, PROVENANCE, lambda s: s.replace(
                '"schema_version":', '"schema_version": "duplicate", "schema_version":', 1)),
            "unknown provenance key": lambda r, _: json_change(r, PROVENANCE,
                lambda v: v.update({"unknown": True})),
            "escaping local link": lambda r, _: text(r, TRACKER,
                lambda s: s + "\n[escape](../../../../outside)\n"),
            "symlinked local input": symlink,
            "floating action": lambda r, _: text(r, WORKFLOW, lambda s: s.replace(
                "3d3c42e5aac5ba805825da76410c181273ba90b1", "v7")),
            "excess token permission": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace("contents: read", "contents: write")),
            "duplicate workflow key": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace("contents: read", "contents: read\n  contents: write")),
            "unsafe workflow trigger": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace("  pull_request:", "  pull_request_target:")),
            "different runner": lambda r, _: text(r, WORKFLOW,
                lambda s: s.replace("pm-cli-qual-20260911-r8", "unrelated-runner")),
            "missing required validator": lambda r, _: (r / "scripts/ci/check-setup.sh").unlink(),
            "invalid shell": lambda r, _: text(r, "scripts/ci/check-setup.sh",
                lambda s: s + "\nif then\n"),
        }
        for name, mutate in cases.items():
            with self.subTest(case=name), tempfile.TemporaryDirectory() as directory:
                root, inventory = self.fixture(directory)
                mutate(root, inventory)
                result = self.check(root, inventory)
                self.assertNotEqual(result.returncode, 0, name + " was accepted")

    def test_missing_required_tool_fails(self):
        with tempfile.TemporaryDirectory() as directory:
            root, inventory = self.fixture(directory)
            result = self.check(root, inventory, dict(os.environ, PATH=directory))
            self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()
