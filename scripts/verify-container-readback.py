#!/usr/bin/env python3
"""Read an externally reviewed GHCR image back; never write to a registry."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile
import urllib.request

IMAGE = "ghcr.io/flint-pay/flint-cli"
REPOSITORY = "flint-pay/flint-cli"
INDEX = "application/vnd.oci.image.index.v1+json"
MANIFEST = "application/vnd.oci.image.manifest.v1+json"


def require(condition, message):
    if not condition:
        raise ValueError(message)


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def file_sha256(path):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def inputs(run_id, expected_hash, tag, source):
    require(re.fullmatch(r"[1-9][0-9]{0,19}", run_id), "Invalid artifact run ID")
    require(re.fullmatch(r"[a-f0-9]{64}", expected_hash), "External manifest SHA-256 required")
    require(re.fullmatch(r"[a-f0-9]{40}", source), "Invalid source SHA")
    version = re.fullmatch(r"cli/v((?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?)", tag)
    require(version is not None, "Invalid release tag")
    if "-" in version[1]:
        require(not any(re.fullmatch(r"0[0-9]+", part) for part in version[1].split("-", 1)[1].split(".")), "Invalid prerelease")
    return version[1]


def verify_origin(run_id, tag, source):
    # API bodies and errors are deliberately not logged: they are not evidence.
    token = os.environ["GITHUB_TOKEN"]

    def api(path):
        request = urllib.request.Request(
            f"https://api.github.com/repos/{REPOSITORY}/{path}",
            headers={"Authorization": f"Bearer {token}", "Accept": "application/vnd.github+json", "User-Agent": "flint-reviewed-readback"},
        )
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response)

    run = api(f"actions/runs/{run_id}/attempts/1")
    require(run["id"] == int(run_id) and run["run_attempt"] == 1, "Original run identity differs")
    require(run["repository"]["full_name"] == REPOSITORY, "Artifact repository differs")
    require(run["event"] == "push" and run["path"] == ".github/workflows/cli-release.yml", "Artifact producer differs")
    require(run["head_sha"] == source and run["head_branch"] == tag, "Artifact tag/source differs")
    jobs = []
    for page in range(1, 11):
        result = api(f"actions/runs/{run_id}/attempts/1/jobs?per_page=100&page={page}")
        jobs.extend(result["jobs"])
        if len(jobs) >= result["total_count"]:
            break
    else:
        raise ValueError("Original job inventory exceeds bound")
    producers = [job for job in jobs if job["name"] == "container-build"]
    require(len(producers) == 1 and producers[0]["conclusion"] == "success", "Original container producer did not succeed")
    # The container artifact must predate completion of the retained first attempt.
    artifacts = []
    for page in range(1, 11):
        result = api(f"actions/runs/{run_id}/artifacts?per_page=100&page={page}")
        artifacts.extend(result["artifacts"])
        if len(artifacts) >= result["total_count"]:
            break
    else:
        raise ValueError("Artifact inventory exceeds bound")
    original = [item for item in artifacts if item["name"] == "flint-cli-container" and not item["expired"]]
    require(len(original) == 1, "Original container artifact unavailable or ambiguous")
    require(producers[0]["started_at"] <= original[0]["created_at"] <= producers[0]["completed_at"], "Container artifact is not from prepared attempt 1")


def reviewed_oci(directory, expected_hash, tag, source):
    directory = Path(directory)
    manifest_path = directory / "candidate-release.json"
    require(manifest_path.is_file() and not manifest_path.is_symlink(), "Expected regular review manifest")
    data = manifest_path.read_bytes()
    require(sha256(data) == expected_hash, "External manifest hash differs")
    review = json.loads(data)
    require(review["format"] == 1 and review["tag"] == tag and review["commit"] == source, "Review tag/source differs")
    require(review["container"]["image"] == IMAGE, "Review image differs")
    digest = review["container"]["digest"]
    require(re.fullmatch(r"sha256:[a-f0-9]{64}", digest), "Invalid reviewed index digest")
    for name in ("container.oci.tar", "container-digest.txt"):
        path = directory / name
        require(path.is_file() and not path.is_symlink(), "Expected regular original OCI artifact")
        require(file_sha256(path) == review["files"][name], "Original OCI artifact hash differs")
    require((directory / "container-digest.txt").read_text().strip() == digest, "Original digest file differs")
    blobs = {}
    files = {}
    seen = set()
    # Read members, never extract archive paths onto the host.
    with tarfile.open(directory / "container.oci.tar", "r:") as archive:
        for member in archive:
            require(member.name not in seen, "Duplicate original OCI member")
            seen.add(member.name)
            if member.isdir():
                require(member.name in ("blobs", "blobs/sha256"), "Unexpected OCI directory")
                continue
            require(member.isfile(), "Nonregular original OCI member")
            if re.fullmatch(r"blobs/sha256/[a-f0-9]{64}", member.name):
                blobs[member.name.rsplit("/", 1)[1]] = archive.extractfile(member).read()
            else:
                require(member.name in ("index.json", "oci-layout"), "Unexpected original OCI member")
                files[member.name] = archive.extractfile(member).read()
    require(json.loads(files["oci-layout"]) == {"imageLayoutVersion": "1.0.0"}, "Invalid OCI layout")
    wrapper = json.loads(files["index.json"])
    require(len(wrapper["manifests"]) == 1 and wrapper["manifests"][0]["digest"] == digest, "Original OCI index differs")
    reachable = set()

    def visit(descriptor):
        key = descriptor["digest"]
        require(re.fullmatch(r"sha256:[a-f0-9]{64}", key), "Invalid OCI descriptor digest")
        key = key[7:]
        content = blobs[key]
        require(len(content) == descriptor["size"] and sha256(content) == key, "OCI descriptor bytes differ")
        if key in reachable:
            return
        reachable.add(key)
        if descriptor["mediaType"] in (INDEX, MANIFEST):
            document = json.loads(content)
            require(document["schemaVersion"] == 2, "Invalid OCI schema")
            children = document["manifests"] if descriptor["mediaType"] == INDEX else [document["config"], *document["layers"]]
            for child in children:
                visit(child)

    visit(wrapper["manifests"][0])
    require(reachable == set(blobs), "Missing or unreferenced original OCI blobs")
    raw_index = blobs[digest[7:]]
    index = json.loads(raw_index)
    images = [item for item in index["manifests"] if item.get("platform", {}).get("os") == "linux"]
    require(sorted(item["platform"]["architecture"] for item in images) == ["amd64", "arm64"], "Expected both reviewed architectures")
    attestations = [item for item in index["manifests"] if item.get("annotations", {}).get("vnd.docker.reference.type") == "attestation-manifest"]
    require(len(index["manifests"]) == 4 and len(attestations) == 2, "Expected both retained attestations")
    require(sorted(item["annotations"]["vnd.docker.reference.digest"] for item in attestations) == sorted(item["digest"] for item in images), "Attestation image binding differs")
    for item in attestations:
        statements = json.loads(blobs[item["digest"][7:]])["layers"]
        predicates = [json.loads(blobs[layer["digest"][7:]])["predicateType"] for layer in statements]
        require(sorted(predicates) == ["https://slsa.dev/provenance/v1", "https://spdx.dev/Document"], "Expected retained provenance and SBOM")
    return digest, raw_index, blobs


def compare_layout(layout, digest, raw_index, blobs):
    layout = Path(layout)
    for name in ("index.json", "oci-layout"):
        require((layout / name).is_file() and not (layout / name).is_symlink(), "Nonregular readback layout metadata")
    require(json.loads((layout / "oci-layout").read_bytes()) == {"imageLayoutVersion": "1.0.0"}, "Invalid readback layout")
    wrapper = json.loads((layout / "index.json").read_bytes())
    require(len(wrapper["manifests"]) == 1 and wrapper["manifests"][0]["digest"] == digest, "Readback index differs")
    require((layout / "blobs").is_dir() and not (layout / "blobs").is_symlink(), "Invalid readback blobs directory")
    directory = layout / "blobs/sha256"
    require(directory.is_dir() and not directory.is_symlink(), "Invalid readback digest directory")
    require({item.name for item in (layout / "blobs").iterdir()} == {"sha256"}, "Unexpected readback digest algorithm")
    require({item.name for item in directory.iterdir()} == set(blobs), "Readback blob inventory differs")
    for key, original in blobs.items():
        path = directory / key
        require(path.is_file() and not path.is_symlink(), "Nonregular readback blob")
        actual = path.read_bytes()
        require(sha256(actual) == key and actual == original, "Readback blob bytes differ")
    require((directory / digest[7:]).read_bytes() == raw_index, "Readback raw index bytes differ")


def skopeo(args, password=None):
    # No subprocess stderr/stdout is retained on failure, and the token is never
    # placed on argv or passed through the subprocess environment.
    environment = {key: value for key, value in os.environ.items() if key != "GITHUB_TOKEN"}
    result = subprocess.run(["skopeo", *args], input=password, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=environment, timeout=300)
    require(result.returncode == 0, f"Skopeo {args[0]} failed; registry state is unverified")
    return result.stdout


def verify(args):
    version = inputs(args.run_id, args.manifest_sha256, args.tag, args.source)
    digest, raw_index, blobs = reviewed_oci(args.directory, args.manifest_sha256, args.tag, args.source)
    with tempfile.TemporaryDirectory(prefix="flint-ghcr-readback-") as scratch:
        authfile = str(Path(scratch) / "auth.json")
        skopeo(["login", "--authfile", authfile, "--username", os.environ["GITHUB_ACTOR"], "--password-stdin", "ghcr.io"], os.environ["GITHUB_TOKEN"].encode())
        tag_ref = f"docker://{IMAGE}:v{version}"
        bound_ref = f"docker://{IMAGE}@{digest}"

        def inspect(reference):
            actual = skopeo(["inspect", "--raw", "--authfile", authfile, reference])
            require(actual == raw_index and "sha256:" + sha256(actual) == digest, "Published raw index differs")

        inspect(tag_ref)
        inspect(bound_ref)
        layout = str(Path(scratch) / "readback")
        # The destination transport is fixed to a fresh local OCI layout.
        skopeo(["copy", "--all", "--preserve-digests", "--authfile", authfile, bound_ref, f"oci:{layout}:readback"])
        compare_layout(layout, digest, raw_index, blobs)
        inspect(tag_ref)
    evidence = {
        "manifest_sha256": args.manifest_sha256,
        "oci_archive_sha256": file_sha256(Path(args.directory) / "container.oci.tar"),
        "index_digest": digest,
        "index_bytes": len(raw_index),
        "equal_blob_count": len(blobs),
        "image_count": 2,
        "attestation_count": 2,
        "provenance_count": 2,
        "sbom_count": 2,
        "source": args.source,
        "tag": args.tag,
        "artifact_run_id": args.run_id,
        "artifact_attempt": 1,
        "verification_run_id": os.environ["GITHUB_RUN_ID"],
        "verification_attempt": os.environ["GITHUB_RUN_ATTEMPT"],
    }
    Path(args.evidence).write_text(json.dumps(evidence, indent=2) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("origin", "verify"))
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--manifest-sha256", required=True)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--source", required=True)
    parser.add_argument("--directory")
    parser.add_argument("--evidence")
    args = parser.parse_args()
    try:
        inputs(args.run_id, args.manifest_sha256, args.tag, args.source)
        if args.mode == "origin":
            verify_origin(args.run_id, args.tag, args.source)
        else:
            require(args.directory and args.evidence, "Original artifact directory and evidence path required")
            verify(args)
    except Exception:
        # Do not expose request headers, tokens, registry errors, or raw content.
        parser.exit(1, "Reviewed container readback failed; no registry-byte verification is claimed.\n")


if __name__ == "__main__":
    main()
