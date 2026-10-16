"""Real HTTP latency demo; only dedicated processes/data are created.

Uses Python's standard library. Agent checks spend configured model tokens.
All runtime files and credentials stay under ignored artifacts/observability-demo.
"""
import argparse
from http.cookiejar import LWPCookieJar
import json
import os
import re
from pathlib import Path
import signal
import socket
import statistics
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from datetime import datetime, timezone

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / "artifacts" / "observability-demo"
STATE = OUT / "state.json"
API = "http://localhost:18080"
GATEWAY = "http://127.0.0.1:18091"
INVENTORY = "http://127.0.0.1:18092"
GATEWAY_SERVICE = "pilot-demo-gateway"
INVENTORY_SERVICE = "pilot-demo-inventory"
LOCAL = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def utc():
    return datetime.now(timezone.utc).isoformat()


def save(path, data):
    path.write_text(json.dumps(data, ensure_ascii=False, indent=2), encoding="utf-8")


def state():
    if not STATE.exists():
        raise RuntimeError("Run start first.")
    return json.loads(STATE.read_text(encoding="utf-8"))


def http(url, data=None, opener=None, timeout=20):
    payload = None if data is None else json.dumps(data).encode()
    req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json"})
    open_url = opener.open if opener else LOCAL.open
    with open_url(req, timeout=timeout) as response:
        return json.load(response)


def poll(fn, timeout=60):
    deadline = time.monotonic() + timeout
    last = "not ready"
    while time.monotonic() < deadline:
        try:
            return fn()
        except (OSError, ValueError, KeyError, AssertionError, RuntimeError) as e:
            last = str(e)
            time.sleep(2)
    raise RuntimeError("Readiness timeout: " + last)


def check(condition, message):
    if not condition:
        raise RuntimeError(message)


def spawn(name, args):
    with (OUT / (name + ".log")).open("ab") as log:
        process = subprocess.Popen(args, cwd=ROOT, stdout=log, stderr=subprocess.STDOUT,
                                   creationflags=subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0)
    return {"name": name, "pid": process.pid, "executable": str(Path(args[0]).resolve())}


def start(base_config):
    if STATE.exists():
        try:
            check(http(API + "/api/v1/health/ready")["data"]["service"] == "pilot-demo-console", "unexpected service")
            check(http(GATEWAY + "/health")["service"] == GATEWAY_SERVICE, "unexpected gateway")
            check(http(INVENTORY + "/health")["service"] == INVENTORY_SERVICE, "unexpected inventory")
            traffic_file = Path(state().get("traffic_file", "missing"))
            check(traffic_file.is_file() and time.time() - traffic_file.stat().st_mtime < 30, "background traffic worker is not active")
            print("Demo is already running: " + API)
            return
        except (OSError, KeyError, RuntimeError):
            raise RuntimeError("Saved demo is incomplete. Run stop, then start; existing processes are not replaced automatically.")
    for port in (18080, 18091, 18092):
        with socket.socket() as probe:
            check(probe.connect_ex(("127.0.0.1", port)) != 0, "Port %d already in use; not replacing its owner." % port)
    OUT.mkdir(parents=True, exist_ok=True)
    binary = OUT / ("obsdemo.exe" if os.name == "nt" else "obsdemo")
    pilot = OUT / ("pilot-demo.exe" if os.name == "nt" else "pilot-demo")
    generated = OUT / "config.local.yaml"
    subprocess.run(["go", "build", "-o", str(binary), "./cmd/obsdemo"], cwd=ROOT, check=True)
    subprocess.run(["go", "build", "-o", str(pilot), "./cmd"], cwd=ROOT, check=True)
    subprocess.run([str(binary), "-role", "prepare", "-config", str(base_config), "-out", str(generated)], cwd=ROOT, check=True)
    current = {"created_at": utc(), "config": str(generated), "processes": [], "phase": None}
    save(STATE, current)
    for role in ("inventory", "gateway"):
        current["processes"].append(spawn(role, [str(binary), "-role", role, "-config", str(generated)]))
        save(STATE, current)
    current["processes"].append(spawn("console", [str(pilot), "-config", str(generated)]))
    save(STATE, current)
    poll(lambda: http(GATEWAY + "/health"))
    poll(lambda: http(INVENTORY + "/health"))
    poll(lambda: http(API + "/api/v1/health/ready"))
    current["traffic_file"] = str(OUT / (str(time.time_ns()) + "-requests.ndjson"))
    current["processes"].append(spawn("load", [str(binary), "-role", "load", "-out", current["traffic_file"]]))
    save(STATE, current)
    subprocess.run(["docker", "exec", "pilot-prometheus", "promtool", "check", "config", "/etc/prometheus/prometheus.yml"], check=True)
    subprocess.run(["docker", "kill", "--signal", "HUP", "pilot-prometheus"], check=True, stdout=subprocess.DEVNULL)
    poll(lambda: targets())
    print("Demo ready: %s; dedicated DB=pilot_demo, ES=pilot_demo_logs. Open in a separate browser profile." % API)


def targets():
    result = http("http://localhost:9090/api/v1/targets")["data"]["activeTargets"]
    demo = [t for t in result if t.get("labels", {}).get("job") == "pilot-slow-demo"]
    check(len(demo) == 2 and all(t["health"] == "up" for t in demo)
          and {t["labels"].get("service") for t in demo} == {GATEWAY_SERVICE, INVENTORY_SERVICE}, "demo Prometheus targets/labels not ready")
    return demo


def traffic(phase, seconds):
    current = state()
    http(INVENTORY + "/__demo/delay", {"delay_ms": 1500 if phase == "slow" else 40, "ttl_seconds": 900})
    record = {"phase": phase, "started_at": utc(), "samples": []}
    current["phase"] = {"name": phase, "started_at": record["started_at"]}
    save(STATE, current)
    print("%s: measuring %ss of continuous real traffic" % (phase, seconds), flush=True)
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        time.sleep(min(1, max(0, deadline - time.monotonic())))
    # Allow requests already in flight at the phase boundary to finish.
    record["ended_at"] = utc()
    time.sleep(4)
    begin, end = datetime.fromisoformat(record["started_at"]), datetime.fromisoformat(record["ended_at"])
    for line in Path(current["traffic_file"]).read_text(encoding="utf-8").splitlines():
        try:
            sample = json.loads(line)
        except json.JSONDecodeError:
            continue  # The writer may currently be appending the final line.
        if begin <= datetime.fromisoformat(sample["timestamp"]) <= end:
            record["samples"].append(sample)
    check(record["samples"], "background traffic is not producing samples; inspect load.log")
    record["samples"].sort(key=lambda s: s["timestamp"])
    record["median_ms"] = statistics.median(s["duration_ms"] for s in record["samples"])
    with LOCAL.open(GATEWAY + "/metrics", timeout=10) as response:
        exported = response.read().decode("utf-8")
    record["gateway_requests_after"] = next(float(line.rsplit(" ", 1)[1]) for line in exported.splitlines()
                                          if line.startswith("pilot_demo_http_requests_total{") and 'route="/checkout"' in line and 'status="200"' in line)
    path = OUT / (str(time.time_ns()) + "-" + phase + ".json")
    save(path, record)
    current["phase"]["record"] = str(path)
    save(STATE, current)
    check(all(s["status"] == 200 and s["trace_id"] for s in record["samples"]), "business requests failed; inspect recorded samples")
    check(record["median_ms"] >= 1000 if phase == "slow" else record["median_ms"] < 500, "unexpected measured HTTP latency")
    print(json.dumps({"phase": phase, "requests": len(record["samples"]), "median_ms": record["median_ms"]}))


def verify():
    phase = state().get("phase")
    check(phase and phase.get("record"), "Complete a traffic phase first.")
    targets()
    recording = json.loads(Path(phase["record"]).read_text(encoding="utf-8"))
    query = {"size": 10, "sort": [{"timestamp": "desc"}], "query": {"bool": {"filter": [
        {"term": {"service": GATEWAY_SERVICE}}, {"range": {"timestamp": {"gte": phase["started_at"]}}}
    ], "must": [{"match_phrase": {"message": "path=/checkout"}}]}}}

    def visible_trace():
        hits = http("http://localhost:9200/pilot_demo_logs/_search", query)["hits"]["hits"]
        check(hits, "demo business logs not yet visible in ES")
        # Try several completed requests: asynchronous exporters may publish them out of order.
        for hit in hits:
            log = hit["_source"]
            trace_id = log["trace_id"]
            try:
                data = http("http://localhost:16686/api/traces/" + trace_id).get("data", [])
            except urllib.error.HTTPError as error:
                if error.code == 404:
                    continue  # ES can publish a log before the batched Span export.
                raise
            for trace_data in data:
                spans = trace_data.get("spans", [])
                services = {k: v["serviceName"] for k, v in trace_data.get("processes", {}).items()}
                gateway = next((s for s in spans if services.get(s["processID"]) == GATEWAY_SERVICE and s["operationName"] == "HTTP GET /checkout"), None)
                client = next((s for s in spans if services.get(s["processID"]) == GATEWAY_SERVICE and s["operationName"] == "GET /inventory"), None)
                inventory = next((s for s in spans if services.get(s["processID"]) == INVENTORY_SERVICE and s["operationName"] == "HTTP GET /inventory"), None)
                if not all((gateway, client, inventory)):
                    continue
                parent = lambda s: [r["spanID"] for r in s.get("references", []) if r["refType"] == "CHILD_OF"]
                check(gateway["spanID"] in parent(client) and client["spanID"] in parent(inventory), "Trace parent/child relationships are broken")
                ms = inventory["duration"] / 1000
                check(ms >= 1000 if phase["name"] == "slow" else ms < 500, "Trace latency does not match current traffic phase")
                check(log["level"] == ("warn" if phase["name"] == "slow" else "info"), "unexpected business log severity")
                return {"trace_id": trace_id, "log_timestamp": log["timestamp"], "log_level": log["level"], "gateway_ms": gateway["duration"] / 1000, "inventory_ms": ms, "span_count": len(spans), "parent_links_valid": True}
        raise RuntimeError("No complete matching business Trace exported yet")

    proof = poll(visible_trace)
    metric = 'pilot_demo_http_requests_total{service="pilot-demo-gateway",route="/checkout",status="200"}'
    def metric_visible():
        result = http("http://localhost:9090/api/v1/query?" + urllib.parse.urlencode({"query": metric}))["data"]["result"]
        check(result and float(result[0]["value"][1]) >= recording["gateway_requests_after"], "completed phase metrics have not been scraped yet")
        return result[0]
    proof["metric"] = poll(metric_visible)
    p95_query = 'histogram_quantile(0.95,sum by(le)(rate(pilot_demo_http_request_duration_seconds_bucket{service="pilot-demo-gateway",route="/checkout"}[1m])))'
    def p95_visible():
        result = http("http://localhost:9090/api/v1/query?" + urllib.parse.urlencode({"query": p95_query}))["data"]["result"]
        check(result, "latency histogram needs multiple scrapes; use at least 120 seconds/phase")
        value = float(result[0]["value"][1])
        check(value > 1 if phase["name"] == "slow" else 0 <= value < .5, "P95 has not reflected the current phase yet")
        return value
    proof["p95_seconds"] = poll(p95_visible)
    proof["phase"] = phase["name"]
    proof["checked_at"] = utc()
    save(OUT / (str(time.time_ns()) + "-verification.json"), proof)
    print(json.dumps(proof, ensure_ascii=False), flush=True)
    return proof


def agent_check(recovery=False):
    jar = LWPCookieJar(str(OUT / "agent.cookies"))
    if Path(jar.filename).exists():
        jar.load(ignore_discard=True, ignore_expires=True)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(jar))
    def api(path, data=None):
        result = poll(lambda: http(API + "/api/v1/" + path, data, opener, timeout=15), timeout=30)
        check(result.get("success"), "API operation failed: " + path)
        return result["data"]
    account = api("account/session", {})
    jar.save(ignore_discard=True, ignore_expires=True)
    api("experiences/settings", {"automatic": False})
    current = state()
    # Each acceptance starts without previous answers/tool messages. Otherwise a
    # recent cached Trace could satisfy the model without a fresh tool call.
    session_id = "demo_" + uuid.uuid4().hex
    current["agent_session"] = session_id
    save(STATE, current)
    query = ("请重新检查 pilot-demo-gateway 最近2分钟的指标及新的请求日志，判断当前是否仍然缓慢。需要查看普通请求日志，不只筛选error。请引用实际查询的Trace ID，不要沿用上一轮结论。" if recovery else
             "pilot-demo-gateway 最近几分钟响应明显变慢。请结合日志、P95指标和链路调查，引用实际查询的Trace ID，区分已确认事实和推测。请查询最近10分钟，日志不只筛选error。")
    query += "环境提供的HTTP耗时指标名为pilot_demo_http_request_duration_seconds_bucket，service和route标签用于筛选；不要用其他框架的默认指标名代替。"
    query += "本轮仅调查当前运行数据，不查询知识库或静态拓扑。请用不超过6次工具调用获取必要证据，证据足够或存在明确缺口时直接回答，不反复探索同一指标。"
    query += "本演示的P95统计口径使用rate[1m]，因为调查阶段只有几分钟；不要用5分钟以上的平滑窗口代替。"
    query += "这是独立的新会话验收，本轮需要重新调用日志、指标和Trace工具；Trace ID必须从本轮日志中选择，不能复用历史工具结果。"
    query += "最终回答控制在500字以内，给出结论、关键证据和仍未证实的部分即可。"
    request = urllib.request.Request(API + "/api/v1/chat/stream", data=json.dumps({"session_id": session_id, "query": query, "run_id": str(uuid.uuid4()), "include_progress": True}).encode(), headers={"Content-Type": "application/json"})
    events = []
    with opener.open(request, timeout=330) as response:
        for raw in response:
            line = raw.decode("utf-8").strip()
            if line == "data: [DONE]":
                break
            if line.startswith("data: "):
                events.append(json.loads(line[6:]))
    answer = ""
    for event in events:
        if event.get("kind") == "answer":
            if event.get("status") == "replace":
                answer = event.get("text", "")
            elif event.get("status") == "delta":
                answer += event.get("text", "")
    tools = [e for e in events if e.get("kind") == "tool" and e.get("status") == "completed"]
    successful = [e for e in tools if isinstance(e.get("result"), dict) and e["result"].get("success") is True]
    log_ids, traces = set(), []
    phase_started = datetime.fromisoformat(current["phase"]["started_at"])
    for event in successful:
        if event["name"] == "query_logs":
            log_ids.update(item["trace_id"] for item in event["result"].get("result", {}).get("items", [])
                           if item.get("trace_id") and item.get("service") == GATEWAY_SERVICE
                           and datetime.fromisoformat(item["timestamp"]) >= phase_started)
        elif event["name"] == "query_traces" and event.get("parameters", {}).get("trace_id") in log_ids:
            traces.append(event)
    metric_results = [e for e in successful if e["name"] == "query_metrics"
                      and "pilot_demo_http_" in e.get("parameters", {}).get("query", "")
                      and any(int(n) > 0 for n in re.findall(r"数据点:\s*(\d+)", e["result"].get("result", "")))]
    report = {"account": account["user_id"], "session_id": session_id, "answer": answer, "tool_names": [e["name"] for e in tools], "trace_ids_from_logs": [e["parameters"]["trace_id"] for e in traces], "events": events}
    report_path = OUT / (str(time.time_ns()) + "-agent-check.json")
    report["accepted"] = False
    save(report_path, report)
    check(any(e.get("kind") == "run" and e.get("status") == "completed" for e in events), "Agent run did not complete")
    check({"query_logs", "query_metrics", "query_traces"} <= {e["name"] for e in successful}, "Agent did not successfully query all three signals; raw events saved")
    check(metric_results, "Agent did not retrieve nonempty demo metric samples; raw events saved")
    check(traces and any(e["parameters"]["trace_id"] in answer for e in traces), "Answer must cite a Trace ID actually retrieved from logs")
    check(any(GATEWAY_SERVICE in e["result"]["result"] and INVENTORY_SERVICE in e["result"]["result"] for e in traces), "Agent did not retrieve a complete cross-service Trace")
    report["accepted"] = True
    save(report_path, report)
    print(json.dumps({k: v for k, v in report.items() if k != "events"}, ensure_ascii=False), flush=True)


def stop():
    if not STATE.exists():
        print("No managed demo processes.")
        return
    current = state()
    # Verify the saved PID's image before terminating. Never stop middleware or production Pilot.
    for process in reversed(current["processes"]):
        pid, expected = process["pid"], Path(process["executable"]).resolve()
        check(expected.parent == OUT.resolve(), "Refusing to stop a process outside the demo directory")
        try:
            if os.name == "nt":
                import ctypes
                from ctypes import wintypes
                kernel = ctypes.WinDLL("kernel32", use_last_error=True)
                kernel.OpenProcess.restype = wintypes.HANDLE
                kernel.OpenProcess.argtypes = [wintypes.DWORD, wintypes.BOOL, wintypes.DWORD]
                kernel.QueryFullProcessImageNameW.argtypes = [wintypes.HANDLE, wintypes.DWORD, wintypes.LPWSTR, ctypes.POINTER(wintypes.DWORD)]
                kernel.TerminateProcess.argtypes = [wintypes.HANDLE, wintypes.UINT]
                kernel.CloseHandle.argtypes = [wintypes.HANDLE]
                handle = kernel.OpenProcess(0x1000 | 1, False, pid)
                if not handle:
                    continue
                try:
                    size, image = wintypes.DWORD(32768), ctypes.create_unicode_buffer(32768)
                    check(kernel.QueryFullProcessImageNameW(handle, 0, image, ctypes.byref(size)), "Cannot identify managed process")
                    check(Path(image.value).resolve() == expected, "PID has been reused; refusing to stop unrelated process")
                    check(kernel.TerminateProcess(handle, 0), "Failed to stop managed process")
                finally:
                    kernel.CloseHandle(handle)
            else:
                actual = Path("/proc/%d/exe" % pid).resolve(strict=True)
                check(actual == expected, "PID has been reused; refusing to stop unrelated process")
                os.kill(pid, signal.SIGTERM)
        except (ProcessLookupError, FileNotFoundError):
            pass
    STATE.rename(OUT / (str(time.time_ns()) + "-stopped-state.json"))
    print("Stopped only managed demo processes. Demo data retained; middleware and normal Pilot untouched.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["start", "baseline", "slow", "recover", "verify", "agent", "cycle", "stop"])
    default_config = ROOT / "artifacts/startup/config.deepseek.yaml"
    parser.add_argument("--config", type=Path, default=default_config if default_config.exists() else ROOT / "manifest/config/config.yaml")
    parser.add_argument("--seconds", type=int, default=120)
    parser.add_argument("--agent", action="store_true", help="cycle: also make real model calls after slow/recovery")
    args = parser.parse_args()
    check(1 <= args.seconds <= 600, "seconds must be between 1 and 600")
    if args.action == "start":
        start(args.config.resolve())
    elif args.action in ("baseline", "slow", "recover"):
        traffic(args.action, args.seconds)
    elif args.action == "verify":
        verify()
    elif args.action == "agent":
        phase = state().get("phase")
        check(phase and phase.get("record"), "Complete a traffic phase before Agent acceptance.")
        agent_check(phase["name"] == "recover")
    elif args.action == "cycle":
        start(args.config.resolve())
        for phase in ("baseline", "slow", "recover"):
            traffic(phase, args.seconds)
            verify()
            if args.agent and phase != "baseline":
                agent_check(phase == "recover")
    else:
        stop()


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print("Demo failed: " + str(error), flush=True)
        raise SystemExit(1)
