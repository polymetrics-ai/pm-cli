#!/usr/bin/env python3
"""Inspect actual setup contracts; incomplete provenance exits 3, never green.

Pinned setup dependencies: PyYAML 6.0.3, markdown-it-py 2.2.0,
mdurl 0.1.2, actionlint 1.7.12, ShellCheck 0.11.0. No auto-installation.
This checks repository structure; actual CI and native execution are separate evidence.
"""
import argparse
import hashlib
import json
import re
import shutil
import subprocess
import sys
from pathlib import Path, PurePosixPath
from urllib.parse import unquote, urlsplit

import yaml
from markdown_it import MarkdownIt


def require(condition, message):
    if not condition:
        raise ValueError(message)


def unique_pairs(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "duplicate object key")
        result[key] = value
    return result


class UniqueLoader(yaml.BaseLoader):
    pass


def mapping(loader, node):
    return unique_pairs((loader.construct_object(k), loader.construct_object(v))
                        for k, v in node.value)


UniqueLoader.add_constructor("tag:yaml.org,2002:map", mapping)


def read_yaml(text):
    require(not any(isinstance(event, yaml.AliasEvent) for event in yaml.parse(text)),
            "YAML aliases require an explicit reviewed policy")
    return yaml.load(text, Loader=UniqueLoader)


def inside(root, path):
    # Reject symlinks even when their resolved target happens to remain inside.
    require(not path.is_symlink(), "symlinked input")
    resolved = path.resolve(strict=True)
    resolved.relative_to(root)
    for part in path.parents:
        if part == root:
            break
        require(not part.is_symlink(), "symlinked input ancestor")
    return resolved


def command(args, root):
    result = subprocess.run(args, cwd=root, capture_output=True, text=True,
                            timeout=60, check=False)
    require(result.returncode == 0,
            f"validator failed: {args!r} (exit {result.returncode})\n"
            f"{result.stdout}{result.stderr}")
    return result.stdout


def fields(value, keys):
    require(isinstance(value, dict) and set(value) == set(keys), "unknown or missing provenance keys")


def string(value):
    require(isinstance(value, str) and bool(value.strip()), "empty or invalid provenance string")


def sequence(value):
    require(isinstance(value, list) and value, "empty or invalid provenance array")
    return value


def strings(value, nonempty=True):
    require(isinstance(value, list) and (value or not nonempty), "invalid provenance string array")
    for item in value:
        string(item)
    require(len(value) == len(set(value)), "duplicate provenance array value")


def relative(value):
    string(value)
    parsed = PurePosixPath(value)
    require(not parsed.is_absolute() and not any(p in {"", ".", ".."} for p in value.split("/"))
            and "\\" not in value and ":" not in value and str(parsed) == value,
            "noncanonical or escaping provenance path")


def digest(value):
    require(isinstance(value, str) and re.fullmatch(r"[0-9a-f]{64}", value), "invalid SHA-256")


def https(value):
    string(value)
    parsed = urlsplit(value)
    require(parsed.scheme == "https" and parsed.hostname and not parsed.username
            and not parsed.password, "invalid provenance HTTPS URL")


def license_expression(value):
    # Closed setup vocabulary; a new license requires reviewed vocabulary expansion.
    string(value)
    if value == "UNVERIFIED":
        return
    identifiers = {"MIT", "Apache-2.0", "BSD-2-Clause", "BSD-3-Clause", "CC-BY-4.0",
                   "CC0-1.0", "ISC", "GPL-3.0-only", "GPL-3.0-or-later", "MPL-2.0"}
    parts = re.findall(r"[A-Za-z0-9.+-]+|[()]|\S", value)
    position = 0

    def expression():
        nonlocal position

        def atom():
            nonlocal position
            require(position < len(parts), "incomplete SPDX expression")
            token = parts[position]
            position += 1
            if token == "(":
                expression()
                require(position < len(parts) and parts[position] == ")", "unclosed SPDX expression")
                position += 1
            else:
                require(token in identifiers, "SPDX identifier needs reviewed vocabulary expansion")

        atom()
        while position < len(parts) and parts[position] in {"AND", "OR"}:
            position += 1
            atom()

    expression()
    require(position == len(parts), "invalid SPDX expression")


def heading_ids(md, path):
    """Resolve rendered Markdown heading links, including duplicate suffixes."""
    require(path.suffix == ".md", "local fragment target is not Markdown")
    tokens = md.parse(path.read_text())
    result = set()
    for index, token in enumerate(tokens):
        if token.type != "heading_open":
            continue
        text = "".join(child.content for child in tokens[index + 1].children or []
                       if child.type in {"text", "code_inline", "image"})
        base = re.sub(r"[^\w\- ]", "", text.lower()).replace(" ", "-")
        slug, suffix = base, 0
        while slug in result:
            suffix += 1
            slug = f"{base}-{suffix}"
        result.add(slug)
    return result


def document_check(tokens, contract):
    """Validate declared structure, not prose meaning or accepted-spec access."""
    headings = [(i, token.tag, tokens[i + 1].content)
                for i, token in enumerate(tokens) if token.type == "heading_open"]
    require(headings and headings[0][0] == 0 and
            [(tag, title) for _, tag, title in headings if tag == "h1"] ==
            [("h1", contract["title"])], "required document title missing or invalid")
    for title in contract["sections"]:
        matches = [i for i, tag, text in headings if tag == "h2" and text == title]
        require(len(matches) == 1, "required document section missing or duplicated")
        start = matches[0]
        end = next((i for i, tag, _ in headings if i > start and tag in {"h1", "h2"}),
                   len(tokens))
        require(any(t.type == "inline" and t.content.strip()
                    for t in tokens[start + 3:end]), "required document section empty")


def provenance_check(root, files, policy):
    name = policy["provenance_contract"]
    relative(name)
    if name not in files:
        return None
    manifest = json.loads(files[name].read_text(), object_pairs_hook=unique_pairs)
    fields(manifest, {"schema_version", "project_skills", "external_sources", "official_fallbacks"})
    require(manifest["schema_version"] == "pm.skill-provenance/v1", "unknown provenance schema")
    projects = sequence(manifest["project_skills"])
    names, paths, references = set(), set(), set()
    for row in projects:
        fields(row, {"name", "path", "sha256", "origin", "license", "references", "overrides"})
        string(row["name"])
        relative(row["path"])
        require(row["path"] == f".agents/skills/{row['name']}/SKILL.md", "wrong project skill path")
        require(row["name"] not in names and row["path"] not in paths, "duplicate project skill")
        names.add(row["name"])
        paths.add(row["path"])
        require(row["origin"] == "repository-original" and row["license"] == "MIT", "wrong project provenance")
        digest(row["sha256"])
        require(row["path"] in files, "project skill absent from checked inventory")
        require(hashlib.sha256(files[row["path"]].read_bytes()).hexdigest() == row["sha256"],
                "stale project skill hash")
        strings(row["references"], nonempty=False)
        require(set(policy.get("required_project_references", [])) <= set(row["references"]),
                "required project reference membership differs from approved setup")
        for target in row["references"]:
            relative(target)
            require(target in files, "required reference absent from checked inventory")
            inside(root, root / target)
            references.add(target)
        strings(row["overrides"])
    actual = {p for p in files if p.startswith('.agents/skills/') and p.endswith('/SKILL.md')}
    require(paths == actual and names == set(policy["required_project_skills"]),
            "project skill membership differs from approved setup")
    external_ids = set()
    for row in sequence(manifest["external_sources"]):
        fields(row, {"id", "url", "revision", "license", "notice", "strategy", "required_for", "files", "overrides"})
        string(row["id"])
        require(row["id"] not in external_ids, "duplicate external source")
        external_ids.add(row["id"])
        if row["url"] is not None:
            https(row["url"])
        if row["revision"] is not None:
            require(isinstance(row["revision"], str) and re.fullmatch(r"[0-9a-f]{40}", row["revision"]),
                    "external revision is not an immutable commit")
        license_expression(row["license"])
        string(row["notice"])
        require(row["strategy"] == "use-installed-no-copy", "unapproved external copy strategy")
        require(row["required_for"] in {"optional-reference", "authorized-review", "authorized-delivery"},
                "unknown external requirement")
        strings(row["overrides"])
        external_paths, roles = set(), set()
        for item in sequence(row["files"]):
            fields(item, {"path", "sha256", "role"})
            relative(item["path"])
            digest(item["sha256"])
            require(item["path"] not in external_paths, "duplicate external file")
            external_paths.add(item["path"])
            require(item["role"] in {"entrypoint", "reference", "license", "notice", "metadata"},
                    "unknown external file role")
            roles.add(item["role"])
        require("entrypoint" in roles, "external entrypoint missing")
    fallback_ids = set()
    for row in sequence(manifest["official_fallbacks"]):
        fields(row, {"id", "url"})
        string(row["id"])
        https(row["url"])
        require(row["id"] not in fallback_ids, "duplicate fallback")
        fallback_ids.add(row["id"])
    return {"project_skills": sorted(paths), "references": sorted(references),
            "external_sources": sorted(external_ids), "fallbacks": sorted(fallback_ids),
            "external_bytes": "recorded only; not read on CI runner"}


def workflow_check(path, actionlint):
    obj = read_yaml(path.read_text())
    require(isinstance(obj, dict), "workflow must be an object")
    require(set(obj) == {"name", "on", "permissions", "concurrency", "jobs"},
            "unapproved workflow capability")
    require(obj["on"] == {"pull_request": {"branches": ["main"],
            "types": ["opened", "synchronize", "reopened", "edited"]}},
            "unapproved workflow triggers")
    require(obj["permissions"] == {"contents": "read"}, "excess workflow permissions")
    require(obj["concurrency"] == {
        "group": "foundation-${{ github.event.pull_request.number }}-"
                 "${{ github.event.action == 'edited' && 'metadata' || 'source' }}",
        "cancel-in-progress": "true",
    }, "unapproved workflow concurrency")
    require(set(obj["jobs"]) == {"metadata", "foundation"}, "unapproved job membership")

    trust = (
        "github.repository == 'polymetrics-ai/pm-cli' && "
        "github.event.pull_request.head.repo.full_name == github.repository && "
        "github.actor == 'karthik-sivadas' && "
        "github.triggering_actor == 'karthik-sivadas'"
    )
    runner = ["self-hosted", "Linux", "X64", "pm-cli-qual-20260911-r8"]

    def checkout_check(step):
        require(set(step) == {"name", "uses", "with"}, "unapproved checkout capability")
        require(step["uses"] ==
                "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
                "checkout is not the reviewed full action pin")
        require(step["with"] == {"persist-credentials": "false", "fetch-depth": "1",
                "submodules": "false", "lfs": "false", "set-safe-directory": "false"},
                "checkout capabilities changed")

    metadata = obj["jobs"]["metadata"]
    require(set(metadata) == {"name", "if", "runs-on", "timeout-minutes", "steps"},
            "unapproved metadata job capability")
    require(metadata["name"] == "pull-request-metadata", "metadata check identity changed")
    require(metadata["if"] == trust, "unapproved metadata trust boundary")
    require(metadata["runs-on"] == runner, "metadata runner routing changed")
    require(metadata["timeout-minutes"] == "3", "metadata timeout changed")
    metadata_steps = metadata["steps"]
    require(isinstance(metadata_steps, list) and len(metadata_steps) == 2,
            "unexpected metadata step membership")
    checkout_check(metadata_steps[0])
    require(metadata_steps[1] == {
        "name": "Validate pull request metadata",
        "timeout-minutes": "2",
        "run": 'set -eu\n'
               'export PATH="/opt/pmcli-tools/venv/bin:/opt/pmcli-tools/bin:$PATH"\n'
               'python3 scripts/install_architecture_docs_dependencies.py \\\n'
               '  --destination "$RUNNER_TEMP/foundation-metadata-deps"\n'
               'PYTHONPATH="$RUNNER_TEMP/foundation-metadata-deps" \\\n'
               '  python3 scripts/validate_architecture_docs.py '
               '--pr-event "$GITHUB_EVENT_PATH"\n',
    }, "unapproved metadata validation step")

    job = obj["jobs"]["foundation"]
    require(set(job) == {"name", "needs", "if", "runs-on", "timeout-minutes", "steps"},
            "unapproved source job capability")
    require(job["name"] ==
            "${{ github.event.action == 'edited' && "
            "'metadata-only (foundation not run)' || 'foundation' }}",
            "metadata-only execution can replace the source gate")
    require(job["needs"] == "metadata", "source qualification does not follow metadata")
    require(job["if"] ==
            "${{ !cancelled() && " + trust + " && github.event.action != 'edited' }}",
            "unapproved source trust or event boundary")
    require(job["runs-on"] == runner, "runner routing changed")
    require(job["timeout-minutes"] == "30", "job timeout changed")
    steps = job["steps"]
    require(isinstance(steps, list) and len(steps) == 3, "unexpected step membership")
    checkout, check, cleanup = steps
    checkout_check(checkout)
    require(check == {"name": "Build and test the foundation", "timeout-minutes": "25",
            "run": "sh scripts/ci/check-foundation.sh"}, "unapproved executable step")
    require(cleanup == {
        "name": "Clean up the job-owned dependency fixture",
        "if": "always()",
        "timeout-minutes": "4",
        "run": 'export PATH="/opt/pmcli-tools/venv/bin:/opt/pmcli-tools/bin:$PATH"\n'
               'python3 scripts/ci/with-stack-fixture.py \\\n'
               '  --cleanup "$RUNNER_TEMP/pmcli-foundation-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT.json"\n',
    }, "unapproved fixture cleanup step")
    config = Path(__file__).with_name("actionlint.yaml")
    command([actionlint, "-no-color", "-config-file", str(config), str(path)], path.parent)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--policy", type=Path, required=True)
    parser.add_argument("--workflow", type=Path)
    parser.add_argument("--paths-file", type=Path,
                        help="Explicit file inventory for an exported read-only snapshot")
    args = parser.parse_args()
    root = args.root.resolve(strict=True)
    policy = json.loads(args.policy.read_text(), object_pairs_hook=unique_pairs)
    require(policy["schema_version"] == "pm.setup-check/v1", "unknown policy version")
    shellcheck = shutil.which("shellcheck")
    actionlint = shutil.which("actionlint")
    require(shellcheck and actionlint, "required ShellCheck/actionlint unavailable")
    if args.paths_file:
        names = json.loads(args.paths_file.read_text(), object_pairs_hook=unique_pairs)
        require(isinstance(names, list) and all(isinstance(x, str) for x in names),
                "invalid export inventory")
    else:
        names = command(["git", "ls-files", "-z"], root).split("\0")
        names = [name for name in names if name]
    require(len(set(names)) == len(names), "duplicate inventory path")
    for name in names:
        relative(name)
    require(names, "no tracked setup inputs")
    require(set(policy["required_files"]) <= set(names), "required setup file missing from tracking")
    selected = {name for name in names if name in policy["required_files"]
                or any(name.startswith(prefix) for prefix in policy["checked_prefixes"])}
    all_files = {name: inside(root, root / name) for name in names}
    files = {name: all_files[name] for name in sorted(selected)}
    md = MarkdownIt("commonmark").enable("table")
    document_tokens, local_edges = {}, {}
    fragments = {}
    links = 0
    skills = 0
    json_count = 0
    shells = 0
    for name, path in files.items():
        if name.endswith(".json"):
            json.loads(path.read_text(), object_pairs_hook=unique_pairs)
            json_count += 1
        if name.endswith(".md"):
            content = path.read_text()
            if name.endswith("/SKILL.md"):
                require(content.startswith("---\n"), "skill front matter missing")
                split = content.split("\n---\n", 1)
                require(len(split) == 2, "unclosed skill front matter")
                front = read_yaml(split[0][4:])
                require(isinstance(front, dict), "invalid skill front matter")
                require(front.get("name") == path.parent.name, "skill name/directory mismatch")
                require(isinstance(front.get("description"), str) and front["description"].strip(),
                        "skill description missing")
                skills += 1
                content = split[1]
            document_tokens[name] = md.parse(content)
            local_edges[name] = set()
            for block in document_tokens[name]:
                for token in block.children or []:
                    if token.type not in {"link_open", "image"}:
                        continue
                    url = token.attrGet("href" if token.type == "link_open" else "src")
                    parsed = urlsplit(url)
                    if parsed.scheme in {"https", "http", "mailto"}:
                        continue
                    require(not parsed.scheme and not parsed.netloc, "unsupported local link scheme")
                    target = unquote(parsed.path)
                    require(not target.startswith("/"), "absolute local documentation link")
                    resolved = path
                    if target:
                        resolved = inside(root, path.parent / target)
                        if token.type == "link_open":
                            local_edges[name].add(resolved.relative_to(root).as_posix())
                        links += 1
                    if parsed.fragment:
                        if resolved not in fragments:
                            fragments[resolved] = heading_ids(md, resolved)
                        require(unquote(parsed.fragment) in fragments[resolved],
                                f"missing local heading: {name} -> {url}")
        if name.endswith(".sh"):
            command(["sh", "-n", str(path)], root)
            command([shellcheck, "--shell=sh", str(path)], root)
            shells += 1
    require(skills and shells, "required skill/shell check selected zero inputs")
    for contract in policy.get("required_markdown_documents", []):
        name = contract["path"]
        require(name in document_tokens, "required document absent from checked inventory")
        document_check(document_tokens[name], contract)
    for ref in policy.get("required_document_links", []):
        require(ref["from"] in files and ref["to"] in files,
                "required document link endpoint absent from checked inventory")
        require(ref["to"] in local_edges.get(ref["from"], set()),
                "required document routing link missing")
    for ref in policy["instruction_references"]:
        source = files[ref["from"]]
        inside(root, source.parent / ref["target"])
    workflow = args.workflow or root / policy["workflow"]
    workflow_check(workflow.resolve(strict=True), actionlint)
    provenance = provenance_check(root, all_files, policy)
    print(json.dumps({"scope": "setup structure only", "tracked_files": len(all_files),
                      "selected_setup_files": len(files),
                      "skills": skills, "local_links": links, "json": json_count,
                      "shells": shells, "workflow": str(workflow),
                      "instruction_discovery": "manual native observation required",
                      "provenance": provenance or "BLOCKED: skills owner manifest absent"}))
    return 0 if provenance else 3


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, KeyError, TypeError, yaml.YAMLError,
            subprocess.TimeoutExpired) as error:
        print(f"setup-check: {type(error).__name__}: {error}", file=sys.stderr)
        sys.exit(1)
