"""Offline uninstall evidence guards; actual Helm/worker reversal is separate."""

import copy
from types import SimpleNamespace
import unittest

from harness import FENCE, OWNER, Failure
from pinned_uninstall import blocked_cleanup, helm_profile_identity


class HelmOwnershipTests(unittest.TestCase):
    def test_only_actual_exact_release_ownership_is_accepted(self):
        obj = {"metadata": {"uid": "profile-uid",
            "labels": {"app.kubernetes.io/managed-by": "Helm"},
            "annotations": {"meta.helm.sh/release-name": "release",
                            "meta.helm.sh/release-namespace": "system"}}}
        self.assertEqual(helm_profile_identity(obj, "release", "system"), "profile-uid")
        for key in ("uid", "labels", "annotations", "deletionTimestamp"):
            value = copy.deepcopy(obj)
            if key == "deletionTimestamp":
                value["metadata"][key] = "2026-10-17T00:00:00Z"
            else:
                value["metadata"].pop(key)
            with self.assertRaises(Failure):
                helm_profile_identity(value, "release", "system")
        with self.assertRaises(Failure):
            helm_profile_identity(obj, "other", "system")
        with self.assertRaises(Failure):
            helm_profile_identity(obj, "release", "other")

    def fixture(self):
        run = "123456abcdef"
        profile = {"metadata": {"uid": "profile-uid", "labels": {OWNER: run},
            "deletionTimestamp": "2026-10-17T00:00:00Z", "finalizers": ["cleanup"]}}
        node = {"metadata": {"labels": {"stale": "ready", "brewlet.sh/owner-uid": "profile-uid"}},
                "spec": {"taints": [{"key": FENCE, "effect": "NoSchedule"}]}}
        operator = {"status": {"availableReplicas": 1}}
        h = SimpleNamespace(run_id=run, profile="profile", control_namespace="system",
            node_snapshot=lambda: node,
            get=lambda kind, *args: profile if kind == "nodeprofile" else operator)
        h.kubectl = lambda *args, **kwargs: SimpleNamespace(
            stdout="yes\n" if args[0] == "auth" else "uninstall cleanup incomplete: deadline exceeded",
            returncode=0)
        return h, profile, node, operator

    def test_failed_uninstall_requires_real_retention_not_a_generic_helm_error(self):
        h, profile, _, _ = self.fixture()
        self.assertEqual(blocked_cleanup(h, "profile-uid", "stale"), profile)
        h.kubectl = lambda *args, **kwargs: SimpleNamespace(
            stdout="yes\n" if args[0] == "auth" else "foreign NodeProfile remains", returncode=0)
        with self.assertRaisesRegex(Failure, "unrelated reason"):
            blocked_cleanup(h, "profile-uid", "stale")

    def test_root_claim_fence_finalizer_and_operator_are_all_required(self):
        for mutation in ("claim", "fence", "finalizer", "operator"):
            with self.subTest(mutation=mutation):
                h, profile, node, operator = self.fixture()
                if mutation == "claim":
                    node["metadata"]["labels"]["brewlet.sh/owner-uid"] = "foreign"
                elif mutation == "fence":
                    node["spec"]["taints"] = []
                elif mutation == "finalizer":
                    profile["metadata"]["finalizers"] = []
                else:
                    operator["status"]["availableReplicas"] = 0
                with self.assertRaises(Failure):
                    blocked_cleanup(h, "profile-uid", "stale")


if __name__ == "__main__":
    unittest.main()
