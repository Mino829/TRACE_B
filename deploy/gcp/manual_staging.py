"""Operator-only closed staging rollout. Invoked by deploy.sh; no cloud writes on import."""

import base64
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import tempfile
import uuid

PROJECT = "trace-staging-async5855"
REGION = "asia-northeast1"
SQL = f"{PROJECT}:{REGION}:trace-postgres"
SERVICE = "trace-backend-preview"
STOPPED_SERVICE = "trace-backend"
JOB = "trace-cleanup"
API_SA = f"trace-preview-runtime@{PROJECT}.iam.gserviceaccount.com"
CLEANUP_SA = f"trace-runtime@{PROJECT}.iam.gserviceaccount.com"
MIGRATION_SA = f"trace-migration@{PROJECT}.iam.gserviceaccount.com"
SCHEDULER_SA = f"trace-scheduler@{PROJECT}.iam.gserviceaccount.com"
PSQL_IMAGE = "docker.io/library/postgres@sha256:e4cb93687b062cd25a0e8f672d85a7708235009d70d31f8a7698d9370092a9c4"
ROOT = Path(__file__).resolve().parents[2]
GUARD = ROOT / "deploy/gcp/ci_staging.py"
FLAGS = (
    "TRACE_REGISTRATION_OPEN", "TRACE_PHOTO_METADATA_ENABLED", "TRACE_PHOTO_ENABLED",
    "TRACE_PUSH_ENABLED", "TRACE_ARRIVAL_ENABLED", "TRACE_DISPLAY_ENABLED", "TRACE_OPERATOR_ENABLED",
)
API_SECRET_NAMES = (
    "TRACE_EDGE_SHARED_SECRET", "TRACE_PUSH_VAPID_PRIVATE_KEY", "TRACE_PUSH_ENCRYPTION_KEY",
    "TRACE_ARRIVAL_PROOF_KEY", "TRACE_DISPLAY_TOKEN_KEY",
)


class Refuse(RuntimeError):
    pass


def require(condition, message):
    if not condition:
        raise Refuse(message)


def required(name):
    value = os.environ.get(name, "")
    require(bool(value), f"Set {name}")
    return value


def expected(name, value):
    require(required(name) == value, f"{name} must be the fixed preview-staging value: {value}")
    return value


def secret(name, resource):
    value = required(name)
    require(re.fullmatch(re.escape(resource) + r":[1-9][0-9]*", value),
            f"{name} must reference {resource}:<numeric-version>")
    return value


def command(*args, timeout=300):
    return subprocess.run(args, check=True, text=True, stdout=subprocess.PIPE, timeout=timeout).stdout.strip()


def cloud(*args, scheduler=False, timeout=300):
    location = "--location" if scheduler else "--region"
    output = command("gcloud", *args, f"--project={PROJECT}", f"{location}={REGION}",
                     "--quiet", "--format=json", timeout=timeout)
    return json.loads(output) if output else {}


def guard(mode):
    subprocess.run([sys.executable, str(GUARD), mode], check=True)


def target():
    expected("GCP_PROJECT_ID", PROJECT)
    expected("GCP_REGION", REGION)
    expected("GCP_SQL_CONNECTION", SQL)
    # Never silently accept the former shared runtime-identity convention.
    require(not os.environ.get("GCP_RUNTIME_SA"), "Remove obsolete GCP_RUNTIME_SA; use separate API/cleanup identities")
    require(not os.environ.get("GCP_ARRIVAL_ISSUER_SECRET"), "Remove obsolete GCP_ARRIVAL_ISSUER_SECRET; operator access is personal")
    for name in ("GCP_PUSH_VAPID_PRIVATE_SECRET", "GCP_PUSH_ENCRYPTION_SECRET",
                 "GCP_ARRIVAL_PROOF_SECRET", "GCP_DISPLAY_TOKEN_SECRET"):
        require(not os.environ.get(name), f"Remove {name}; this closed staging profile does not deploy optional feature keys")
    for name, value in (("GCP_SERVICE", SERVICE), ("GCP_STOPPED_SERVICE", STOPPED_SERVICE), ("GCP_JOB", JOB)):
        require(name not in os.environ or os.environ[name] == value, f"{name} cannot override this fixed staging profile")


def image():
    value = required("GCP_IMAGE")
    require(re.fullmatch(re.escape(f"{REGION}-docker.pkg.dev/{PROJECT}/trace/backend@sha256:") + r"[0-9a-f]{64}", value),
            "GCP_IMAGE must be the reviewed backend image digest in the fixed staging Artifact Registry repository")
    return value


def existing():
    # Describe every target before the first write; never create missing services/jobs/scheduler.
    for service in (SERVICE, STOPPED_SERVICE):
        service_value = cloud("run", "services", "describe", service)
        require(not any(item.get("tag") for section in ("spec", "status")
                        for item in service_value.get(section, {}).get("traffic", [])),
                "Remove revision tags before rollout; manual zero alone does not disable tag-only revisions")
    cloud("run", "jobs", "describe", JOB)
    cloud("scheduler", "jobs", "describe", JOB, scheduler=True)


def backup():
    backup_id = required("GCP_BACKUP_ID")
    require(re.fullmatch(r"[0-9]+", backup_id), "GCP_BACKUP_ID must be an operator-created backup ID")
    value = json.loads(command("gcloud", "sql", "backups", "describe", backup_id,
                              "--instance=trace-postgres", f"--project={PROJECT}", "--format=json"))
    require(str(value.get("id")) == backup_id and value.get("instance") == "trace-postgres"
            and value.get("status") == "SUCCESSFUL" and value.get("type") == "ON_DEMAND",
            "Backup must be a successful operator-created ON_DEMAND backup of trace-postgres")
    try:
        quiesced = datetime.fromisoformat(required("GCP_QUIESCED_AT").replace("Z", "+00:00"))
        started = datetime.fromisoformat(value["startTime"].replace("Z", "+00:00"))
        completed = datetime.fromisoformat(value["endTime"].replace("Z", "+00:00"))
    except (KeyError, ValueError, TypeError):
        raise Refuse("Set GCP_QUIESCED_AT to the recorded UTC maintenance time; backup timestamps must be valid") from None
    now = datetime.now(timezone.utc)
    require(all(stamp.tzinfo is not None for stamp in (quiesced, started, completed))
            and quiesced <= started <= completed <= now,
            "Backup must start after the recorded quiesce and complete before migration/grants")


def latest_schema():
    versions = []
    for path in (ROOT / "internal/store/migrations").glob("*.sql"):
        match = re.fullmatch(r"([0-9]{3})_.+\.sql", path.name)
        require(match is not None, "Review migration manifest filenames before rollout")
        versions.append(int(match[1]))
    versions.sort()
    require(versions and versions == list(range(1, len(versions) + 1)),
            "The reviewed migration manifest must be contiguous and unique")
    return versions[-1]


def schema_sql(*versions):
    allowed = ", ".join(str(version) for version in versions)
    return f"""DO $guard$ BEGIN
IF (SELECT count(*) FROM schema_migrations) NOT IN ({allowed})
 OR (SELECT min(version) FROM schema_migrations) <> 1
 OR (SELECT max(version) FROM schema_migrations) <> (SELECT count(*) FROM schema_migrations) THEN
 RAISE EXCEPTION 'Expected contiguous schema through one of: {allowed}';
END IF;
END $guard$;
"""


def runtime_sql():
    # Read the reviewed grant manifest, not a second table list which can silently drift.
    sql = (ROOT / "deploy/gcp/runtime-grants.sql").read_text()
    checks = []
    for permissions, tables in re.findall(r"GRANT\s+([A-Z, ]+)\s+ON\s+([^;]+?)\s+TO\s+trace_app\s*;", sql):
        if permissions.strip() in ("CONNECT", "USAGE"):
            continue
        for table in tables.split(","):
            table = table.strip()
            require(re.fullmatch(r"[a-z_]+", table), "Review unsupported runtime-grants.sql table syntax")
            for permission in permissions.split(","):
                checks.append(f"IF NOT has_table_privilege('trace_app', 'public.{table}', '{permission.strip()}') THEN "
                              f"RAISE EXCEPTION 'Missing runtime grants on {table}'; END IF;")
    require(bool(checks), "runtime-grants.sql contains no verifiable table grants")
    return schema_sql(latest_schema()) + """DO $guard$ BEGIN
IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'trace_app' AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole)
 OR NOT has_database_privilege('trace_app', 'trace', 'CONNECT')
 OR has_database_privilege('trace_app', 'trace', 'CREATE')
 OR NOT has_schema_privilege('trace_app', 'public', 'USAGE')
 OR EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
            WHERE n.nspname = 'public' AND c.relowner = 'trace_app'::regrole)
 OR has_schema_privilege('trace_app', 'public', 'CREATE')
 OR has_table_privilege('trace_app', 'public.schema_migrations', 'INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') THEN
 RAISE EXCEPTION 'Unsafe runtime role privileges';
END IF;
""" + "\n".join(checks) + "\nEND $guard$;\n"


def remote_sql(sql, *, owner):
    # Unique, labelled ownership prevents cleanup from deleting any pre-existing job.
    operation = uuid.uuid4().hex
    name = f"trace-preview-sql-{operation}"
    account = expected("GCP_MIGRATION_SA", MIGRATION_SA) if owner else expected("GCP_CLEANUP_RUNTIME_SA", CLEANUP_SA)
    password = secret("GCP_OWNER_PASSWORD_SECRET", "trace-owner-db-password") if owner else secret("GCP_APP_PASSWORD_SECRET", "trace-app-db-password")
    encoded = base64.b64encode(sql.encode()).decode()
    # The pinned image uses BusyBox; propagate decoder failures past psql's empty-input success.
    script = f"set -o pipefail; printf %s {encoded} | base64 -d | psql --no-psqlrc --no-password --set=ON_ERROR_STOP=1 --single-transaction --file=-"
    delimiter = f"__{operation}__"
    try:
        cloud("run", "jobs", "create", name, f"--image={PSQL_IMAGE}", f"--service-account={account}",
              "--command=/bin/sh", f"--args=^{delimiter}^-ceu{delimiter}{script}",
              f"--set-cloudsql-instances={SQL}",
              f"--set-env-vars=PGHOST=/cloudsql/{SQL},PGDATABASE=trace,PGUSER={'trace_owner' if owner else 'trace_app'},PGCONNECT_TIMEOUT=15,PGSSLMODE=disable",
              f"--set-secrets=PGPASSWORD={password}", f"--labels=trace-preview-operation={operation}",
              "--tasks=1", "--parallelism=1", "--max-retries=0", "--task-timeout=120s", "--cpu=1", "--memory=512Mi")
        cloud("run", "jobs", "execute", name, "--wait", timeout=600)
    finally:
        # Also inspect after a failed create, which may have reached the server.
        # A describe/delete failure is a real operational error, never reported as successful cleanup.
        try:
            owned = cloud("run", "jobs", "describe", name)
        except subprocess.CalledProcessError as error:
            raise Refuse(f"Could not verify temporary SQL job cleanup: inspect {name}; do not delete unrelated jobs") from error
        require(owned.get("metadata", {}).get("labels", {}).get("trace-preview-operation") == operation,
                f"Refusing to delete a temporary SQL job without its ownership label: {name}")
        cloud("run", "jobs", "delete", name)


def runtime_env():
    # Cloud Run accepts JSON as an env YAML file. Accept only flat, quoted string
    # scalars in the operator file: no aliases, nested values, tags or YAML coercion.
    path = ROOT / "deploy/gcp/runtime.env.yaml"
    require(path.is_file(), f"Prepare {path}")
    values = {}
    for number, line in enumerate(path.read_text().splitlines(), 1):
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        match = re.fullmatch(r"([A-Z][A-Z0-9_]*):\s*(\"(?:[^\"\\]|\\.)*\"|'(?:[^']|'')*')\s*(?:#.*)?", line)
        require(match is not None, f"runtime.env.yaml:{number}: use a top-level quoted string value")
        key, raw = match.groups()
        require(key not in values, f"Duplicate runtime setting: {key}")
        try:
            value = json.loads(raw) if raw.startswith('"') else raw[1:-1].replace("''", "'")
        except ValueError:
            raise Refuse(f"Invalid quoted runtime value at line {number}") from None
        require("REPLACE" not in value.upper(), f"Review placeholder in runtime setting: {key}")
        values[key] = value
    for key, value in {"DB_HOST": f"/cloudsql/{SQL}", "DB_NAME": "trace", "DB_USER": "trace_app", "DB_SSLMODE": "disable", **dict.fromkeys(FLAGS, "false")}.items():
        require(values.get(key) == value, f"Closed staging profile requires {key}={value}")
    require("DATABASE_URL" not in values and "DB_PASSWORD" not in values and "TRACE_SESSION_TTL" not in values,
            "Remove password/DB URL overrides and obsolete TRACE_SESSION_TTL from runtime.env.yaml")
    require("TRACE_ARRIVAL_ISSUER_KEY" not in values, "Remove obsolete TRACE_ARRIVAL_ISSUER_KEY; operator access is personal")
    require(not any(key in values for key in API_SECRET_NAMES), "Use pinned Secret references, never credentials in runtime.env.yaml")
    try:
        connections = int(values.get("TRACE_DB_MAX_CONNECTIONS", ""))
    except ValueError:
        raise Refuse("Set TRACE_DB_MAX_CONNECTIONS between 1 and 5") from None
    require(1 <= connections <= 5, "TRACE_DB_MAX_CONNECTIONS must be between 1 and 5")
    return values


def require_probe():
    require(re.fullmatch(r"[\x21-\x7e]{32,}", required("TRACE_STAGING_EDGE_SHARED_SECRET")),
            "Set TRACE_STAGING_EDGE_SHARED_SECRET to the existing Edge probe credential (never print it)")

def preview_boundary():
    service = cloud("run", "services", "describe", SERVICE)
    require(service.get("metadata", {}).get("annotations", {}).get("run.googleapis.com/invoker-iam-disabled") != "true",
            "Preview must preserve its enabled invoker IAM check")
    policy = cloud("run", "services", "get-iam-policy", SERVICE)
    require(any(binding.get("role") == "roles/run.invoker" and "allUsers" in binding.get("members", [])
                and not binding.get("condition") for binding in policy.get("bindings", [])),
            "Preview must retain its existing public-invoker + Edge-secret boundary")
    return policy


def prepared(release, values, password, edge):
    service = cloud("run", "services", "describe", SERVICE)
    revision = cloud("run", "revisions", "describe", service["status"]["latestReadyRevisionName"])
    job = cloud("run", "jobs", "describe", JOB)
    for spec, account, mode, secrets in (
        (revision["spec"], API_SA, "serve", {"DB_PASSWORD": password, "TRACE_EDGE_SHARED_SECRET": edge}),
        (job["spec"]["template"]["spec"]["template"]["spec"], CLEANUP_SA, "cleanup", {"DB_PASSWORD": password}),
    ):
        require(spec.get("serviceAccountName") == account and len(spec.get("containers", [])) == 1,
                "Prepared runtime identity/container count changed; keep maintenance")
        container = spec["containers"][0]
        require(container.get("image") == release and container.get("args") == [mode] and not container.get("command"),
                "Prepared runtime image/command changed; keep maintenance")
        env = container.get("env", [])
        require(len({item["name"] for item in env}) == len(env), "Prepared runtime has duplicate environment keys")
        plain = {item["name"]: item["value"] for item in env if "value" in item}
        references = {}
        for item in env:
            if "value" not in item:
                ref = item.get("valueFrom", {}).get("secretKeyRef", {})
                references[item["name"]] = f"{ref.get('name', '')}:{ref.get('key', '')}"
        require(plain == values and references == secrets,
                "Prepared closed environment/Secret references changed; keep maintenance")


def serve(release):
    expected("GCP_API_RUNTIME_SA", API_SA)
    expected("GCP_CLEANUP_RUNTIME_SA", CLEANUP_SA)
    password = secret("GCP_APP_PASSWORD_SECRET", "trace-app-db-password")
    edge = secret("GCP_EDGE_SHARED_SECRET", "trace-preview-edge-secret")
    require_probe()
    values = runtime_env()
    policy = preview_boundary()
    guard("check-maintenance")
    remote_sql(runtime_sql(), owner=False)
    # Optional feature credentials are deliberately not installed while features are closed.
    # Cleanup gets only the application DB password, never Edge/push/arrival/display keys.
    with tempfile.TemporaryDirectory(prefix="trace-preview-env-") as directory:
        env_file = Path(directory) / "runtime.json"
        env_file.write_text(json.dumps(values))
        guard("check-maintenance")
        cloud("run", "jobs", "update", JOB, f"--image={release}", "--args=cleanup", "--command=",
              f"--service-account={CLEANUP_SA}", f"--set-cloudsql-instances={SQL}", f"--env-vars-file={env_file}",
              f"--set-secrets=DB_PASSWORD={password}", "--tasks=1", "--parallelism=1", "--max-retries=0",
              "--task-timeout=120s", "--cpu=1", "--memory=512Mi")
        guard("check-maintenance")
        # update, not deploy: the existing preview's public IAM + invoker-check boundary is preserved.
        cloud("run", "services", "update", SERVICE, f"--image={release}", "--args=serve", "--command=",
              f"--service-account={API_SA}", "--execution-environment=gen2", f"--set-cloudsql-instances={SQL}",
              f"--env-vars-file={env_file}", f"--set-secrets=DB_PASSWORD={password},TRACE_EDGE_SHARED_SECRET={edge}",
              "--port=8080", "--cpu=1", "--memory=512Mi", "--concurrency=40", "--timeout=3600s",
              "--min-instances=0", "--max-instances=1", "--cpu-throttling",
              "--startup-probe=httpGet.path=/health/ready,httpGet.port=8080,initialDelaySeconds=0,periodSeconds=5,timeoutSeconds=2,failureThreshold=12",
              "--liveness-probe=httpGet.path=/health/live,httpGet.port=8080,periodSeconds=30,timeoutSeconds=2,failureThreshold=3")
    # Route only the reviewed revision while both services are still explicitly stopped.
    cloud("run", "services", "update-traffic", SERVICE, "--to-latest")
    guard("check-maintenance")
    prepared(release, values, password, edge)
    require(preview_boundary() == policy, "Preview IAM changed externally; keep maintenance and review manually")
    try:
        cloud("run", "services", "update", SERVICE, "--scaling=auto", "--min=0", "--max=1")
        guard("check-active")
    except BaseException:
        # Failure recovery stops only the new preview, never reactivates an old-schema/GPS writer.
        cloud("run", "services", "update", SERVICE, "--scaling=0")
        raise


def main():
    require(len(sys.argv) == 2 and sys.argv[1] in ("check", "quiesce", "migrate", "runtime-grants", "serve", "schedule"),
            "Usage: bash deploy/gcp/deploy.sh check|quiesce|migrate|runtime-grants|serve|schedule")
    target()
    mode = sys.argv[1]
    if mode == "check":
        guard("check-active")
        return
    release = image()  # Reject mutable/wrong-target images before every mutating mode.
    existing()
    if mode == "quiesce":
        cloud("scheduler", "jobs", "pause", JOB, scheduler=True)
        # Private first: even a partial failure never intentionally restores the old writer.
        cloud("run", "services", "update", STOPPED_SERVICE, "--scaling=0")
        cloud("run", "services", "update", SERVICE, "--scaling=0")
        guard("check-maintenance")
    elif mode in ("migrate", "runtime-grants"):
        expected("GCP_MIGRATION_SA", MIGRATION_SA)
        password = secret("GCP_OWNER_PASSWORD_SECRET", "trace-owner-db-password")
        guard("check-maintenance")
        backup()
        if mode == "migrate":
            remote_sql(schema_sql(3, latest_schema()), owner=True)
            guard("check-maintenance")
            cloud("run", "jobs", "describe", "trace-migrate")
            cloud("run", "jobs", "update", "trace-migrate", f"--image={release}", "--args=migrate", "--command=",
                  f"--service-account={MIGRATION_SA}", f"--set-cloudsql-instances={SQL}",
                  f"--set-env-vars=DB_HOST=/cloudsql/{SQL},DB_NAME=trace,DB_USER=trace_owner,DB_SSLMODE=disable,TRACE_DB_MAX_CONNECTIONS=2",
                  f"--set-secrets=DB_PASSWORD={password}", "--tasks=1", "--parallelism=1", "--max-retries=0",
                  "--task-timeout=120s", "--cpu=1", "--memory=512Mi")
            cloud("run", "jobs", "execute", "trace-migrate", "--wait", timeout=600)
            remote_sql(schema_sql(latest_schema()), owner=True)
        else:
            grants = (ROOT / "deploy/gcp/runtime-grants.sql").read_text()
            remote_sql(schema_sql(latest_schema()) + grants + runtime_sql(), owner=True)
        guard("check-maintenance")
    elif mode == "serve":
        serve(release)
    else:
        expected("GCP_SCHEDULER_SA", SCHEDULER_SA)
        require_probe()
        guard("check-active")
        api = cloud("run", "services", "describe", SERVICE)
        job = cloud("run", "jobs", "describe", JOB)
        traffic = [item for item in api["status"]["traffic"] if item.get("percent", 0) > 0]
        require(len(traffic) == 1 and traffic[0].get("percent") == 100 and traffic[0].get("revisionName"),
                "Scheduler resume requires a single known active preview revision")
        revision = cloud("run", "revisions", "describe", traffic[0]["revisionName"])
        require(revision["spec"]["containers"][0]["image"] == release
                and job["spec"]["template"]["spec"]["template"]["spec"]["containers"][0]["image"] == release,
                "Scheduler resume requires the reviewed image on BOTH preview and cleanup")
        schedule = cloud("scheduler", "jobs", "describe", JOB, scheduler=True)
        http = schedule.get("httpTarget", {})
        require(schedule.get("state") == "PAUSED" and schedule.get("schedule") == "*/5 * * * *"
                and schedule.get("timeZone") == "Etc/UTC" and http.get("httpMethod") == "POST"
                and http.get("uri") == f"https://run.googleapis.com/v2/projects/{PROJECT}/locations/{REGION}/jobs/{JOB}:run"
                and http.get("oauthToken", {}).get("serviceAccountEmail") == SCHEDULER_SA
                and http.get("oauthToken", {}).get("scope") == "https://www.googleapis.com/auth/cloud-platform"
                and not http.get("body"),
                "Review the existing paused Scheduler target/auth/schedule; this command never recreates or rewrites it")
        guard("check-active")
        cloud("scheduler", "jobs", "resume", JOB, scheduler=True)


if __name__ == "__main__":
    # Ensure temporary-job finally blocks run on ordinary operator termination too.
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))
    try:
        main()
    except (Refuse, subprocess.CalledProcessError, subprocess.TimeoutExpired, OSError, KeyError, ValueError) as error:
        print(f"Refusing manual staging rollout: {error}", file=sys.stderr)
        sys.exit(1)
