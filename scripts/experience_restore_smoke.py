"""Actual archive/restore UI + storage regression on an isolated test account.

Uses existing published test knowledge; no model requests.
"""
import json
from pathlib import Path
from playwright.sync_api import sync_playwright

ROOT=Path(__file__).resolve().parents[1]
BASE='http://127.0.0.1:8080'
OUT=ROOT/'artifacts'/'workspace-ui'

def main():
    with sync_playwright() as p:
        browser=p.chromium.launch(headless=True,channel='msedge')
        fixture=ROOT/'artifacts'/'experience-smoke'
        ctx=browser.new_context(storage_state=str(fixture/'browser-state.json'),viewport={'width':1440,'height':1000})
        entry=json.loads((fixture/'result.json').read_text(encoding='utf-8'))['entry_id']
        def get(path):
            r=ctx.request.get(BASE+path);assert r.ok,(r.status,r.text());return r.json()['data']
        def post(body):
            r=ctx.request.post(BASE+'/api/v1/experiences/'+entry,data=body);assert r.ok,(r.status,r.text());return r.json()['data']
        d=get('/api/v1/experiences/'+entry)
        if d['entry']['lifecycle']=='archived':d=post({'action':'restore','revision':d['entry']['revision']})
        published_number=d['entry']['published_version'];assert published_number>0
        published=get(f'/api/v1/experiences/{entry}?version={published_number}')['version']
        if d['version']['review_status']!='pending':d=post({'action':'revise','revision':d['entry']['revision']})
        pending=d['version'];latest=d['entry']['latest_version']
        page=ctx.new_page();errors=[];page.on('pageerror',lambda e:errors.append(str(e)))
        page.goto(BASE,wait_until='networkidle');page.wait_for_function("document.getElementById('userId').value.startsWith('account_')")
        page.locator('[data-tab="knowledge"]').click()
        page.evaluate('id=>window.PilotKnowledge.open(id)',entry)
        page.wait_for_selector('#experienceReading h3')
        page.locator('#experienceVersion').select_option(str(published_number))
        page.wait_for_selector('#experienceActions button:text-is("归档")')
        page.locator('#experienceNote').fill('恢复发布验收：暂时停用，检查已发布内容与待审修订隔离。')
        page.get_by_role('button',name='归档',exact=True).click()
        page.wait_for_selector('#experienceActions button:text-is("恢复发布")')
        archived=get('/api/v1/experiences/'+entry)
        assert archived['entry']['lifecycle']=='archived'
        page.get_by_role('button',name='恢复发布',exact=True).click()
        page.wait_for_function("document.getElementById('experienceDrawerMeta').textContent.includes('已发布')")
        restored=get('/api/v1/experiences/'+entry)
        assert restored['entry']['lifecycle']=='published'
        assert restored['entry']['published_version']==published_number
        assert restored['entry']['latest_version']==latest
        assert restored['version']==pending,'restore altered the pending revision'
        assert get(f'/api/v1/experiences/{entry}?version={published_number}')['version']==published
        assert page.locator('#experienceEdit').is_visible(),'pending revision must become editable again'
        assert not page.get_by_role('button',name='恢复发布',exact=True).count()
        duplicate=ctx.request.post(BASE+'/api/v1/experiences/'+entry,data={'action':'restore','revision':archived['entry']['revision']})
        assert duplicate.status==409
        page.reload(wait_until='networkidle');page.wait_for_function("document.getElementById('userId').value.startsWith('account_')")
        page.locator('[data-tab="knowledge"]').click();page.evaluate('id=>window.PilotKnowledge.open(id)',entry)
        page.wait_for_function("document.getElementById('experienceDrawerMeta').textContent.includes('已发布')")
        assert not errors,errors
        OUT.mkdir(parents=True,exist_ok=True)
        page.screenshot(path=str(OUT/'restored-experience.png'),full_page=True)
        (OUT/'restore-result.json').write_text(json.dumps({'checks':['archive_ui','restore_ui','approved_version_unchanged','pending_revision_unchanged','duplicate_conflict','reload_persisted'],'model_calls':0},indent=2),encoding='utf-8')
        ctx.close();browser.close();print('Archive/restore UI and actual storage passed; no model calls.')

if __name__=='__main__':main()
