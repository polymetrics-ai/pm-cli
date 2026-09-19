#!/bin/sh
# Retrieve or re-verify the official GitHub documentation retained in this directory.
#
#   ./retrieve.sh            offline check: re-verify every retained byte and every
#                            manifest quote against the files in this directory.
#   ./retrieve.sh --refetch  fetch every documented URL again into a temporary
#                            directory with the same request shape and compare the
#                            SHA-256 of each response with manifest.json. The
#                            retained documents are never overwritten; a difference
#                            is reported and the script exits non-zero.
#
# Needs curl and python3. No credentials are used: every URL is public.
set -eu

HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

if [ "${1:---check}" = "--check" ]; then
    exec python3 "$HERE/verify.py"
fi

if [ "${1}" != "--refetch" ]; then
    echo "usage: $0 [--check|--refetch]" >&2
    exit 2
fi

exec python3 - "$HERE" <<'PY'
import hashlib
import json
import os
import subprocess
import sys
import tempfile

here = sys.argv[1]
manifest = json.load(open(os.path.join(here, "manifest.json"), encoding="utf-8"))
documents = manifest["identities"]["identity_1_original_provider_bytes"]["documents"]
scratch = tempfile.mkdtemp(prefix="pm-official-documentation-refetch.")
print(f"refetching {len(documents)} documented URLs into {scratch}\n")

failed = 0
for doc in documents:
    name = os.path.basename(doc["retained_path"])
    headers = os.path.join(scratch, name + ".headers")
    body = os.path.join(scratch, name)
    result = subprocess.run(
        ["curl", "-sS", "--location", "--max-time", "60",
         "--user-agent", "pm-cli-connector-evidence/1.0",
         "-D", headers, "-o", body, "-w", "%{http_code} %{content_type}",
         doc["requested_url"]],
        capture_output=True, text=True, check=False)
    if result.returncode != 0:
        print(f"DIFF {name}: curl failed: {result.stderr.strip()}")
        failed += 1
        continue
    with open(body, "rb") as handle:
        raw = handle.read()
    digest = hashlib.sha256(raw).hexdigest()
    status, _, content_type = result.stdout.strip().partition(" ")
    same_bytes = digest == doc["retained_sha256"]
    same_status = status == str(doc["http_status"])
    same_type = content_type == doc["content_type"]
    verdict = "MATCH" if (same_bytes and same_status and same_type) else "DIFF "
    if verdict.strip() == "DIFF":
        failed += 1
    print(f"{verdict} {name}: status {status} (manifest {doc['http_status']}), "
          f"content-type {content_type!r} (manifest {doc['content_type']!r})")
    print(f"      bytes {len(raw)} (manifest {doc['retained_byte_length']}), "
          f"sha256 {digest}")
    print(f"      manifest sha256 {doc['retained_sha256']}")
    if not same_bytes:
        print("      the provider bytes changed since retrieval: the retained copy stays authoritative, "
              "and a re-admission review is required before any fact is reused")
    new_headers = open(headers, encoding="utf-8", errors="replace").read()
    for line in new_headers.splitlines():
        if line.lower().startswith(("etag:", "last-modified:")):
            print(f"      response header this time: {line}")

print()
if failed:
    print(f"{failed} of {len(documents)} documents differ from the manifest; retained bytes were not modified")
    sys.exit(1)
print(f"all {len(documents)} documented URLs reproduce the retained bytes exactly")
PY
