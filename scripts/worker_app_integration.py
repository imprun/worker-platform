"""전용 개발 프로젝트에서 워커 앱과 API Gateway 왕복을 검증한다."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import time
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener


NAMESPACE = "wp-product-verification"
WORKER_API = "/apis/workerplatform.imprun.dev/v1alpha3"
DIGEST_HEADER = "X-Imprun-Worker-Package-Digest"
SHA = re.compile(r"[a-f0-9]{40}")
PACKAGE = re.compile(r"[a-z0-9][a-z0-9./_:-]*@sha256:[a-f0-9]{64}")
LABEL = re.compile(r"[a-z0-9][a-z0-9-]{0,62}")
IDENTITY = re.compile(r"[A-Za-z0-9_-]{1,80}")


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def ensure(condition, message):
    if not condition:
        raise ValueError(message)


def tool(arguments, stdin=None, env=None):
    run = subprocess.run(arguments, input=stdin, env=env, text=True, capture_output=True, timeout=30)
    ensure(run.returncode == 0, f"{arguments[0]} 실행 실패")
    return run.stdout.strip()


def endpoint(value):
    url = urlsplit(value)
    ensure(url.scheme == "https" and url.hostname and url.username is None and
           not (url.query or url.fragment) and url.path in ("", "/"), "테스트 HTTPS origin을 확인하세요")
    return value.rstrip("/")


def credential(filename):
    value = Path(filename).read_text(encoding="utf-8").strip()
    ensure(value and not any(char in value for char in ("\r", "\n")), "테스트 자격증명 파일이 비었거나 잘못됐습니다")
    return value


def await_state(label, predicate, seconds=200):
    until = time.monotonic() + seconds
    while time.monotonic() < until:
        if result := predicate():
            return result
        time.sleep(1)
    raise ValueError(f"{label} 대기 시간 초과")


def app_identity(organization, project, name):
    joined = "/".join(json.dumps(field) for field in (organization, project, name))
    digest = hashlib.sha256(joined.encode()).hexdigest()
    return "app-" + digest[:36], digest


class Verification:
    def __init__(self, config, worker_sha, api_sha):
        self.cfg = config
        self.worker_sha = worker_sha
        self.api_sha = api_sha
        self.notes = [f"worker-platform: `{worker_sha}`", f"api-platform: `{api_sha}`"]
        ensure(SHA.fullmatch(worker_sha) and SHA.fullmatch(api_sha), "두 저장소의 전체 SHA가 필요합니다")
        ensure(all(IDENTITY.fullmatch(config[key]) for key in ("organization_id", "project_id")),
               "격리 조직·프로젝트 ID를 확인하세요")
        ensure(all(LABEL.fullmatch(config[key]) for key in ("project", "profile", "gateway", "pool")),
               "테스트 프로젝트, 프로필, Gateway 또는 풀 이름이 잘못됐습니다")
        self.worker = endpoint(config["worker_url"])
        self.api = endpoint(config["api_url"]) + "/api/v0.9"
        self.gateway = endpoint(config["gateway_url"])
        header = config.get("gateway_header", "Authorization")
        ensure(header.lower() in ("authorization", "api-key", "x-api-key"), "테스트 Gateway 헤더 이름을 확인하세요")
        self.gateway_auth = {header: credential(config["gateway_credential_file"])}
        self.management_auth = {"Authorization": "Bearer " + credential(config["management_token_file"])}
        self.http = build_opener(NoRedirect)
        self.kube = ("kubectl", "--context", os.environ["KUBE_CONTEXT"], "--request-timeout=20s")
        self.ate = ("kubectl", "ate", "--context", os.environ["KUBE_CONTEXT"], "--token-file", os.environ["ATE_TOKEN_FILE"])
        self.slug = "worker-proof-" + secrets.token_hex(6)
        self.api_owner = "워커 검증 " + secrets.token_hex(16)
        self.app, _ = app_identity(config["organization_id"], config["project_id"], self.slug)
        self.worker_name, self.owner = app_identity(config["organization_id"], config["project_id"], self.app)
        self.workers_path = f"{WORKER_API}/namespaces/{NAMESPACE}/httpworkers"
        self.collection = f"{self.worker}/projects/{config['project']}/worker-apps"
        self.development = f"{self.collection}/{self.app}/environments/development"
        self.rest = f"{self.api}/rest-apis/{self.slug}"
        self.connections = f"{self.api}/projects/{config['project']}/worker-apps/{self.app}/connections"
        self.public_url = f"{self.gateway}/{self.slug}/verify"
        self.pg = {**os.environ, "PGSERVICEFILE": config["pg_service_file"], "PGSERVICE": config["pg_service"]}
        self.may_have_app = False
        self.may_have_api = False
        self.may_have_route = False

    def call(self, method, url, headers, value=None, allowed=(200,)):
        body = value if value is None or isinstance(value, bytes) else json.dumps(value).encode()
        request = Request(url, data=body, headers={"Accept": "application/json", "Content-Type": "application/json", **headers}, method=method)
        try:
            response = self.http.open(request, timeout=10)
        except HTTPError as error:
            response = error
        except URLError:
            raise ValueError("테스트 HTTPS 요청 실패") from None
        with response:
            ensure(response.status in allowed, f"{method} 요청에서 HTTP {response.status}")
            raw = response.read(1 << 20)
            data = json.loads(raw) if raw and 200 <= response.status < 300 else None
            return response.status, data, response.headers

    def kube_json(self, *args):
        return json.loads(tool((*self.kube, *args, "-o", "json")))

    def raw(self, path):
        return json.loads(tool((*self.kube, "get", "--raw", path)))

    def psql(self, sql):
        sqlvars = {"fixture_org": self.cfg["organization_id"], "fixture_project": self.cfg["project_id"],
                   "fixture_app": self.app, "fixture_name": self.slug}
        variables = [part for entry in sqlvars.items() for part in ("-v", "=".join(entry))]
        return tool(("psql", "-XAtq", "-v", "ON_ERROR_STOP=1", *variables), stdin=sql, env=self.pg)

    def ate_objects(self, resource):
        objects = json.loads(tool((*self.ate, "get", resource, "-a", NAMESPACE, "-o", "json")))
        key = {"actors": "actors", "actor-templates": "actorTemplates"}[resource]
        ensure(not (set(objects) - {key}), "ATE 테스트 목록 형식이 예상과 다릅니다")
        return objects.get(key, [])

    def preflight(self):
        for actual, expected in (
            (self.kube_json("get", "namespace", "kube-system"), "cluster_uid"),
            (self.kube_json("get", "namespace", NAMESPACE), "namespace_uid"),
            (self.kube_json("-n", "ate-system", "get", "workerpool", self.cfg["pool"]), "pool_uid"),
            (json.loads(tool((*self.ate, "get", "atespace", NAMESPACE, "-o", "json"))), "atespace_uid"),
        ):
            ensure(actual["metadata"]["uid"] == self.cfg[expected] and bool(self.cfg[expected]), f"테스트 리소스 UID 불일치: {expected}")
        for workload, sha in (
            ("worker_workload", self.worker_sha), ("controller_workload", self.worker_sha),
            ("api_workload", self.api_sha), ("gateway_workload", self.api_sha),
        ):
            name = self.cfg[workload]
            ensure(LABEL.fullmatch(name), "테스트 Deployment 이름이 잘못됐습니다")
            resource = self.kube_json("-n", NAMESPACE, "get", "deployment", name)
            status = resource["status"]
            count = resource["spec"].get("replicas", 1)
            source = resource["spec"]["template"]["metadata"].get("annotations", {}).get("imprun.dev/source-sha")
            ensure(source == sha and count > 0 and status.get("observedGeneration") == resource["metadata"]["generation"] and
                   all(status.get(field) == count for field in ("replicas", "updatedReplicas", "readyReplicas", "availableReplicas")),
                   f"{workload}의 준비 상태와 검증 SHA가 일치하지 않습니다")
            images = [container["image"] for container in resource["spec"]["template"]["spec"]["containers"]]
            ensure(images and all(PACKAGE.fullmatch(image) for image in images), "검증 Deployment는 이미지 digest를 고정해야 합니다")
            self.notes.append(f"{workload} 이미지: " + ", ".join(image.split("@")[1] for image in images))
        profile = self.raw(f"{WORKER_API}/namespaces/{NAMESPACE}/workerprofiles/{self.cfg['profile']}")["spec"]
        ensure(profile["atespace"] == NAMESPACE and profile["architecture"] == "arm64" and profile["sandboxClass"] == "gvisor" and
               profile["workerPool"] == {"namespace": "ate-system", "name": self.cfg["pool"]} and
               profile["runtimeImage"] == self.cfg["runtime_image"] and PACKAGE.fullmatch(profile["runtimeImage"]),
               "고정된 런타임 이미지와 전용 ARM64 gVisor 프로필이 필요합니다")
        self.notes.append("공통 Actor 런타임: " + profile["runtimeImage"].split("@")[1])
        ensure(not self.raw(self.workers_path)["items"] and
               not self.kube_json("-n", NAMESPACE, "get", "httproutes")["items"] and
               all(not self.ate_objects(resource) for resource in ("actors", "actor-templates")),
               "테스트 공간에 기존 워커나 라우트가 있습니다")
        ensure(self.psql("SELECT current_database();") == self.cfg["pg_database"], "워커 앱 전용 데이터베이스를 확인하세요")
        ensure(self.psql("SELECT count(*) FROM worker_apps WHERE organization_id = :'fixture_org' AND project_id = :'fixture_project';") == "0",
               "테스트 프로젝트에 기존 워커 앱이 있습니다")
        _, scope, _ = self.call("GET", f"{self.api}/projects/{self.cfg['project']}/worker-app-access", self.management_auth)
        ensure(scope == {"organizationId": self.cfg["organization_id"], "projectId": self.cfg["project_id"]},
               "관리 토큰이 격리된 테스트 프로젝트를 가리키지 않습니다")
        _, gateway, _ = self.call("GET", self.api + "/gateways/" + self.cfg["gateway"], self.management_auth)
        ensure(gateway["id"] == self.cfg["gateway"] and gateway["version"] == self.cfg["gateway_version"] and
               self.gateway in [value.rstrip("/") for value in gateway.get("endpoints", [])], "등록된 전용 개발 Gateway를 확인하세요")
        self.call("GET", self.rest, self.management_auth, allowed=(404,))
        self.call("GET", self.development + "/target", self.management_auth, allowed=(404,))
        self.call("POST", self.public_url, self.gateway_auth, {"verification": self.slug}, allowed=(404,))
        self.notes.append("검증 SHA와 전용 Kubernetes·PostgreSQL 범위 확인")

    def worker_status(self):
        _, status, _ = self.call("GET", self.development, self.management_auth)
        return status

    def update(self, package):
        registry, digest = package.split("@")
        first = self.worker_status()["latestAttempt"]
        def submit():
            status = self.worker_status()
            ensure(status["latestAttempt"]["generation"] == first["generation"], "다른 요청이 테스트 앱의 배포를 변경했습니다")
            body = {"resourceVersion": status["resourceVersion"], "package": {"repository": registry, "digest": digest}}
            code, _, _ = self.call("PUT", self.development, self.management_auth, body, allowed=(202, 409))
            return code == 202
        await_state("배포 상태 충돌 해소", submit, 20)

    def is_state(self, state, active, attempted):
        status = self.worker_status()
        return (status.get("active", {}).get("appPackage") == active and
                status["latestAttempt"]["state"] == state and status["latestAttempt"].get("appPackage") == attempted)

    def expect_echo(self, stage, package):
        request = {"verification": self.slug, "stage": stage, "numbers": [1, 2, 3]}
        _, response, headers = self.call("POST", self.public_url, self.gateway_auth, request)
        ensure(response == request, f"{stage}: Echo 왕복의 JSON 값 불일치")
        digest = headers.get(DIGEST_HEADER)
        ensure(digest == package.split("@")[1], f"{stage}: 실제 응답 패키지 digest 불일치")
        self.notes.append(f"{stage}: HTTP 200, digest `{digest}`, 요청·응답 `{json.dumps(request, ensure_ascii=False)}`")
        return True

    def import_openapi(self):
        # 외부 POST 경로와 워커 /invoke의 대응을 업로드할 때 명시한다.
        json_value = {"application/json": {"schema": {}}}
        spec = {
            "openapi": "3.0.3",
            "info": {"title": self.slug, "version": "1.0"},
            "paths": {"/verify": {"post": {
                "operationId": "verifyEcho",
                "requestBody": {"required": True, "content": json_value},
                "responses": {"200": {"description": "같은 JSON 값", "content": json_value}},
            }}},
        }
        fields = {
            "id": self.slug, "displayName": self.slug, "description": self.api_owner,
            "projectId": self.cfg["project"], "version": "1.0", "context": "/" + self.slug,
            "upstream": json.dumps({"main": {}}),
            "workerAppConnections": json.dumps([{
                "path": "/verify",
                "workerApp": {"appId": self.app, "environment": "development", "method": "POST", "path": "/invoke"},
            }]),
        }
        boundary = secrets.token_hex(18)
        parts = [f'--{boundary}\r\nContent-Disposition: form-data; name="{key}"\r\n\r\n{value}'
                 for key, value in fields.items()]
        parts.append(
            f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="echo.json"\r\n'
            f'Content-Type: application/json\r\n\r\n{json.dumps(spec)}'
        )
        parts.append(f"--{boundary}--\r\n")
        headers = {**self.management_auth, "Content-Type": f"multipart/form-data; boundary={boundary}"}
        self.may_have_api = True
        _, created, _ = self.call("POST", self.api + "/rest-apis/import-openapi", headers,
                                  "\r\n".join(parts).encode(), allowed=(201,))
        ensure(self.owns_api(created), "OpenAPI 가져오기 응답의 API 소유권이 일치하지 않습니다")
        _, saved, _ = self.call("GET", self.rest + "/openapi", self.management_auth)
        ensure(json.loads(saved["content"]) == spec, "저장한 OpenAPI가 업로드한 명세와 다릅니다")
        self.check_connection(deployed=False)
        self.notes.append("OpenAPI 외부 POST /verify → 개발 워커 POST /invoke 연결 저장 확인")

    def owns_api(self, api):
        return api["id"] == self.slug and api["description"] == self.api_owner and api["projectId"] == self.cfg["project"]

    def check_connection(self, deployed):
        _, links, _ = self.call("GET", self.connections, self.management_auth)
        ensure(any(link["apiId"] == self.slug and link["method"] == "POST" and link["path"] == "/verify" and
                   link["configured"] and link["deployed"] is deployed for link in links),
               "워크플로에서 만든 API가 워커 앱 영향 목록과 일치하지 않습니다")

    def verify(self):
        initial, broken, next_package = (self.cfg[key] for key in ("package_v1", "package_failure", "package_v2"))
        ensure(all(PACKAGE.fullmatch(value) for value in (initial, broken, next_package)) and
               len({value.split("@")[1] for value in (initial, broken, next_package)}) == 3,
               "서로 다른 v1·실패·v2 패키지 digest가 필요합니다")
        self.notes.append("시험 패키지 digest: " + ", ".join(value.split("@")[1] for value in (initial, broken, next_package)))
        registry, digest = initial.split("@")
        self.may_have_app = True
        _, app, _ = self.call("POST", self.collection, self.management_auth, {
            "name": self.slug, "profile": self.cfg["profile"], "package": {"repository": registry, "digest": digest},
        }, allowed=(201,))
        ensure(app["id"] == self.app and app["projectId"] == self.cfg["project_id"], "앱 등록 범위가 기대와 다릅니다")
        self.call("PUT", self.development, self.management_auth, {}, allowed=(202,))
        await_state("활성 v1", lambda: self.is_state("ready", initial, initial))

        trial = {"verification": self.slug, "stage": "app-test"}
        _, result, _ = self.call("POST", self.development + "/invoke", self.management_auth, trial)
        ensure(result == {"status": 200, "body": trial}, "Gateway 배포 전 워커 앱 JSON 시험 실패")
        self.notes.append("Gateway 배포 전 워커 앱 JSON 시험: HTTP 200, 응답 값 일치")

        self.import_openapi()
        self.may_have_route = True
        _, deployment, _ = self.call("POST", self.rest + "/deployments", self.management_auth, {
            "name": self.slug, "base": "current", "gatewayId": self.cfg["gateway"],
        }, allowed=(201,))
        deployment_id = deployment["deploymentId"]
        ensure(IDENTITY.fullmatch(deployment_id), "생성된 Gateway 배포 ID가 잘못됐습니다")
        def gateway_ready():
            _, current, _ = self.call("GET", self.rest + "/deployments/" + deployment_id, self.management_auth)
            ensure(current.get("status") != "FAILED", "개발 Gateway 배포 실패")
            return current.get("status") == "DEPLOYED"
        await_state("개발 Gateway 배포", gateway_ready)
        self.check_connection(deployed=True)
        self.call("POST", self.public_url, {}, {"verification": self.slug}, allowed=(401, 403))
        self.call("DELETE", self.collection + "/" + self.app, self.management_auth, allowed=(409,))
        self.notes.append("활성 개발 Gateway가 사용하는 워커 앱의 삭제를 HTTP 409로 거부")
        self.notes.append(f"개발 Gateway URL: {self.public_url}")
        self.expect_echo("v1 최초 호출", initial)

        self.update(broken)
        await_state("실패 표시와 v1 유지", lambda: self.is_state("failed", initial, broken))
        self.expect_echo("후보 실패 중 v1 호출", initial)

        self.update(next_package)
        await_state("v2 준비 중 v1 유지", lambda: self.is_state("preparing", initial, next_package), 45)
        self.expect_echo("v2 준비 중 v1 호출", initial)
        await_state("활성 v2", lambda: self.is_state("ready", next_package, next_package))
        self.expect_echo("v2 호출", next_package)

        self.update(initial)
        await_state("활성 v1 롤백", lambda: self.is_state("ready", initial, initial))
        self.expect_echo("같은 URL에서 v1 롤백", initial)

    def undeploy_gateway(self):
        _, deployments, _ = self.call("GET", self.rest + "/deployments", self.management_auth)
        items = deployments["list"]
        ensure(len(items) == deployments["pagination"]["total"] <= 1, "실행에서 소유한 배포만 해제할 수 있습니다")
        if not items:
            return
        deployment = items[0]
        deployment_id = deployment["deploymentId"]
        gateway = self.cfg["gateway"]
        ensure(deployment["name"] == self.slug and deployment["gatewayId"] == gateway and
               IDENTITY.fullmatch(deployment_id), "테스트 Gateway 배포 소유권 불일치")
        status = deployment["status"]
        if status == "UNDEPLOYED":
            return
        url = self.rest + "/deployments/" + deployment_id
        if status in ("DEPLOYING", "DEPLOYED"):
            self.call("POST", url + "/undeploy?gatewayId=" + gateway, self.management_auth)
        else:
            ensure(status == "UNDEPLOYING", "테스트 Gateway 배포를 해제할 수 없는 상태입니다")

        def undeployed():
            _, current, _ = self.call("GET", url, self.management_auth)
            ensure(current["status"] in ("UNDEPLOYING", "UNDEPLOYED"), "테스트 Gateway 배포 해제 실패")
            return current["status"] == "UNDEPLOYED"

        await_state("테스트 Gateway 배포 해제", undeployed, 60)

    def cleanup(self):
        failures = []
        if self.may_have_api:
            try:
                code, api, _ = self.call("GET", self.rest, self.management_auth, allowed=(200, 404))
                if code == 200:
                    ensure(self.owns_api(api), "테스트 API 소유권 불일치")
                    if self.may_have_route:
                        self.undeploy_gateway()
                if self.may_have_route:
                    await_state("Gateway 경로 정리", lambda: self.call(
                        "POST", self.public_url, self.gateway_auth, {"verification": self.slug},
                        allowed=(200, 404, 502, 503))[0] == 404, 60)
                if code == 200:
                    self.call("DELETE", self.rest, self.management_auth, allowed=(200, 204, 404))
                    self.call("GET", self.rest, self.management_auth, allowed=(404,))
            except (ValueError, KeyError, OSError):
                failures.append("테스트 API·Gateway 배포")
        if self.may_have_app and failures:
            failures.append("테스트 워커 앱 정리 보류")
        elif self.may_have_app:
            try:
                workers = self.raw(self.workers_path)["items"]
                matches = [item["metadata"] for item in workers if item["metadata"]["name"] == self.worker_name]
                if matches:
                    metadata = matches[0]
                    ensure(metadata.get("annotations", {}).get("workerplatform.imprun.dev/worker-app-owner") == self.owner,
                           "테스트 HTTPWorker 소유권 불일치")
                    options = {"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": {"uid": metadata["uid"]}}
                    tool((*self.kube, "delete", "--raw", self.workers_path + "/" + self.worker_name, "-f", "-"),
                         stdin=json.dumps(options))
                    await_state("HTTPWorker·HTTPRoute 정리", lambda: not any(
                        item["metadata"]["name"] == self.worker_name
                        for items in (self.raw(self.workers_path)["items"], self.kube_json("-n", NAMESPACE, "get", "httproutes")["items"])
                        for item in items), 60)
                    # 삭제 전 시작한 조정이 있으면 90초 제한을 넘겨 종료될 때까지 기다린다.
                    time.sleep(95)
                for resource, singular in (("actors", "actor"), ("actor-templates", "actor-template")):
                    prefix = "wp-" + self.worker_name + "-"
                    for entry in self.ate_objects(resource):
                        name = entry["metadata"]["name"]
                        if re.fullmatch(re.escape(prefix) + r"[a-f0-9]{18}", name):
                            tool((*self.ate, "delete", singular, "-a", NAMESPACE, name))
                    await_state(f"{resource} 정리", lambda: not any(
                        entry["metadata"]["name"].startswith(prefix) for entry in self.ate_objects(resource)), 60)
                owned = "organization_id = :'fixture_org' AND project_id = :'fixture_project' AND app_id = :'fixture_app' AND name = :'fixture_name'"
                self.psql("DELETE FROM worker_apps WHERE " + owned + ";")
                ensure(self.psql("SELECT count(*) FROM worker_apps WHERE " + owned + ";") == "0", "테스트 앱 저장 데이터가 남았습니다")
            except (ValueError, KeyError, OSError, subprocess.SubprocessError):
                failures.append("테스트 워커 앱")
        self.notes.append("정리: " + (", ".join(failures) + " 확인 실패" if failures else
                                      "테스트 소유 리소스 정리 완료" if self.may_have_app or self.may_have_api else "생성된 리소스 없음"))
        ensure(not failures, "일부 테스트 리소스를 정리하지 못했습니다")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("worker_sha")
    parser.add_argument("api_sha")
    parser.add_argument("--check-dispatch", action="store_true", help="수동 요청의 입력과 실행 조건을 확인한다")
    args = parser.parse_args()
    if args.check_dispatch:
        if os.environ.get("GITHUB_REF") != "refs/heads/main":
            parser.error("워커 앱 실환경 검증은 Worker Platform main에서 실행하세요")
        if os.environ.get("PRODUCT_ATE_ENABLED") != "true":
            parser.error("워커 앱 실환경 검증에 ATE_INTEGRATION_ENABLED=true가 필요합니다")
        if not SHA.fullmatch(args.worker_sha) or not SHA.fullmatch(args.api_sha):
            parser.error("Worker Platform과 API Platform의 전체 SHA가 필요합니다")
        return 0

    result = None
    failed = False
    try:
        config = json.loads(Path(os.environ["ATE_PRODUCT_FIXTURE_FILE"]).read_text(encoding="utf-8"))
        result = Verification(config, args.worker_sha, args.api_sha)
        result.preflight()
        result.verify()
    except (KeyError, OSError, ValueError, subprocess.SubprocessError) as error:
        # 외부 HTTP·DB 응답과 예외의 원본 메시지는 기록하지 않는다.
        print("워커 앱 실환경 검증 실패:", str(error) if type(error) is ValueError else type(error).__name__)
        failed = True
    finally:
        if result is not None:
            try:
                result.cleanup()
            except (KeyError, OSError, ValueError, subprocess.SubprocessError):
                failed = True
                print("워커 앱 실환경 정리 확인 실패")
        if summary := os.environ.get("GITHUB_STEP_SUMMARY"):
            notes = result.notes if result is not None else ["검증 환경 초기화 실패", "생성된 리소스 없음"]
            run = os.environ.get("GITHUB_RUN_ID")
            if run:
                notes.append(f"Actions 실행: {os.environ['GITHUB_SERVER_URL']}/{os.environ['GITHUB_REPOSITORY']}/actions/runs/{run}")
            with open(summary, "a", encoding="utf-8") as stream:
                stream.write("## 워커 앱 제품 검증\n\n" +
                             "\n".join(f"- {note}" for note in notes) +
                             ("\n- 결과: 실패\n" if failed else "\n- 결과: 통과\n"))
    return int(failed)


if __name__ == "__main__":
    raise SystemExit(main())
