"""Real browser + configured model + SQL acceptance, using clearly labelled test cases.

No production incident is asserted. Run against a local Pilot with DeepSeek Flash.
Artifacts stay under ignored artifacts/issue-experience-smoke.
"""
import json
import pathlib
import sys
import time
from playwright.sync_api import sync_playwright

ROOT=pathlib.Path(__file__).resolve().parents[1]
OUT=ROOT/'artifacts'/'issue-experience-smoke'
BASE='http://127.0.0.1:8080'

def main():
    OUT.mkdir(parents=True,exist_ok=True)
    with sync_playwright() as p:
        browser=p.chromium.launch(headless=True,channel='msedge')
        resume='--resume' in sys.argv and (OUT/'progress.json').exists() and (OUT/'browser-state.json').exists()
        context=browser.new_context(viewport={'width':1440,'height':1000},**({'storage_state':str(OUT/'browser-state.json')} if resume else {}))
        page=context.new_page();errors=[]
        page.on('pageerror',lambda e:errors.append(str(e)))
        page.goto(BASE,wait_until='networkidle')
        page.wait_for_function("document.getElementById('userId').value.startsWith('account_')")
        progress=json.loads((OUT/'progress.json').read_text(encoding='utf8')) if resume else {'session':page.locator('#sessionId').input_value(),'initial_runs':[]}
        session=progress['session'];page.locator('#sessionId').fill(session);page.locator('#sessionId').dispatch_event('change')
        context.storage_state(path=str(OUT/'browser-state.json'))
        (OUT/'progress.json').write_text(json.dumps(progress),encoding='utf8')
        def get(path):
            r=context.request.get(BASE+path);assert r.ok,(r.status,r.text());return r.json()['data']
        def post(path,body):
            r=context.request.post(BASE+path,data=body);assert r.ok,(r.status,r.text());return r.json()['data']
        def send(query,reuse=False):
            previous=[]
            if reuse:previous=[r for r in get('/api/v1/chat/runs?session_id='+page.locator('#sessionId').input_value()) if r['query']==query and r['status']=='completed']
            if previous:
                run=previous[0]['id'];job=get('/api/v1/experiences/jobs/'+run)
                if job['status']=='failed':post('/api/v1/experiences/jobs/'+run,{})
            else:
                page.locator('#queryInput').fill(query);page.locator('#chatForm').evaluate('form=>form.requestSubmit()')
                page.wait_for_function("!document.getElementById('sendBtn').disabled",timeout=180000)
                page.wait_for_selector('[data-run-id]',timeout=10000)
                run=page.locator('[data-run-id]').last.get_attribute('data-run-id')
            for _ in range(110):
                r=context.request.get(BASE+'/api/v1/experiences/jobs/'+run)
                if r.ok:
                    job=r.json()['data']
                    if job['status'] in ('succeeded','no_draft','failed'):
                        assert job['status']!='failed',job
                        print(json.dumps({'run':run,'job':job['status'],'draft_count':job['draft_count']},ensure_ascii=False),flush=True)
                        return run,job
                time.sleep(2)
            raise AssertionError('background issue job timed out')
        queries=[
            '现在做知识库验收，我会提供两个测试排障案例，不代表当前平台发生真实生产事故。第一个是 demo-mysql 的 MySQL 连接池超时：我观测到活跃连接100/100，错误为 connection pool timeout，怀疑未归还连接；现在仍在排查，没有修复和验证。请简要分析，区分事实和假设，不要自行把问题标记解决。',
            '先放下 MySQL，另一个独立测试案例是 demo-redis 的 Redis READONLY 错误：应用误连接只读副本；我已将应用连接改到主节点，写入和读取验证通过，Redis故障已解决。请简要记录原因、已实施方案和适用条件。MySQL问题仍未解决。',
            '回到之前 demo-mysql 的同一个问题：我确认连接未归还来自忘记 defer rows.Close()，补上关闭逻辑后连接使用回落到10/100，压测不再出现 connection pool timeout，MySQL故障已解决，验证通过。不是新的故障。请总结这一个问题，不要混入Redis根因。'
        ]
        for index,query in enumerate(queries):
            if len(progress['initial_runs'])>index:continue
            run,_=send(query,reuse=resume);progress['initial_runs'].append(run);(OUT/'progress.json').write_text(json.dumps(progress),encoding='utf8')
        a,b,c=progress['initial_runs']
        issues=get('/api/v1/experiences/issues?session_id='+session)
        assert len(issues)==2,issues
        mysql=next(i for i in issues if 'mysql' in (i['title']+' '+i['service']).lower())
        redis=next(i for i in issues if 'redis' in (i['title']+' '+i['service']).lower())
        assert mysql['status']=='resolved' and redis['status']=='resolved',issues
        assert mysql['revision']>=2 and redis['revision']>=1
        for issue in issues:
            assert issue['knowledge'],issue
        page.locator('#sessionIssues').evaluate('e=>e.open=true')
        page.locator('#issuesRefresh').click()
        page.wait_for_function("document.querySelectorAll('.issue-row').length===2")
        page.screenshot(path=str(OUT/'issues-desktop.png'),full_page=True)
        drafts=get('/api/v1/experiences?tab=all')['items']
        assert len(drafts)==2,drafts
        for d in drafts:
            page.evaluate('id=>window.PilotKnowledge.open(id)',d['entry']['id'])
            page.wait_for_function("!document.getElementById('experienceEditor').hidden")
            page.locator('.experience-evidence').evaluate('e=>e.open=true')
            assert page.locator('#experienceEvidence').inner_text().find('来源问题')>=0
            if d['version']['review_status']=='pending' and d['version']['number']==1:
                page.get_by_role('button',name='审核通过并发布',exact=True).click()
                page.wait_for_function("document.getElementById('experienceDrawerMeta').textContent.includes('已发布')")
            page.locator('#experienceClose').click()
        # Failed edit preserves user text; server optimistic revision also tested.
        mysql_kb=next(d for d in drafts if 'mysql' in (d['version']['title']+' '+d['version']['service']).lower())
        original=get('/api/v1/experiences/'+mysql_kb['entry']['id'])
        revised=post('/api/v1/experiences/'+original['entry']['id'],{'action':'revise','revision':original['entry']['revision']}) if original['version']['review_status']!='pending' else original
        page.evaluate('id=>window.PilotKnowledge.open(id)',original['entry']['id'])
        page.wait_for_function("!document.getElementById('experienceEditor').hidden")
        field=page.locator('#experienceEditor textarea[name="conditions"]')
        if '人工补充' not in field.input_value():field.fill(field.input_value()+'\n人工补充：仅用于当前连接池配置。')
        path='**/api/v1/experiences/'+original['entry']['id']
        def fail_save(route):
            if route.request.method=='POST':route.fulfill(status=503,content_type='application/json',body=json.dumps({'success':False,'message':'验收注入保存失败'}))
            else:route.continue_()
        page.route(path,fail_save)
        page.get_by_role('button',name='保存草稿',exact=True).click()
        page.wait_for_selector('#experienceDrawerError:not([hidden])')
        assert '人工补充' in field.input_value()
        page.unroute(path,fail_save)
        page.get_by_role('button',name='保存草稿',exact=True).click()
        page.wait_for_function("document.getElementById('experienceDirty').hidden")
        page.locator('#experienceClose').click()
        # A new session retrieves approved v1 while v2 is pending.
        if progress.get('test_session'):
            page.locator('#sessionId').fill(progress['test_session']);page.locator('#sessionId').dispatch_event('change')
        else:
            page.locator('#newSessionBtn').click();progress['test_session']=page.locator('#sessionId').input_value();(OUT/'progress.json').write_text(json.dumps(progress),encoding='utf8')
        d,_=send('请调用 search_experience 检索 demo-mysql MySQL connection pool timeout 的历史经验，说明原因和已实施处理，注明知识ID和版本。这里只检索历史，不报告新故障、不执行修复。',reuse=True)
        runs=get('/api/v1/chat/runs?session_id='+page.locator('#sessionId').input_value())
        retrieval=next(r for r in runs if r['id']==d)
        references=[e for e in retrieval['events'] if e['kind']=='knowledge_reference']
        assert any(e.get('result',{}).get('entry_id')==original['entry']['id'] and e['result']['version']==1 for e in references),references
        assert original['entry']['id'] in retrieval['answer'],retrieval['answer']
        linked_run,_=send('提供一个新的测试故障实例，和旧实例不是同一次事故：demo-mysql 的同版本环境又出现 connection pool timeout，根因同样是忘记 defer rows.Close()，按已有经验补上后连接回落10/100，MySQL故障已解决，验证通过。没有新的根因或方案，希望将这个新案例关联已有知识，不重复创建。',reuse=True)
        linked=get('/api/v1/experiences/issues?session_id='+page.locator('#sessionId').input_value())
        assert any(any(k['entry_id']==original['entry']['id'] for k in i['knowledge']) for i in linked),linked
        assert get('/api/v1/experiences?tab=all')['total']==2,'similar case blindly duplicated knowledge'
        enrich_run,_=send('补充刚才新 MySQL 案例的适用条件，这仍是同一新案例不是第三个：我们使用 MySQL 8.0，database/sql 的 QueryContext，Rows.Close 必须在所有提前返回错误的分支执行；加上这个遗漏分支的检查后测试通过，MySQL故障已解决。请将这项新增环境与边界条件整理为已有经验的待审补充，保留此前人工内容，不发布。',reuse=True)
        enriched=get('/api/v1/experiences/'+original['entry']['id'])
        assert enriched['entry']['latest_version']==2 and enriched['entry']['published_version']==1,enriched
        assert enriched['version']['review_status']=='pending'
        assert '人工补充' in enriched['version']['conditions'],'automatic proposal overwrote human edit'
        assert get('/api/v1/experiences?tab=all')['total']==2
        # Manual organization is a page action, not another Main Agent turn.
        manual_session=page.locator('#sessionId').input_value()
        before_runs={r['id'] for r in get('/api/v1/chat/runs?session_id='+manual_session)}
        page.locator('#sessionIssues').evaluate('e=>e.open=true')
        with page.expect_response(lambda r:r.request.method=='POST' and r.url.endswith('/sessions/'+manual_session+'/organize')) as queued:
            page.locator('#issuesOrganize').click()
        assert queued.value.ok,queued.value.text()
        for _ in range(110):
            organized=get('/api/v1/experiences/jobs/'+enrich_run)
            if organized['status'] in ('succeeded','no_draft','failed'):
                assert organized['status']!='failed',organized
                break
            time.sleep(2)
        else:raise AssertionError('manual issue organization timed out')
        assert organized['manual'] and organized['tracked']
        assert {r['id'] for r in get('/api/v1/chat/runs?session_id='+manual_session)}==before_runs,'page action started a Main Agent turn'
        assert get('/api/v1/experiences?tab=all')['total']==2,'manual organization duplicated knowledge'
        # Refresh restores persisted Issue memory and review state.
        page.locator('#sessionId').fill(session);page.locator('#sessionId').dispatch_event('change');page.reload(wait_until='networkidle')
        page.locator('#sessionId').fill(session);page.locator('#sessionId').dispatch_event('change');page.locator('#sessionIssues').evaluate('e=>e.open=true')
        page.wait_for_function("document.querySelectorAll('.issue-row').length===2")
        page.set_viewport_size({'width':390,'height':844})
        page.evaluate('id=>window.PilotKnowledge.open(id)',original['entry']['id']);page.wait_for_function("!document.getElementById('experienceEditor').hidden")
        page.screenshot(path=str(OUT/'issues-mobile-drawer.png'),full_page=True)
        assert page.evaluate('document.documentElement.scrollWidth<=innerWidth')
        other=browser.new_context();other.request.post(BASE+'/api/v1/account/session',data={})
        assert other.request.get(BASE+'/api/v1/experiences/'+original['entry']['id']).status==404
        assert other.request.get(BASE+'/api/v1/experiences/issues?session_id='+session).json()['data']==[]
        other.close();assert not errors,errors
        (OUT/'result.json').write_text(json.dumps({'runs':[a,b,c,d,linked_run,enrich_run],'session':session,'issues':[i['id'] for i in issues],'entries':[x['entry']['id'] for x in drafts],'checks':['real_model_sse','interleaved_issues','incremental_summary','auto_drafts','drawer_review_publish','failed_save_preserved','published_version_retrieval','similar_case_link','enrich_pending_version','human_edit_preserved','manual_organize_button','reload','mobile','owner_isolation']},ensure_ascii=False,indent=2),encoding='utf8')
        context.storage_state(path=str(OUT/'browser-state.json'))
        browser.close();print('Issue experience browser checks passed',flush=True)

if __name__=='__main__':main()
