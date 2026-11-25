import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from scripts import worker_app_integration as smoke


class ProductIntegrationTest(unittest.TestCase):
    def test_distinct_packages_and_actual_response_are_checked(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(smoke.os.environ, {
            "KUBE_CONTEXT": "isolated", "ATE_TOKEN_FILE": "/runner/ate-token",
        }):
            credential = Path(directory) / "token"
            credential.write_text("test-token", encoding="utf-8")
            config = {
                key: "fixture" for key in ("organization_id", "project_id", "project", "profile",
                                            "gateway", "pool", "pg_service_file", "pg_service")
            }
            config.update({
                "worker_url": "https://worker.example.test",
                "api_url": "https://api.example.test",
                "gateway_url": "https://gateway.example.test",
                "gateway_credential_file": str(credential),
                "management_token_file": str(credential),
            })
            for key, char in (("package_v1", "1"), ("package_failure", "2"), ("package_v2", "3")):
                config[key] = "registry.example.test/tests/echo@sha256:" + char * 64
            test = smoke.Verification(config, "1" * 40, "2" * 40)
            calls = []
            active = config["package_v1"]

            def response(method, url, headers, value=None, allowed=(200,)):
                calls.append((method, url, value))
                if url == test.collection:
                    return 201, {"id": test.app, "projectId": config["project_id"]}, {}
                if url.endswith("/deployments"):
                    return 201, {"deploymentId": "test-deployment"}, {}
                if url.endswith("/deployments/test-deployment"):
                    return 200, {"status": "DEPLOYED"}, {}
                if url == test.public_url and headers:
                    return 200, value, {smoke.DIGEST_HEADER: active.split("@")[1]}
                return allowed[0], None, {}

            def status(state, serving, attempted):
                nonlocal active
                active = serving
                states.append((state, serving, attempted))
                return True

            states = []
            with (patch.object(test, "call", side_effect=response),
                  patch.object(test, "is_state", side_effect=status),
                  patch.object(test, "update") as update):
                test.verify()
                self.assertEqual([config["package_failure"], config["package_v2"], config["package_v1"]],
                                 [call.args[0] for call in update.call_args_list])
            self.assertEqual(["ready", "failed", "preparing", "ready", "ready"], [state[0] for state in states])
            requests = [value for _, url, value in calls if url == test.public_url and "stage" in value]
            self.assertEqual(5, len(requests))
            self.assertEqual(5, sum("HTTP 200" in note for note in test.notes))
            with patch.object(test, "call", side_effect=lambda _method, _url, _headers, data: (
                200, data, {smoke.DIGEST_HEADER: "sha256:" + "4" * 64},
            )):
                with self.assertRaisesRegex(ValueError, "실제 응답 패키지 digest 불일치"):
                    test.expect_echo("리비전 불일치", config["package_v1"])

    def test_requires_origin_without_userinfo_or_path(self):
        self.assertEqual("https://example.test", smoke.endpoint("https://example.test/"))
        for url in ("http://example.test", "https://user:secret@example.test", "https://example.test/path"):
            with self.assertRaisesRegex(ValueError, "HTTPS origin"):
                smoke.endpoint(url)
