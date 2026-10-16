import unittest

from export_result import export


class ExportTests(unittest.TestCase):
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

    def test_intervened_run_cannot_export_clean_success(self):
        result = export({"outcome": "passed", "cleanSelectedSuitePassed": True,
                         "assertions": [], "interventions": [{"reason": "changed test state"}]})
        self.assertFalse(result["cleanSelectedSuitePassed"])
        self.assertEqual("intervened", result["outcome"])


if __name__ == "__main__":
    unittest.main()
