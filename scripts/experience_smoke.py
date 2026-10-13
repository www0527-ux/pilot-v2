"""Browser smoke test against a running Pilot (uses the configured model).

Run explicitly: artifacts/startup/embedding-venv/Scripts/python.exe scripts/experience_smoke.py
Creates a separate browser/device account and keeps its state in the smoke artifact.
"""
import json
import pathlib
import time
from playwright.sync_api import sync_playwright

ROOT = pathlib.Path(__file__).resolve().parents[1]
OUT = ROOT / "artifacts" / "experience-smoke"

def main():
    OUT.mkdir(parents=True, exist_ok=True)
    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True, channel="msedge")
        context = browser.new_context(viewport={"width": 1440, "height": 1000})
        page = context.new_page()
        errors = []
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.goto("http://127.0.0.1:8080/", wait_until="networkidle")
        page.wait_for_function("document.getElementById('userId').value.startsWith('account_')")
        page.locator('[data-tab="knowledge"]').click()
        page.locator("#experienceAutomatic").check()
        page.wait_for_function("!document.getElementById('experienceAutomatic').disabled")
        page.locator('[data-tab="chat"]').click()
        # These are observed test facts, not a fabricated production incident.
        question = (
            "请整理分析 Pilot 开发测试遇到的一个真实问题：Windows 上 go test ./internal/memory "
            "报 Binary was compiled with CGO_ENABLED=0, go-sqlite3 requires cgo to work。"
            "我已经确认机器没有 gcc。改用 golang:1.26-alpine 容器安装 gcc musl-dev 后，"
            "旧版 go-sqlite3 v1.14.17 又报 pread64/pwrite64/off64_t 未定义；"
            "在测试容器使用 CGO_CFLAGS='-O2 -g -Dpread64=pread -Dpwrite64=pwrite -Doff64_t=off_t' "
            "后，存储测试已经通过。先检索历史经验，再解释原因、方案适用范围及局限。"
            "这些是我提供的测试记录，不要声称观测平台能验证本机编译器环境，也不要执行修改。"
        )
        page.locator("#queryInput").fill(question)
        page.locator("#chatForm").evaluate("form => form.requestSubmit()")
        page.wait_for_function("!document.getElementById('sendBtn').disabled", timeout=180000)
        page.wait_for_selector("[data-run-id]", timeout=15000)
        run_id = page.locator("[data-run-id]").last.get_attribute("data-run-id")
        result = None
        for _ in range(90):
            response = context.request.get(f"http://127.0.0.1:8080/api/v1/experiences/jobs/{run_id}")
            if response.ok:
                result = response.json()["data"]
                if result["status"] in ("succeeded", "no_draft", "failed"):
                    break
            time.sleep(2)
        if not result or result["status"] != "succeeded":
            raise AssertionError(f"knowledge extraction did not produce a draft: {result}")
        response = context.request.get(f"http://127.0.0.1:8080/api/v1/experiences?run_id={run_id}")
        detail = response.json()["data"]["items"][0]
        entry_id = detail["entry"]["id"]
        page.evaluate("id => window.PilotKnowledge.open(id)", entry_id)
        page.wait_for_selector("#experienceEditor:not([hidden])")
        root = page.locator('#experienceEditor textarea[name="root_cause"]')
        root.fill(root.input_value() + "\n人工补充：兼容宏只用于当前 Linux 测试容器，后续应评估驱动升级。")
        page.locator("#experienceClose").click()
        page.wait_for_selector("#experienceLeave:not([hidden])")
        page.locator('[data-leave="stay"]').click()
        page.get_by_role("button", name="保存草稿", exact=True).click()
        page.wait_for_function("document.getElementById('experienceDirty').hidden && !document.getElementById('experienceDrawerError').textContent")
        page.screenshot(path=str(OUT / "draft-drawer.png"), full_page=True)
        page.get_by_role("button", name="审核通过并发布", exact=True).click()
        page.wait_for_function("document.getElementById('experienceDrawerMeta').textContent.includes('已发布')")
        page.locator("#experienceClose").click()
        page.reload(wait_until="networkidle")
        page.locator('[data-tab="knowledge"]').click()
        page.locator('[data-experience-tab="published"]').click()
        page.wait_for_function("document.getElementById('experienceList').textContent.includes('go-sqlite3') || document.getElementById('experienceList').textContent.includes('CGO') || document.getElementById('experienceList').querySelector('.experience-row')")
        fresh = context.request.get(f"http://127.0.0.1:8080/api/v1/experiences/{entry_id}").json()["data"]
        assert fresh["version"]["review_status"] == "approved"
        assert fresh["version"]["verification"] == "unverified"
        assert "人工补充" in fresh["version"]["root_cause"]
        # A second browser account cannot see the first account's knowledge/run.
        other = browser.new_context()
        other.request.post("http://127.0.0.1:8080/api/v1/account/session", data={})
        assert other.request.get(f"http://127.0.0.1:8080/api/v1/experiences/{entry_id}").status == 404
        assert other.request.post(f"http://127.0.0.1:8080/api/v1/experiences/jobs/{run_id}", data={}).status == 404
        other.close()
        page.screenshot(path=str(OUT / "published-list.png"), full_page=True)
        context.storage_state(path=str(OUT / "browser-state.json"))
        assert not errors, errors
        (OUT / "result.json").write_text(json.dumps({"run_id": run_id, "entry_id": entry_id, "job_status": result["status"], "draft_count": result["draft_count"], "checks": ["ADK + real model", "durable draft", "dirty close", "edit/save", "publish", "reload", "account isolation"]}, ensure_ascii=False, indent=2), encoding="utf-8")
        print(json.dumps({"success": True, "run_id": run_id, "draft_count": result["draft_count"]}))
        browser.close()

if __name__ == "__main__":
    main()
