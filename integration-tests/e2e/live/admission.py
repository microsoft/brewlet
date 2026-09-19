#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Scenario A: native Brewlet evidence through live Ratify/Gatekeeper admission.

Run from any directory with Python 3. See the live runbook for prerequisites.
Every assertion is mandatory; infrastructure or inconclusive denials fail.
"""

import base64
import contextlib
import copy
import hashlib
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tarfile
import urllib.error
import urllib.request

from common import Fixture, OWNER_LABEL, ROOT, redact, run, wait
from admission_helpers import (
    ARTIFACT, BUILDER, VERIFIER, Registry, assert_admission, assert_candidates,
    assert_discoverable, assert_rejection_reasons, digest, encoded, extract_result, signed_envelope,
    variant_statement,
)
from admission_registry import native_registry


HERE = Path(__file__).resolve().parent
RATIFY_COMMIT = "f5fd56fe58ba0a604eca247276acb7899e026435"
RATIFY_IMAGE = ("ghcr.io/notaryproject/ratify:v1.4.5@sha256:"
                "6607d1f84bf314dcaea51c6b7f4cbe30dfc85cdd8feb8fa40094e0bdb067b43c")
GATEKEEPER_COMMIT = "5be06a95665624a619a8082677dcf942043bf514"
GATEKEEPER_IMAGE = ("openpolicyagent/gatekeeper:v3.18.3@sha256:"
                    "20e9c73472d39644de0fa5941a06894ae79dab41359996c4e0e352b6fb1a62cd")
RELEASE_SOURCE = "f0b9334f7b29177d2ba4b49b044163ef69e16af7"
PLUGIN_GOAL = "sh.brewlet:brewlet-maven-plugin:0.5.0:"
REPOSITORY = "apps/admission"
CONTENT_CACHE = "/admission-no-content-cache"
REJECTION_REASONS = {
    "wrong-key": "DSSE key ID is not trusted",
    "obsolete": "DSSE key ID is not trusted",
    "wrong-builder": "identity does not match",
    "wrong-subject": "in-toto subject digest mismatch",
    "malformed": "decode DSSE envelope",
    "tampered": "DSSE signature verification failed",
    "incomplete": "applicationJarDigest is not a sha256 digest",
}


def download(url, destination):
    with urllib.request.urlopen(url, timeout=90) as response:
        destination.write_bytes(response.read())
    return hashlib.sha256(destination.read_bytes()).hexdigest()


class Admission:
    def __init__(self, fixture):
        self.f = fixture
        self.registry = Registry(fixture)
        self.expected = {}
        self.reasons = {}
        self.subjects = {}
        self.port = None
        self.ca = None
        self.successful = set()

    def command(self, name, argv, **kwargs):
        result = self.f.run(argv, check=False, **kwargs)
        self.f.save(name + ".log", result.stdout + result.stderr)
        if result.returncode:
            raise RuntimeError(f"{name} failed ({result.returncode}); see {self.f.work}")
        return result

    def kube_json(self, *argv):
        return json.loads(self.f.kube(*argv).stdout)

    def manifest(self, filename):
        # kubectl's own YAML decoder avoids a second Python package dependency.
        result = self.f.kube("create", "--dry-run=client", "--validate=false",
                             "-f", str(self.f.source / "admission/deploy" / filename),
                             "-o", "json")
        return json.loads(result.stdout)

    def setup_registry_route(self):
        self.registry_config, self.registry_credentials, self.registry_storage = native_registry(self.f)
        self.f.own_container(self.f.node)
        node_id = self.f.node_id
        self.f.own_container(self.f.registry_id)
        internal = self.f.registry_internal
        path = "/etc/containerd/certs.d/" + internal
        host = f'server = "http://{internal}"\n[host."http://{internal}"]\n  capabilities = ["pull", "resolve"]\n'
        self.f.run(["docker", "exec", node_id, "mkdir", "-p", path])
        self.f.run(["docker", "exec", "-i", node_id, "tee", path + "/hosts.toml"], input=host)
        address = self.f.registry_ip
        self.registry_ip = address
        self.f.record("admission-fixture-network", {
            "registry": self.f.registry, "registryInternal": internal,
            "registryIP": address, "plainHTTP": "only the invocation-owned registry",
            "containerdHosts": path, "tls": "fixture CA verified; no insecure TLS mode",
        })

    def keys(self):
        self.current = self.f.private / "admission-current.pem"
        self.obsolete = self.f.private / "admission-obsolete.pem"
        self.public = self.f.private / "admission-current.pub"
        for key in [self.current, self.obsolete]:
            run(["openssl", "genpkey", "-algorithm", "EC", "-pkeyopt",
                 "ec_paramgen_curve:P-256", "-out", str(key)])
            key.chmod(0o600)
        run(["openssl", "pkey", "-in", str(self.current), "-pubout",
             "-out", str(self.public)])

    def publish(self):
        project = self.f.private / "admission-project"
        project.mkdir()
        fixtures = ROOT / "integration-tests/fixtures"
        for name in ["managed-dependency-bom", "managed-dependency-bundle", "demo-app"]:
            shutil.copytree(fixtures / name, project / name,
                            ignore=shutil.ignore_patterns("target", "*.class"))
        def maven(name, project_name, *arguments):
            self.command(name, [*self.f.maven_args, "-f",
                                str(project / project_name / "pom.xml"), *arguments],
                         timeout=900)
        maven("admission-bom", "managed-dependency-bom", "install")
        bundle = self.f.registry + "/platform/admission:release"
        maven("admission-bundle", "managed-dependency-bundle", "package",
              PLUGIN_GOAL + "dependency-bundle",
              "-Dbrewlet.dependencyBundleImage=" + bundle,
              "-Dbrewlet.sourceBom=com.example.platform:approved-bom:1.0.0",
              "-Dbrewlet.signingKey=" + str(self.current),
              "-Dbrewlet.signerIdentity=live-platform-builder")
        app = self.f.registry + "/" + REPOSITORY + ":release"
        maven("admission-application", "demo-app", "-Pmanaged-dependencies", "package",
              PLUGIN_GOAL + "push", "-Dbrewlet.image=" + app,
              "-Dbrewlet.dependencyBundle=" + bundle,
              "-Dbrewlet.mainClass=com.example.Hello",
              "-Dbrewlet.signingKey=" + str(self.current),
              "-Dbrewlet.trustedPublicKey=" + str(self.public),
              "-Dbrewlet.trustedSignerIdentity=live-platform-builder",
              "-Dbrewlet.builderIdentity=" + BUILDER)
        raw, manifest = self.registry.manifest(REPOSITORY, "release")
        self.release_subject = {"mediaType": manifest["mediaType"], "digest": digest(raw),
                                "size": len(raw)}
        index = self.registry.referrers(REPOSITORY, digest(raw))
        candidates = [item for item in index["manifests"] if item.get("artifactType") == ARTIFACT]
        if len(candidates) != 1:
            raise AssertionError(f"released Maven plugin must publish one native attestation: {index}")
        candidate = candidates[0]["digest"]
        _, evidence = self.registry.manifest(REPOSITORY, candidate)
        envelope = self.registry.blob(REPOSITORY, evidence["layers"][0]["digest"])
        self.statement = json.loads(base64.b64decode(json.loads(envelope)["payload"]))
        if self.statement["predicate"]["finalImageDigest"] != digest(raw):
            raise AssertionError("Maven evidence does not bind the final published index")
        self.expected["release"] = {candidate: True}
        self.subjects["release"] = self.release_subject
        self.f.save("admission-release-referrers.json", index)
        self.f.save("admission-release-statement.json", self.statement)
        self.f.record("admission-release-publication", {
            "version": "0.5.0", "source": RELEASE_SOURCE,
            "publisher": PLUGIN_GOAL + "push", "subject": digest(raw), "candidate": candidate,
        })

    def fixture(self, name, kinds):
        subject = self.registry.clone_subject(REPOSITORY, "release", name)
        expected = {}
        for number, kind in enumerate(kinds):
            statement = variant_statement(self.statement, subject, kind)
            key = self.obsolete if kind in ("wrong-key", "obsolete") else self.current
            envelope = signed_envelope(statement, key)
            if kind == "malformed":
                envelope = b'{"payload":not-json}'
            elif kind == "tampered":
                document = json.loads(envelope)
                payload = json.loads(base64.b64decode(document["payload"]))
                payload["predicate"]["sourceBom"] = "tampered:after-signing:1"
                document["payload"] = base64.b64encode(encoded(payload)).decode()
                envelope = encoded(document)
            candidate = self.registry.publish(REPOSITORY, subject, envelope,
                                              f"{name}-{number}")
            expected[candidate] = kind == "valid"
            if kind in REJECTION_REASONS:
                self.reasons[candidate] = REJECTION_REASONS[kind]
        self.subjects[name] = subject
        self.expected[name] = expected
        self.discovery(name)
        return self.image(name)

    def discovery(self, name):
        index = self.registry.referrers(REPOSITORY, self.subjects[name]["digest"])
        self.f.save(f"admission-{name}-discovery.json", index)
        assert_discoverable(index, self.expected[name])
        return index

    def image(self, name):
        return self.f.registry_internal + "/" + REPOSITORY + "@" + self.subjects[name]["digest"]

    def install_gatekeeper(self):
        target = self.f.private / "admission-gatekeeper.yaml"
        checksum = download(
            f"https://raw.githubusercontent.com/open-policy-agent/gatekeeper/{GATEKEEPER_COMMIT}/deploy/gatekeeper.yaml",
            target)
        text = target.read_text().replace("openpolicyagent/gatekeeper:v3.18.3",
                                          GATEKEEPER_IMAGE)
        # The controller must have external-data enabled from its first start.
        text = text.replace("- --operation=webhook",
                            "- --enable-external-data\n"
                            "        - --external-data-provider-response-cache-ttl=0\n"
                            "        - --operation=webhook")
        text = text.replace("replicas: 3", "replicas: 1")
        self.f.kube("apply", "-f", "-", input=text, timeout=180)
        self.f.kube("-n", "gatekeeper-system", "rollout", "status",
                    "deployment/gatekeeper-controller-manager", "--timeout=240s", timeout=260)
        self.f.kube("wait", "--for=condition=Established",
                    "crd/providers.externaldata.gatekeeper.sh", "--timeout=120s")
        webhook = self.f.get("validatingwebhookconfiguration",
                             "gatekeeper-validating-webhook-configuration")
        for item in webhook["webhooks"]:
            if item["name"] == "validation.gatekeeper.sh":
                item["failurePolicy"] = "Fail"
                item["timeoutSeconds"] = 30
        self.f.apply(webhook)
        self.f.record("admission-gatekeeper", {
            "sourceCommit": GATEKEEPER_COMMIT, "sourceSHA256": checksum,
            "image": GATEKEEPER_IMAGE, "externalDataCacheTTL": 0,
            "failurePolicy": "Fail", "replicas": 1,
        })

    def build_ratify(self):
        build = self.f.private / "admission-image"
        build.mkdir()
        module = self.f.source / "admission/ratify-verifier"
        environment = ["env", "GOOS=linux", "GOARCH=" + self.f.arch, "CGO_ENABLED=0"]
        self.command("admission-build-verifier",
                     environment + ["go", "build", "-trimpath", "-o",
                                    str(build / VERIFIER), "."], cwd=module, timeout=900)
        self.command("admission-build-other",
                     environment + ["go", "build", "-trimpath", "-o",
                                    str(build / "admission-other"),
                                    str(HERE / "admission_other.go")],
                     cwd=module, timeout=900)
        cache = build / "admission-no-content-cache"
        (cache / "blobs/sha256").mkdir(parents=True)
        (cache / "blobs/sha256/.keep").write_text("")
        (cache / "index.json").write_text('{"schemaVersion":2,"manifests":[]}')
        (cache / "oci-layout").write_text('{"imageLayoutVersion":"1.0.0"}')
        (build / "Dockerfile").write_text(
            f"FROM {RATIFY_IMAGE}\n"
            "ENV RATIFY_CONFIG=/home/nonroot/.ratify\n"
            "COPY --chown=0:0 --chmod=0555 admission-no-content-cache /admission-no-content-cache\n"
            f"COPY --chown=65532:65532 --chmod=0555 {VERIFIER} admission-other "
            "/home/nonroot/.ratify/plugins/\n")
        self.baked_repository = "brewlet.local/" + self.f.name + "-ratify"
        image = self.baked_repository + ":fixture"
        self.command("admission-build-image", ["docker", "build", "--provenance=false",
                                             "--platform", "linux/" + self.f.arch,
                                             "--label", f"{OWNER_LABEL}={self.f.name}",
                                             "-t", image, str(build)], timeout=600)
        self.baked_image_id = json.loads(self.f.run(
            ["docker", "image", "inspect", image]).stdout)[0]["Id"]
        self.f.load_image(image)
        self.f.own_container(self.f.node)
        node_id = self.f.node_id
        rows = self.f.run(["docker", "exec", node_id, "ctr", "-n", "k8s.io",
                           "images", "ls"]).stdout.splitlines()
        self.baked_digest = next(row.split()[2] for row in rows if row.split()[0] == image)
        self.baked_image = self.baked_repository + "@" + self.baked_digest
        self.f.run(["docker", "exec", node_id, "ctr", "-n", "k8s.io",
                    "images", "tag", image, self.baked_image])
        self.f.record("admission-verifier-delivery", {
            "route": "documented baked-image", "baseImage": RATIFY_IMAGE,
            "source": RELEASE_SOURCE, "binarySHA256": hashlib.sha256(
                (build / VERIFIER).read_bytes()).hexdigest(),
            "image": self.baked_image,
            "otherVerifier": "test-only competing verifier; never an attestation substitute",
        })

    def certificates(self):
        self.ca = self.f.private / "admission-ca.pem"
        ca_key = self.f.private / "admission-ca.key"
        tls_key = self.f.private / "admission-tls.key"
        csr = self.f.private / "admission-tls.csr"
        cert = self.f.private / "admission-tls.pem"
        ext = self.f.private / "admission-tls.ext"
        ext.write_text("subjectAltName=DNS:ratify.ratify-service,"
                       "DNS:ratify.ratify-service.svc,DNS:localhost,IP:127.0.0.1\n"
                       "extendedKeyUsage=serverAuth\n")
        run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
             "-keyout", str(ca_key), "-out", str(self.ca), "-days", "2",
             "-subj", "/CN=Brewlet disposable admission fixture"])
        run(["openssl", "req", "-newkey", "rsa:2048", "-nodes",
             "-keyout", str(tls_key), "-out", str(csr),
             "-subj", "/CN=ratify.ratify-service"])
        run(["openssl", "x509", "-req", "-in", str(csr), "-CA", str(self.ca),
             "-CAkey", str(ca_key), "-CAcreateserial", "-out", str(cert),
             "-days", "2", "-extfile", str(ext)])
        for key in (ca_key, tls_key):
            key.chmod(0o600)
        return {"crt": cert.read_text(), "key": tls_key.read_text(),
                "caCert": self.ca.read_text(), "caKey": ca_key.read_text(),
                "cabundle": base64.b64encode(self.ca.read_bytes()).decode()}

    def install_ratify(self):
        archive = self.f.private / "admission-ratify.tar.gz"
        checksum = download(f"https://codeload.github.com/notaryproject/ratify/tar.gz/{RATIFY_COMMIT}",
                            archive)
        extracted = self.f.private / "admission-ratify-source"
        extracted.mkdir()
        with tarfile.open(archive) as tar:
            members = [member for member in tar.getmembers()
                       if "/charts/ratify/" in member.name]
            for member in members:
                if not (member.isfile() or member.isdir()) or ".." in Path(member.name).parts:
                    raise RuntimeError("unsafe Ratify archive member")
                tar.extract(member, extracted, filter="data")
        chart = extracted / ("ratify-" + RATIFY_COMMIT) / "charts/ratify"
        values = {
            "image": {"repository": self.baked_repository,
                      "tag": "fixture@" + self.baked_digest, "pullPolicy": "Never"},
            "notation": {"enabled": False}, "cosign": {"enabled": False},
            "upgradeCRDs": {"enabled": False},
            "provider": {"tls": self.certificates(), "cache": {"enabled": False},
                         "enableMutation": False,
                         "timeout": {"validationTimeoutSeconds": 20}},
            "oras": {"useHttp": True, "cache": {"enabled": False}},
            "policy": {"useRego": True}, "logger": {"level": "debug"},
            "resources": {"requests": {"cpu": "100m", "memory": "256Mi"},
                          "limits": {"cpu": "1000m", "memory": "768Mi"}},
        }
        valuefile = self.f.private / "admission-ratify-values.json"
        valuefile.write_text(json.dumps(values))
        valuefile.chmod(0o600)
        self.command("admission-ratify-install", [
            "helm", "--kubeconfig", str(self.f.kubeconfig), "--kube-context",
            self.f.context, "upgrade", "--install", "ratify", str(chart),
            "--namespace", "ratify-service", "--create-namespace",
            "--values", str(valuefile), "--timeout", "240s"], timeout=300)
        deployment = self.f.get("deployment", "ratify", "-n", "ratify-service")
        deployment["spec"]["strategy"] = {"type": "Recreate", "rollingUpdate": None}
        podspec = deployment["spec"]["template"]["spec"]
        podspec["containers"][0]["image"] = self.baked_image
        podspec["hostAliases"] = [
            {"ip": self.registry_ip, "hostnames": [self.f.registry_internal.split(":")[0]]}]
        self.f.apply(deployment)
        self.f.kube("-n", "ratify-service", "rollout", "status", "deployment/ratify",
                    "--timeout=240s", timeout=260)
        wait("Ratify current Pod and Service endpoints agree", self.ratify_endpoints_ready,
             timeout=120, interval=1)
        self.f.record("admission-ratify", {
            "sourceCommit": RATIFY_COMMIT, "archiveSHA256": checksum,
            "chartVersion": "1.15.6", "appVersion": "v1.4.5",
            "tls": "unique fixture CA; SAN validated by Gatekeeper and report client",
            "providerCache": False, "orasCache": False,
        })

    def policies(self):
        store = self.manifest("10-ratify-store.yaml")
        store["spec"]["parameters"].update(
            cacheEnabled=False, useHttp=True, localCachePath=CONTENT_CACHE)
        self.f.apply(store)
        verifier = self.manifest("20-ratify-verifier.yaml")
        verifier["spec"].pop("source")
        # Ratify v1.4.5's live CRD rejects spec.type; spec.name selects the plugin.
        # Kept explicit as a candidate correction, never an unmodified-release pass.
        verifier["spec"].pop("type")
        verifier["spec"]["parameters"] = {
            "trustedPublicKey": self.public.read_text(), "expectedBuilderIdentity": BUILDER}
        self.f.apply(verifier)
        self.f.apply(self.manifest("30-ratify-policy.yaml"))
        self.f.apply(self.manifest("40-gatekeeper-constrainttemplate.yaml"))
        def constraint_crd_ready():
            result = self.f.kube("get", "crd",
                                 "brewletmanageddependencies.constraints.gatekeeper.sh",
                                 "--ignore-not-found", "-o", "json")
            if not result.stdout.strip():
                return False
            return any(condition["type"] == "Established" and condition["status"] == "True"
                       for condition in json.loads(result.stdout).get("status", {}).get("conditions", []))
        wait("Gatekeeper generated constraint CRD established", constraint_crd_ready,
             timeout=180, interval=1)
        self.f.apply(self.manifest("50-gatekeeper-constraint.yaml"))
        self.f.record("admission-shipped-resources", {
            "source": RELEASE_SOURCE,
            "substitutions": ["fixture public key and builder identity",
                              "baked plugin (no source.artifact)",
                              "candidate correction: remove unsupported Verifier.spec.type",
                              "RATIFY_CONFIG selects baked plugin directory",
                              "ORAS content cache uses immutable empty OCI layout, forcing remote fetch",
                              "owned-registry HTTP; discovery/provider caches disabled"],
            "enforcement": "deny; unchanged verifier identity and predicate semantics",
        })

    @contextlib.contextmanager
    def report_transport(self):
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            self.port = listener.getsockname()[1]
        logfile = self.f.work / "admission-ratify-port-forward.log"
        with logfile.open("w") as output:
            process = subprocess.Popen(
                ["kubectl", "--kubeconfig", str(self.f.kubeconfig), "--context",
                 self.f.context, "-n", "ratify-service", "port-forward",
                 "service/ratify", f"{self.port}:6001", "--address=127.0.0.1"],
                stdout=output, stderr=subprocess.STDOUT)
            self.f.children.append(process)
            try:
                def ready():
                    if process.poll() is not None:
                        raise RuntimeError("Ratify port-forward stopped; " + logfile.read_text())
                    try:
                        with socket.create_connection(("127.0.0.1", self.port), timeout=1):
                            return True
                    except OSError:
                        return False
                wait("Ratify TLS report transport", ready, timeout=60, interval=1)
                yield
            finally:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=10)
                self.port = None

    def report(self, name, suffix="", allowed=None, other=None):
        image = self.image(name)
        request = {"apiVersion": "externaldata.gatekeeper.sh/v1beta1",
                   "kind": "ProviderRequest", "request": {"keys": [image]}}
        response = run([
            "curl", "--fail-with-body", "--silent", "--show-error", "--max-time", "30",
            "--cacert", str(self.ca), "-H", "Content-Type: application/json",
            "--data-binary", "@-",
            f"https://127.0.0.1:{self.port}/ratify/gatekeeper/v1/verify"],
            input=json.dumps(request), check=False, timeout=40)
        if response.returncode:
            self.f.save(f"admission-{name}{suffix}-report-error.log",
                        response.stdout + response.stderr)
            raise RuntimeError(f"Ratify TLS report request failed: {response.stderr}")
        document = json.loads(response.stdout)
        self.f.save(f"admission-{name}{suffix}-report.json", document)
        result = extract_result(document, image)
        if allowed is None:
            allowed = any(self.expected[name].values())
        assert_candidates(result, self.expected[name], allowed, other,
                          subject_digest=self.subjects[name]["digest"])
        # Ratify 1.4.5's subprocess decoder drops errorReason. Supplement its
        # real digest-bound reports with raw stdout from the unchanged plugin.
        raw_reports = {}
        for reference in self.expected[name]:
            if reference in self.reasons:
                raw_reports[reference] = {"verifierReports": [self.probe(name, reference, suffix)]}
        assert_rejection_reasons(raw_reports, {reference: self.reasons[reference]
                                              for reference in raw_reports})
        return result

    def probe(self, name, reference, suffix):
        index = self.registry.referrers(REPOSITORY, self.subjects[name]["digest"])
        descriptor = next(item for item in index["manifests"] if item["digest"] == reference)
        request = {
            "config": {"name": VERIFIER, "type": VERIFIER, "artifactTypes": ARTIFACT,
                       "trustedPublicKey": self.public.read_text(), "expectedBuilderIdentity": BUILDER},
            "storeConfig": {"version": "1.0.0", "pluginBinDirs": None,
                            "store": {"name": "oras", "useHttp": True, "cacheEnabled": False,
                                      "cosignEnabled": False, "localCachePath": CONTENT_CACHE}},
            "referenceDesc": descriptor,
        }
        response = self.f.kube("-n", "ratify-service", "exec", "-i", "deployment/ratify",
                               "--", "/home/nonroot/.ratify/plugins/admission-other",
                               "--probe", self.image(name), input=json.dumps(request))
        raw = json.loads(response.stdout)
        self.f.save(f"admission-{name}{suffix}-plugin-{reference.split(':')[1][:12]}.json", raw)
        if raw.get("isSuccess") is not False or raw.get("verifierName") != VERIFIER:
            raise AssertionError(f"supplemental plugin result disagrees with actual failure: {raw}")
        return raw

    def pod(self, name, image=None, namespace=None, brewlet=True):
        container = {"name": "application", "image": image or self.image(name),
                     "securityContext": {"allowPrivilegeEscalation": False,
                                         "capabilities": {"drop": ["ALL"]}},
                     "resources": {"requests": {"cpu": "100m", "memory": "128Mi"},
                                   "limits": {"cpu": "500m", "memory": "256Mi"}}}
        spec = {"containers": [container], "restartPolicy": "Never",
                "securityContext": {"runAsNonRoot": True, "runAsUser": 10001,
                                    "seccompProfile": {"type": "RuntimeDefault"}}}
        if brewlet:
            spec["runtimeClassName"] = "brewlet"
        return {"apiVersion": "v1", "kind": "Pod",
                "metadata": {"name": "admission-" + name,
                             "namespace": namespace or self.f.namespace},
                "spec": spec}

    def admit(self, name, document, allowed, update=False, persist=False, subresource=None):
        args = ["replace" if update else "create", "-f", "-", "-o", "json"]
        if not persist:
            args.append("--dry-run=server")
        if subresource:
            args.append("--subresource=" + subresource)
        result = self.f.kube(*args, input=json.dumps(document), check=False, timeout=45)
        self.f.save("admission-" + name + "-api.log", result.stdout + result.stderr)
        evidence = assert_admission(result, allowed)
        self.f.record("admission-" + name, evidence)
        self.successful.add(name)
        return json.loads(result.stdout) if result.returncode == 0 else None

    def assert_enforcement_ready(self):
        self.fixture("readiness", [])
        def enforcing():
            response = self.f.kube("create", "--dry-run=server", "-f", "-",
                                   input=json.dumps(self.pod("readiness")), check=False)
            self.f.save("admission-enforcement-readiness.log", response.stdout + response.stderr)
            try:
                assert_admission(response, False)
                return True
            except AssertionError:
                return False
        wait("Brewlet Gatekeeper deny policy synchronized", enforcing, timeout=180, interval=3)

    def positive(self):
        self.discovery("release")
        with self.report_transport():
            def registered():
                try:
                    return self.report("release")
                except (AssertionError, urllib.error.URLError, RuntimeError) as error:
                    self.f.save("admission-verifier-registration-error.log", str(error))
                    return False
            wait("Brewlet external verifier registered", registered, timeout=180, interval=3)
        image = self.image("release")
        self.admit("trusted-create", self.pod("release"), True)
        self.f.java_application("admission-trusted", image)
        wait("JavaApplication generated Deployment", lambda: bool(self.f.kube(
            "get", "deployment", "admission-trusted", "-n", self.f.namespace,
            "--ignore-not-found", "-o", "name").stdout.strip()), timeout=120, interval=1)
        self.f.wait_ready("admission-trusted")
        pods = self.f.get("pods", "-n", self.f.namespace)
        actual = [pod for pod in pods["items"]
                  if pod["metadata"]["name"].startswith("admission-trusted-")]
        if not actual:
            raise AssertionError("JavaApplication produced no pods")
        for pod in actual:
            if pod["spec"].get("runtimeClassName") != "brewlet":
                raise AssertionError("JavaApplication did not select real Brewlet runtime")
            if any(container["image"] != image for container in pod["spec"]["containers"]):
                raise AssertionError("generated Pod image is not the admitted final digest")
            if not any(condition["type"] == "Ready" and condition["status"] == "True"
                       for condition in pod.get("status", {}).get("conditions", [])):
                raise AssertionError("generated Pod is not Ready")
        body = self.f.service_get("admission-trusted")
        text = body.stdout if hasattr(body, "stdout") else str(body)
        if "Hello" not in text:
            raise AssertionError("trusted image did not serve the real application: " + text)
        self.f.save("admission-trusted-service.log", text)
        self.f.record("admission-trusted-runtime", {
            "image": image, "readyPods": [pod["metadata"]["name"] for pod in actual],
            "served": text,
        })

    def negatives(self):
        for kind in ["unsigned", "wrong-key", "wrong-builder", "wrong-subject",
                     "malformed", "tampered", "incomplete"]:
            self.fixture(kind, [] if kind == "unsigned" else [kind])
            with self.report_transport():
                self.report(kind)
            self.admit(kind, self.pod(kind), False)
        self.fixture("cross-candidate", ["wrong-builder", "incomplete"])
        with self.report_transport():
            self.report("cross-candidate")
        self.admit("cross-candidate", self.pod("cross-candidate"), False)
        self.fixture("valid-plus-malformed", ["valid", "malformed"])
        with self.report_transport():
            self.report("valid-plus-malformed")
        self.admit("valid-plus-malformed", self.pod("valid-plus-malformed"), True)

    def rotation(self):
        self.fixture("rotation", ["valid"])
        current = next(iter(self.expected["rotation"]))
        with self.report_transport():
            first = self.report("rotation", "-current")
        self.admit("rotation-current", self.pod("rotation"), True)
        statement = variant_statement(self.statement, self.subjects["rotation"], "obsolete")
        obsolete = self.registry.publish(REPOSITORY, self.subjects["rotation"],
                                         signed_envelope(statement, self.obsolete), "obsolete")
        self.expected["rotation"][obsolete] = False
        self.reasons[obsolete] = REJECTION_REASONS["obsolete"]
        self.discovery("rotation")
        with self.report_transport():
            second = self.report("rotation", "-both")
        self.admit("rotation-both", self.pod("rotation"), True)
        self.registry.delete_candidate(REPOSITORY, current)
        del self.expected["rotation"][current]
        self.discovery("rotation")
        with self.report_transport():
            third = self.report("rotation", "-obsolete")
        self.admit("rotation-obsolete", self.pod("rotation"), False)
        timestamps = [result.get("timestamp") for result in (first, second, third)]
        if not all(timestamps) or len(set(timestamps)) != 3:
            raise AssertionError("rotation reports do not prove three fresh evaluations")
        self.f.record("admission-rotation-evidence", {
            "image": self.image("rotation"), "currentCandidate": current,
            "obsoleteCandidate": obsolete, "timestamps": timestamps,
            "allCachesDisabled": True,
        })

    def competing_verifier(self):
        other = {"apiVersion": "config.ratify.deislabs.io/v1beta1", "kind": "Verifier",
                 "metadata": {"name": "admission-other"},
                 "spec": {"name": "admission-other",
                          "version": "1.0.0", "artifactTypes": ARTIFACT,
                          "parameters": {"success": True}}}
        self.f.apply(other)
        self.fixture("other-success", ["wrong-key"])
        # Rollouts guarantee the CRD informer and subprocess configuration changed;
        # no cached verification decision is used as a readiness shortcut.
        self.restart_ratify()
        with self.report_transport():
            self.report("other-success", other=True)
        self.admit("other-success-not-substitute", self.pod("other-success"), False)
        other["spec"]["parameters"]["success"] = False
        self.f.apply(other)
        self.fixture("other-failure", ["valid"])
        self.restart_ratify()
        with self.report_transport():
            self.report("other-failure", other=False)
        self.admit("other-failure-not-veto", self.pod("other-failure"), True)
        self.f.kube("delete", "verifier", "admission-other")
        self.restart_ratify()

    def restart_ratify(self):
        self.f.kube("-n", "ratify-service", "rollout", "restart", "deployment/ratify")
        self.f.kube("-n", "ratify-service", "rollout", "status",
                    "deployment/ratify", "--timeout=180s", timeout=200)
        wait("Ratify rollout old Pods removed and Service endpoints synchronized",
             self.ratify_endpoints_ready, timeout=120, interval=1)
        def admission_ready():
            response = self.f.kube("create", "--dry-run=server", "-f", "-", "-o", "name",
                                   input=json.dumps(self.pod("provider-ready", self.image("release"))),
                                   check=False)
            self.f.save("admission-provider-readiness.log", response.stdout + response.stderr)
            if response.returncode == 0:
                return True
            assert_admission(response, False)
            return False
        wait("Gatekeeper reaches the restarted Ratify Service", admission_ready,
             timeout=120, interval=2)

    def ratify_endpoints_ready(self):
        pods = self.f.get("pods", "-n", "ratify-service",
                          "-l", "app.kubernetes.io/name=ratify")["items"]
        if len(pods) != 1 or pods[0]["metadata"].get("deletionTimestamp"):
            return False
        if not any(condition["type"] == "Ready" and condition["status"] == "True"
                   for condition in pods[0].get("status", {}).get("conditions", [])):
            return False
        addresses = {address["ip"]
                     for subset in self.f.get("endpoints", "ratify", "-n", "ratify-service").get("subsets", [])
                     for address in subset.get("addresses", [])}
        return addresses == {pods[0]["status"]["podIP"]}

    def api_paths(self):
        self.fixture("api-paths", ["valid"])
        self.fixture("api-paths-unsigned", [])
        valid, invalid = self.image("api-paths"), self.image("api-paths-unsigned")
        for name, image, allowed in [("init-valid", valid, True), ("init-invalid", invalid, False)]:
            pod = self.pod("api-paths")
            pod["spec"]["initContainers"] = [{"name": "initialize", "image": image}]
            self.admit(name, pod, allowed)
        # A scheduling gate makes the persisted UPDATE subject a valid, inert Pod.
        # No ordinary-image init/ephemeral execution is asserted.
        pod = self.pod("updates", image=valid)
        pod["spec"]["schedulingGates"] = [{"name": "live.brewlet.invalid/admission-only"}]
        pod["spec"]["initContainers"] = [{"name": "initialize", "image": valid}]
        self.admit("update-base-create", pod, True, persist=True)
        original = self.f.get("pod", "admission-updates", "-n", self.f.namespace)
        for label, container in [("regular", "containers"), ("init", "initContainers")]:
            change = copy.deepcopy(original)
            change["spec"][container][0]["image"] = invalid
            self.admit(label + "-update-invalid", change, False, update=True)
            change = copy.deepcopy(original)
            change["spec"][container][0]["image"] = self.image("release")
            self.admit(label + "-update-valid", change, True, update=True)
        for label, image, allowed in [("ephemeral-valid", valid, True),
                                      ("ephemeral-invalid", invalid, False)]:
            change = copy.deepcopy(original)
            change["spec"]["ephemeralContainers"] = [
                {"name": "inspect", "image": image, "targetContainerName": "application"}]
            self.admit(label, change, allowed, update=True, subresource="ephemeralcontainers")
        self.admit("non-brewlet-create", self.pod("non-brewlet", invalid, brewlet=False), True)
        ordinary = self.pod("ordinary-update", invalid, brewlet=False)
        ordinary["spec"]["schedulingGates"] = [{"name": "live.brewlet.invalid/admission-only"}]
        self.admit("non-brewlet-base", ordinary, True, persist=True)
        ordinary = self.f.get("pod", "admission-ordinary-update", "-n", self.f.namespace)
        ordinary["metadata"].setdefault("labels", {})["live.brewlet.invalid/updated"] = "yes"
        self.admit("non-brewlet-update", ordinary, True, update=True)
        for namespace in ["kube-system", "gatekeeper-system", "ratify-service"]:
            self.admit("excluded-" + namespace,
                       self.pod("excluded", invalid, namespace=namespace), True)
        # The Brewlet component namespace is deliberately not an exclusion.
        self.admit("brewlet-namespace-not-excluded",
                   self.pod("not-excluded", invalid, namespace="brewlet"), False)

    def fetch_failure(self):
        self.fixture("fetch-failure", ["valid"])
        candidate = next(iter(self.expected["fetch-failure"]))
        _, manifest = self.registry.manifest(REPOSITORY, candidate)
        blob = manifest["layers"][0]["digest"]
        # Zot intentionally rejects blob DELETE. Remove only this invocation's
        # exact, hash-checked evidence file to inject a real registry fetch fault.
        self.f.own_container(self.f.registry_id)
        path = self.registry_storage / REPOSITORY / "blobs/sha256" / blob.split(":")[1]
        if digest(path.read_bytes()) != blob:
            raise AssertionError("refusing to remove a nonmatching fixture evidence blob")
        path.unlink()
        try:
            self.registry.blob(REPOSITORY, blob)
        except urllib.error.HTTPError as error:
            if error.code not in (404, 500):
                raise
            status = error.code
        else:
            raise AssertionError("fault injection did not produce a real HTTP blob fetch failure")
        self.f.save("admission-fetch-fault.json",
                    {"candidate": candidate, "removedEvidenceBlob": blob, "httpStatus": status})
        self.discovery("fetch-failure")
        self.expected["fetch-failure"][candidate] = False
        self.reasons[candidate] = "fetch DSSE envelope blob"
        with self.report_transport():
            result = self.report("fetch-failure")
        self.admit("registry-fetch-failure", self.pod("fetch-failure"), False)

    def authentication_failure(self):
        self.fixture("authentication", ["valid"])
        self.discovery("authentication")
        # A real registry htpasswd challenge, not a mock provider response.
        # Changing only the invocation-owned registry config preserves its data.
        self.f.own_container(self.f.registry_id)
        original = self.registry_config.read_text()
        password = base64.urlsafe_b64encode(os.urandom(24)).decode()
        entry = run(["htpasswd", "-niB", "live-fixture"], input=password + "\n").stdout
        self.registry_credentials.write_text(entry)
        protected = json.loads(original)
        protected["http"]["auth"] = {"htpasswd": {"path": "/etc/zot/htpasswd"}, "failDelay": 0}
        try:
            self.registry_config.write_text(json.dumps(protected))
            self.f.run(["docker", "restart", self.f.registry_id])
            def challenges():
                try:
                    self.registry.request("GET", "/v2/")
                    return False
                except urllib.error.HTTPError as error:
                    return error.code == 401 and "Basic" in error.headers.get("WWW-Authenticate", "")
                except OSError:
                    return False
            wait("owned registry requires real authentication", challenges, timeout=60, interval=1)
            self.admit("registry-authentication-failure", self.pod("authentication"), False)
            self.f.record("admission-authentication-challenge",
                          {"status": 401, "scheme": "Basic", "subject": self.image("authentication"),
                           "previouslyVerified": False})
        finally:
            self.f.own_container(self.f.registry_id)
            self.registry_config.write_text(original)
            self.f.run(["docker", "restart", self.f.registry_id])
            def restored():
                try:
                    return self.registry.request("GET", "/v2/")
                except OSError:
                    return False
            wait("owned registry restored", restored, timeout=60, interval=1)

    def unavailable_provider(self):
        self.fixture("unavailable", ["valid"])
        self.f.kube("-n", "ratify-service", "scale", "deployment/ratify", "--replicas=0")
        def no_endpoints():
            endpoints = self.f.get("endpoints", "ratify", "-n", "ratify-service")
            return not any(subset.get("addresses") for subset in endpoints.get("subsets", []))
        try:
            wait("Ratify has no serving endpoints", no_endpoints, timeout=90, interval=2)
            self.admit("ratify-unavailable", self.pod("unavailable"), False)
            self.f.record("admission-provider-outage", {
                "subject": self.image("unavailable"), "previouslyVerified": False,
                "endpoints": self.f.get("endpoints", "ratify", "-n", "ratify-service")})
        finally:
            self.f.kube("-n", "ratify-service", "scale", "deployment/ratify", "--replicas=1")
            self.f.kube("-n", "ratify-service", "rollout", "status", "deployment/ratify",
                        "--timeout=180s", timeout=200)

    def diagnostics(self):
        errors = []
        commands = {
            "ratify": ["-n", "ratify-service", "logs", "deployment/ratify",
                       "--all-containers", "--tail=1500"],
            "gatekeeper": ["-n", "gatekeeper-system", "logs",
                           "deployment/gatekeeper-controller-manager",
                           "--all-containers", "--tail=1000"],
            "constraint": ["get", "brewletmanageddependencies", "-o", "yaml"],
            "template": ["get", "constrainttemplate", "brewletmanageddependencies", "-o", "yaml"],
            "events": ["get", "events", "-A", "-o", "json"],
            "workloads": ["get", "pods,deployments,javaapplications", "-A", "-o", "json"],
            "provider": ["get", "providers", "-o", "json"],
        }
        for name, argv in commands.items():
            try:
                result = self.f.kube(*argv, check=False, timeout=30)
                self.f.save("admission-diag-" + name + ".log", result.stdout + result.stderr)
                if result.returncode:
                    errors.append({"surface": name, "returncode": result.returncode,
                                   "error": result.stderr or result.stdout})
            except Exception as error:
                errors.append({"surface": name, "error": str(error)})
        try:
            self.f.own_container(self.f.registry_id)
            result = self.f.run(["docker", "logs", "--tail", "1500", self.f.registry_id],
                         check=False, timeout=30)
            self.f.save("admission-diag-registry.log", result.stdout + result.stderr)
            if result.returncode:
                errors.append({"surface": "registry", "returncode": result.returncode,
                               "error": result.stderr or result.stdout})
        except Exception as error:
            errors.append({"surface": "registry", "error": str(error)})
        try:
            self.f.save("admission-diagnostics-result.json",
                        {"complete": not errors, "errors": errors})
        except Exception as error:
            errors.append({"surface": "diagnostic-result", "error": str(error)})
        if errors:
            print(redact("Admission diagnostic capture failures: " + json.dumps(errors)),
                  file=sys.stderr, flush=True)
        return errors

    def execute(self):
        try:
            self.setup_registry_route()
            self.keys()
            self.publish()
            self.build_ratify()
            self.install_gatekeeper()
            self.install_ratify()
            self.policies()
            self.assert_enforcement_ready()
            self.positive()
            self.negatives()
            self.rotation()
            self.competing_verifier()
            self.api_paths()
            self.fetch_failure()
            self.authentication_failure()
            self.unavailable_provider()
            self.f.record("admission-scenario-complete", {
                "mandatoryAssertions": sorted(self.successful),
                "ordinaryEphemeralExecutionClaimed": False,
                "release": "0.5.0", "candidateProductChanges": [
                    "remove unsupported Verifier.spec.type"] + (
                    [] if self.f.candidate == "release" else [self.f.candidate]),
            })
        finally:
            primary_failure = sys.exc_info()[0] is not None
            errors = self.diagnostics()
            try:
                if getattr(self, "baked_image_id", None):
                    image = json.loads(self.f.run(
                        ["docker", "image", "inspect", self.baked_image_id]).stdout)[0]
                    if image["Config"].get("Labels", {}).get(OWNER_LABEL) != self.f.name:
                        raise RuntimeError("refusing to remove a foreign baked Ratify image")
                    self.f.run(["docker", "image", "rm", self.baked_image_id])
            except Exception as error:
                errors.append({"surface": "baked-image-cleanup", "error": str(error)})
                print(redact("Admission baked-image cleanup failed: " + str(error)),
                      file=sys.stderr, flush=True)
            if errors and not primary_failure:
                raise RuntimeError(redact("Required admission diagnostics/cleanup failed: " + json.dumps(errors)))


def main():
    for name in ("htpasswd", "go"):
        if not shutil.which(name):
            raise SystemExit(f"Scenario A prerequisite missing: {name}; htpasswd comes from Apache utilities")
    with Fixture("admission") as fixture:
        Admission(fixture).execute()


if __name__ == "__main__":
    main()
