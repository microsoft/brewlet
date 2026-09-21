#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Apply publishing metadata to a tagged POM without replacing its build."""

import argparse
from pathlib import Path
from xml.dom import Node, minidom


NAMESPACE = "http://maven.apache.org/POM/4.0.0"


def children(parent, name):
    return [
        node for node in parent.childNodes
        if node.nodeType == Node.ELEMENT_NODE
        and node.namespaceURI == NAMESPACE
        and node.localName == name
    ]


def child(parent, name, required=True):
    matches = children(parent, name)
    if len(matches) > 1 or (required and not matches):
        raise ValueError(f"Expected one {name} element in {parent.nodeName}")
    return matches[0] if matches else None


def text(node):
    return "".join(
        part.data for part in node.childNodes if part.nodeType == Node.TEXT_NODE
    ).strip()


def read_pom(path):
    document = minidom.parse(str(path))
    project = document.documentElement
    if project.namespaceURI != NAMESPACE or project.localName != "project":
        raise ValueError(f"{path}: expected a Maven project")
    for name, expected in (
        ("groupId", "sh.brewlet"),
        ("artifactId", "brewlet-maven-plugin"),
        ("packaging", "maven-plugin"),
    ):
        if text(child(project, name)) != expected:
            raise ValueError(f"{path}: expected {name}={expected}")
    return document


def release_profile(project):
    profiles = child(project, "profiles", required=False)
    matches = [] if profiles is None else [
        profile for profile in children(profiles, "profile")
        if text(child(profile, "id")) == "central-release"
    ]
    if len(matches) > 1:
        raise ValueError("Multiple central-release profiles")
    return matches[0] if matches else None


def prepare(template_path, target_path):
    template = read_pom(template_path)
    target = read_pom(target_path)
    source = template.documentElement
    project = target.documentElement
    profile = release_profile(source)
    if profile is None:
        raise ValueError("Publishing template has no central-release profile")

    for name in ("developers", "scm"):
        metadata = child(source, name)
        if child(project, name, required=False) is None:
            project.appendChild(target.createTextNode("\n  "))
            project.appendChild(target.importNode(metadata, deep=True))

    existing = release_profile(project)
    replacement = target.importNode(profile, deep=True)
    if existing is not None:
        existing.parentNode.replaceChild(replacement, existing)
    else:
        profiles = child(project, "profiles", required=False)
        if profiles is None:
            profiles = target.createElementNS(NAMESPACE, "profiles")
            project.appendChild(target.createTextNode("\n  "))
            project.appendChild(profiles)
        profiles.appendChild(target.createTextNode("\n    "))
        profiles.appendChild(replacement)
        profiles.appendChild(target.createTextNode("\n  "))

    target_path.write_bytes(target.toxml(encoding="UTF-8"))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("template", type=Path)
    parser.add_argument("target", type=Path)
    args = parser.parse_args()
    prepare(args.template, args.target)
    print(f"Applied Central publishing metadata to {args.target}")
