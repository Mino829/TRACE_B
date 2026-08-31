"""Offline tests: no Google Cloud/GitHub requests and no production DB access."""

import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("ci_staging", Path(__file__).resolve().parents[1] / "ci_staging.py")
ci = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ci)
SHA = "a" * 40
BASELINE = "b" * 40
OLD_IMAGE = f"{ci.IMAGE_BASE}:initial-verified"
NEW_IMAGE = f"{ci.IMAGE_BASE}@sha256:{'c' * 64}"
URL = "https://trace-backend-example-an.a.run.app"
CANDIDATE_URL = "https://ci-123-1---trace-backend-example-an.a.run.app"


def runtime(mode):
    values = {"DB_HOST": f"/cloudsql/{ci.PROJECT}:{ci.REGION}:trace-postgres", "DB_NAME": "trace",
              "DB_USER": "trace_app", "TRACE_REGISTRATION_OPEN": "false", "TRACE_CONTACT": "keep this value"}
    env = [{"name": name, "value": value} for name, value in values.items()]
    env.append({"name": "DB_PASSWORD", "valueFrom": {"secretKeyRef": {"name": "trace-app-db-password", "key": "1"}}})
    return {"serviceAccountName": ci.RUNTIME_SA, "containers": [{"image": OLD_IMAGE, "args": [mode], "env": env}]}


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        env = {"GITHUB_REPOSITORY": ci.REPOSITORY, "GITHUB_EVENT_NAME": "push", "GITHUB_REF": "refs/heads/main",
               "GITHUB_SHA": SHA, "GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "1", "RUNNER_TEMP": self.temp.name,
               "GH_TOKEN": "fake-gh-token", "GCP_WIF_PROVIDER": "test-provider", "STAGING_SCHEMA_BASELINE": BASELINE,
               "GITHUB_OUTPUT": f"{self.temp.name}/outputs", "GITHUB_STEP_SUMMARY": f"{self.temp.name}/summary",
               "TRACE_ID_TOKEN": "fake-id-token"}
        self.env_patch = patch.dict(os.environ, env)
        self.env_patch.start()
        self.addCleanup(self.env_patch.stop)
        self.service = {"metadata": {"annotations": {}}, "spec": {"template": {"spec": runtime("serve")}},
                        "status": {"url": URL, "conditions": [{"type": "Ready", "status": "True"}],
                                   "latestCreatedRevisionName": "old-revision", "latestReadyRevisionName": "old-revision",
                                   "traffic": [{"revisionName": "old-revision", "percent": 100}]}}
        self.job = {"spec": {"template": {"spec": {"template": {"spec": runtime("cleanup")}}}}}
        self.policy = {"bindings": []}
        self.calls = []
        self.fail_execution = False
        self.fail_job_update = False
        self.http_failure = None
        self.build_result = None
        self.cloud_patch = patch.object(ci, "cloud", side_effect=self.cloud)
        self.http_patch = patch.object(ci, "request_json", side_effect=self.http)
        self.cloud_patch.start()
        self.http_patch.start()
        self.addCleanup(self.cloud_patch.stop)
        self.addCleanup(self.http_patch.stop)
        self.release = ci.Release()

    def http(self, url, token):
        if url.startswith("https://api.github.com/"):
            return {"object": {"sha": SHA}}
        if token is None:
            raise ci.urllib.error.HTTPError(url, 403, "Authentication required", {}, None)
        if self.http_failure and self.http_failure in url:
            return {"status": "bad", "registrationOpen": True}
        return {"status": "ready"} if url.endswith("/health/ready") else {"registrationOpen": False}

    def cloud(self, *args, **kwargs):
        self.calls.append(args)
        prefix = args[:3]
        if prefix == ("run", "services", "describe"):
            return copy.deepcopy(self.service)
        if prefix == ("run", "jobs", "describe"):
            return copy.deepcopy(self.job)
        if prefix == ("run", "services", "get-iam-policy"):
            return copy.deepcopy(self.policy)
        if args[:2] == ("builds", "submit"):
            image = next(arg.split("=", 2)[2] for arg in args if arg.startswith("--substitutions=_IMAGE="))
            self.build_result = {"id": "build-1", "status": "SUCCESS", "results": {"images": [{"name": image, "digest": "sha256:" + "c" * 64}]}}
            return {"id": "build-1"}
        if args[:2] == ("builds", "describe"):
            return copy.deepcopy(self.build_result)
        if prefix == ("run", "services", "update"):
            self.assertIn("--no-traffic", args)
            self.service["spec"]["template"]["spec"]["containers"][0]["image"] = NEW_IMAGE
            self.service["status"]["traffic"].append({"tag": "ci-123-1", "revisionName": "new-revision", "url": CANDIDATE_URL})
        elif prefix == ("run", "jobs", "update"):
            image = next(arg.split("=", 1)[1] for arg in args if arg.startswith("--image="))
            ci.job_spec(self.job)["containers"][0]["image"] = image
            if self.fail_job_update and image == NEW_IMAGE:
                raise RuntimeError("simulated uncertain update after server accepted image")
        elif prefix == ("run", "jobs", "execute"):
            if self.fail_execution:
                raise RuntimeError("simulated cleanup failure")
        elif prefix == ("run", "services", "update-traffic"):
            destinations = [arg for arg in args if arg.startswith("--to-revisions=")]
            if destinations:
                revision = destinations[0].split("=")[1]
                for item in self.service["status"]["traffic"]:
                    item.pop("percent", None)
                self.service["status"]["traffic"].insert(0, {"revisionName": revision, "percent": 100})
            if "--remove-tags=ci-123-1" in args:
                self.service["status"]["traffic"] = [item for item in self.service["status"]["traffic"] if item.get("tag") != "ci-123-1"]
        else:
            raise AssertionError(f"Unexpected gcloud operation: {args}")
        return {}

    def candidate(self):
        self.release.prepare()
        self.release.build()
        self.release.candidate()

    def test_trusted_push_only(self):
        for key, value in [("GITHUB_EVENT_NAME", "pull_request"), ("GITHUB_EVENT_NAME", "pull_request_target"),
                           ("GITHUB_REF", "refs/heads/feature"), ("GITHUB_REPOSITORY", "attacker/TRACE_B")]:
            with self.subTest(key=key, value=value), patch.dict(os.environ, {key: value}):
                with self.assertRaises(ci.RefuseRelease):
                    ci.Release()
        self.assertEqual(self.calls, [])

    def test_schema_change_stops_before_cloud_access(self):
        with patch.object(ci, "run", side_effect=[SHA, "", "internal/store/migrations/002.sql"]):
            with self.assertRaisesRegex(ci.RefuseRelease, "DB migration/grants changed"):
                self.release.guard()
        self.assertEqual(self.calls, [])

    def test_clean_schema_guard(self):
        with patch.object(ci, "run", side_effect=[SHA, "", ""]) as command:
            self.release.guard()
            self.assertIn("deploy/gcp/runtime-grants.sql", command.call_args.args)

    def test_unknown_schema_baseline_stops(self):
        with patch.object(ci, "run", side_effect=[SHA, subprocess.CalledProcessError(1, ["git"])]):
            with self.assertRaises(subprocess.CalledProcessError):
                self.release.guard()

    def test_stale_commit_stops(self):
        with patch.object(ci, "request_json", return_value={"object": {"sha": "d" * 40}}):
            with self.assertRaisesRegex(ci.RefuseRelease, "stale"):
                self.release.newest()

    def test_public_iam_is_rejected(self):
        for member in ["allUsers", "allAuthenticatedUsers"]:
            self.policy = {"bindings": [{"members": [member]}]}
            with self.assertRaisesRegex(ci.RefuseRelease, "Public IAM"):
                self.release.prepare()

    def test_disabled_iam_is_rejected(self):
        self.service["metadata"]["annotations"]["run.googleapis.com/invoker-iam-disabled"] = "true"
        with self.assertRaisesRegex(ci.RefuseRelease, "IAM authentication"):
            self.release.prepare()

    def test_public_http_is_rejected_without_a_resource_binding(self):
        with patch.object(ci, "request_json", return_value={"status": "ready"}):
            with self.assertRaisesRegex(ci.RefuseRelease, "Unauthenticated"):
                self.release.prepare()

    def test_anonymous_server_error_is_not_treated_as_authentication(self):
        with patch.object(ci, "request_json", side_effect=ci.urllib.error.HTTPError(URL, 503, "Unavailable", {}, None)):
            with self.assertRaises(ci.urllib.error.HTTPError):
                ci.require_private(URL)

    def test_wrong_identity_or_open_registration_is_rejected(self):
        for mode in ("serve", "cleanup"):
            spec = runtime(mode)
            spec["serviceAccountName"] = "trace-migration@example.invalid"
            with self.assertRaises(ci.RefuseRelease):
                ci.check_runtime(spec, mode)
            spec = runtime(mode)
            next(item for item in spec["containers"][0]["env"] if item["name"] == "TRACE_REGISTRATION_OPEN")["value"] = "true"
            with self.assertRaises(ci.RefuseRelease):
                ci.check_runtime(spec, mode)

    def test_database_url_and_wrong_secret_are_rejected(self):
        spec = runtime("serve")
        spec["containers"][0]["env"].append({"name": "DATABASE_URL", "value": "do-not-log"})
        with self.assertRaisesRegex(ci.RefuseRelease, "DATABASE_URL"):
            ci.check_runtime(spec, "serve")
        spec = runtime("serve")
        spec["containers"][0]["env"][-1]["valueFrom"]["secretKeyRef"]["key"] = "latest"
        with self.assertRaisesRegex(ci.RefuseRelease, "numbered"):
            ci.check_runtime(spec, "serve")

    def test_split_traffic_requires_manual_review(self):
        self.service["status"]["traffic"] = [{"revisionName": "old", "percent": 90}, {"revisionName": "other", "percent": 10}]
        with self.assertRaises(ci.RefuseRelease):
            self.release.prepare()

    def test_failed_build_never_deploys(self):
        self.release.prepare()
        with patch.object(ci, "cloud", return_value={"id": "build-1", "status": "FAILURE"}):
            with self.assertRaises(ci.RefuseRelease):
                self.release.build()
        self.assertFalse(any(call[:3] == ("run", "services", "update") for call in self.calls))

    def test_missing_digest_never_deploys(self):
        self.release.prepare()
        with patch.object(ci, "cloud", return_value={"id": "build-1", "status": "SUCCESS", "results": {"images": []}}):
            with self.assertRaisesRegex(ci.RefuseRelease, "digest"):
                self.release.build()

    def test_success_preserves_config_and_promotes_digest(self):
        old_config = copy.deepcopy(self.service["spec"]["template"]["spec"]["containers"][0]["env"])
        self.candidate()
        self.assertEqual(ci.active_revision(self.service), "old-revision")
        self.release.verify()
        self.release.cleanup_job()
        self.release.promote()
        self.release.finish()
        self.assertEqual(ci.active_revision(self.service), "new-revision")
        self.assertEqual(ci.job_spec(self.job)["containers"][0]["image"], NEW_IMAGE)
        self.assertEqual(old_config, self.service["spec"]["template"]["spec"]["containers"][0]["env"])
        self.assertTrue(self.release.state["success"])
        self.assertFalse(any(item.get("tag") == "ci-123-1" for item in self.service["status"]["traffic"]))
        commands = " ".join(" ".join(call) for call in self.calls)
        for forbidden in ("--set-env-vars", "--set-secrets", "--allow-unauthenticated", "trace-migrate", "add-iam-policy-binding"):
            self.assertNotIn(forbidden, commands)
        self.assertNotIn("fake-id-token", self.release.state_path.read_text())
        self.assertNotIn("fake-gh-token", self.release.state_path.read_text())

    def test_candidate_health_failure_leaves_old_traffic_and_job(self):
        self.candidate()
        self.http_failure = CANDIDATE_URL
        with self.assertRaises(ci.RefuseRelease):
            self.release.verify()
        self.release.rollback()
        self.release.finish()
        self.assertEqual(ci.active_revision(self.service), "old-revision")
        self.assertEqual(ci.job_spec(self.job)["containers"][0]["image"], OLD_IMAGE)

    def test_cleanup_failure_rolls_back_only_job(self):
        self.candidate()
        self.release.verify()
        self.fail_execution = True
        with self.assertRaises(RuntimeError):
            self.release.cleanup_job()
        ci.Release().rollback()  # State must survive separate workflow steps.
        self.assertEqual(ci.active_revision(self.service), "old-revision")
        self.assertEqual(ci.job_spec(self.job)["containers"][0]["image"], OLD_IMAGE)

    def test_uncertain_job_update_is_recovered(self):
        self.candidate()
        self.release.verify()
        self.fail_job_update = True
        with self.assertRaises(RuntimeError):
            self.release.cleanup_job()
        ci.Release().rollback()
        self.assertEqual(ci.job_spec(self.job)["containers"][0]["image"], OLD_IMAGE)

    def test_post_switch_failure_restores_api_and_cleanup(self):
        self.candidate()
        self.release.verify()
        self.release.cleanup_job()
        self.http_failure = URL
        with self.assertRaises(ci.RefuseRelease):
            self.release.promote()
        self.release.rollback()
        self.assertEqual(ci.active_revision(self.service), "old-revision")
        self.assertEqual(ci.job_spec(self.job)["containers"][0]["image"], OLD_IMAGE)

    def test_external_changes_are_not_overwritten(self):
        self.candidate()
        self.release.verify()
        self.release.cleanup_job()
        ci.job_spec(self.job)["containers"][0]["image"] = "someone-elses-image"
        with self.assertRaisesRegex(ci.RefuseRelease, "changed outside"):
            self.release.promote()
        with self.assertRaisesRegex(ci.RefuseRelease, "refusing to overwrite"):
            self.release.rollback()
        self.assertEqual(ci.job_spec(self.job)["containers"][0]["image"], "someone-elses-image")

    def test_unverified_steps_are_rejected(self):
        self.candidate()
        with self.assertRaises(ci.RefuseRelease):
            self.release.cleanup_job()
        with self.assertRaises(ci.RefuseRelease):
            self.release.promote()

    def test_gcloud_commands_always_target_staging(self):
        with patch.object(ci, "run", return_value='{"ok": true}') as command:
            self.cloud_patch.stop()
            try:
                self.assertEqual(ci.cloud("run", "services", "describe", ci.SERVICE), {"ok": True})
            finally:
                self.cloud_patch.start()
            self.assertIn("--project=trace-staging-507216", command.call_args.args)
            self.assertIn("--region=asia-northeast1", command.call_args.args)

    def test_rollback_attempts_job_recovery_even_if_traffic_recovery_fails(self):
        self.candidate()
        self.release.verify()
        self.release.cleanup_job()
        self.release.state["traffic_attempted"] = True
        self.service["status"]["traffic"] = [{"revisionName": "external-revision", "percent": 100}]
        with self.assertRaisesRegex(ci.RefuseRelease, "Manual recovery"):
            self.release.rollback()
        self.assertEqual(ci.active_revision(self.service), "external-revision")
        self.assertEqual(ci.job_spec(self.job)["containers"][0]["image"], OLD_IMAGE)

    def test_url_and_redirect_safety(self):
        for url in ("http://example.run.app", "https://example.com", "https://example.run.app.evil.test", "https://user@example.run.app", "https://example.run.app/?x=1"):
            with self.subTest(url=url), self.assertRaises(ci.RefuseRelease):
                ci.service_url(url)
        with self.assertRaises(ci.RefuseRelease):
            ci.NoRedirect().redirect_request(None, None, 302, "", {}, "https://evil.test")


if __name__ == "__main__":
    unittest.main()
