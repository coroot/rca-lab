#!/usr/bin/env python3
"""Validates that every rca-lab image referenced by a FailureScenario resolves
to a real, tracked tag — so scenarios can't drift from what CI actually builds.

Sources of truth:
  - versions.yaml         base image tags (one per service)
  - variants/registry.yaml  bad-deploy variant image tags (generated from the
                            services/*/variants/*/variant.yaml files)

A scenario image ghcr.io/coroot/rca-lab/<svc>:<tag> is valid when <tag> is
either that service's base tag in versions.yaml or a variant tag in the
registry. Exits non-zero (and lists the offenders) on any mismatch.
"""
import glob
import re
import sys

import yaml

PREFIX = "ghcr.io/coroot/rca-lab/"

versions = yaml.safe_load(open("versions.yaml"))
base = {name: str(tag) for name, tag in versions.get("services", {}).items()}

variant_images = set()
try:
    reg = yaml.safe_load(open("variants/registry.yaml")) or {}
    for v in reg.get("variants", []):
        variant_images.add(v["image"])
except FileNotFoundError:
    pass

# Collect every rca-lab image referenced anywhere in the scenario library.
refs = []  # (file, image)
for path in glob.glob("scenarios/**/*.yaml", recursive=True):
    text = open(path).read()
    for m in re.finditer(rf"{re.escape(PREFIX)}[\w.-]+:[\w.-]+", text):
        refs.append((path, m.group(0)))

errors = []
for path, image in refs:
    name_tag = image[len(PREFIX):]
    svc, _, tag = name_tag.partition(":")
    if image in variant_images:
        continue
    if svc in base and tag == base[svc]:
        continue
    if svc not in base:
        errors.append(f"{path}: unknown service '{svc}' in {image}")
    else:
        errors.append(
            f"{path}: {image} does not match base tag "
            f"'{base[svc]}' (versions.yaml) or any variant in variants/registry.yaml"
        )

if errors:
    print("Scenario image validation FAILED:")
    for e in errors:
        print("  -", e)
    sys.exit(1)
print(f"Scenario image validation OK ({len(refs)} references checked)")
