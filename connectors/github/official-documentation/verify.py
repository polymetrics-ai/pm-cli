#!/usr/bin/env python3
"""Verify the retained bytes and every quote in manifest.json.

Independent of the tool that authored the manifest: this script re-reads the
retained documents, re-checks their SHA-256 and byte length against the manifest,
re-derives each cited section from the retained bytes, and re-extracts each quote
from the recorded byte range using the normalization the manifest documents.

Exit status 0 means every retained byte and every citation is accounted for.
Any digest mismatch, out-of-section locator, or quote that does not match its
locator is a blocker, never a substitution.

Usage: python3 verify.py [--quiet]
"""
import hashlib
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
MANIFEST = os.path.join(HERE, "manifest.json")
TITLED = '<h1 id="title-h1"'
BLOCK_END = re.compile(r"</(?:td|th|tr|li|p|pre|div|h[1-4]|ul|ol|table|tbody|thead)\s*>")
ANY_TAG = re.compile(r"<[^>]+>")
HEADING = re.compile(r'<h([1-4])(?:\s[^>]*?)?id="([^"]+)"')
KIND_START = {
    "prose": "<p",
    "listitem": "<li",
    "preformatted": "<pre",
    "code": "<code",
    "row": "<tr",
    "heading2": "<h2",
    "heading3": "<h3",
}
ENTITIES = (("&quot;", '"'), ("&lt;", "<"), ("&gt;", ">"), ("&amp;", "&"),
            ("&#x27;", "'"), ("&#39;", "'"), ("&nbsp;", " "))

failures = []
checks = 0


def check(condition, message):
    global checks
    checks += 1
    if not condition:
        failures.append(message)
    return condition


def render(fragment):
    text = BLOCK_END.sub(" ", fragment)
    text = ANY_TAG.sub("", text)
    for entity, char in ENTITIES:
        text = text.replace(entity, char)
    text = text.replace("\u00a0", " ")
    return re.sub(r"\s+", " ", text).strip()


def section_range(data, anchor):
    base = data.find(TITLED)
    if base < 0:
        return None
    region = data[base:]
    heads = list(HEADING.finditer(region))
    for index, match in enumerate(heads):
        if match.group(2) != anchor:
            continue
        level = int(match.group(1))
        end = len(region)
        for later in heads[index + 1:]:
            if int(later.group(1)) <= level:
                end = later.start()
                break
        return base + match.start(), base + end
    return None


def read_locator_file(entry):
    return open(os.path.join(HERE, entry["locator"]["file"]), "rb").read()


def verify_document(doc, documents_by_id, verbose):
    path = os.path.join(HERE, "documents", os.path.basename(doc["retained_path"]))
    if not check(os.path.exists(path), f"{doc['document_id']}: retained file missing at {path}"):
        return None
    raw = open(path, "rb").read()
    digest = hashlib.sha256(raw).hexdigest()
    check(digest == doc["retained_sha256"],
          f"{doc['document_id']}: sha256 mismatch: manifest {doc['retained_sha256']} file {digest}")
    check(len(raw) == doc["retained_byte_length"],
          f"{doc['document_id']}: byte length mismatch: manifest {doc['retained_byte_length']} file {len(raw)}")
    if doc["content_length_header"] is not None:
        check(doc["content_length_header"] == doc["retained_byte_length"],
              f"{doc['document_id']}: content-length header {doc['content_length_header']} != retained bytes {doc['retained_byte_length']}")
    if verbose and doc["content_length_header"] is None:
        print(f"    (no content-length header on this response; byte length taken from the retained file)")
    if verbose:
        print(f"  {doc['document_id']}: {len(raw)} bytes sha256 {digest}")
    for entry in doc["retrieval_evidence"]:
        verify_exact(entry)
    return raw


def verify_exact(entry, raw=None):
    start, end = entry["locator"]["byte_range"]
    raw = raw if raw is not None else read_locator_file(entry)
    actual = raw[start:end]
    check(actual.decode("utf-8", "replace") == entry["quote"],
          f"{entry['document_id']}: exact-bytes quote mismatch at {entry['locator']['file']} "
          f"{entry['locator']['byte_range']}: manifest {entry['quote']!r} file {actual[:120]!r}")


def verify_element(entry, documents_by_id):
    locator = entry["locator"]
    document_id = entry["document_id"]
    if document_id not in documents_by_id:
        check(False, f"{document_id}: cited document is not declared in the manifest")
        return
    check(documents_by_id[document_id]["retained_path"].endswith(locator["file"]),
          f"{document_id}: locator file {locator['file']} is not the retained path "
          f"{documents_by_id[document_id]['retained_path']}")
    data = read_locator_file(entry).decode("utf-8", "replace")
    anchor = locator["anchor"].lstrip("#")
    bounds = section_range(data, anchor)
    if not check(bounds is not None, f"{document_id}: cited anchor #{anchor} does not exist in the retained bytes"):
        return
    section_start, section_end = bounds
    check(list(bounds) == locator["section_byte_range"],
          f"{document_id} #{anchor}: section byte range drift: manifest {locator['section_byte_range']} actual {list(bounds)}")
    start, end = locator["byte_range"]
    check(section_start <= start < end <= section_end,
          f"{document_id} #{anchor}: element range {locator['byte_range']} is outside its section {list(bounds)}")
    check(data[start:end].startswith(KIND_START[locator["element"]]),
          f"{document_id} #{anchor}: element at {start} is not a {locator['element']} element")
    rendered = render(data[start:end])
    check(rendered == entry["quote"],
          f"{document_id} #{anchor} @{start}: quote does not match the retained bytes:\n"
          f"      manifest {entry['quote'][:200]!r}\n      extracted {rendered[:200]!r}")


def main():
    verbose = "--quiet" not in sys.argv
    manifest = json.load(open(MANIFEST, encoding="utf-8"))
    identity_1 = manifest["identities"]["identity_1_original_provider_bytes"]
    identity_2 = manifest["identities"]["identity_2_authored_normalized_facts"]

    documents_by_id = {doc["document_id"]: doc for doc in identity_1["documents"]}
    blobs = {}
    print("retained documents")
    for doc in identity_1["documents"]:
        blobs[doc["document_id"]] = verify_document(doc, documents_by_id, verbose)

    print("facts")
    for fact in identity_2["facts"]:
        check(bool(fact.get("evidence")), f"{fact['fact_id']}: no evidence records")
        check(bool(fact.get("authored_statement")), f"{fact['fact_id']}: no authored statement")
        check(bool(fact.get("must_not_be_read_as")), f"{fact['fact_id']}: no must_not_be_read_as boundary")
        for entry in fact["evidence"]:
            if entry["locator"]["element"] == "exact_bytes":
                verify_exact(entry)
            else:
                verify_element(entry, documents_by_id)
        if verbose:
            print(f"  {fact['fact_id']} {fact['name']}: {len(fact['evidence'])} citations verified")

    print("license, pin and version-selector evidence")
    license_block = manifest["license_and_attribution"]
    for entry in license_block["license_evidence"]:
        verify_exact(entry)
    for entry in license_block["attribution_source_pin"]["resolution_evidence"]:
        verify_exact(entry)
    for entry in manifest["retained_page_api_version_selector"]:
        verify_exact(entry)
    if verbose:
        print(f"  {len(license_block['license_evidence'])} license citations, "
              f"{len(license_block['attribution_source_pin']['resolution_evidence'])} pin citations, "
              f"{len(manifest['retained_page_api_version_selector'])} version-selector citations verified")

    print("documented absences")
    for absence in identity_2["documented_absences"]:
        item = absence["check"]
        if item["kind"] == "occurrence_count_in_retained_bytes":
            text = open(os.path.join(HERE, item["file"]), "rb").read().decode("utf-8", "replace").lower()
            for needle in item["needles"]:
                found = len(re.findall(re.escape(needle.lower()), text))
                check(found == item["occurrences"][needle],
                      f"{absence['absence_id']}: {item['document_id']} contains {found} occurrences of "
                      f"{needle!r}, manifest says {item['occurrences'][needle]}")
        elif item["kind"] == "header_absence":
            for document in item["documents"]:
                text = open(os.path.join(HERE, document["file"]), "rb").read().decode("utf-8", "replace").lower()
                for name in item["header_names"]:
                    found = len(re.findall(rf"(?m)^{re.escape(name)}:", text))
                    expected = item["occurrences"][document["document_id"]][name]
                    check(found == expected,
                          f"{absence['absence_id']}: {document['document_id']} header {name!r} appears {found} times, "
                          f"manifest says {expected}")
        if verbose:
            print(f"  {absence['absence_id']} verified")

    check(manifest["identities"]["identity_3_generated_output"]["exists"] is False,
          "generated output identity claims to exist; this capture must not declare generated artifacts")

    print()
    if failures:
        print(f"FAIL: {len(failures)} of {checks} checks failed")
        for failure in failures:
            print(f"  - {failure}")
        return 1
    print(f"OK: {checks} checks passed; "
          f"{len(identity_1['documents'])} documents, "
          f"{sum(len(f['evidence']) for f in identity_2['facts'])} citations, "
          f"{len(identity_2['documented_absences'])} absences, "
          f"{len(identity_2['unverified_items'])} unverified items")
    return 0


if __name__ == "__main__":
    sys.exit(main())
