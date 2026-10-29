"""Offline operator safety boundaries; no credentials or cloud access."""

from datetime import datetime, timezone
import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("manual_staging", Path(__file__).resolve().parents[1] / "manual_staging.py")
manual = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(manual)


class ManualSafetyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.env = {"GCP_PROJECT_ID": manual.PROJECT, "GCP_REGION": manual.REGION,
                    "GCP_SQL_CONNECTION": manual.SQL,
                    "GCP_IMAGE": f"{manual.REGION}-docker.pkg.dev/{manual.PROJECT}/trace/backend@sha256:{'a' * 64}",
                    "GCP_BACKUP_ID": "123", "GCP_QUIESCED_AT": "2026-10-28T10:00:00Z"}
        environment = patch.dict(os.environ, self.env, clear=True)
        environment.start()
        self.addCleanup(environment.stop)
        root = patch.object(manual, "ROOT", self.root)
        root.start()
        self.addCleanup(root.stop)
        for name in ("command", "guard"):
            blocked = patch.object(manual, name, side_effect=AssertionError("Offline test attempted a subprocess"))
            blocked.start()
            self.addCleanup(blocked.stop)
        self.runtime = {"DB_HOST": f"/cloudsql/{manual.SQL}", "DB_NAME": "trace", "DB_USER": "trace_app",
                        "DB_SSLMODE": "disable", "TRACE_DB_MAX_CONNECTIONS": "5",
                        **dict.fromkeys(manual.FLAGS, "false")}

    def write_runtime(self, values, extra=""):
        path = self.root / "deploy/gcp/runtime.env.yaml"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("\n".join(f"{key}: {json.dumps(value)}" for key, value in values.items()) + "\n" + extra)

    def test_wrong_target_mutable_image_or_obsolete_keys_never_touch_cloud(self):
        variants = [{"GCP_PROJECT_ID": "another-project"}, {"GCP_REGION": "us-central1"},
                    {"GCP_SQL_CONNECTION": manual.SQL + "-other"},
                    {"GCP_IMAGE": f"{manual.REGION}-docker.pkg.dev/{manual.PROJECT}/trace/backend:latest"},
                    {"GCP_RUNTIME_SA": "old-account"}, {"GCP_DISPLAY_TOKEN_SECRET": "old-key:1"},
                    {"GCP_ARRIVAL_ISSUER_SECRET": "obsolete-key:1"}]
        for values in variants:
            with self.subTest(values=values), patch.dict(os.environ, values), patch.object(manual, "cloud") as cloud:
                with patch.object(sys, "argv", ["deploy.sh", "quiesce"]), self.assertRaises(manual.Refuse):
                    manual.main()
                cloud.assert_not_called()

    def test_runtime_never_enables_features_or_embeds_credentials(self):
        for name in manual.FLAGS:
            with self.subTest(feature=name):
                self.write_runtime({**self.runtime, name: "true"})
                with self.assertRaises(manual.Refuse):
                    manual.runtime_env()
        for name in ("DB_PASSWORD", "DATABASE_URL", "TRACE_EDGE_SHARED_SECRET", "TRACE_PUSH_VAPID_PRIVATE_KEY", "TRACE_ARRIVAL_ISSUER_KEY"):
            with self.subTest(credential=name):
                self.write_runtime({**self.runtime, name: "offline-not-a-credential"})
                with self.assertRaises(manual.Refuse):
                    manual.runtime_env()

    def test_operator_flag_must_be_explicitly_closed_without_cloud_access(self):
        for value in ("true", None):
            runtime = {**self.runtime, "TRACE_OPERATOR_ENABLED": value}
            if value is None:
                del runtime["TRACE_OPERATOR_ENABLED"]
            self.write_runtime(runtime)
            with patch.object(manual, "cloud") as cloud, self.assertRaises(manual.Refuse):
                manual.runtime_env()
            cloud.assert_not_called()

    def test_ambiguous_or_incomplete_runtime_is_rejected(self):
        for extra in ('DB_USER: "another-role"\n', 'nested:\n  DB_USER: "trace_owner"\n'):
            self.write_runtime(self.runtime, extra)
            with self.assertRaises(manual.Refuse):
                manual.runtime_env()
        for changes in ({"DB_HOST": "/cloudsql/other-instance"}, {"TRACE_DB_MAX_CONNECTIONS": "6"},
                        {"TRACE_CONTACT": "REPLACE_CONTACT"}):
            self.write_runtime({**self.runtime, **changes})
            with self.assertRaises(manual.Refuse):
                manual.runtime_env()
        incomplete = dict(self.runtime)
        del incomplete[manual.FLAGS[0]]
        self.write_runtime(incomplete)
        with self.assertRaises(manual.Refuse):
            manual.runtime_env()

    def test_backup_must_be_completed_on_demand_after_quiesce(self):
        backup = {"id": "123", "instance": "trace-postgres", "status": "SUCCESSFUL", "type": "ON_DEMAND",
                  "startTime": "2026-10-28T10:01:00Z", "endTime": "2026-10-28T10:02:00Z"}

        class Clock(datetime):
            @classmethod
            def now(cls, tz=None):
                return datetime(2026, 10, 28, 11, tzinfo=timezone.utc)

        cases = [{"id": "124"}, {"instance": "other-database"}, {"status": "RUNNING"},
                 {"type": "AUTOMATED"}, {"startTime": "2026-10-28T09:59:00Z"},
                 {"endTime": "2026-10-28T12:00:00Z"}, {"endTime": "invalid"}]
        with patch.object(manual, "datetime", Clock):
            for changes in cases:
                with self.subTest(changes=changes), patch.object(manual, "command", return_value=json.dumps({**backup, **changes})):
                    with self.assertRaises(manual.Refuse):
                        manual.backup()
            with patch.object(manual, "command", return_value=json.dumps(backup)):
                manual.backup()
                with patch.dict(os.environ, {"GCP_QUIESCED_AT": ""}), self.assertRaises(manual.Refuse):
                    manual.backup()

    def test_tag_only_revision_prevents_quiesce_before_any_cloud_write(self):
        tagged = {"status": {"traffic": [{"tag": "old-writer", "revisionName": "old-schema"}]}}
        mutations = []

        def cloud(*args, **kwargs):
            if args[:3] == ("run", "services", "describe"):
                return tagged
            mutations.append(args)
            return {}

        with patch.object(sys, "argv", ["deploy.sh", "quiesce"]), patch.object(manual, "cloud", side_effect=cloud):
            with self.assertRaises(manual.Refuse):
                manual.main()
        self.assertEqual(mutations, [])

    def test_missing_duplicate_or_noncontiguous_migrations_cannot_define_release_schema(self):
        directory = self.root / "internal/store/migrations"
        directory.mkdir(parents=True)
        for names in ([], ["001_initial.sql", "003_gap.sql"], ["001_a.sql", "001_duplicate.sql"], ["bad.sql"]):
            with self.subTest(names=names):
                for name in names:
                    (directory / name).write_text("SELECT 1;\n")
                with self.assertRaises(manual.Refuse):
                    manual.latest_schema()
                for path in directory.iterdir():
                    path.unlink()


if __name__ == "__main__":
    unittest.main()
