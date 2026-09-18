#!/usr/bin/env python3
"""Derive a bounded, deterministic operation-closure capture for a connector lock.

A whole-provider OpenAPI document can be far larger than the admitted compiler
input bound (internal/compile budgets and the 16 MiB contract document bound),
and internal/compile discovery currently enumerates every operation of a
captured document. This step therefore extracts exactly one declared operation
plus its complete transitive reference closure into a small, valid OpenAPI root
document that the pinned compiler can consume.

Determinism: the derivation is a pure function of the verified upstream bytes
and the source lock's declared operation pointer. Object members keep their
upstream order, strings are emitted as raw UTF-8 with only the mandatory JSON
escapes, numbers keep their exact source text, and the referenced components
are ordered by kind then name. The command never rewrites the upstream capture
and never invents a source fact: every copied value is byte-for-byte the value
that the verified upstream document contains at that pointer.

Usage:
  connector_closure_capture.py --source-lock PATH --source SOURCE_ID=PATH
      [--output-root DIR] [--expect-sha256 HEX] [--expect-byte-length N]
      [--check]

The derived capture is written to
<output-root>/<connector-dir>/captures/<source_id> unless --check is given, in
which case the existing file is compared instead. Success prints the source
identity, the derived byte length and the derived SHA-256, which are the values
the source lock must record for the compiler input.
"""

from __future__ import annotations

import argparse
import decimal
import hashlib
import json
import os
import sys

MAXIMUM_DOCUMENT_BYTES = 16 << 20
CAPTURE_ROOT_MEMBERS = ("openapi", "info", "servers")
METHOD_NAMES = ("get", "put", "post", "delete", "options", "head", "patch", "trace")


class DerivationError(Exception):
    """Raised when the declared closure cannot be derived safely."""


def decode_pointer(pointer: str) -> list[str]:
    if pointer == "":
        return []
    if not pointer.startswith("/"):
        raise DerivationError("JSON pointer is malformed: %r" % pointer)
    tokens = []
    for raw in pointer[1:].split("/"):
        token = raw.replace("~1", "/").replace("~0", "~")
        if "~" in token:
            raise DerivationError("JSON pointer escape is malformed: %r" % pointer)
        tokens.append(token)
    return tokens


def resolve_pointer(document, pointer: str):
    current = document
    for token in decode_pointer(pointer):
        if isinstance(current, list):
            if not token.isdigit() or (len(token) > 1 and token[0] == "0"):
                raise DerivationError("JSON pointer %r does not address an array member" % pointer)
            index = int(token)
            if index >= len(current):
                raise DerivationError("JSON pointer %r is outside the captured document" % pointer)
            current = current[index]
        elif isinstance(current, dict):
            if token not in current:
                raise DerivationError("JSON pointer %r is absent from the captured document" % pointer)
            current = current[token]
        else:
            raise DerivationError("JSON pointer %r crosses a scalar value" % pointer)
    return current


def parse_json_ordered(raw: bytes):
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise DerivationError("captured document is not valid UTF-8: %s" % exc) from exc
    return json.loads(text, parse_float=decimal.Decimal, parse_int=int)


def collect_references(value, into: set[str]) -> None:
    if isinstance(value, dict):
        reference = value.get("$ref")
        if isinstance(reference, str):
            if not reference.startswith("#/"):
                raise DerivationError(
                    "declared reference %r is outside the captured closure; external references are not admitted" % reference
                )
            into.add(reference)
            return
        for member in value.values():
            collect_references(member, into)
    elif isinstance(value, list):
        for member in value:
            collect_references(member, into)


def component_target(reference: str) -> tuple[str, str]:
    tokens = decode_pointer(reference[1:])
    if len(tokens) != 3 or tokens[0] != "components":
        raise DerivationError(
            "reference %r is not a component reference that the closure capture can carry" % reference
        )
    return tokens[1], tokens[2]


def derive_closure(document, pointer: str):
    """Return the derived root document for one operation pointer."""
    if pointer.startswith("#"):
        pointer = pointer[1:]
    tokens = decode_pointer(pointer)
    if len(tokens) != 3 or tokens[0] != "paths" or tokens[2] not in METHOD_NAMES:
        raise DerivationError("source lock pointer %r is not an operation pointer" % pointer)
    path, method = tokens[1], tokens[2]
    paths = document.get("paths")
    if not isinstance(paths, dict) or path not in paths:
        raise DerivationError("declared operation path %r is absent from the captured document" % path)
    path_item = paths[path]
    if not isinstance(path_item, dict) or method not in path_item:
        raise DerivationError("declared operation %s %s is absent from the captured document" % (method.upper(), path))

    derived_path_item = {method: path_item[method]}
    for carried in ("parameters", "servers"):
        if carried in path_item:
            derived_path_item[carried] = path_item[carried]

    derived = {}
    for member in CAPTURE_ROOT_MEMBERS:
        if member in document:
            derived[member] = document[member]
    derived["paths"] = {path: derived_path_item}

    pending: set[str] = set()
    collect_references(derived_path_item, pending)
    collected: dict[str, dict[str, object]] = {}
    seen: set[str] = set()
    while pending:
        reference = sorted(pending)[0]
        pending.discard(reference)
        if reference in seen:
            continue
        seen.add(reference)
        kind, name = component_target(reference)
        target = resolve_pointer(document, reference[1:])
        collected.setdefault(kind, {})[name] = target
        collect_references(target, pending)

    components: dict[str, dict[str, object]] = {}
    for kind in sorted(collected):
        components[kind] = {name: collected[kind][name] for name in sorted(collected[kind])}
    if components:
        derived["components"] = components
    return derived


def encode_json(value) -> str:
    parts: list[str] = []
    _encode(value, 0, parts)
    parts.append("\n")
    return "".join(parts)


def _encode(value, depth: int, parts: list[str]) -> None:
    indent = "  " * depth
    nested_indent = "  " * (depth + 1)
    if isinstance(value, dict):
        if not value:
            parts.append("{}")
            return
        parts.append("{\n")
        for index, (key, member) in enumerate(value.items()):
            parts.append(nested_indent if index == 0 else ",\n" + nested_indent)
            parts.append(json.dumps(key, ensure_ascii=False) + ": ")
            _encode(member, depth + 1, parts)
        parts.append("\n" + indent + "}")
        return
    if isinstance(value, list):
        if not value:
            parts.append("[]")
            return
        parts.append("[\n")
        for index, member in enumerate(value):
            parts.append(nested_indent if index == 0 else ",\n" + nested_indent)
            _encode(member, depth + 1, parts)
        parts.append("\n" + indent + "]")
        return
    if isinstance(value, str):
        parts.append(json.dumps(value, ensure_ascii=False))
        return
    if value is True:
        parts.append("true")
        return
    if value is False:
        parts.append("false")
        return
    if value is None:
        parts.append("null")
        return
    if isinstance(value, int):
        parts.append(str(value))
        return
    if isinstance(value, decimal.Decimal):
        parts.append(format(value, "f"))
        return
    raise DerivationError("captured document contains a value that cannot be emitted exactly")


def read_regular_file(path: str) -> bytes:
    target = os.path.realpath(path)
    if os.path.islink(path):
        raise DerivationError("capture path must not be a symlink: %s" % path)
    if not os.path.isfile(target):
        raise DerivationError("capture path is not a regular file: %s" % path)
    with open(target, "rb") as handle:
        data = handle.read(MAXIMUM_DOCUMENT_BYTES + 1)
    if len(data) > MAXIMUM_DOCUMENT_BYTES:
        raise DerivationError("captured document exceeds the %d-byte bound" % MAXIMUM_DOCUMENT_BYTES)
    return data


def write_atomic(path: str, document: bytes) -> None:
    directory = os.path.dirname(path) or "."
    os.makedirs(directory, exist_ok=True)
    temporary = os.path.join(directory, "." + os.path.basename(path) + ".tmp")
    with open(temporary, "wb") as handle:
        handle.write(document)
        handle.flush()
        os.fsync(handle.fileno())
    os.chmod(temporary, 0o644)
    os.replace(temporary, path)


def parse_source(value: str) -> tuple[str, str]:
    if "=" not in value:
        raise DerivationError("--source must be SOURCE_ID=PATH")
    source_id, path = value.split("=", 1)
    if not source_id or not path:
        raise DerivationError("--source must be SOURCE_ID=PATH")
    return source_id, path


def run(arguments: list[str], stdout, stderr) -> int:
    parser = argparse.ArgumentParser(prog="connector-closure-capture", add_help=True)
    parser.add_argument("--source-lock", required=True)
    parser.add_argument("--source", required=True, action="append", default=[])
    parser.add_argument("--output-root", default=".")
    parser.add_argument("--expect-sha256", default="")
    parser.add_argument("--expect-byte-length", type=int, default=-1)
    parser.add_argument("--check", action="store_true")
    parsed = parser.parse_args(arguments)

    try:
        lock = json.loads(read_regular_file(parsed.source_lock).decode("utf-8"))
        connector_id = lock["connector_id"]
        if not isinstance(connector_id, str) or not connector_id:
            raise DerivationError("source lock connector identity is missing")
        sources = dict(parse_source(value) for value in parsed.source)
        documents = lock["source_set"]["documents"]
    except (DerivationError, KeyError, ValueError) as exc:
        print("connector-closure-capture: %s" % exc, file=stderr)
        return 1

    failures = 0
    for document in documents:
        source_id = document.get("source_id")
        pointer = document.get("pointer", "")
        if not source_id or source_id not in sources:
            continue
        if not pointer:
            raise DerivationError("source %s declares no operation pointer to capture" % source_id)
        try:
            raw = read_regular_file(sources[source_id])
        except DerivationError as exc:
            print("connector-closure-capture: %s" % exc, file=stderr)
            return 1
        digest = hashlib.sha256(raw).hexdigest()
        if parsed.expect_sha256 and digest != parsed.expect_sha256:
            print(
                "connector-closure-capture: upstream capture %s SHA-256 %s does not match the expected %s"
                % (source_id, digest, parsed.expect_sha256),
                file=stderr,
            )
            return 1
        if parsed.expect_byte_length >= 0 and len(raw) != parsed.expect_byte_length:
            print(
                "connector-closure-capture: upstream capture %s is %d bytes, expected %d"
                % (source_id, len(raw), parsed.expect_byte_length),
                file=stderr,
            )
            return 1
        try:
            derived = encode_json(derive_closure(parse_json_ordered(raw), pointer)).encode("utf-8")
        except DerivationError as exc:
            print("connector-closure-capture: %s: %s" % (source_id, exc), file=stderr)
            return 1
        if len(derived) > MAXIMUM_DOCUMENT_BYTES:
            print(
                "connector-closure-capture: derived capture %s exceeds the %d-byte compiler bound"
                % (source_id, MAXIMUM_DOCUMENT_BYTES),
                file=stderr,
            )
            return 1

        destination = os.path.join(parsed.output_root, "connectors", connector_id, "captures", source_id)
        derived_digest = hashlib.sha256(derived).hexdigest()
        if parsed.check:
            try:
                existing = read_regular_file(destination)
            except DerivationError as exc:
                print("connector-closure-capture: %s" % exc, file=stderr)
                return 1
            if existing != derived:
                print(
                    "connector-closure-capture: capture %s does not match its deterministic derivation" % destination,
                    file=stderr,
                )
                failures += 1
                continue
        else:
            write_atomic(destination, derived)
        print("%s %d %s %s" % (source_id, len(derived), derived_digest, destination))
    if failures:
        return 1
    if not parsed.check:
        print("upstream %d %s" % (len(raw), hashlib.sha256(raw).hexdigest()))
    return 0


def main() -> int:
    return run(sys.argv[1:], sys.stdout, sys.stderr)


if __name__ == "__main__":
    sys.exit(main())
