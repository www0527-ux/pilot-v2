"""Actual page/storage regression; no model calls and no mocked business records.

Uses an isolated account's existing experience and Run fixtures. Request failure
injection only verifies that unsaved edits survive a transport error.
"""
import json
from pathlib import Path
from playwright.sync_api import sync_playwright

ROOT=Path(__file__).resolve().parents[1]
OUT=ROOT/'artifacts'/'workspace-ui'
BASE='http://127.0.0.1:8080'

def main():
    OUT.mkdir(parents=True,exist_ok=True)
    with sync_playwright() as p:
        browser=p.chromium.launch(headless=True,channel='msedge')
        fixture=ROOT/'artifacts'/'experience-smoke'
        context=browser.new_context(storage_state=str(fixture/'browser-state.json'),viewport={'width':1600,'height':1000})
        page=context.new_page();errors=[];requests=[]
        page.on('pageerror',lambda e:errors.append(str(e)))
        page.on('request',lambda r:requests.append((r.method,r.url)))
        page.goto(BASE,wait_until='networkidle')
        page.wait_for_function("document.getElementById('userId').value.startsWith('account_')")
        assert page.evaluate("(()=>{const ids=[...document.querySelectorAll('[id]')].map(n=>n.id);return ids.length===new Set(ids).size})()")
        assert page.locator('.controls-card').count()==0
        before=len(requests);page.locator('#newSessionBtn').click()
        page.wait_for_function("document.getElementById('sessionId').value==='' && !document.getElementById('chatMessages').children.length")
        assert not any('/sessions/' in u and m=='POST' for m,u in requests[before:])
        page.screenshot(path=str(OUT/'chat-empty-desktop.png'),full_page=True)
        # Restore real runs by owned experience provenance.
        entry=json.loads((fixture/'result.json').read_text(encoding='utf-8'))['entry_id']
        detail=context.request.get(BASE+'/api/v1/experiences/'+entry).json()['data']
        session=detail['entry']['session_id']
        page.evaluate('id=>window.PilotWorkspace.select(id,true)',session)
        page.wait_for_selector('.msg-assistant')
        assert page.locator('.execution-panel').count()>0
        trail=page.locator('.execution-panel').first
        assert not trail.evaluate('n=>n.open')
        trail.locator(':scope > summary').click();assert trail.evaluate('n=>n.open')
        trail.locator(':scope > summary').click()
        count=page.locator('.msg').count()
        page.locator('#newSessionBtn').click();page.wait_for_function("document.getElementById('sessionId').value===''")
        page.locator('#loadHistoryBtn').click()
        page.locator('.history-row').first.click()
        page.wait_for_selector('.msg-assistant');assert page.locator('.msg').count()==count
        page.evaluate('id=>window.PilotKnowledge.open(id)',entry)
        page.wait_for_selector('#experienceReading h3')
        assert not page.locator('#experienceEditor').is_visible()
        chat=page.locator('.chat-layout').bounding_box();drawer=page.locator('#experienceDrawer').bounding_box()
        assert chat['x']+chat['width']<=drawer['x']+1, (chat,drawer)
        page.screenshot(path=str(OUT/'chat-review-desktop.png'),full_page=True)
        # Shared reviewer is moved, not recreated, on knowledge navigation.
        page.locator('[data-tab="knowledge"]').click()
        assert page.locator('#knowledgeDetailMount #experienceDrawer').count()==1
        assert page.locator('#experienceReading h3').inner_text()==detail['version']['title']
        if page.get_by_role('button',name='创建修订版本',exact=True).count():
            page.get_by_role('button',name='创建修订版本',exact=True).click()
        page.wait_for_selector('#experienceEdit:not([hidden])')
        page.locator('#experienceEdit').click()
        field=page.locator('#experienceEditor textarea[name="conditions"]')
        field.fill(field.input_value()+'\n界面优化验收：保存失败时保留编辑。')
        page.locator('#newSessionBtn').evaluate('n=>n.click()')
        page.wait_for_selector('#experienceLeave:not([hidden])')
        assert page.locator('#sessionId').input_value()==session
        page.get_by_role('button',name='继续编辑',exact=True).click()
        path=BASE+'/api/v1/experiences/'+entry
        def fail(route):
            if route.request.method=='POST':route.fulfill(status=503,content_type='application/json',body=json.dumps({'success':False,'message':'验收临时网络错误'}))
            else:route.continue_()
        page.route(path,fail);page.get_by_role('button',name='保存草稿',exact=True).click()
        page.wait_for_selector('#experienceDrawerError:not([hidden])')
        assert '保留编辑' in field.input_value()
        page.unroute(path,fail);page.get_by_role('button',name='保存草稿',exact=True).click()
        page.wait_for_function("document.getElementById('experienceDirty').hidden && document.getElementById('experienceEditor').hidden")
        assert '保留编辑' in page.locator('#experienceReading').inner_text()
        page.screenshot(path=str(OUT/'knowledge-desktop.png'),full_page=True)
        page.get_by_role('button',name='审核通过并发布',exact=True).click()
        page.wait_for_function("document.getElementById('experienceDrawerMeta').textContent.includes('已发布')")
        # Reject a new draft through the same actual mutation API.
        page.get_by_role('button',name='创建修订版本',exact=True).click()
        page.wait_for_selector('#experienceActions button:text-is("拒绝收录")')
        page.get_by_role('button',name='拒绝收录',exact=True).click()
        page.wait_for_function("document.getElementById('experienceDrawerMeta').textContent.includes('已拒绝')")
        page.locator('[data-tab="chat"]').click()
        assert page.locator('#chatReviewMount #experienceDrawer').count()==1
        page.set_viewport_size({'width':390,'height':844})
        mobile_drawer=page.locator('#experienceDrawer').bounding_box()
        assert abs(mobile_drawer['y'])<1 and abs(mobile_drawer['height']-844)<1,mobile_drawer
        page.wait_for_selector('#toast:not(.show)')
        page.screenshot(path=str(OUT/'chat-review-mobile.png'),full_page=True)
        assert page.evaluate('document.documentElement.scrollWidth<=innerWidth')
        page.locator('#experienceClose').click()
        assert not page.locator('#experienceDrawer').is_visible()
        page.screenshot(path=str(OUT/'chat-mobile.png'),full_page=True)
        assert page.evaluate('document.documentElement.scrollWidth<=innerWidth')
        assert not errors,errors
        (OUT/'result.json').write_text(json.dumps({'checks':['lazy_new_session','real_history_restore','process_toggle','desktop_side_review','shared_reviewer','dirty_navigation_guard','save_failure_retains_edits','real_save_publish_reject','mobile_no_overflow'],'model_calls':0},ensure_ascii=False,indent=2),encoding='utf-8')
        context.close();browser.close();print('Workspace real storage UI checks passed; no model calls.')

if __name__=='__main__':main()
