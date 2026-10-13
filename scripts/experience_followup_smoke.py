"""Continue the isolated account created by experience_smoke.py.

Requires a running Pilot, Python Playwright and installed Edge. The browser
state artifact contains a credential: never print it or commit artifacts.
"""
import json
import pathlib
from playwright.sync_api import sync_playwright

ROOT = pathlib.Path(__file__).resolve().parents[1]
OUT = ROOT / "artifacts" / "experience-smoke"
BASE = "http://127.0.0.1:8080"

def main():
    result = json.loads((OUT / "result.json").read_text(encoding="utf-8"))
    entry_id = result["entry_id"]
    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True, channel="msedge")
        context = browser.new_context(storage_state=str(OUT / "browser-state.json"), viewport={"width":1440,"height":1000})
        page = context.new_page()
        errors = []
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.goto(BASE, wait_until="networkidle")
        page.wait_for_function("document.getElementById('userId').value.startsWith('account_')")
        context.request.post(BASE + "/api/v1/experiences/settings", data={"automatic": False})
        # Publish immutable v1 -> pending v2; verify a failed save keeps edits.
        page.evaluate("id => window.PilotKnowledge.open(id)", entry_id)
        page.wait_for_function("document.getElementById('experienceDrawerMeta').textContent.includes('已发布')")
        if page.get_by_role("button", name="创建修订版本", exact=True).count():
            page.get_by_role("button", name="创建修订版本", exact=True).click()
        page.wait_for_function("document.getElementById('experienceDrawerMeta').textContent.includes('v2') && document.getElementById('experienceDrawerMeta').textContent.includes('待审核')")
        text = page.locator('#experienceEditor textarea[name="solution"]')
        updated = text.input_value() + "\n人工修订验收：先明确是容器兼容宏，再考虑驱动升级。"
        text.fill(updated)

        def fail_save(route):
            if route.request.method == "POST":
                route.fulfill(status=503, content_type="application/json", body=json.dumps({"success":False,"message":"测试：临时保存失败"}))
            else:
                route.continue_()

        path = BASE + "/api/v1/experiences/" + entry_id
        page.route(path, fail_save)
        page.get_by_role("button", name="保存草稿", exact=True).click()
        page.wait_for_function("document.getElementById('experienceDrawerError').textContent.includes('临时保存失败')")
        assert text.input_value() == updated
        assert page.locator("#experienceDirty").is_visible()
        page.unroute(path, fail_save)
        page.get_by_role("button", name="保存草稿", exact=True).click()
        page.wait_for_function("document.getElementById('experienceDirty').hidden && document.getElementById('experienceDrawerError').hidden")
        assert page.locator("#experienceVersion option").count() == 2
        latest = context.request.get(path).json()["data"]
        assert latest["entry"]["published_version"] == 1
        assert latest["version"]["number"] == 2
        page.locator("#experienceClose").click()

        # A new chat should retrieve approved v1, never unpublished v2.
        page.locator('[data-tab="chat"]').click()
        page.locator("#newSessionBtn").click()
        session = page.locator("#sessionId").input_value()
        page.locator("#queryInput").fill("在我的历史排障经验中，用关键词 go-sqlite3 搜索 Alpine 下 pread64/pwrite64/off64_t 未定义的问题。搜索我的全部经验，不限定 service，service 请留空。请调用 search_experience 工具，然后引用查到的 KB ID 和版本，说明兼容宏的适用范围。不要查询观测平台，不要声称本次环境已验证。")
        page.locator("#chatForm").evaluate("form => form.requestSubmit()")
        page.wait_for_function("!document.getElementById('sendBtn').disabled", timeout=180000)
        records = context.request.get(BASE + "/api/v1/chat/runs", params={"session_id":session}).json()["data"]
        run = records[0]
        (OUT / "followup-run.json").write_text(json.dumps(run,ensure_ascii=False,indent=2),encoding="utf-8")
        assert run["status"] == "completed", run["status"]
        refs = [e["result"] for e in run["events"] if e["kind"] == "knowledge_reference"]
        assert any(r["entry_id"] == entry_id and r["version"] == 1 for r in refs), refs
        assert entry_id in run["answer"], "final answer omitted the requested knowledge ID"
        page.wait_for_function("document.getElementById('chatMessages').textContent.includes('检索历史经验')")
        page.screenshot(path=str(OUT / "next-session-retrieval.png"), full_page=True)
        # Publish v2 after retrieval, then inspect immutable v1 in the same UI.
        page.evaluate("id => window.PilotKnowledge.open(id)", entry_id)
        page.wait_for_function("document.getElementById('experienceDrawerMeta').textContent.includes('v2')")
        page.get_by_role("button", name="审核通过并发布", exact=True).click()
        page.wait_for_function("document.getElementById('experienceDrawerMeta').textContent.includes('已通过')")
        page.locator("#experienceVersion").select_option("1")
        page.wait_for_function("document.getElementById('experienceDrawerMeta').textContent.includes('v1')")
        assert page.locator('#experienceEditor textarea[name="solution"]').is_disabled()
        assert "人工修订验收" not in page.locator('#experienceEditor textarea[name="solution"]').input_value()
        assert not errors, errors
        result["retrieval_run_id"] = run["id"]
        result["checks"] += ["failed save keeps edits", "new revision", "new session retrieves only published v1", "real model cites knowledge ID", "publish v2", "immutable v1 UI"]
        (OUT / "result.json").write_text(json.dumps(result, ensure_ascii=False, indent=2), encoding="utf-8")
        print(json.dumps({"success":True,"retrieval_run_id":run["id"],"checks":result["checks"]}))
        browser.close()

if __name__ == "__main__":
    main()
