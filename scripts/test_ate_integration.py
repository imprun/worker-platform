import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from scripts import ate_integration as gate


class IntegrationGateTest(unittest.TestCase):
    def test_database_jobs_and_test_files_do_not_require_live_ate(self):
        paths = ("internal/jobs/engine.go\0internal/jobs/pool_slots.go\0internal/jobs/schema_pool_slots.sql\0"
                 "internal/ateclient/client_test.go\0examples/echo/run.sh\0examples/echo/standalone/main.go\0"
                 "Dockerfile.echo\0docs/testing.md\0go.sum\0")
        with patch.object(gate, "output", side_effect=[paths, "+github.com/jackc/pgx/v5 v5.9.0 hash\n"]):
            self.assertEqual([], gate.changes("base", "head"))

    def test_ate_router_gateway_and_runtime_changes_require_live_smoke(self):
        paths = ("internal/ateclient/client.go\0internal/platform/ate_attempt.go\0examples/echo/gateway.yaml\0"
                 "cmd/actor-runtime/main.go\0Dockerfile.actor-runtime\0go.mod\0")
        with patch.object(gate, "output", side_effect=[paths, "+\tgo.ate.dev/substrate v1.1.0\n"]):
            self.assertEqual(sorted(paths.rstrip("\0").split("\0")), gate.changes("base", "head"))

    def test_worker_app_server_requires_live_ate(self):
        paths = ("cmd/run-api/main.go\0internal/workerapps/service.go\0"
                 "internal/workerapps/schema.sql\0Dockerfile.run-api\0"
                 "scripts/worker_app_integration.py\0scripts/verify-worker-oci.py\0")
        with patch.object(gate, "output", return_value=paths):
            self.assertEqual(sorted(paths.rstrip("\0").split("\0")), gate.changes("base", "head"))

    def test_no_change_run_skips_before_any_fixture_request(self):
        with (patch("sys.argv", ["ate_integration.py", "--run", "base"]),
              patch.object(gate, "resolve", side_effect=["base-sha", "head-sha"]),
              patch.object(gate, "changes", return_value=[]),
              patch.object(gate, "require_fixture") as check_fixture,
              patch.object(gate.subprocess, "run") as smoke):
            gate.main()
            check_fixture.assert_not_called()
            smoke.assert_not_called()

    def test_actions_output_and_guarded_smoke_use_the_same_gate(self):
        with (tempfile.TemporaryDirectory() as directory,
              patch.object(gate, "resolve", side_effect=["base-sha", "head-sha", "base-sha", "head-sha", "head-sha"]),
              patch.object(gate, "changes", return_value=["internal/ateclient/client.go"]),
              patch.object(gate, "output", return_value=""),
              patch.object(gate, "require_fixture") as check_fixture,
              patch.object(gate.subprocess, "run") as smoke):
            out = Path(directory) / "output"
            with (patch.dict(gate.os.environ, {"GITHUB_OUTPUT": str(out)}),
                  patch("sys.argv", ["ate_integration.py", "base"])):
                gate.main()
            self.assertEqual("required=true\n", out.read_text(encoding="utf-8"))
            with patch("sys.argv", ["ate_integration.py", "--run", "base"]):
                gate.main()
            check_fixture.assert_called_once()
            smoke.assert_called_once_with(("sh", "examples/echo/run.sh", "--packaged"), cwd=gate.ROOT, check=True)


class FixtureTest(unittest.TestCase):
    def setUp(self):
        env = {
            "KUBE_CONTEXT": "dev",
            "ATE_TOKEN_FILE": "test-token-file",
            "ATE_TEST_CLUSTER_UID": "cluster-uid",
            "ATE_TEST_NAMESPACE_UID": "namespace-uid",
            "ATE_TEST_POOL_UID": "pool-uid",
            "ATE_TEST_SPACE_UID": "space-uid",
        }
        self.enterContext(patch.dict(gate.os.environ, env))
        self.identities = [json.dumps({"metadata": {"uid": env[key]}}) for key in env if key.endswith("_UID")]

    def test_wrong_cluster_stops_before_other_requests(self):
        with patch.object(gate, "output", return_value='{"metadata":{"uid":"different"}}') as kube:
            with self.assertRaisesRegex(ValueError, "cluster UID"):
                gate.require_fixture()
            self.assertEqual(1, kube.call_count)

    def test_fixture_with_an_existing_actor_is_rejected(self):
        with patch.object(gate, "output", side_effect=[*self.identities, '{"actors":[{"metadata":{"uid":"present"}}]}']):
            with self.assertRaisesRegex(ValueError, "empty actors list"):
                gate.require_fixture()

    def test_empty_verified_fixture_passes(self):
        with patch.object(gate, "output", side_effect=[*self.identities, "{}", '{"actorTemplates":[]}', '{"items":[]}']):
            gate.require_fixture()
