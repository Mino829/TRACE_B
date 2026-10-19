#!/usr/bin/env python3
"""Image-only, fail-closed staging releases. Run only from the gated Actions job.

Uses Python's standard library plus git/gcloud; no DB credentials or DDL.
State lives in RUNNER_TEMP so source uploads cannot contain it or auth tokens.
"""

import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

PROJECT = "trace-staging-507216"
REGION = "asia-northeast1"
REPOSITORY = "Mino829/TRACE_B"
SERVICE = "trace-backend"
JOB = "trace-cleanup"
RUNTIME_SA = f"trace-runtime@{PROJECT}.iam.gserviceaccount.com"
BUILD_SA = f"trace-build@{PROJECT}.iam.gserviceaccount.com"
IMAGE_BASE = f"{REGION}-docker.pkg.dev/{PROJECT}/trace/backend"
SOURCE_BUCKET = f"gs://{PROJECT}-github-build-source/source"
SCHEMA_PATHS = ["internal/store/migrate.go", "internal/store/migrations", "deploy/gcp/runtime-grants.sql"]


class RefuseRelease(RuntimeError):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise RefuseRelease("Refusing a redirect during an authenticated request")


def request_json(url, token, *, edge_secret=None):
    headers = {"Accept": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    if edge_secret:
        headers["X-Trace-Edge-Secret"] = edge_secret
    req = urllib.request.Request(url, headers=headers)
    try:
        with urllib.request.build_opener(NoRedirect).open(req, timeout=10) as response:
            if response.status != 200:
                raise RefuseRelease(f"Unexpected HTTP status: {response.status}")
            return json.load(response)
    except urllib.error.HTTPError as error:
        error.close()
        raise


def run(*args, timeout=180):
    return subprocess.run(args, check=True, text=True, stdout=subprocess.PIPE, timeout=timeout).stdout.strip()


def cloud(*args, timeout=180):
    result = run("gcloud", *args, f"--project={PROJECT}", f"--region={REGION}", "--quiet", "--format=json", timeout=timeout)
    return json.loads(result) if result else {}


def service_url(value):
    parsed = urllib.parse.urlsplit(value)
    if (parsed.scheme != "https" or not parsed.hostname or not parsed.hostname.endswith(".run.app")
            or parsed.username or parsed.password or parsed.port or parsed.path not in ("", "/")
            or parsed.query or parsed.fragment):
        raise RefuseRelease("Expected a default HTTPS Cloud Run URL")
    return value.rstrip("/")


def require_private(url):
    # A service-level IAM policy alone cannot rule out inherited public access.
    try:
        request_json(f"{url}/health/ready", None)
    except urllib.error.HTTPError as error:
        error.close()
        if error.code in (401, 403):
            return
        raise
    raise RefuseRelease("Unauthenticated health request succeeded; refusing public staging")


def active_revision(service):
    traffic = [item for item in service["status"]["traffic"] if item.get("percent", 0) > 0]
    if len(traffic) != 1 or traffic[0]["percent"] != 100 or not traffic[0].get("revisionName"):
        raise RefuseRelease("Staging must route 100% to one known revision; review split traffic manually")
    return traffic[0]["revisionName"]


def job_spec(job):
    return job["spec"]["template"]["spec"]["template"]["spec"]


def check_runtime(spec, mode):
    if spec.get("serviceAccountName") != RUNTIME_SA or len(spec.get("containers", [])) != 1:
        raise RefuseRelease("Unexpected runtime identity or container count")
    container = spec["containers"][0]
    if container.get("args") != [mode]:
        raise RefuseRelease("Unexpected container command arguments")
    env = {item["name"]: item for item in container.get("env", [])}
    expected = {
        "DB_HOST": f"/cloudsql/{PROJECT}:{REGION}:trace-postgres",
        "DB_NAME": "trace", "DB_USER": "trace_app", "TRACE_REGISTRATION_OPEN": "false",
    }
    for key, value in expected.items():
        if env.get(key, {}).get("value") != value:
            raise RefuseRelease(f"Review existing runtime setting: {key}")
    if "DATABASE_URL" in env:
        raise RefuseRelease("DATABASE_URL overrides DB_*; review it manually before enabling CI")
    if "TRACE_SESSION_TTL" in env:
        raise RefuseRelease("TRACE_SESSION_TTL is obsolete; migrate service environment first")
    secret = env.get("DB_PASSWORD", {}).get("valueFrom", {}).get("secretKeyRef", {})
    if secret.get("name") != "trace-app-db-password" or not re.fullmatch(r"[1-9][0-9]*", secret.get("key", "")):
        raise RefuseRelease("DB_PASSWORD must reference a numbered app-password Secret version")
    if mode == "serve":
        edge = env.get("TRACE_EDGE_SHARED_SECRET", {})
        probe_secret = probe_edge_secret()
        if not edge or edge.get("value") == "":
            if probe_secret:
                raise RefuseRelease("Remove the unused STAGING_EDGE_SHARED_SECRET or configure the runtime edge Secret")
        else:
            ref = edge.get("valueFrom", {}).get("secretKeyRef", {})
            if not ref.get("name") or not re.fullmatch(r"[1-9][0-9]*", ref.get("key", "")):
                raise RefuseRelease("TRACE_EDGE_SHARED_SECRET must reference a numbered Secret version")
            if not probe_secret:
                raise RefuseRelease("Set STAGING_EDGE_SHARED_SECRET to match the runtime edge Secret before deploying")


def probe_edge_secret():
    secret = os.environ.get("TRACE_STAGING_EDGE_SHARED_SECRET", "")
    if secret and not re.fullmatch(r"[\x21-\x7e]{32,}", secret):
        # Reject unsafe header values before urllib can include them in an error.
        raise RefuseRelease("STAGING_EDGE_SHARED_SECRET must contain at least 32 visible ASCII characters")
    return secret


class Release:
    def __init__(self):
        if (os.environ.get("GITHUB_REPOSITORY") != REPOSITORY or os.environ.get("GITHUB_EVENT_NAME") != "push"
                or os.environ.get("GITHUB_REF") != "refs/heads/main"):
            raise RefuseRelease("Only a push to Mino829/TRACE_B main may deploy")
        self.sha = os.environ.get("GITHUB_SHA", "")
        self.run_id = os.environ.get("GITHUB_RUN_ID", "")
        self.attempt = os.environ.get("GITHUB_RUN_ATTEMPT", "")
        if not re.fullmatch(r"[0-9a-f]{40}", self.sha) or not self.run_id.isdigit() or not self.attempt.isdigit():
            raise RefuseRelease("Invalid GitHub execution identity")
        self.tag = f"ci-{self.run_id}-{self.attempt}"
        self.state_path = Path(os.environ["RUNNER_TEMP"]) / f"trace-staging-{self.run_id}-{self.attempt}.json"
        self.state = json.loads(self.state_path.read_text()) if self.state_path.exists() else {}

    def save(self):
        self.state_path.write_text(json.dumps(self.state))

    def output(self, name, value):
        if "\n" in value or "\r" in value:
            raise RefuseRelease("Unexpected multiline output")
        with open(os.environ["GITHUB_OUTPUT"], "a") as output:
            output.write(f"{name}={value}\n")

    def newest(self):
        ref = request_json(f"https://api.github.com/repos/{REPOSITORY}/git/ref/heads/main", os.environ["GH_TOKEN"])
        if ref["object"]["sha"] != self.sha:
            raise RefuseRelease("A newer main commit exists; this stale run will not be deployed")

    def guard(self):
        if not os.environ.get("GCP_WIF_PROVIDER"):
            raise RefuseRelease("Set GCP_WORKLOAD_IDENTITY_PROVIDER before enabling staging deployment")
        if run("git", "rev-parse", "HEAD") != self.sha:
            raise RefuseRelease("Checkout does not match the tested commit")
        baseline = os.environ.get("STAGING_SCHEMA_BASELINE", "")
        if not re.fullmatch(r"[0-9a-f]{40}", baseline):
            raise RefuseRelease("STAGING_SCHEMA_BASELINE must be a full, manually verified commit SHA")
        run("git", "merge-base", "--is-ancestor", baseline, self.sha)
        if run("git", "diff", "--name-only", baseline, self.sha, "--", *SCHEMA_PATHS):
            raise RefuseRelease("DB migration/grants changed: apply and verify them manually, then advance STAGING_SCHEMA_BASELINE")
        self.newest()

    def resources(self):
        return cloud("run", "services", "describe", SERVICE), cloud("run", "jobs", "describe", JOB)

    def private_runtime(self, service, job):
        if service.get("metadata", {}).get("annotations", {}).get("run.googleapis.com/invoker-iam-disabled") == "true":
            raise RefuseRelease("IAM authentication is disabled; do not auto-deploy public staging")
        policy = cloud("run", "services", "get-iam-policy", SERVICE)
        if any(member in ("allUsers", "allAuthenticatedUsers")
               for binding in policy.get("bindings", []) for member in binding.get("members", [])):
            raise RefuseRelease("Public IAM binding found; review it manually")
        max_scale = (service.get("spec", {}).get("template", {}).get("metadata", {})
                     .get("annotations", {}).get("autoscaling.knative.dev/maxScale"))
        if max_scale != "1":
            raise RefuseRelease("Go-memory positions require a Cloud Run maximum of one API instance")
        check_runtime(service["spec"]["template"]["spec"], "serve")
        check_runtime(job_spec(job), "cleanup")

    def prepare(self):
        service, job = self.resources()
        self.private_runtime(service, job)
        status = service["status"]
        if (not any(c.get("type") == "Ready" and c.get("status") == "True" for c in status.get("conditions", []))
                or status.get("latestCreatedRevisionName") != status.get("latestReadyRevisionName")):
            raise RefuseRelease("The existing service has an unfinished/failed deployment")
        self.state = {"old_revision": active_revision(service), "old_job_image": job_spec(job)["containers"][0]["image"],
                      "service_url": service_url(status["url"])}
        require_private(self.state["service_url"])
        self.save()
        self.output("service_url", self.state["service_url"])

    def build(self):
        image_tag = f"{IMAGE_BASE}:{self.sha}-{self.run_id}-{self.attempt}"
        submitted = cloud("builds", "submit", ".", "--config=cloudbuild.ci.yaml",
                      f"--service-account=projects/{PROJECT}/serviceAccounts/{BUILD_SA}",
                      f"--gcs-source-staging-dir={SOURCE_BUCKET}", f"--substitutions=_IMAGE={image_tag}",
                      "--suppress-logs", "--timeout=1200s", timeout=1500)
        # Read the final resource explicitly instead of depending on which
        # snapshot a particular gcloud release prints after waiting.
        build = cloud("builds", "describe", submitted["id"])
        if build.get("status") != "SUCCESS":
            raise RefuseRelease("Cloud Build did not succeed")
        images = [image for image in build.get("results", {}).get("images", []) if image.get("name") == image_tag]
        if len(images) != 1 or not re.fullmatch(r"sha256:[0-9a-f]{64}", images[0].get("digest", "")):
            raise RefuseRelease("Cloud Build did not return the expected immutable image digest")
        self.state["image"] = f"{IMAGE_BASE}@{images[0]['digest']}"
        self.state["build_id"] = build["id"]
        self.save()

    def unchanged(self):
        service, job = self.resources()
        self.private_runtime(service, job)
        if active_revision(service) != self.state["old_revision"]:
            raise RefuseRelease("Service traffic changed outside this run; stop and review")
        expected = self.state["image"] if self.state.get("job_verified") else self.state["old_job_image"]
        if job_spec(job)["containers"][0]["image"] != expected:
            raise RefuseRelease("Cleanup image changed outside this run; stop and review")

    def candidate(self):
        self.newest()
        self.unchanged()
        self.state["candidate_attempted"] = True
        self.save()
        cloud("run", "services", "update", SERVICE, f"--image={self.state['image']}",
              "--no-traffic", f"--tag={self.tag}", timeout=300)
        service = cloud("run", "services", "describe", SERVICE)
        tags = [item for item in service["status"]["traffic"] if item.get("tag") == self.tag]
        if len(tags) != 1 or active_revision(service) != self.state["old_revision"]:
            raise RefuseRelease("Candidate tag/traffic was not created as expected")
        self.state.update(candidate_url=service_url(tags[0]["url"]), candidate_revision=tags[0]["revisionName"])
        self.save()

    def probe(self, url):
        token = os.environ.get("TRACE_ID_TOKEN", "")
        if not token:
            raise RefuseRelease("Missing private health-check ID token")
        edge_secret = probe_edge_secret()
        for attempt in range(3):
            try:
                ready = request_json(f"{url}/health/ready", token)
                config = request_json(f"{url}/v1/public/config", token, edge_secret=edge_secret)
                if ready.get("status") != "ready" or config.get("registrationOpen") is not False:
                    raise RefuseRelease("Health check failed or registration is not closed")
                return
            except (urllib.error.URLError, TimeoutError):
                if attempt == 2:
                    raise
                time.sleep(2)

    def verify(self):
        require_private(self.state["candidate_url"])
        self.probe(self.state["candidate_url"])
        self.state["candidate_verified"] = True
        self.save()

    def cleanup_job(self):
        if not self.state.get("candidate_verified"):
            raise RefuseRelease("Verify the candidate before updating cleanup")
        self.newest()
        self.unchanged()
        self.state["job_attempted"] = True
        self.save()
        cloud("run", "jobs", "update", JOB, f"--image={self.state['image']}")
        cloud("run", "jobs", "execute", JOB, "--wait", timeout=480)
        self.state["job_verified"] = True
        self.save()

    def promote(self):
        if not self.state.get("job_verified"):
            raise RefuseRelease("Verify cleanup before switching API traffic")
        self.newest()
        self.unchanged()
        require_private(self.state["candidate_url"])
        self.probe(self.state["candidate_url"])
        self.state["traffic_attempted"] = True
        self.save()
        cloud("run", "services", "update-traffic", SERVICE, f"--to-revisions={self.state['candidate_revision']}=100")
        service = cloud("run", "services", "describe", SERVICE)
        if active_revision(service) != self.state["candidate_revision"]:
            raise RefuseRelease("Traffic did not switch to the verified candidate")
        require_private(self.state["service_url"])
        self.probe(self.state["service_url"])
        self.state["success"] = True
        self.save()
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as summary:
            summary.write(f"### Staging deployment succeeded\n\n- Commit: `{self.sha}`\n"
                          f"- Image: `{self.state['image']}`\n- Revision: `{self.state['candidate_revision']}`\n"
                          "- DB readiness and closed registration verified. Cleanup execution succeeded.\n")

    def rollback(self):
        if not self.state or self.state.get("success"):
            return
        failures = []
        if self.state.get("traffic_attempted"):
            try:
                current = active_revision(cloud("run", "services", "describe", SERVICE))
                if current == self.state["candidate_revision"]:
                    cloud("run", "services", "update-traffic", SERVICE, f"--to-revisions={self.state['old_revision']}=100")
                elif current != self.state["old_revision"]:
                    raise RefuseRelease("Traffic changed externally; refusing to overwrite it")
            except Exception as error:
                failures.append(str(error))
        if self.state.get("job_attempted"):
            try:
                current = job_spec(cloud("run", "jobs", "describe", JOB))["containers"][0]["image"]
                if current == self.state["image"]:
                    cloud("run", "jobs", "update", JOB, f"--image={self.state['old_job_image']}")
                elif current != self.state["old_job_image"]:
                    raise RefuseRelease("Cleanup image changed externally; refusing to overwrite it")
            except Exception as error:
                failures.append(str(error))
        if failures:
            raise RefuseRelease("Manual recovery required: " + "; ".join(failures))
        print("Previous traffic/cleanup image retained or restored; DB changes were never run.")

    def finish(self):
        if self.state.get("candidate_attempted"):
            service = cloud("run", "services", "describe", SERVICE)
            if any(item.get("tag") == self.tag for item in service["status"]["traffic"]):
                cloud("run", "services", "update-traffic", SERVICE, f"--remove-tags={self.tag}")


if __name__ == "__main__":
    commands = ("guard", "prepare", "build", "candidate", "verify", "cleanup_job", "promote", "rollback", "finish")
    try:
        if len(sys.argv) != 2 or sys.argv[1] not in commands:
            raise RefuseRelease("Usage: ci_staging.py " + "|".join(commands))
        getattr(Release(), sys.argv[1])()
    except Exception as error:
        print(f"::error::{error}", file=sys.stderr)
        sys.exit(1)
