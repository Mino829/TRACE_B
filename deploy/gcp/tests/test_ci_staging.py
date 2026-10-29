"""Offline safety/state-transition tests: no Cloud, GitHub or database access."""

import copy
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("ci_staging", Path(__file__).resolve().parents[1] / "ci_staging.py")
ci = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ci)
SHA = "a" * 40
BASELINE = "b" * 40
OLD_IMAGE = f"{ci.IMAGE_BASE}@sha256:{'b' * 64}"
NEW_IMAGE = f"{ci.IMAGE_BASE}@sha256:{'c' * 64}"
URL = "https://trace-backend-preview-example-an.a.run.app"
CANDIDATE_URL = "https://ci-123-1---trace-backend-preview-example-an.a.run.app"
EDGE_SECRET = "offline-preview-edge-key-not-a-real-secret"
BUNDLE = {"coordinateVersion": "coordinates-reviewed", "campusGraphVersion": "graph-reviewed",
          "threeDMapVersion": "map-reviewed"}


def runtime(mode):
    values = {"DB_HOST": f"/cloudsql/{ci.PROJECT}:{ci.REGION}:trace-postgres", "DB_NAME": "trace",
              "DB_USER": "trace_app", "DB_SSLMODE": "disable", "TRACE_DB_MAX_CONNECTIONS": "5",
              **{name: "false" for name in ci.FEATURE_FLAGS},
              **{name: BUNDLE[field] for field, name in ci.BUNDLE_ENV.items()}}
    env = [{"name": name, "value": value} for name, value in values.items()]
    env.append({"name": "DB_PASSWORD", "valueFrom": {"secretKeyRef": {"name": "trace-app-db-password", "key": "1"}}})
    if mode == "serve":
        env.append({"name": "TRACE_EDGE_SHARED_SECRET", "valueFrom": {
            "secretKeyRef": {"name": "trace-preview-edge-secret", "key": "2"}}})
    identity = ci.API_RUNTIME_SA if mode == "serve" else ci.CLEANUP_RUNTIME_SA
    return {"serviceAccountName": identity, "containers": [{"image": OLD_IMAGE, "args": [mode], "env": env,
            "resources": {"limits": {"cpu": "1", "memory": "512Mi"}}}]}


def environment(spec, name):
    return next(item for item in spec["containers"][0]["env"] if item["name"] == name)


def closed_config():
    return {"registrationOpen": False, "livePresenceEnabled": False, "photo": {"enabled": False},
            "notification": {"enabled": False}, "spatial": dict(BUNDLE)}


@contextmanager
def edge_server(secret=EDGE_SECRET, *, unprotected=False, redirect=False):
    requests = []

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            requests.append((self.path, self.headers.get("Authorization")))
            if redirect and self.headers.get("X-Trace-Edge-Secret"):
                self.send_response(302)
                self.send_header("Location", "/credential-leak")
                self.end_headers()
                return
            if self.path == "/health/ready":
                payload = {"status": "ready"}
            elif self.path == "/v1/public/config":
                if not unprotected and self.headers.get("X-Trace-Edge-Secret") != secret:
                    self.send_error(403)
                    return
                payload = closed_config()
            else:
                self.send_error(404)
                return
            body = json.dumps(payload).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args):
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=lambda: server.serve_forever(poll_interval=0.01), daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}", requests
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        env = {"GITHUB_REPOSITORY": ci.REPOSITORY, "GITHUB_EVENT_NAME": "push", "GITHUB_REF": "refs/heads/main",
               "GITHUB_SHA": SHA, "GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "1", "RUNNER_TEMP": self.temp.name,
               "GH_TOKEN": "offline-gh-token", "GCP_WIF_PROVIDER": "offline-provider", "STAGING_SCHEMA_BASELINE": BASELINE,
               "GITHUB_STEP_SUMMARY": f"{self.temp.name}/summary",
               "TRACE_STAGING_EDGE_SHARED_SECRET": EDGE_SECRET}
        self.env_patch = patch.dict(os.environ, env, clear=True)
        self.env_patch.start()
        self.addCleanup(self.env_patch.stop)
        self.service = {"metadata": {"generation": 1, "annotations": {"run.googleapis.com/maxScale": "1"}},
                        "spec": {"template": {"metadata": {"annotations": {"autoscaling.knative.dev/maxScale": "12"}},
                                              "spec": runtime("serve")}},
                        "status": {"observedGeneration": 1, "url": URL,
                                   "conditions": [{"type": "Ready", "status": "True"}],
                                   "latestCreatedRevisionName": "old-revision", "latestReadyRevisionName": "old-revision",
                                   "traffic": [{"revisionName": "old-revision", "percent": 100}]}}
        # Maintenance must not require an old-schema stopped service to have new runtime settings.
        self.private = {"metadata": {"generation": 1, "annotations": {"run.googleapis.com/scalingMode": "manual",
                                                    "run.googleapis.com/manualInstanceCount": "0"}},
                        "status": {"observedGeneration": 1, "conditions": [{"type": "Ready", "status": "False"}]}}
        self.job = {"spec": {"template": {"spec": {"template": {"spec": runtime("cleanup")}}}}}
        self.policy = {"bindings": [{"role": "roles/run.invoker", "members": ["allUsers"]}]}
        self.scheduler = {"state": "PAUSED"}
        self.executions = []
        self.writes = []
        self.fail_execution = False
        self.fail_job_update = False
        self.build_status = "SUCCESS"
        self.build_digest = "sha256:" + "c" * 64
        self.http_failure = None
        self.config = closed_config()
        self.cloud_patch = patch.object(ci, "cloud", side_effect=self.cloud)
        self.http_patch = patch.object(ci, "request_json", side_effect=self.http)
        self.cloud_patch.start()
        self.http_patch.start()
        self.addCleanup(self.cloud_patch.stop)
        self.addCleanup(self.http_patch.stop)
        self.release = ci.Release()

    def http(self, url, token, *, edge_secret=None):
        if url.startswith("https://api.github.com/"):
            return {"object": {"sha": SHA}}
        if url.endswith("/v1/public/config") and edge_secret != EDGE_SECRET:
            error = ci.urllib.error.HTTPError(url, 403, "Edge access denied", {}, None)
            error.close()
            raise error
        if self.http_failure and url.startswith(self.http_failure):
            return {"status": "bad", "registrationOpen": True}
        return {"status": "ready"} if url.endswith("/health/ready") else copy.deepcopy(self.config)

    def cloud(self, *args, **kwargs):
        prefix = args[:3]
        if prefix == ("run", "services", "describe"):
            return copy.deepcopy(self.private if args[3] == ci.STOPPED_SERVICE else self.service)
        if prefix == ("run", "jobs", "describe"):
            return copy.deepcopy(self.job)
        if prefix == ("run", "services", "get-iam-policy"):
            return copy.deepcopy(self.policy)
        if prefix == ("scheduler", "jobs", "describe"):
            return copy.deepcopy(self.scheduler)
        if args[:4] == ("run", "jobs", "executions", "list"):
            return copy.deepcopy(self.executions)
        if args[:2] == ("builds", "describe"):
            return {"id": "build-1", "status": self.build_status,
                    "results": {"images": [{"name": self.build_image, "digest": self.build_digest}]}}
        self.writes.append(args)
        if args[:2] == ("builds", "submit"):
            self.build_image = next(arg.split("=", 2)[2] for arg in args if arg.startswith("--substitutions=_IMAGE="))
            return {"id": "build-1"}
        if prefix == ("run", "services", "update"):
            self.service["spec"]["template"]["spec"]["containers"][0]["image"] = NEW_IMAGE
            self.service["status"].update(latestCreatedRevisionName="new-revision", latestReadyRevisionName="new-revision")
            self.service["status"]["traffic"].append({"tag": "ci-123-1", "revisionName": "new-revision", "url": CANDIDATE_URL})
        elif prefix == ("run", "jobs", "update"):
            image = next(arg.split("=", 1)[1] for arg in args if arg.startswith("--image="))
            ci.job_spec(self.job)["containers"][0]["image"] = image
            if self.fail_job_update and image == NEW_IMAGE:
                raise RuntimeError("uncertain update after server accepted image")
        elif prefix == ("run", "jobs", "execute"):
            if self.fail_execution:
                raise RuntimeError("cleanup failed")
        elif prefix == ("run", "services", "update-traffic"):
            destinations = [arg for arg in args if arg.startswith("--to-revisions=")]
            if destinations:
                revision = destinations[0].split("=")[1]
                for item in self.service["status"]["traffic"]:
                    item.pop("percent", None)
                self.service["status"]["traffic"].insert(0, {"revisionName": revision, "percent": 100})
            else:
                self.service["status"]["traffic"] = [item for item in self.service["status"]["traffic"] if item.get("tag") != "ci-123-1"]
        else:
            raise AssertionError(f"Unexpected cloud mutation: {args}")
        return {}

    def candidate(self):
        self.release.prepare()
        self.release.build()
        self.release.candidate()

    def maintenance(self):
        self.service["metadata"]["annotations"] = {"run.googleapis.com/scalingMode": "manual",
                                                     "run.googleapis.com/manualInstanceCount": "0"}
        self.service["spec"] = {}  # Old schema/runtime is intentionally irrelevant here.

    def test_readonly_commands_work_without_github_identity_and_do_not_write(self):
        with patch.dict(os.environ, {"TRACE_STAGING_EDGE_SHARED_SECRET": EDGE_SECRET}, clear=True):
            self.assertEqual(ci.main(["check-active"]), 0)
            self.maintenance()
            with patch.dict(os.environ, {}, clear=True):
                self.assertEqual(ci.main(["check-maintenance"]), 0)
        self.assertEqual(self.writes, [])

    def test_maintenance_rejects_writers_and_unsettled_executions(self):
        self.maintenance()
        stopped = copy.deepcopy(self.service)
        private = copy.deepcopy(self.private)
        for target in (self.service, self.private):
            for annotations in ({}, {"run.googleapis.com/scalingMode": "manual", "run.googleapis.com/manualInstanceCount": "1"},
                                {"run.googleapis.com/scalingMode": "automatic", "run.googleapis.com/manualInstanceCount": "0"}):
                saved = target["metadata"]["annotations"]
                target["metadata"]["annotations"] = annotations
                with self.assertRaises(ci.RefuseRelease):
                    ci.check_maintenance()
                target["metadata"]["annotations"] = saved
        self.service, self.private = stopped, private
        self.scheduler["state"] = "ENABLED"
        with self.assertRaises(ci.RefuseRelease):
            ci.check_maintenance()
        self.scheduler["state"] = "PAUSED"
        for status in ({}, {"runningCount": 1}, {"completionTime": "finished", "runningCount": 1}):
            self.executions = [{"status": status}]
            with self.assertRaises(ci.RefuseRelease):
                ci.check_maintenance()
        self.executions = [{"status": {"completionTime": "finished", "failedCount": 1}}]
        ci.check_maintenance()
        self.assertEqual(self.writes, [])

    def test_tag_only_revisions_cannot_bypass_maintenance_zero(self):
        self.maintenance()
        for target in (self.service, self.private):
            for section in ("spec", "status"):
                with self.subTest(target=target is self.private, section=section):
                    scope = target.setdefault(section, {})
                    saved = scope.get("traffic")
                    scope["traffic"] = [{"tag": "legacy-writer", "revisionName": "old-schema"}]
                    with self.assertRaises(ci.RefuseRelease):
                        ci.check_maintenance()
                    if saved is None:
                        del scope["traffic"]
                    else:
                        scope["traffic"] = saved
        self.assertEqual(self.writes, [])

    def test_stopped_service_requires_reconciliation_not_serving_readiness(self):
        self.maintenance()
        self.service["status"]["conditions"] = [{"type": "Ready", "status": "False"}]
        ci.check_maintenance()
        for metadata, status in (({}, {}), ({"generation": 2}, {"observedGeneration": 1})):
            with self.assertRaises(ci.RefuseRelease):
                ci.require_reconciled({"metadata": metadata, "status": status}, serving=False)
        self.assertEqual(self.writes, [])

    def test_unreviewed_tag_blocks_release_before_build_and_before_cleanup(self):
        self.service["status"]["traffic"].append({"tag": "legacy", "revisionName": "old-schema"})
        with self.assertRaises(ci.RefuseRelease):
            self.release.prepare()
        self.assertEqual(self.writes, [])
        self.service["status"]["traffic"].pop()
        self.candidate()
        mutations = len(self.writes)
        self.service["status"]["traffic"].append({"tag": "outside-run", "revisionName": "external"})
        with self.assertRaises(ci.RefuseRelease):
            self.release.verify()
        self.assertEqual(len(self.writes), mutations)

    def test_pool_budget_or_unused_credentials_block_release_before_build(self):
        for spec in (self.service["spec"]["template"]["spec"], ci.job_spec(self.job)):
            pool = environment(spec, "TRACE_DB_MAX_CONNECTIONS")
            for value in ("0", "6", "invalid"):
                with self.subTest(pool=value):
                    pool["value"] = value
                    with self.assertRaises(ci.RefuseRelease):
                        self.release.prepare()
            pool["value"] = "5"
            for name in ci.API_SECRETS:
                if name == "TRACE_EDGE_SHARED_SECRET":
                    continue
                spec["containers"][0]["env"].append({"name": name, "value": "unused-offline-credential"})
                with self.subTest(unused=name), self.assertRaises(ci.RefuseRelease):
                    self.release.prepare()
                spec["containers"][0]["env"].pop()
        self.assertEqual(self.writes, [])

    def test_active_private_service_blocks_ci_before_mutations(self):
        self.private["metadata"]["annotations"]["run.googleapis.com/manualInstanceCount"] = "1"
        with self.assertRaises(ci.RefuseRelease):
            self.release.prepare()
        self.assertEqual(self.writes, [])

    def test_effective_service_scaling_and_cpu_profile_are_required(self):
        original = copy.deepcopy(self.service)
        changes = [("run.googleapis.com/maxScale", None), ("run.googleapis.com/maxScale", "2"),
                   ("run.googleapis.com/minScale", "1"), ("run.googleapis.com/scalingMode", "manual"),
                   ("run.googleapis.com/scalingMode", "session"), ("run.googleapis.com/manualInstanceCount", "1")]
        for name, value in changes:
            self.service = copy.deepcopy(original)
            annotations = self.service["metadata"]["annotations"]
            annotations.pop(name, None) if value is None else annotations.update({name: value})
            with self.subTest(annotation=name, value=value), self.assertRaises(ci.RefuseRelease):
                ci.check_active()
        for cpu, memory, throttling, revision_min in (("2", "512Mi", "true", "0"), ("1", "1Gi", "true", "0"),
                                                       ("1", "512Mi", "false", "0"), ("1", "512Mi", "true", "1")):
            self.service = copy.deepcopy(original)
            template = self.service["spec"]["template"]
            template["spec"]["containers"][0]["resources"]["limits"] = {"cpu": cpu, "memory": memory}
            template["metadata"]["annotations"].update({"run.googleapis.com/cpu-throttling": throttling,
                                                        "autoscaling.knative.dev/minScale": revision_min})
            with self.assertRaises(ci.RefuseRelease):
                ci.check_active()
        self.service = original
        ci.check_active()  # A revision max=12 is bounded by the required service max=1.
        self.assertEqual(self.writes, [])

    def test_iam_and_edge_reference_must_preserve_preview_boundary(self):
        for policy in ({"bindings": []}, {"bindings": [{"role": "roles/viewer", "members": ["allUsers"]}]},
                       {"bindings": [{"role": "roles/run.invoker", "members": ["allUsers"], "condition": {"expression": "false"}}]}):
            with patch.object(self, "policy", policy), self.assertRaises(ci.RefuseRelease):
                self.release.prepare()
        self.service["metadata"]["annotations"]["run.googleapis.com/invoker-iam-disabled"] = "true"
        with self.assertRaises(ci.RefuseRelease):
            self.release.prepare()
        del self.service["metadata"]["annotations"]["run.googleapis.com/invoker-iam-disabled"]
        edge = environment(self.service["spec"]["template"]["spec"], "TRACE_EDGE_SHARED_SECRET")
        for value in ({"name": "TRACE_EDGE_SHARED_SECRET", "value": EDGE_SECRET},
                      {"name": "TRACE_EDGE_SHARED_SECRET"},
                      {"name": "TRACE_EDGE_SHARED_SECRET", "valueFrom": {"secretKeyRef": {"name": "trace-preview-edge-secret", "key": "latest"}}}):
            saved = copy.deepcopy(edge)
            edge.clear()
            edge.update(value)
            with self.assertRaises(ci.RefuseRelease):
                self.release.prepare()
            edge.clear()
            edge.update(saved)
        self.assertEqual(self.writes, [])

    def test_identity_split_app_credentials_and_closed_flags_are_required(self):
        for mode, spec in (("serve", self.service["spec"]["template"]["spec"]), ("cleanup", ci.job_spec(self.job))):
            original = copy.deepcopy(spec)
            spec["serviceAccountName"] = ci.CLEANUP_RUNTIME_SA if mode == "serve" else ci.API_RUNTIME_SA
            with self.assertRaises(ci.RefuseRelease):
                ci.check_active()
            spec.clear()
            spec.update(copy.deepcopy(original))
            for flag in ci.FEATURE_FLAGS:
                environment(spec, flag)["value"] = "true"
                with self.assertRaises(ci.RefuseRelease):
                    ci.check_active()
                environment(spec, flag)["value"] = "false"
            for name, value in (("DB_HOST", "/cloudsql/wrong:region:instance"), ("DB_USER", "postgres")):
                saved = environment(spec, name)["value"]
                environment(spec, name)["value"] = value
                with self.assertRaises(ci.RefuseRelease):
                    ci.check_active()
                environment(spec, name)["value"] = saved
            password = environment(spec, "DB_PASSWORD")["valueFrom"]["secretKeyRef"]
            password["key"] = "latest"
            with self.assertRaises(ci.RefuseRelease):
                ci.check_active()
            password["key"] = "1"
        for name in ci.API_SECRETS:
            ci.job_spec(self.job)["containers"][0]["env"].append({"name": name, "value": "never-disclose"})
            with self.assertRaises(ci.RefuseRelease):
                ci.check_active()
            ci.job_spec(self.job)["containers"][0]["env"].pop()
        self.assertEqual(self.writes, [])

    def test_stable_images_and_bundle_credentials_must_agree(self):
        cleanup = ci.job_spec(self.job)
        for image in (f"{ci.IMAGE_BASE}:latest", NEW_IMAGE):
            cleanup["containers"][0]["image"] = image
            with self.assertRaises(ci.RefuseRelease):
                self.release.prepare()
        cleanup["containers"][0]["image"] = OLD_IMAGE
        for name in ("DB_PASSWORD", *ci.BUNDLE_ENV.values()):
            entry = environment(cleanup, name)
            saved = copy.deepcopy(entry)
            if name == "DB_PASSWORD":
                entry["valueFrom"]["secretKeyRef"]["key"] = "3"
            else:
                entry["value"] = "another-bundle"
            with self.assertRaises(ci.RefuseRelease):
                ci.check_active()
            entry.clear()
            entry.update(saved)
        self.assertEqual(self.writes, [])

    def test_unsettled_service_and_split_traffic_are_rejected(self):
        self.service["status"]["observedGeneration"] = 0
        with self.assertRaises(ci.RefuseRelease):
            ci.check_active()
        self.service["status"]["observedGeneration"] = 1
        self.service["status"]["traffic"] = [{"revisionName": "old", "percent": 90}, {"revisionName": "other", "percent": 10}]
        with self.assertRaises(ci.RefuseRelease):
            ci.check_active()
        self.assertEqual(self.writes, [])

    def test_trusted_push_schema_barrier_and_latest_commit(self):
        for key, value in (("GITHUB_EVENT_NAME", "pull_request"), ("GITHUB_EVENT_NAME", "pull_request_target"),
                           ("GITHUB_REF", "refs/heads/feature"), ("GITHUB_REPOSITORY", "attacker/TRACE_B")):
            with patch.dict(os.environ, {key: value}), self.assertRaises(ci.RefuseRelease):
                ci.Release()
        with patch.object(ci, "run", side_effect=[SHA, "", "internal/store/migrations/004.sql"]):
            with self.assertRaises(ci.RefuseRelease):
                self.release.guard()
        with patch.object(ci, "run", side_effect=[SHA, subprocess.CalledProcessError(1, ["git"])]):
            with self.assertRaises(subprocess.CalledProcessError):
                self.release.guard()
        with patch.object(ci, "run", side_effect=[SHA, "", ""]):
            self.release.guard()
        with patch.object(ci, "request_json", return_value={"object": {"sha": "d" * 40}}):
            with self.assertRaises(ci.RefuseRelease):
                self.release.newest()
        self.assertEqual(self.writes, [])

    def test_build_failure_or_missing_digest_never_deploys(self):
        for status, digest in (("FAILURE", self.build_digest), ("SUCCESS", "")):
            self.release.prepare()
            self.build_status, self.build_digest = status, digest
            with self.assertRaises(ci.RefuseRelease):
                self.release.build()
            self.assertEqual(ci.active_revision(self.service), "old-revision")
            self.assertEqual(self.service["spec"]["template"]["spec"]["containers"][0]["image"], OLD_IMAGE)
            self.assertEqual(ci.job_spec(self.job)["containers"][0]["image"], OLD_IMAGE)

    def test_success_preserves_runtime_and_promotes_immutable_image(self):
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
        ci.require_stopped(self.private, ci.STOPPED_SERVICE)
        for secret in (EDGE_SECRET, "offline-gh-token"):
            self.assertNotIn(secret, self.release.state_path.read_text())
            self.assertNotIn(secret, Path(os.environ["GITHUB_STEP_SUMMARY"]).read_text())

    def test_candidate_health_or_open_config_keeps_old_writers(self):
        self.candidate()
        for field, value in (("livePresenceEnabled", None), ("registrationOpen", True),
                             ("photo", {"enabled": True}), ("notification", {"enabled": True}),
                             ("spatial", {"coordinateVersion": "wrong"})):
            saved = copy.deepcopy(self.config)
            self.config[field] = value
            with self.assertRaises(ci.RefuseRelease):
                self.release.verify()
            self.config = saved
            self.assertFalse(self.release.state.get("candidate_verified"))
        self.release.rollback()
        self.release.finish()
        self.assertEqual(ci.active_revision(self.service), "old-revision")
        self.assertEqual(ci.job_spec(self.job)["containers"][0]["image"], OLD_IMAGE)

    def test_cleanup_failure_and_uncertain_update_restore_verified_old_job(self):
        self.candidate()
        self.release.verify()
        self.fail_job_update = True
        with self.assertRaises(RuntimeError):
            self.release.cleanup_job()
        ci.Release().rollback()
        self.assertEqual(ci.job_spec(self.job)["containers"][0]["image"], OLD_IMAGE)
        self.fail_job_update = False
        self.fail_execution = True
        with self.assertRaises(RuntimeError):
            self.release.cleanup_job()
        ci.Release().rollback()
        self.assertEqual(ci.active_revision(self.service), "old-revision")
        self.assertEqual(ci.job_spec(self.job)["containers"][0]["image"], OLD_IMAGE)

    def test_failed_or_uncertain_cutover_keeps_gps_free_candidate_and_cleanup(self):
        self.candidate()
        self.release.verify()
        self.release.cleanup_job()
        self.http_failure = URL
        with self.assertRaises(ci.RefuseRelease):
            self.release.promote()
        ci.Release().rollback()
        self.assertEqual(ci.active_revision(self.service), "new-revision")
        self.assertEqual(ci.job_spec(self.job)["containers"][0]["image"], NEW_IMAGE)
        self.service["status"]["traffic"] = [{"revisionName": "old-revision", "percent": 100}]
        ci.Release().rollback()
        self.assertEqual(ci.active_revision(self.service), "new-revision")
        ci.require_stopped(self.private, ci.STOPPED_SERVICE)

    def test_external_image_traffic_or_config_drift_is_not_overwritten(self):
        self.candidate()
        self.release.verify()
        self.release.cleanup_job()
        ci.job_spec(self.job)["containers"][0]["image"] = "external-image"
        with self.assertRaises(ci.RefuseRelease):
            self.release.promote()
        with self.assertRaises(ci.RefuseRelease):
            self.release.rollback()
        self.assertEqual(ci.job_spec(self.job)["containers"][0]["image"], "external-image")
        ci.job_spec(self.job)["containers"][0]["image"] = NEW_IMAGE
        self.release.state["traffic_attempted"] = True
        self.service["status"]["traffic"] = [{"revisionName": "external-revision", "percent": 100}]
        with self.assertRaises(ci.RefuseRelease):
            self.release.rollback()
        self.assertEqual(ci.active_revision(self.service), "external-revision")
        self.assertEqual(ci.job_spec(self.job)["containers"][0]["image"], NEW_IMAGE)
        environment(ci.job_spec(self.job), "TRACE_COORDINATE_VERSION")["value"] = "external-bundle"
        before = len(self.writes)
        with self.assertRaises(ci.RefuseRelease):
            self.release.rollback()
        self.assertEqual(len(self.writes), before)

    def test_external_runtime_change_blocks_candidate_verification_and_recovery(self):
        self.candidate()
        spec = self.service["spec"]["template"]["spec"]
        spec["containers"][0]["env"].append({"name": "TRACE_CONTACT", "value": "externally reviewed contact"})
        before = len(self.writes)
        for action in (self.release.verify, self.release.rollback):
            with self.assertRaises(ci.RefuseRelease):
                action()
        self.assertEqual(len(self.writes), before)
        self.assertEqual(ci.active_revision(self.service), "old-revision")
        self.assertEqual(ci.job_spec(self.job)["containers"][0]["image"], OLD_IMAGE)

    def test_unsafe_private_reactivation_blocks_later_mutations_and_recovery(self):
        self.candidate()
        self.release.verify()
        self.private["metadata"]["annotations"]["run.googleapis.com/manualInstanceCount"] = "1"
        before = len(self.writes)
        for action in (self.release.cleanup_job, self.release.rollback, self.release.finish):
            with self.assertRaises(ci.RefuseRelease):
                action()
        self.assertEqual(len(self.writes), before)

    def test_unverified_candidate_or_cleanup_never_promotes(self):
        self.candidate()
        with self.assertRaises(ci.RefuseRelease):
            self.release.cleanup_job()
        with self.assertRaises(ci.RefuseRelease):
            self.release.promote()
        self.assertEqual(ci.active_revision(self.service), "old-revision")
        self.assertEqual(ci.job_spec(self.job)["containers"][0]["image"], OLD_IMAGE)

    def test_wrong_probe_key_or_missing_edge_boundary_stops_before_mutations(self):
        with patch.dict(os.environ, {"TRACE_STAGING_EDGE_SHARED_SECRET": "another-visible-but-wrong-edge-key"}):
            with self.assertRaises(ci.urllib.error.HTTPError):
                self.release.prepare()
        with patch.object(ci, "request_json", return_value=closed_config()):
            with self.assertRaises(ci.RefuseRelease):
                self.release.prepare()
        self.assertEqual(self.writes, [])

    def test_candidate_edge_mismatch_never_updates_cleanup_or_traffic(self):
        self.candidate()
        before = len(self.writes)
        with patch.dict(os.environ, {"TRACE_STAGING_EDGE_SHARED_SECRET": "another-visible-but-wrong-edge-key"}):
            with self.assertRaises(ci.urllib.error.HTTPError):
                self.release.verify()
        self.assertFalse(self.release.state.get("candidate_verified"))
        self.assertEqual(len(self.writes), before)
        self.assertEqual(ci.active_revision(self.service), "old-revision")
        self.assertEqual(ci.job_spec(self.job)["containers"][0]["image"], OLD_IMAGE)

    def test_real_http_edge_boundary_wrong_key_and_redirect_safety(self):
        self.http_patch.stop()
        try:
            with edge_server() as (url, requests):
                ci.probe(url, BUNDLE)
                self.assertTrue(all(token is None for _, token in requests))
            for options, error in (({"unprotected": True}, ci.RefuseRelease),
                                   ({"secret": "different-edge-key"}, ci.urllib.error.HTTPError),
                                   ({"redirect": True}, ci.RefuseRelease)):
                with edge_server(**options) as (url, requests):
                    with self.assertRaises(error):
                        ci.probe(url, BUNDLE)
                    self.assertFalse(any(path == "/credential-leak" for path, _ in requests))
        finally:
            self.http_patch.start()

    def test_missing_or_unsafe_probe_key_refuses_without_disclosure(self):
        for secret in ("", "unsafe-newline\n" + "x" * 32):
            with patch.dict(os.environ, {"TRACE_STAGING_EDGE_SHARED_SECRET": secret}):
                error_output = io.StringIO()
                with patch("sys.stderr", error_output):
                    self.assertEqual(ci.main(["check-active"]), 1)
                if secret:
                    self.assertNotIn(secret, error_output.getvalue())
        self.assertEqual(self.writes, [])

    def test_url_safety_rejects_non_cloud_run_and_embedded_credentials(self):
        for url in ("http://example.run.app", "https://example.com", "https://example.run.app.evil.test",
                    "https://user@example.run.app", "https://example.run.app/?credential=value"):
            with self.assertRaises(ci.RefuseRelease):
                ci.service_url(url)


if __name__ == "__main__":
    unittest.main()
