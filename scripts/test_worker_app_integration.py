from email import policy
from email.parser import BytesParser
from io import StringIO
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from scripts import worker_app_integration as smoke


class ProductIntegrationTest(unittest.TestCase):
    def test_manual_dispatch_reports_missing_prerequisites(self):
        for ref, enabled, api_sha, message in (
            ("refs/heads/feature", "true", "2" * 40, "Worker Platform main"),
            ("refs/heads/main", "false", "2" * 40, "ATE_INTEGRATION_ENABLED=true"),
            ("refs/heads/main", "true", "abbrev", "전체 SHA"),
        ):
            with (self.subTest(ref=ref, enabled=enabled, api_sha=api_sha),
                  patch.dict(smoke.os.environ, {"GITHUB_REF": ref, "PRODUCT_ATE_ENABLED": enabled}, clear=True),
                  patch("sys.argv", ["worker_app_integration.py", "--check-dispatch", "1" * 40, api_sha]),
                  patch("sys.stderr", new_callable=StringIO) as stderr,
                  patch.object(smoke, "Verification") as verification,
                  self.assertRaises(SystemExit) as error):
                smoke.main()
            self.assertEqual(2, error.exception.code)
            self.assertIn(message, stderr.getvalue())
            verification.assert_not_called()

    def test_valid_manual_dispatch_checks_inputs_without_fixture_credentials(self):
        with (patch.dict(smoke.os.environ, {"GITHUB_REF": "refs/heads/main", "PRODUCT_ATE_ENABLED": "true"}, clear=True),
              patch("sys.argv", ["worker_app_integration.py", "--check-dispatch", "1" * 40, "2" * 40]),
              patch.object(smoke, "Verification") as verification):
            self.assertEqual(0, smoke.main())
            verification.assert_not_called()

    def test_distinct_packages_and_actual_response_are_checked(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(smoke.os.environ, {
            "KUBE_CONTEXT": "isolated", "ATE_TOKEN_FILE": "/runner/ate-token",
        }):
            credential = Path(directory) / "token"
            credential.write_text("test-token", encoding="utf-8")
            config = {
                key: "fixture" for key in ("organization_id", "project_id", "project", "profile",
                                            "gateway", "pool")
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
            spec = None
            deployed = False

            def response(method, url, headers, value=None, allowed=(200,)):
                nonlocal spec, deployed
                calls.append((method, url, value))
                if url == test.collection:
                    return 201, {"id": test.app, "projectId": config["project_id"]}, {}
                if url == test.development + "/invoke":
                    return 200, {"status": 200, "body": value}, {}
                if url == test.api + "/rest-apis/import-openapi":
                    form = BytesParser(policy=policy.default).parsebytes(
                        f"Content-Type: {headers['Content-Type']}\r\n\r\n".encode() + value
                    )
                    fields = {part.get_param("name", header="content-disposition"): part.get_payload(decode=True)
                              for part in form.iter_parts()}
                    spec = json.loads(fields["file"])
                    self.assertEqual("verifyEcho", spec["paths"]["/verify"]["post"]["operationId"])
                    self.assertEqual([{
                        "path": "/verify",
                        "workerApp": {"appId": test.app, "environment": "development", "method": "POST", "path": "/invoke"},
                    }], json.loads(fields["workerAppConnections"]))
                    return 201, {"id": fields["id"].decode(), "description": fields["description"].decode(),
                                 "projectId": fields["projectId"].decode()}, {}
                if url == test.rest + "/openapi":
                    return 200, {"content": json.dumps(spec)}, {}
                if url == test.connections:
                    return 200, [{"apiId": test.slug, "method": "POST", "path": "/verify",
                                  "configured": True, "deployed": deployed}], {}
                if url.endswith("/deployments"):
                    return 201, {"deploymentId": "test-deployment"}, {}
                if url.endswith("/deployments/test-deployment"):
                    deployed = True
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
            self.assertEqual(6, sum("HTTP 200" in note for note in test.notes))
            self.assertIn(("DELETE", test.collection + "/" + test.app, None), calls)
            with patch.object(test, "call", side_effect=lambda _method, _url, _headers, data: (
                200, data, {smoke.DIGEST_HEADER: "sha256:" + "4" * 64},
            )):
                with self.assertRaisesRegex(ValueError, "실제 응답 패키지 digest 불일치"):
                    test.expect_echo("리비전 불일치", config["package_v1"])
            test.may_have_app = False
            owned_api = {"id": test.slug, "description": test.api_owner, "projectId": config["project"]}
            deployment = {"deploymentId": "test-deployment", "name": test.slug, "gatewayId": config["gateway"],
                          "status": "DEPLOYED"}
            listed = {"list": [deployment], "pagination": {"total": 1}}
            responses = [
                (200, owned_api, {}),
                (200, listed, {}),
                (200, {"status": "UNDEPLOYING"}, {}),
                (200, {"status": "UNDEPLOYING"}, {}),
                (200, {"status": "UNDEPLOYED"}, {}),
                (404, None, {}),
                (204, None, {}),
                (404, None, {}),
            ]
            auth = test.management_auth
            url = test.rest + "/deployments/test-deployment"
            with (patch.object(test, "call", side_effect=responses) as call, patch.object(smoke.time, "sleep")):
                test.cleanup()
                self.assertEqual([
                    ("GET", test.rest, auth),
                    ("GET", test.rest + "/deployments", auth),
                    ("POST", url + "/undeploy?gatewayId=" + config["gateway"], auth),
                    ("GET", url, auth),
                    ("GET", url, auth),
                    ("POST", test.public_url, test.gateway_auth, {"verification": test.slug}),
                    ("DELETE", test.rest, auth),
                    ("GET", test.rest, auth),
                ], [request.args for request in call.call_args_list])

            test.may_have_app = True
            failed = {"list": [{**deployment, "status": "FAILED"}], "pagination": {"total": 1}}
            with (patch.object(test, "call", side_effect=[(200, owned_api, {}), (200, failed, {})]),
                  patch.object(test, "raw") as kubernetes):
                with self.assertRaisesRegex(ValueError, "정리하지 못했습니다"):
                    test.cleanup()
                kubernetes.assert_not_called()
            self.assertIn("테스트 워커 앱 정리 보류", test.notes[-1])

            test.may_have_app = False
            with patch.object(test, "call", return_value=(200, {
                "id": test.slug, "description": "다른 작업에서 만든 API", "projectId": config["project"],
            }, {})) as call:
                with self.assertRaisesRegex(ValueError, "정리하지 못했습니다"):
                    test.cleanup()
                self.assertEqual(["GET"], [request.args[0] for request in call.call_args_list])

            test.may_have_api = False
            test.may_have_app = True
            app = {"id": test.app, "name": test.slug, "projectId": config["project_id"]}
            worker = {"metadata": {"name": test.worker_name, "uid": "test-uid", "annotations": {
                "workerplatform.imprun.dev/worker-app-owner": test.owner,
            }}}
            worker_lists = [{"items": [worker]}, {"items": []}]
            responses = [(200, [app], {}), (202, None, {}), (204, None, {}), (200, [], {}), (404, None, {})]
            with (patch.object(test, "call", side_effect=responses) as call,
                  patch.object(test, "raw", side_effect=worker_lists) as kube,
                  patch.object(test, "kube_json", return_value={"items": []}),
                  patch.object(test, "ate_objects", return_value=[]),
                  patch.object(smoke.time, "sleep"),
                  patch.object(smoke, "tool") as commands):
                test.cleanup()
                self.assertEqual([
                    ("GET", test.collection, auth),
                    ("DELETE", test.app_url, auth),
                    ("DELETE", test.app_url, auth),
                    ("GET", test.collection, auth),
                    ("GET", test.development + "/target", auth),
                ], [request.args for request in call.call_args_list])
                self.assertEqual([(202, 204), (202, 204)], [
                    request.kwargs["allowed"] for request in call.call_args_list if request.args[0] == "DELETE"
                ])
                self.assertEqual(2, kube.call_count)
                commands.assert_not_called()
            self.assertIn("워커 앱 삭제 API: HTTP 204", test.notes)

            with (patch.object(test, "call", return_value=(200, [{**app, "name": "다른 실행의 앱"}], {})) as call,
                  patch.object(test, "raw", return_value={"items": []}),
                  patch.object(test, "ate_objects") as ate):
                with self.assertRaisesRegex(ValueError, "정리하지 못했습니다"):
                    test.cleanup()
                call.assert_called_once_with("GET", test.collection, auth)
                ate.assert_not_called()

    def test_requires_origin_without_userinfo_or_path(self):
        self.assertEqual("https://example.test", smoke.endpoint("https://example.test/"))
        for url in ("http://example.test", "https://user:secret@example.test", "https://example.test/path"):
            with self.assertRaisesRegex(ValueError, "HTTPS origin"):
                smoke.endpoint(url)
