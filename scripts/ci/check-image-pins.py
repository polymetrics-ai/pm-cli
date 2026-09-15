#!/usr/bin/env python3
"""Verify selected Docker Hub index bytes; never fetch layers or run containers."""
import hashlib
import json
from pathlib import Path
import re
import urllib.parse
import urllib.request


ROOT = Path(__file__).resolve().parents[2]
MEDIA_TYPES = {
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
}


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError("unexpected registry redirect")


def unique_pairs(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


opener = urllib.request.build_opener(NoRedirect())


def fetch(url, headers=None):
    with opener.open(urllib.request.Request(url, headers=headers or {}), timeout=30) as response:
        body = response.read(2 * 1024 * 1024 + 1)
        if len(body) > 2 * 1024 * 1024:
            raise ValueError("registry response exceeds 2MiB")
        return body, response.headers


lock = json.loads((ROOT / "build/toolchains.lock.json").read_text(), object_pairs_hook=unique_pairs)
images = lock["images"]
if not isinstance(images, list) or not 1 <= len(images) <= 32:
    raise ValueError("missing or unbounded selected image inventory")
for image in images:
    name = image["name"].removeprefix("docker.io/")
    if "/" not in name:
        name = "library/" + name
    if not re.fullmatch(r"[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+", name):
        raise ValueError("unsupported Docker Hub repository reference")
    expected = image["index_digest"]
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", expected):
        raise ValueError("invalid pinned index digest")
    query = urllib.parse.urlencode({"service": "registry.docker.io", "scope": f"repository:{name}:pull"})
    token_bytes, _ = fetch("https://auth.docker.io/token?" + query)
    token = json.loads(token_bytes, object_pairs_hook=unique_pairs)["token"]
    body, headers = fetch(f"https://registry-1.docker.io/v2/{name}/manifests/{expected}",
                          {"Authorization": "Bearer " + token, "Accept": ", ".join(sorted(MEDIA_TYPES))})
    actual = "sha256:" + hashlib.sha256(body).hexdigest()
    if actual != expected or headers.get("Docker-Content-Digest") != actual:
        raise ValueError(f"image index digest mismatch: {name}")
    index = json.loads(body, object_pairs_hook=unique_pairs)
    if index.get("mediaType") not in MEDIA_TYPES or not index.get("manifests"):
        raise ValueError(f"not a populated image index: {name}")
    platforms = sorted({f"{row['platform']['os']}/{row['platform']['architecture']}"
                        for row in index["manifests"] if "platform" in row})
    if "linux/amd64" not in platforms:
        raise ValueError(f"selected index lacks the current Linux/amd64 target: {name}")
    print(json.dumps({"image": name, "index_digest": actual, "platforms": platforms}))
print("Image identity PASS: manifest bytes only; no layers, containers or stack qualification.")
