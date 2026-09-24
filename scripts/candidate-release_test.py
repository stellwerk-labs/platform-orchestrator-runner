#!/usr/bin/env python3
import importlib.util
from pathlib import Path
import re
import unittest

spec = importlib.util.spec_from_file_location("candidate", Path(__file__).with_name("candidate-release.py"))
candidate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(candidate)
SHA = "a" * 40


class CandidateReleaseTests(unittest.TestCase):
    def test_canonical_candidate_and_exact_commit(self):
        for tag in ("v3.1.0-rc.1", "v3.1.0-rc.12", "v0.7.0-rc.1"):
            with self.subTest(tag=tag):
                candidate.validate_identity(tag, SHA, SHA, SHA)

    def test_stable_tags_and_noncanonical_or_injected_input_fail(self):
        for tag in ("v3.1.0", "latest", "v03.1.0-rc.1", "v3.1.0-rc.0", "v3.1.0-rc.01",
                    "v3.1.0-rc.1+build", "../notes", "v3.1.0-rc.1\nother", "v3.1.0-rc.1;echo unsafe"):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                candidate.validate_identity(tag, SHA, SHA, SHA)

    def test_retargeted_tags_and_mismatched_checkout_fail(self):
        for values in (("a" * 39, SHA, SHA), (SHA, "b" * 40, SHA),
                       (SHA, SHA, "b" * 40), ("A" * 40, SHA, SHA)):
            with self.subTest(values=values), self.assertRaises(ValueError):
                candidate.validate_identity("v3.1.0-rc.1", *values)

    def test_preexisting_environment_requires_reviewers(self):
        candidate.validate_environment({
            "name": "public-release-candidate",
            "protection_rules": [{"type": "required_reviewers", "reviewers": [{"type": "Team", "reviewer": {"id": 1}}]}],
        })
        for environment in ({}, {"name": "public-release-candidate"},
                            {"name": "public-release-candidate", "protection_rules": [{"type": "required_reviewers", "reviewers": []}]},
                            {"name": "public-release-candidate", "protection_rules": [{"type": "wait_timer", "wait_timer": 5}]}):
            with self.subTest(environment=environment), self.assertRaises(ValueError):
                candidate.validate_environment(environment)

    def test_release_and_image_absence_fail_closed(self):
        candidate.validate_absence_status(404)
        for status in (200, 401, 403, 429, 500, 503):
            with self.subTest(status=status), self.assertRaises(ValueError):
                candidate.validate_absence_status(status)
        candidate.validate_release_page("v3.1.0-rc.1", [])
        candidate.validate_release_page("v3.1.0-rc.1", [{"tag_name": "v3.0.0", "draft": False}])
        for draft in (True, False):
            with self.subTest(draft=draft), self.assertRaises(ValueError):
                candidate.validate_release_page("v3.1.0-rc.1", [{"tag_name": "v3.1.0-rc.1", "draft": draft}])
        for page in ({"message": "not found"}, [None], [{}], [{"tag_name": "v3.0.0", "draft": "false"}],
                     [{"tag_name": "v3.0.0", "draft": False}] * 101):
            with self.subTest(page=page), self.assertRaises(ValueError):
                candidate.validate_release_page("v3.1.0-rc.1", page)

    def test_candidate_publication_is_manual_guarded_and_ci_gated(self):
        text = Path(__file__).parents[1].joinpath(".github/workflows/build-and-push.yaml").read_text()
        jobs = dict(re.findall(r"^  ([a-z-]+):\n(.*?)(?=^  [a-z-]+:\n|\Z)", text, re.M | re.S))
        publication = jobs["candidate-release"]
        image_step = next(step for step in publication.split("\n      - ")
                          if "uses: docker/build-push-action@" in step)
        self.assertIn("\n          context: .\n", image_step,
                      "Build the checked-out approved SHA, not the workflow event's default Git context")
        for required in ("github.repository == 'stellwerk-labs/platform-orchestrator-runner'",
                         "github.event_name == 'workflow_dispatch'", "environment: public-release-candidate",
                         "- candidate-preflight", "- test-unit", "- test-integration", "- helm-validate",
                         "--verify-tag --prerelease --latest=false",
                         "ref: ${{ inputs.candidate_sha }}", "--check-image-absent \"$GITHUB_REPOSITORY\""):
            self.assertIn(required, publication)
        for forbidden in ("git tag ", ":latest", "--force"):
            self.assertNotIn(forbidden, publication)
        self.assertIn("/environments/public-release-candidate", jobs["candidate-preflight"])

    def test_stable_tag_publication_excludes_rcs_and_forks(self):
        text = Path(__file__).parents[1].joinpath(".github/workflows/build-and-push.yaml").read_text()
        jobs = dict(re.findall(r"^  ([a-z-]+):\n(.*?)(?=^  [a-z-]+:\n|\Z)", text, re.M | re.S))
        stable = jobs["stable-release"]
        stable_condition = next(line for line in stable.splitlines() if line.strip().startswith("if:"))
        self.assertIn("github.repository == 'stellwerk-labs/platform-orchestrator-runner'", stable_condition)
        self.assertIn("github.event_name == 'push'", stable_condition)
        self.assertIn("startsWith(github.ref, 'refs/tags/v')", stable_condition)
        self.assertIn("!contains(github.ref_name, '-rc.')", stable_condition)
        for gate in ("test-unit", "test-integration", "helm-validate"):
            self.assertIn("- " + gate, stable)
            self.assertIn("inputs.candidate_sha || github.ref", jobs[gate])

    def test_candidate_pushes_do_not_duplicate_manual_release_gates(self):
        workflows = Path(__file__).parents[1] / ".github/workflows"
        ci_trigger = (workflows / "ci.yaml").read_text().split("\nenv:", 1)[0]
        self.assertIn('    branches-ignore:\n      - "release/module-management-rc.*"', ci_trigger)
        self.assertIn('    tags-ignore:\n      - "v*-rc.*"', ci_trigger)
        release_trigger = (workflows / "build-and-push.yaml").read_text().split("\npermissions:", 1)[0]
        self.assertIn("  workflow_dispatch:\n", release_trigger)
        self.assertIn('    tags:\n      - "v*.*.*"\n      - "!v*-rc.*"', release_trigger)


if __name__ == "__main__":
    unittest.main()
