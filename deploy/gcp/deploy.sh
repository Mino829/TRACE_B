#!/usr/bin/env bash
# Run from repository root after reviewing docs/google-cloud.md.
# Does not create Cloud SQL, service accounts, secrets, or public access.
set -euo pipefail
mode="${1:-}"
case "$mode" in migrate|serve|schedule) ;; *) echo "Usage: bash deploy/gcp/deploy.sh migrate|serve|schedule" >&2; exit 2;; esac
: "${GCP_PROJECT_ID:?set GCP_PROJECT_ID}"
: "${GCP_REGION:?set GCP_REGION}"
: "${GCP_SQL_CONNECTION:?set GCP_SQL_CONNECTION}"
: "${GCP_IMAGE:?set GCP_IMAGE}"
if [[ "$GCP_PROJECT_ID$GCP_IMAGE$GCP_SQL_CONNECTION" == *replace* ]]; then
  echo "Replace example project, image and SQL connection values first" >&2; exit 2
fi
common=(--project="$GCP_PROJECT_ID" --region="$GCP_REGION")
case "$mode" in
  migrate)
    : "${GCP_MIGRATION_SA:?set GCP_MIGRATION_SA}"
    : "${GCP_OWNER_PASSWORD_SECRET:?set GCP_OWNER_PASSWORD_SECRET}"
    gcloud run jobs deploy trace-migrate "${common[@]}" \
      --image="$GCP_IMAGE" --args=migrate --service-account="$GCP_MIGRATION_SA" \
      --set-cloudsql-instances="$GCP_SQL_CONNECTION" \
      --set-env-vars="^;^DB_HOST=/cloudsql/$GCP_SQL_CONNECTION;DB_NAME=trace;DB_USER=trace_owner;DB_SSLMODE=disable;TRACE_DB_MAX_CONNECTIONS=2" \
      --set-secrets="DB_PASSWORD=$GCP_OWNER_PASSWORD_SECRET" \
      --tasks=1 --parallelism=1 --max-retries=0 --task-timeout=120s --cpu=1 --memory=512Mi
    gcloud run jobs execute trace-migrate "${common[@]}" --wait
    ;;
  serve)
    : "${GCP_RUNTIME_SA:?set GCP_RUNTIME_SA}"
    : "${GCP_APP_PASSWORD_SECRET:?set GCP_APP_PASSWORD_SECRET}"
    env_file=deploy/gcp/runtime.env.yaml
    if [[ ! -f "$env_file" ]] || grep -q REPLACE "$env_file"; then
      echo "Prepare $env_file with real deployment settings" >&2; exit 2
    fi
    runtime_secrets="DB_PASSWORD=$GCP_APP_PASSWORD_SECRET"
    if [[ -n "${GCP_DISPLAY_ISSUER_SECRET:-}" || -n "${GCP_DISPLAY_TOKEN_SECRET:-}" ]]; then
      : "${GCP_DISPLAY_ISSUER_SECRET:?set both display Secret Manager version references}"
      : "${GCP_DISPLAY_TOKEN_SECRET:?set both display Secret Manager version references}"
      runtime_secrets+=",TRACE_DISPLAY_ISSUER_KEY=$GCP_DISPLAY_ISSUER_SECRET,TRACE_DISPLAY_TOKEN_KEY=$GCP_DISPLAY_TOKEN_SECRET"
    fi
    gcloud run jobs deploy trace-cleanup "${common[@]}" \
      --image="$GCP_IMAGE" --args=cleanup --service-account="$GCP_RUNTIME_SA" \
      --set-cloudsql-instances="$GCP_SQL_CONNECTION" --env-vars-file="$env_file" \
      --set-secrets="$runtime_secrets" \
      --tasks=1 --parallelism=1 --max-retries=2 --task-timeout=120s --cpu=1 --memory=512Mi
    # Position projection and rate limits remain process-local. Never run
    # overlapping revisions or more than one active Backend instance.
    gcloud run deploy trace-backend "${common[@]}" \
      --image="$GCP_IMAGE" --args=serve --service-account="$GCP_RUNTIME_SA" \
      --execution-environment=gen2 --set-cloudsql-instances="$GCP_SQL_CONNECTION" \
      --env-vars-file="$env_file" --set-secrets="$runtime_secrets" \
      --port=8080 --cpu=1 --memory=512Mi --concurrency=40 --timeout=3600s \
      --min=1 --max=1 --no-cpu-throttling --no-allow-unauthenticated --invoker-iam-check \
      --startup-probe="httpGet.path=/health/ready,httpGet.port=8080,initialDelaySeconds=0,periodSeconds=5,timeoutSeconds=2,failureThreshold=12" \
      --liveness-probe="httpGet.path=/health/live,httpGet.port=8080,periodSeconds=30,timeoutSeconds=2,failureThreshold=3"
    ;;
  schedule)
    : "${GCP_SCHEDULER_SA:?set GCP_SCHEDULER_SA}"
    scheduler=(--project="$GCP_PROJECT_ID" --location="$GCP_REGION")
    action=create
    if gcloud scheduler jobs describe trace-cleanup "${scheduler[@]}" >/dev/null 2>&1; then action=update; fi
    gcloud scheduler jobs "$action" http trace-cleanup "${scheduler[@]}" \
      --schedule="*/5 * * * *" --time-zone=Etc/UTC \
      --uri="https://run.googleapis.com/v2/projects/$GCP_PROJECT_ID/locations/$GCP_REGION/jobs/trace-cleanup:run" \
      --http-method=POST --oauth-service-account-email="$GCP_SCHEDULER_SA" \
      --oauth-token-scope=https://www.googleapis.com/auth/cloud-platform
    ;;
esac
