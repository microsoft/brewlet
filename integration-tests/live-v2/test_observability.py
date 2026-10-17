"""Evidence parsing contracts, not proof of actual event or metrics delivery."""

import copy
import json
import unittest

from harness import Failure
from observability import metric_samples, runtime_events, sample_value, verify_binding


class ObservabilityEvidenceTests(unittest.TestCase):
    def fixture(self):
        runtime = {"id": "python", "inventoryUID": "original-inventory",
                   "generation": 9007199254740993, "sourceDigest": "sha256:source"}
        lease = {"runtime": runtime, "taskID": "actual-container"}
        pod = {"metadata": {"namespace": "apps", "name": "api", "uid": "original-pod"}}
        labels = {"namespace": "apps", "pod": "api", "pod_uid": "original-pod",
                  "container": "app", "container_id": "actual-container", "id": "python",
                  "inventory_uid": "original-inventory", "generation": "9007199254740993",
                  "digest": "sha256:source"}
        event = {"reason": "RuntimeImageResolved", "containerID": "actual-container",
                 "id": "python", "inventoryUID": "original-inventory",
                 "generation": "9007199254740993", "digest": "sha256:source"}
        return lease, pod, labels, event

    def test_prometheus_quoted_labels_and_histograms(self):
        samples = metric_samples('# HELP example ignored\n'
            'example{pod="two, words",path="a\\\\b\\n\\"c"} 1\n'
            'duration_bucket{id="python",le="+Inf"} 3\n'
            'duration_count{id="python"} 3\n')
        self.assertEqual(sample_value(samples, "example", {"pod": "two, words", "path": 'a\\b\n"c'}), 1)
        self.assertEqual(sample_value(samples, "duration_count", {"id": "python"}), 3)

    def test_malformed_duplicate_and_nonstring_labels_are_not_accepted(self):
        for text in ('a{id="one",id="two"} 1', 'a{id=1} 1', 'a{id="one",} 1',
                     'a{id="one"bad="two"} 1', 'not a metric'):
            with self.subTest(text=text), self.assertRaises((Failure, ValueError)):
                metric_samples(text)

    def test_missing_duplicate_or_nonfinite_samples_are_not_success(self):
        for samples in ([], [("a", {}, 1), ("a", {}, 1)], [("a", {}, float("nan"))]):
            with self.subTest(samples=samples), self.assertRaises(Failure):
                sample_value(samples, "a", {})

    def test_runtime_event_filter_preserves_exact_generation(self):
        _, _, _, event = self.fixture()
        events = runtime_events('timestamp k8s.io /tasks/start {"irrelevant":true}\n'
            'timestamp k8s.io /brewlet/runtime-image ' + json.dumps(event) + '\n')
        self.assertEqual(events, [event])

    def test_foreign_namespace_and_malformed_event_are_not_evidence(self):
        for line in ('timestamp foreign /brewlet/runtime-image {}',
                     'timestamp k8s.io /brewlet/runtime-image {',
                     'timestamp k8s.io /brewlet/runtime-image []'):
            with self.subTest(line=line), self.assertRaises((Failure, ValueError)):
                runtime_events(line)

    def test_binding_joins_both_actual_pod_and_actual_lease(self):
        lease, pod, labels, event = self.fixture()
        samples = [("brewlet_container_runtime_image_info", labels, 1)]
        self.assertEqual(verify_binding(samples, [event], lease, pod), labels)
        for key in labels:
            changed = dict(labels, **{key: "foreign"})
            with self.subTest(label=key), self.assertRaises(Failure):
                verify_binding([("brewlet_container_runtime_image_info", changed, 1)], [event], lease, pod)

    def test_missing_duplicate_lossy_or_foreign_start_event_is_not_evidence(self):
        lease, pod, labels, event = self.fixture()
        samples = [("brewlet_container_runtime_image_info", labels, 1)]
        for events in ([], [event, event]):
            with self.subTest(events=events), self.assertRaises(Failure):
                verify_binding(samples, events, lease, pod)
        for key in event:
            changed = copy.deepcopy(event)
            changed[key] = float(9007199254740993) if key == "generation" else "foreign"
            with self.subTest(key=key), self.assertRaises(Failure):
                verify_binding(samples, [changed], lease, pod)
