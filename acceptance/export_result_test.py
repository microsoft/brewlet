import unittest

from export_result import export


class ExportTests(unittest.TestCase):
    def test_wasm_release_provenance_is_exported_without_private_input_paths(self):
        result = export({"assertions": [], "wasmRuntimes": {"spin": {"after": {
            "releaseVersion": "4.2.3", "base": "example.invalid/base@sha256:" + "a" * 64,
            "binarySHA256": "b" * 64, "layout": "/private/input", "binary": "/private/executable",
            "futurePrivateField": "SECRET"}}}})
        record = result["wasmRuntimes"]["spin"]["after"]
        self.assertEqual(record["releaseVersion"], "4.2.3")
        self.assertNotIn("layout", record)
        self.assertNotIn("binary", record)
        self.assertNotIn("futurePrivateField", record)

    def test_credentials_and_raw_observations_are_not_exported(self):
        source = {
            "outcome": "passed", "cleanSelectedSuitePassed": True,
            "fullAcceptance": False, "privateKey": "SECRET",
            "kubeconfig": "SECRET", "failure": "SECRET",
            "assertions": [{"name": "safe", "outcome": "passed",
                            "requirements": ["RUN-08"], "evidence": {"env": "SECRET"}}],
        }
        result = export(source)
        self.assertNotIn("privateKey", result)
        self.assertNotIn("kubeconfig", result)
        self.assertNotIn("failure", result)
        self.assertNotIn("evidence", result["assertions"][0])
        self.assertFalse(result["fullAcceptance"])

    def test_smaller_local_language_scope_is_not_lost_in_public_results(self):
        result = export({"outcome": "passed", "selectedScenario": "control",
                         "selectedLanguages": ["python"], "fullAcceptance": False,
                         "gaps": ["Other languages omitted"], "assertions": []})
        self.assertEqual(result["selectedLanguages"], ["python"])
        self.assertFalse(result["fullAcceptance"])
        self.assertEqual(result["gaps"], ["Other languages omitted"])

    def test_intervened_run_cannot_export_clean_success(self):
        result = export({"outcome": "passed", "cleanSelectedSuitePassed": True,
                         "assertions": [], "interventions": [{"reason": "changed test state"}]})
        self.assertFalse(result["cleanSelectedSuitePassed"])
        self.assertEqual("intervened", result["outcome"])

    def test_partial_control_scope_is_preserved_without_full_acceptance(self):
        result = export({"outcome": "passed", "selectedScenario": "control",
                         "cleanSelectedSuitePassed": True, "fullAcceptance": False,
                         "gaps": ["CRI accounting and other callbacks omitted"], "assertions": []})
        self.assertEqual(result["selectedScenario"], "control")
        self.assertEqual(result["gaps"], ["CRI accounting and other callbacks omitted"])
        self.assertFalse(result["fullAcceptance"])


if __name__ == "__main__":
    unittest.main()
