"""Real page + real storage UI regression, reusing isolated test accounts.

Does not call the model provider. Run issue_experience_smoke first for Issue data;
the prior experience-smoke fixture supplies an existing model-generated entry.
This does not replace the full model acceptance in issue_experience_smoke.
"""
import json
import pathlib
from playwright.sync_api import sync_playwright

ROOT=pathlib.Path(__file__).resolve().parents[1]
OUT=ROOT/'artifacts'/'issue-experience-smoke'
OLD=ROOT/'artifacts'/'experience-smoke'
BASE='http://127.0.0.1:8080'

def main():
    with sync_playwright() as p:
        browser=p.chromium.launch(headless=True,channel='msedge')
        context=browser.new_context(storage_state=str(OUT/'browser-state.json'),viewport={'width':1440,'height':1000})
        page=context.new_page();errors=[]
        page.on('pageerror',lambda e:errors.append(str(e)))
        page.goto(BASE,wait_until='networkidle')
        page.wait_for_function("document.getElementById('userId').value.startsWith('account_')")
        session=json.loads((OUT/'progress.json').read_text())['session']
        page.locator('#sessionId').fill(session);page.locator('#sessionId').dispatch_event('change')
        page.locator('#sessionIssues').evaluate('e=>e.open=true')
        page.wait_for_selector('.issue-row')
        initial_count=page.locator('.issue-row').count()
        row=page.locator('.issue-row').first;issue=row.get_attribute('data-issue-id')
        row.evaluate('e=>e.open=true');title=row.get_by_label('问题名称',exact=True);original=title.input_value()
        title.fill(original+'（界面验收）');row.get_by_role('button',name='改名',exact=True).click()
        page.wait_for_function("document.getElementById('issueNotice').textContent.includes('已保存')")
        page.wait_for_function("document.querySelector('.issue-row summary').textContent.includes('界面验收')")
        row=page.locator(f'.issue-row[data-issue-id="{issue}"]');row.evaluate('e=>e.open=true')
        row.locator('.issue-event-list').evaluate('e=>e.open=true')
        refs=row.locator('.issue-event-list input[type="checkbox"]');assert refs.count()>=2
        refs.nth(0).check();refs.nth(1).check();row.get_by_label('新问题名称').fill('界面验收拆分问题')
        row.get_by_role('button',name='拆出选中事件',exact=True).click()
        page.wait_for_function("n=>document.querySelectorAll('.issue-row').length===n",arg=initial_count+1)
        split=page.locator('.issue-row').filter(has=page.locator('summary',has_text='界面验收拆分问题')).first
        split.evaluate('e=>e.open=true');split.get_by_label('目标问题',exact=True).select_option(issue)
        split.get_by_role('button',name='合并到目标',exact=True).click()
        page.wait_for_function("n=>document.querySelectorAll('.issue-row').length===n",arg=initial_count)
        page.reload(wait_until='networkidle');page.locator('#sessionId').fill(session);page.locator('#sessionId').dispatch_event('change')
        page.locator('#sessionIssues').evaluate('e=>e.open=true');page.wait_for_selector('.issue-row')
        assert page.locator('.issue-row').get_attribute('data-issue-id')==issue
        page.locator('.issue-row').evaluate('e=>e.open=true')
        page.screenshot(path=str(OUT/'issues-current-desktop.png'),full_page=True)
        page.set_viewport_size({'width':390,'height':844})
        page.screenshot(path=str(OUT/'issues-current-mobile.png'),full_page=True)
        assert page.evaluate('document.documentElement.scrollWidth<=innerWidth')
        other=browser.new_context();other.request.post(BASE+'/api/v1/account/session',data={})
        assert other.request.get(BASE+'/api/v1/experiences/issues?session_id='+session).json()['data']==[]
        other.close();context.close()
        context=browser.new_context(storage_state=str(OLD/'browser-state.json'),viewport={'width':1440,'height':1000})
        page=context.new_page();page.on('pageerror',lambda e:errors.append(str(e)))
        page.goto(BASE,wait_until='networkidle');page.wait_for_function("document.getElementById('userId').value.startsWith('account_')")
        entry=json.loads((OLD/'result.json').read_text(encoding='utf8'))['entry_id']
        page.evaluate('id=>window.PilotKnowledge.open(id)',entry)
        page.wait_for_function("!document.getElementById('experienceEditor').hidden")
        if page.get_by_role('button',name='创建修订版本',exact=True).count():
            page.get_by_role('button',name='创建修订版本',exact=True).click()
        page.wait_for_selector('#experienceActions button:text-is("保存草稿")')
        field=page.locator('#experienceEditor textarea[name="conditions"]');field.fill(field.input_value()+'\n本次界面回归：保留未保存内容。')
        path=BASE+'/api/v1/experiences/'+entry
        def fail(route):
            if route.request.method=='POST':route.fulfill(status=503,content_type='application/json',body=json.dumps({'success':False,'message':'界面验收临时失败'}))
            else:route.continue_()
        page.route(path,fail);page.get_by_role('button',name='保存草稿',exact=True).click()
        page.wait_for_selector('#experienceDrawerError:not([hidden])')
        assert '保留未保存内容' in field.input_value()
        page.unroute(path,fail);page.get_by_role('button',name='保存草稿',exact=True).click()
        page.wait_for_function("document.getElementById('experienceDirty').hidden && document.getElementById('experienceDrawerError').hidden")
        page.locator('.experience-evidence').evaluate('e=>e.open=true')
        page.screenshot(path=str(OUT/'review-current-desktop.png'),full_page=True)
        page.set_viewport_size({'width':390,'height':844});page.screenshot(path=str(OUT/'review-current-mobile.png'),full_page=True)
        assert page.evaluate('document.documentElement.scrollWidth<=innerWidth')
        page.get_by_role('button',name='审核通过并发布',exact=True).click()
        page.wait_for_function("document.getElementById('experienceDrawerMeta').textContent.includes('已发布')")
        page.locator('#experienceVersion').select_option('1')
        page.wait_for_function("document.getElementById('experienceDrawerMeta').textContent.includes('v1')")
        assert page.locator('#experienceEditor textarea[name="conditions"]').is_disabled()
        assert '本次界面回归' not in page.locator('#experienceEditor textarea[name="conditions"]').input_value()
        assert not errors,errors
        (OUT/'ui-result.json').write_text(json.dumps({'checks':['real_issue_rename','real_issue_split_merge','reload_persisted','owner_isolation','save_failure_keeps_text','save_publish','immutable_old_version','desktop_mobile_no_overflow'],'model_calls':0},indent=2),encoding='utf8')
        browser.close();print('Real storage UI checks passed (no model calls)',flush=True)

if __name__=='__main__':main()
