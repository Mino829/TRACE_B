#!/usr/bin/env python3
"""Fail-closed, image-only preview staging releases and readonly operator guards.

Uses Python's standard library plus git/gcloud; no DB credentials, IAM writes or DDL.
CI state lives in RUNNER_TEMP, outside source uploads and auth tokens.
"""

import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request

PROJECT = "trace-staging-async5855"
REGION = "asia-northeast1"
REPOSITORY = "Mino829/TRACE_B"
SERVICE = "trace-backend-preview"
STOPPED_SERVICE = "trace-backend"
JOB = "trace-cleanup"
API_RUNTIME_SA = f"trace-preview-runtime@{PROJECT}.iam.gserviceaccount.com"
CLEANUP_RUNTIME_SA = f"trace-runtime@{PROJECT}.iam.gserviceaccount.com"
BUILD_SA = f"trace-build@{PROJECT}.iam.gserviceaccount.com"
IMAGE_BASE = f"{REGION}-docker.pkg.dev/{PROJECT}/trace/backend"
SOURCE_BUCKET = f"gs://{PROJECT}-github-build-source/source"
SCHEMA_PATHS = ["internal/store/migrate.go", "internal/store/migrations", "deploy/gcp/runtime-grants.sql"]
FEATURE_FLAGS = (
    "TRACE_REGISTRATION_OPEN", "TRACE_PHOTO_METADATA_ENABLED", "TRACE_PHOTO_ENABLED",
    "TRACE_PUSH_ENABLED", "TRACE_ARRIVAL_ENABLED", "TRACE_DISPLAY_ENABLED", "TRACE_OPERATOR_ENABLED",
)
BUNDLE_ENV = {
    "coordinateVersion": "TRACE_COORDINATE_VERSION",
    "campusGraphVersion": "TRACE_CAMPUS_GRAPH_VERSION",
    "threeDMapVersion": "TRACE_THREE_D_MAP_VERSION",
}
API_SECRETS = (
    "TRACE_EDGE_SHARED_SECRET", "TRACE_PUSH_VAPID_PRIVATE_KEY", "TRACE_PUSH_ENCRYPTION_KEY",
    "TRACE_ARRIVAL_PROOF_KEY", "TRACE_DISPLAY_TOKEN_KEY",
)


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
    location = "--location" if args[0] == "scheduler" else "--region"
    result = run("gcloud", *args, f"--project={PROJECT}", f"{location}={REGION}", "--quiet", "--format=json", timeout=timeout)
    return json.loads(result) if result else {}


def service_url(value):
    parsed = urllib.parse.urlsplit(value)
    if (parsed.scheme != "https" or not parsed.hostname or not parsed.hostname.endswith(".run.app")
            or parsed.username or parsed.password or parsed.port or parsed.path not in ("", "/")
            or parsed.query or parsed.fragment):
        raise RefuseRelease("Expected a default HTTPS Cloud Run URL")
    return value.rstrip("/")


def require_edge_boundary(url):
    try:
        request_json(f"{url}/v1/public/config", None)
    except urllib.error.HTTPError as error:
        error.close()
        if error.code == 403:
            return
        raise RefuseRelease("The public config must reject a missing Edge key with HTTP 403") from None
    raise RefuseRelease("The public config accepted a missing Edge key")


def active_revision(service):
    traffic = [item for item in service["status"]["traffic"] if item.get("percent", 0) > 0]
    if len(traffic) != 1 or traffic[0]["percent"] != 100 or not traffic[0].get("revisionName"):
        raise RefuseRelease("Staging must route 100% to one known revision; review split traffic manually")
    return traffic[0]["revisionName"]


def job_spec(job):
    return job["spec"]["template"]["spec"]["template"]["spec"]


def check_runtime(spec, mode):
    identity = API_RUNTIME_SA if mode == "serve" else CLEANUP_RUNTIME_SA
    if spec.get("serviceAccountName") != identity or len(spec.get("containers", [])) != 1:
        raise RefuseRelease("Unexpected runtime identity or container count")
    container = spec["containers"][0]
    if container.get("args") != [mode] or container.get("command"):
        raise RefuseRelease("Unexpected container entrypoint")
    entries = container.get("env", [])
    env = {item["name"]: item for item in entries}
    if len(env) != len(entries):
        raise RefuseRelease("Duplicate runtime environment names")
    expected = {
        "DB_HOST": f"/cloudsql/{PROJECT}:{REGION}:trace-postgres",
        "DB_NAME": "trace", "DB_USER": "trace_app", "DB_SSLMODE": "disable",
        **{name: "false" for name in FEATURE_FLAGS},
    }
    for key, value in expected.items():
        if env.get(key, {}).get("value") != value:
            raise RefuseRelease(f"Review existing runtime setting: {key}")
    try:
        connections = int(env.get("TRACE_DB_MAX_CONNECTIONS", {}).get("value", ""))
    except (TypeError, ValueError):
        raise RefuseRelease("Set an explicit application DB connection budget") from None
    if not 1 <= connections <= 5:
        raise RefuseRelease("Application DB pool must stay within the staging connection budget of five")
    if "TRACE_ARRIVAL_ISSUER_KEY" in env:
        raise RefuseRelease("Remove obsolete service issuer credentials; operator access is personal")
    if any(name in env for name in API_SECRETS if name != "TRACE_EDGE_SHARED_SECRET"):
        raise RefuseRelease("Remove unused optional feature credentials from the closed runtime")
    if "DATABASE_URL" in env:
        raise RefuseRelease("DATABASE_URL overrides DB_*; review it manually before enabling CI")
    if "TRACE_SESSION_TTL" in env:
        raise RefuseRelease("TRACE_SESSION_TTL is obsolete; migrate service environment first")
    secret = env.get("DB_PASSWORD", {}).get("valueFrom", {}).get("secretKeyRef", {})
    if (secret.get("name") != "trace-app-db-password"
            or not re.fullmatch(r"[1-9][0-9]*", secret.get("key", "")) or "value" in env["DB_PASSWORD"]):
        raise RefuseRelease("DB_PASSWORD must reference a numbered app-password Secret version")
    if mode == "serve":
        edge = env.get("TRACE_EDGE_SHARED_SECRET", {})
        ref = edge.get("valueFrom", {}).get("secretKeyRef", {})
        if (ref.get("name") != "trace-preview-edge-secret"
                or not re.fullmatch(r"[1-9][0-9]*", ref.get("key", "")) or "value" in edge):
            raise RefuseRelease("TRACE_EDGE_SHARED_SECRET must reference the numbered preview Edge Secret")
        if not probe_edge_secret():
            raise RefuseRelease("Set TRACE_STAGING_EDGE_SHARED_SECRET to match the runtime Edge Secret")
    elif any(name in env for name in API_SECRETS):
        raise RefuseRelease("Cleanup must not receive API Edge, arrival or display secrets")
    for name in BUNDLE_ENV.values():
        if not env.get(name, {}).get("value"):
            raise RefuseRelease(f"Missing explicit runtime bundle setting: {name}")
    return env


def probe_edge_secret():
    secret = os.environ.get("TRACE_STAGING_EDGE_SHARED_SECRET", "")
    if secret and not re.fullmatch(r"[\x21-\x7e]{32,}", secret):
        # Reject unsafe header values before urllib can include them in an error.
        raise RefuseRelease("STAGING_EDGE_SHARED_SECRET must contain at least 32 visible ASCII characters")
    return secret


def require_reviewed_tags(service, allowed_tag=None):
    # A tag-only revision can still start under manual scaling, even at zero.
    if any(item.get("tag") and item["tag"] != allowed_tag
           for section in ("spec", "status") for item in service.get(section, {}).get("traffic", [])):
        raise RefuseRelease("Remove unreviewed revision tags before maintenance or release")


def require_stopped(service, name):
    # gcloud Run v1 uses lowercase enum strings on *service* annotations.
    # --scaling=0 removes min/max and writes manual + manualInstanceCount=0.
    annotations = service.get("metadata", {}).get("annotations", {})
    if (annotations.get("run.googleapis.com/scalingMode") != "manual"
            or annotations.get("run.googleapis.com/manualInstanceCount") != "0"):
        raise RefuseRelease(f"{name} must be explicitly manual-scaled to zero")
    require_reviewed_tags(service)
    require_reconciled(service, serving=False)


def require_reconciled(service, *, serving=True):
    metadata, status = service.get("metadata", {}), service.get("status", {})
    generation = str(metadata.get("generation", ""))
    if not re.fullmatch(r"[1-9][0-9]*", generation) or str(status.get("observedGeneration")) != generation:
        raise RefuseRelease("Service configuration has not reconciled")
    if serving and not any(c.get("type") == "Ready" and c.get("status") == "True"
                           for c in status.get("conditions", [])):
        raise RefuseRelease("Service is not Ready")


def check_scaling(service):
    annotations = service.get("metadata", {}).get("annotations", {})
    revision = service["spec"]["template"].get("metadata", {}).get("annotations", {})
    # SDK --min=0 removes minScale. A revision max of 12 is safe only when
    # bounded by the service-level max of 1; revision max alone is insufficient.
    if (annotations.get("run.googleapis.com/scalingMode", "automatic") != "automatic"
            or "run.googleapis.com/manualInstanceCount" in annotations
            or annotations.get("run.googleapis.com/maxScale") != "1"
            or annotations.get("run.googleapis.com/minScale", "0") != "0"
            or revision.get("autoscaling.knative.dev/minScale", "0") != "0"):
        raise RefuseRelease("Preview requires automatic service-level max=1, min=0 scaling")
    limits = service["spec"]["template"]["spec"]["containers"][0].get("resources", {}).get("limits", {})
    if (limits.get("cpu") not in ("1", "1000m") or limits.get("memory") != "512Mi"
            or revision.get("run.googleapis.com/cpu-throttling", "true") != "true"):
        raise RefuseRelease("Preview requires CPU=1, memory=512Mi and request-based CPU")


def check_preview_profile(service, job, *, stable_images, allowed_tag=None):
    require_stopped(cloud("run", "services", "describe", STOPPED_SERVICE), STOPPED_SERVICE)
    require_reviewed_tags(service, allowed_tag)
    require_reconciled(service)
    if service.get("metadata", {}).get("annotations", {}).get("run.googleapis.com/invoker-iam-disabled") == "true":
        raise RefuseRelease("Preserve the preview invoker IAM check")
    policy = cloud("run", "services", "get-iam-policy", SERVICE)
    if not any(binding.get("role") == "roles/run.invoker" and "allUsers" in binding.get("members", [])
               and not binding.get("condition") for binding in policy.get("bindings", [])):
        raise RefuseRelease("Preserve the existing public-invoker preview Edge boundary")
    api_env = check_runtime(service["spec"]["template"]["spec"], "serve")
    cleanup_env = check_runtime(job_spec(job), "cleanup")
    check_scaling(service)
    for name in ("DB_PASSWORD", *BUNDLE_ENV.values()):
        if api_env[name] != cleanup_env[name]:
            raise RefuseRelease(f"API and cleanup runtime settings differ: {name}")
    if stable_images:
        api_image = service["spec"]["template"]["spec"]["containers"][0]["image"]
        cleanup_image = job_spec(job)["containers"][0]["image"]
        if not immutable_image(api_image) or api_image != cleanup_image:
            raise RefuseRelease("API and cleanup must use the same reviewed immutable staging image")
        if (active_revision(service) != service["status"].get("latestReadyRevisionName")
                or service["status"].get("latestCreatedRevisionName") != service["status"].get("latestReadyRevisionName")):
            raise RefuseRelease("The reviewed latest revision must be the only active preview revision")
    return {field: api_env[name]["value"] for field, name in BUNDLE_ENV.items()}


def immutable_image(image):
    return re.fullmatch(re.escape(IMAGE_BASE) + r"@sha256:[0-9a-f]{64}", image or "") is not None


def probe(url, bundle):
    require_edge_boundary(url)
    secret = probe_edge_secret()
    if not secret:
        raise RefuseRelease("Set TRACE_STAGING_EDGE_SHARED_SECRET for the readonly Edge probe")
    ready = request_json(f"{url}/health/ready", None, edge_secret=secret)
    config = request_json(f"{url}/v1/public/config", None, edge_secret=secret)
    if ready.get("status") != "ready" or config.get("registrationOpen") is not False:
        raise RefuseRelease("Health check failed or registration is not closed")
    if config.get("livePresenceEnabled") is not False:
        raise RefuseRelease("GPS-free candidate is not active; refusing deployment")
    if (config.get("photo", {}).get("enabled") is not False
            or config.get("notification", {}).get("enabled") is not False):
        raise RefuseRelease("Photo and push features must remain closed")
    if config.get("spatial") != bundle:
        raise RefuseRelease("Public config does not match the reviewed runtime bundle")


def check_maintenance():
    for name in (SERVICE, STOPPED_SERVICE):
        require_stopped(cloud("run", "services", "describe", name), name)
    scheduler = cloud("scheduler", "jobs", "describe", JOB)
    if scheduler.get("state") != "PAUSED":
        raise RefuseRelease("Pause the cleanup Scheduler before maintenance")
    executions = cloud("run", "jobs", "executions", "list", f"--job={JOB}")
    if not isinstance(executions, list):
        raise RefuseRelease("Cannot establish cleanup execution state")
    for execution in executions:
        status = execution.get("status", {})
        if not status.get("completionTime") or status.get("runningCount", 0):
            raise RefuseRelease("Cleanup has a nonterminal execution; wait for it to finish")


def check_active():
    service = cloud("run", "services", "describe", SERVICE)
    job = cloud("run", "jobs", "describe", JOB)
    bundle = check_preview_profile(service, job, stable_images=True)
    if service["status"].get("latestCreatedRevisionName") != service["status"].get("latestReadyRevisionName"):
        raise RefuseRelease("The preview has an unfinished deployment")
    active_revision(service)
    probe(service_url(service["status"]["url"]), bundle)


def runtime_snapshot(service, job):
    # Compare runtime configuration, not generated resource versions/timestamps.
    # Images intentionally differ while the verified candidate is being staged.
    template = json.loads(json.dumps(service["spec"]["template"]))
    cleanup = json.loads(json.dumps(job_spec(job)))
    for spec in (template["spec"], cleanup):
        spec["containers"][0].pop("image", None)
    def annotations(resource):
        generated = {"run.googleapis.com/operation-id", "run.googleapis.com/client-name",
                     "run.googleapis.com/client-version", "run.googleapis.com/urls",
                     "serving.knative.dev/creator", "serving.knative.dev/lastModifier"}
        return {name: value for name, value in resource.get("metadata", {}).get("annotations", {}).items()
                if name not in generated}
    snapshot = {"service_annotations": annotations(service), "template_spec": template["spec"],
                "revision_annotations": annotations(template), "cleanup_spec": cleanup,
                "cleanup_annotations": annotations(job["spec"]["template"])}
    return hashlib.sha256(json.dumps(snapshot, sort_keys=True).encode()).hexdigest()


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

    def prepare(self):
        service, job = self.resources()
        bundle = check_preview_profile(service, job, stable_images=True)
        status = service["status"]
        self.state = {"old_revision": active_revision(service), "old_job_image": job_spec(job)["containers"][0]["image"],
                      "old_service_image": service["spec"]["template"]["spec"]["containers"][0]["image"],
                      "service_url": service_url(status["url"]), "bundle": bundle,
                      "runtime_snapshot": runtime_snapshot(service, job)}
        probe(self.state["service_url"], bundle)
        self.save()

    def build(self):
        self.newest()
        self.unchanged()
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
        check_preview_profile(service, job, stable_images=False,
                              allowed_tag=self.tag if self.state.get("candidate_revision") else None)
        if runtime_snapshot(service, job) != self.state["runtime_snapshot"]:
            raise RefuseRelease("Runtime configuration changed outside this run; stop and review")
        expected_api = self.state["image"] if self.state.get("candidate_revision") else self.state["old_service_image"]
        if service["spec"]["template"]["spec"]["containers"][0]["image"] != expected_api:
            raise RefuseRelease("API image changed outside this run; stop and review")
        if active_revision(service) != self.state["old_revision"]:
            raise RefuseRelease("Service traffic changed outside this run; stop and review")
        expected = self.state["image"] if self.state.get("job_verified") else self.state["old_job_image"]
        if job_spec(job)["containers"][0]["image"] != expected:
            raise RefuseRelease("Cleanup image changed outside this run; stop and review")
        if self.state.get("candidate_revision"):
            tags = [item for item in service["status"]["traffic"] if item.get("tag") == self.tag]
            if (len(tags) != 1 or tags[0].get("revisionName") != self.state["candidate_revision"]
                    or tags[0].get("url") != self.state["candidate_url"]):
                raise RefuseRelease("Candidate tag changed outside this run; stop and review")

    def candidate(self):
        self.newest()
        self.unchanged()
        self.state["candidate_attempted"] = True
        self.save()
        cloud("run", "services", "update", SERVICE, f"--image={self.state['image']}",
              "--no-traffic", f"--tag={self.tag}", timeout=300)
        service = cloud("run", "services", "describe", SERVICE)
        require_reviewed_tags(service, self.tag)
        tags = [item for item in service["status"]["traffic"] if item.get("tag") == self.tag]
        if (len(tags) != 1 or active_revision(service) != self.state["old_revision"]
                or tags[0].get("revisionName") != service["status"].get("latestReadyRevisionName")
                or service["status"].get("latestCreatedRevisionName") != service["status"].get("latestReadyRevisionName")):
            raise RefuseRelease("Candidate tag/traffic/readiness was not created as expected")
        self.state.update(candidate_url=service_url(tags[0]["url"]), candidate_revision=tags[0]["revisionName"])
        self.save()

    def verify(self):
        self.unchanged()
        probe(self.state["candidate_url"], self.state["bundle"])
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
        service, job = self.resources()
        check_preview_profile(service, job, stable_images=False, allowed_tag=self.tag)
        if (runtime_snapshot(service, job) != self.state["runtime_snapshot"]
                or job_spec(job)["containers"][0]["image"] != self.state["image"]):
            raise RefuseRelease("Cleanup update drifted before execution; review manually")
        cloud("run", "jobs", "execute", JOB, "--wait", timeout=480)
        self.state["job_verified"] = True
        self.save()

    def promote(self):
        if not self.state.get("job_verified"):
            raise RefuseRelease("Verify cleanup before switching API traffic")
        self.newest()
        self.unchanged()
        probe(self.state["candidate_url"], self.state["bundle"])
        self.state["traffic_attempted"] = True
        self.save()
        cloud("run", "services", "update-traffic", SERVICE, f"--to-revisions={self.state['candidate_revision']}=100")
        service = cloud("run", "services", "describe", SERVICE)
        if active_revision(service) != self.state["candidate_revision"]:
            raise RefuseRelease("Traffic did not switch to the verified candidate")
        check_preview_profile(service, cloud("run", "jobs", "describe", JOB), stable_images=True, allowed_tag=self.tag)
        probe(self.state["service_url"], self.state["bundle"])
        self.state["success"] = True
        self.save()
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as summary:
            summary.write(f"### Staging deployment succeeded\n\n- Commit: `{self.sha}`\n"
                          f"- Image: `{self.state['image']}`\n- Revision: `{self.state['candidate_revision']}`\n"
                          "- DB readiness and closed registration verified. Cleanup execution succeeded.\n")

    def rollback(self):
        if not self.state or self.state.get("success"):
            return
        service, job = self.resources()
        check_preview_profile(service, job, stable_images=False,
                              allowed_tag=self.tag if self.state.get("candidate_attempted") else None)
        if runtime_snapshot(service, job) != self.state["runtime_snapshot"]:
            raise RefuseRelease("Manual recovery required: runtime configuration changed externally")
        current_image = service["spec"]["template"]["spec"]["containers"][0]["image"]
        if current_image not in (self.state["old_service_image"], self.state.get("image")):
            raise RefuseRelease("Manual recovery required: API image changed externally")
        failures = []
        cutover_attempted = self.state.get("traffic_attempted")
        if cutover_attempted:
            if (not self.state.get("candidate_verified") or not self.state.get("job_verified")
                    or job_spec(job)["containers"][0]["image"] != self.state["image"]):
                raise RefuseRelease("Manual recovery required: verified candidate/cleanup state changed")
            try:
                current = active_revision(cloud("run", "services", "describe", SERVICE))
                if current == self.state["old_revision"]:
                    cloud("run", "services", "update-traffic", SERVICE, f"--to-revisions={self.state['candidate_revision']}=100")
                elif current != self.state["candidate_revision"]:
                    raise RefuseRelease("Traffic changed externally; refusing to overwrite it")
            except Exception as error:
                failures.append(str(error))
        if self.state.get("job_attempted") and not cutover_attempted:
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
        print("GPS-free candidate and cleanup retained; fix forward with registration closed." if cutover_attempted else
              "Previous verified traffic/cleanup retained or restored; DB changes were never run.")

    def finish(self):
        if self.state.get("candidate_attempted"):
            service = cloud("run", "services", "describe", SERVICE)
            check_preview_profile(service, cloud("run", "jobs", "describe", JOB), stable_images=False, allowed_tag=self.tag)
            tags = [item for item in service["status"]["traffic"] if item.get("tag") == self.tag]
            if tags:
                if len(tags) != 1 or tags[0].get("revisionName") != self.state.get("candidate_revision"):
                    raise RefuseRelease("Candidate tag changed externally; refusing to remove it")
                cloud("run", "services", "update-traffic", SERVICE, f"--remove-tags={self.tag}")


def main(argv=None):
    argv = sys.argv[1:] if argv is None else argv
    readonly = {"check-maintenance": check_maintenance, "check-active": check_active}
    commands = ("guard", "prepare", "build", "candidate", "verify", "cleanup_job", "promote", "rollback", "finish")
    try:
        if len(argv) != 1 or argv[0] not in (*readonly, *commands):
            raise RefuseRelease("Usage: ci_staging.py " + "|".join((*readonly, *commands)))
        if argv[0] in readonly:
            readonly[argv[0]]()
        else:
            getattr(Release(), argv[0])()
    except Exception as error:
        print(f"::error::{error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
