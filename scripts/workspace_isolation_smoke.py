"""Delay real API responses/SSE replay to check view ownership; no model calls.

Held responses come from the running service, not fabricated fixtures.
"""
import json
from pathlib import Path
from playwright.sync_api import sync_playwright

ROOT=Path(__file__).resolve().parents[1];BASE='http://127.0.0.1:8080'
OUT=ROOT/'artifacts'/'workspace-ui'

def main():
    with sync_playwright() as p:
        browser=p.chromium.launch(headless=True,channel='msedge')
        fixture=ROOT/'artifacts'/'experience-smoke'
        ctx=browser.new_context(storage_state=str(fixture/'browser-state.json'),viewport={'width':1440,'height':1000})
        entry=json.loads((fixture/'result.json').read_text(encoding='utf-8'))['entry_id']
        detail=ctx.request.get(BASE+'/api/v1/experiences/'+entry).json()['data'];sid=detail['entry']['session_id']
        account=ctx.request.post(BASE+'/api/v1/account/session',data={}).json()['data']['user_id']
        records=ctx.request.get(BASE+'/api/v1/chat/runs?session_id='+sid+'&user_id='+account).json()['data']
        record=next(r for r in records if r['status']=='completed')
        held=[];requests=[];errors=[]
        def hold(route):held.append((route,route.fetch()))
        def wait_held():
            for _ in range(50):
                if held:return
                page.wait_for_timeout(100)
            raise AssertionError('real API response was not held within 5 seconds')
        page=ctx.new_page();page.on('pageerror',lambda e:errors.append(str(e)))
        page.on('request',lambda r:requests.append((r.method,r.url)))
        page.add_init_script('localStorage.setItem("pilot-session",'+json.dumps(json.dumps({'userId':account,'sessionId':sid}))+');localStorage.setItem("pilot-active-run",'+json.dumps(json.dumps({'id':record['id'],'query':record['query'],'session':{'userId':account,'sessionId':sid},'mode':'stream'}))+');')
        page.route('**/chat/runs/*/events?*',hold)
        page.goto(BASE,wait_until='domcontentloaded')
        page.wait_for_function('state.sending && !!state.activeRun')
        wait_held()
        # UI leaves a live subscription without cancelling or starting another Run.
        page.locator('#newSessionBtn').click()
        assert page.locator('#sessionId').input_value()==''
        assert page.locator('.msg').count()==0
        assert page.evaluate('state.sending && !!state.activeRun')
        route,response=held.pop(0);route.fulfill(response=response)
        page.wait_for_function('!state.sending')
        assert page.locator('.msg').count()==0
        assert not any(m=='POST' and '/chat/stream' in u for m,u in requests)
        page.unroute('**/chat/runs/*/events?*',hold)
        # Cached stream receives the complete final answer, isolated from blank view.
        page.evaluate('id=>window.PilotWorkspace.select(id)',sid)
        page.wait_for_selector('.msg-assistant .msg-content')
        assert page.locator('.msg-assistant .msg-content').inner_text().strip()
        assert page.evaluate('document.querySelector(".execution-panel").open===false')
        # A late history response cannot replace the new blank session.
        held=[];page.route('**/chat/history?*',hold)
        page.evaluate('id=>{window.PilotWorkspace.select(id,true)}',sid)
        page.wait_for_function('state.historyLoading')
        wait_held()
        page.locator('#newSessionBtn').click()
        for route,response in held:route.fulfill(response=response)
        page.wait_for_timeout(300)
        assert page.locator('.msg').count()==0
        assert page.locator('#sessionId').input_value()==''
        assert not page.locator('#sendBtn').is_disabled()
        page.unroute('**/chat/history?*',hold)
        # A late reviewer fetch cannot reopen a panel after session switch.
        held=[];page.route(BASE+'/api/v1/experiences/'+entry,hold)
        page.evaluate('id=>{window.PilotKnowledge.open(id)}',entry)
        wait_held()
        page.locator('#newSessionBtn').click()
        for route,response in held:route.fulfill(response=response)
        page.wait_for_timeout(300)
        assert not page.locator('#experienceDrawer').is_visible()
        assert not errors,errors
        OUT.mkdir(parents=True,exist_ok=True)
        (OUT/'isolation-result.json').write_text(json.dumps({'checks':['actual_sse_replay','switch_during_subscription','no_duplicate_post','cached_final_answer','late_history_ignored','late_review_ignored'],'model_calls':0},indent=2),encoding='utf-8')
        ctx.close();browser.close();print('Actual SSE replay and asynchronous view isolation passed; no model calls.')

if __name__=='__main__':main()
