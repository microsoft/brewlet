"""Offline producer-node wiring; never invokes Maven, Docker or Kubernetes."""

import json
from pathlib import Path
import shlex
import shutil
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch
import uuid

from harness import Failure
import maven_node


class MavenNodeTests(unittest.TestCase):
    def setUp(self):
        self.work = Path(__file__).parent / (".maven-node-unit-" + uuid.uuid4().hex)
        self.work.mkdir(mode=0o700)
        self.addCleanup(shutil.rmtree, self.work)

    def harness(self):
        target = "sha256:" + "a" * 64
        delivery = SimpleNamespace(publish=Mock(side_effect=lambda layout, descriptor, repository:
                                    "registry.test:5000/" + repository + "@" + descriptor["digest"]))
        h = SimpleNamespace(work=self.work, cli_command=["/private/brewlet path", "--literal", "a'b"],
                            delivery=delivery)

        def run(command, **kwargs):
            store = next((str(a).split("=", 1)[1] for a in command
                          if str(a).startswith("-Dbrewlet.store=")), None)
            if store:
                path = Path(store)
                path.mkdir()
                (path / "index.json").write_text(json.dumps({
                    "schemaVersion": 2, "manifests": [{"digest": target}]}))
            return SimpleNamespace(returncode=0)

        h.run = Mock(side_effect=run)
        return h, target

    def test_maven_produces_both_ordinary_variants_with_private_repo_and_explicit_platforms(self):
        h, target = self.harness()
        with patch("maven_node.artifact_target", return_value=(
                target, [["dependency", "app", "launch"]] * 2)):
            references = maven_node.prepare(h)
        self.assertEqual(set(references), {"bundled", "managed"})
        self.assertTrue(all(value.endswith("@" + target) for value in references.values()))
        self.assertEqual(h.delivery.publish.call_count, 2)
        commands = [list(map(str, call.args[0])) for call in h.run.call_args_list]
        self.assertEqual(len(commands), 4)
        self.assertTrue(any("layer-set" in " ".join(command) for command in commands))
        pushes = [command for command in commands if any(a.endswith(":push") for a in command)]
        self.assertEqual(len(pushes), 2)
        for command in pushes:
            self.assertIn("-Dbrewlet.platforms=linux/amd64,linux/arm64", command)
            self.assertIn("-Dbrewlet.executable=" + str(self.work / "maven-brewlet"), command)
            self.assertFalse(any("runtimeImages" in value for value in command))
        managed = next(c for c in pushes if any("java-maven-managed" in value for value in c))
        self.assertIn("-Dbrewlet.layerSetInput=" + str(self.work / "oci-maven-dependencies"), managed)
        for call in h.run.call_args_list:
            self.assertEqual(call.kwargs["env"], {
                "MAVEN_OPTS": "-Dmaven.repo.local=" + str(self.work / "m2")})
        line = (self.work / "maven-brewlet").read_text().splitlines()[-1]
        self.assertEqual(shlex.split(line), ["exec", *h.cli_command, "$@"])
        self.assertEqual((self.work / "maven-brewlet").stat().st_mode & 0o777, 0o700)

    def test_changed_transport_digest_is_rejected(self):
        h, target = self.harness()
        h.delivery.publish.side_effect = None
        h.delivery.publish.return_value = "registry.test/app@sha256:" + "b" * 64
        with patch("maven_node.artifact_target", return_value=(
                target, [["dependency", "app", "launch"]] * 2)):
            with self.assertRaisesRegex(Failure, "changed the produced OCI target"):
                maven_node.prepare(h)

    def test_missing_real_base_execution_never_launches_a_producer(self):
        h = SimpleNamespace(initial={})
        with patch("maven_node.prepare", side_effect=AssertionError("producer invoked")):
            with self.assertRaisesRegex(Failure, "requires real base execution"):
                maven_node.execute(h)


if __name__ == "__main__":
    unittest.main()
