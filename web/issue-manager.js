/* Incremental Issue memory UI; raw evidence is referenced by durable event IDs. */
(() => {
  const api='/api/v1/experiences', el=id=>document.getElementById(id);
  let items=[], unassigned=[], serial=0, busy=false, editing=false, shownSession='';
  const session=()=>el('sessionId').value.trim();
  const labels={investigating:'排查中',resolved:'已解决',unresolved:'尚未解决'};
  async function request(path,body) {
    const r=await fetch(path,{cache:'no-store',...(body?{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)}:{})});
    const data=await r.json(); if(!r.ok || !data.success)throw new Error(data.message || `请求失败 ${r.status}`);return data.data;
  }
  const button=(text,fn)=>{const b=document.createElement('button');b.type='button';b.className='btn btn-subtle';b.textContent=text;b.addEventListener('click',fn);return b;};
  const notice=text=>{el('issueNotice').textContent=text;};
  async function mutate(issue,input) {
    if(busy)return;busy=true;notice('正在保存问题调整…');
    const selected=session();
    const nodes=Array.from(el('sessionIssues').querySelectorAll('button,input,select'));const previous=nodes.map(n=>n.disabled);nodes.forEach(n=>n.disabled=true);
    let saved=false;
    try{await request(`${api}/issues/${encodeURIComponent(issue.id)}`,{revision:issue.revision,...input});saved=true;if(session()===selected)notice('已保存。已发布知识保留；可以重新整理，生成待审核修订。');}
    catch(e){if(session()===selected)notice(`${e.message}。未提交的修改保留；如内容已更新，请重新加载。`);}
    finally{nodes.forEach((n,i)=>n.disabled=previous[i]);busy=false;}
    if(saved && session()===selected){editing=false;await refresh(true);}
    else if(session()!==selected)await refresh(true);
    return saved && session()===selected;
  }
  function render() {
    const list=el('issueList');const expanded=new Set(Array.from(list.querySelectorAll('.issue-row[open]')).map(n=>n.dataset.issueId));list.replaceChildren();
    if(unassigned.length){const pending=document.createElement('details');pending.className='issue-event-list';const title=document.createElement('summary');title.textContent=`待归属事件（展示最多100项，普通问答也可能留在这里）· ${unassigned.length}`;pending.append(title);unassigned.forEach(e=>{const label=document.createElement('label'),input=document.createElement('input');input.type='checkbox';input.value=e.event_id;label.append(input,document.createTextNode(e.event_id));pending.append(label);});const target=document.createElement('select');target.setAttribute('aria-label','待归属事件的目标问题');items.forEach(i=>{const o=document.createElement('option');o.value=i.id;o.textContent=i.title;target.append(o);});pending.append(target,button('归入选定问题',()=>{const issue=items.find(i=>i.id===target.value);const ids=Array.from(pending.querySelectorAll('input:checked')).map(i=>i.value);if(!issue || !ids.length){notice('请选择事件和问题');return;}mutate(issue,{action:'assign',event_ids:ids});}));pending.addEventListener('change',()=>editing=true);list.append(pending);}
    if(!items.length){list.append(document.createTextNode('尚未识别到排障问题。每轮结束后会在后台整理。'));return;}
    items.forEach(issue=>{
      const row=document.createElement('details');row.className='issue-row';row.dataset.issueId=issue.id;row.open=expanded.has(issue.id);
      const heading=document.createElement('summary');heading.textContent=`${issue.title} · ${labels[issue.status]} · ${issue.evidence_count} 条证据 · ${issue.knowledge.length?'已关联经验':'未生成知识'}`;row.append(heading);
      const text=document.createElement('p');text.textContent=issue.summary;row.append(text);
      issue.knowledge.forEach(k=>row.append(button('查看关联经验',()=>window.PilotKnowledge.open(k.entry_id))));
      const controls=document.createElement('div');controls.className='issue-controls';
      const title=document.createElement('input');title.value=issue.title;title.maxLength=200;title.setAttribute('aria-label','问题名称');
      controls.append(title,button('改名',()=>mutate(issue,{action:'rename',title:title.value})));
      const state=document.createElement('select');state.setAttribute('aria-label','处理状态');Object.entries(labels).forEach(([value,label])=>{const o=document.createElement('option');o.value=value;o.textContent=label;o.selected=issue.status===value;state.append(o);});
      const basis=document.createElement('input');basis.placeholder='已解决时填写验证或确认依据';basis.setAttribute('aria-label','解决依据');basis.value=issue.resolution_basis || '';basis.maxLength=2400;
      controls.append(state,basis,button('更新状态',async()=>{const saved=await mutate(issue,{action:'status',status:state.value,basis:basis.value});if(saved && state.value==='resolved')await organize();}));
      const target=document.createElement('select');target.setAttribute('aria-label','目标问题');const first=document.createElement('option');first.textContent='选择目标问题';first.value='';target.append(first);
      items.filter(i=>i.id!==issue.id).forEach(i=>{const o=document.createElement('option');o.value=i.id;o.textContent=i.title;target.append(o);});
      const targetData=()=>({target_id:target.value,target_revision:items.find(i=>i.id===target.value)?.revision || 0});
      controls.append(target,button('合并到目标',()=>{if(!target.value){notice('请选择目标问题');return;}mutate(issue,{action:'merge',...targetData()});}));
      const refs=document.createElement('details');refs.className='issue-event-list';const summary=document.createElement('summary');summary.textContent=`调整事件归属（${issue.events.length} 项）`;refs.append(summary);
      issue.events.forEach(e=>{const label=document.createElement('label'),checkbox=document.createElement('input');checkbox.type='checkbox';checkbox.value=e.event_id;label.append(checkbox,document.createTextNode(`${e.kind==='evidence'?'工具证据':'消息'} · ${e.event_id}`));refs.append(label);});
      const selected=()=>Array.from(refs.querySelectorAll('input:checked')).map(n=>n.value);
      const newTitle=document.createElement('input');newTitle.placeholder='拆分后的新问题名称';newTitle.setAttribute('aria-label','新问题名称');newTitle.maxLength=200;
      controls.append(refs,newTitle,button('拆出选中事件',()=>{if(!selected().length || !newTitle.value.trim()){notice('选择事件并填写新问题名称');return;}mutate(issue,{action:'split',title:newTitle.value,event_ids:selected()});}),button('移到目标问题',()=>{if(!selected().length || !target.value){notice('选择事件和目标问题');return;}mutate(issue,{action:'move',event_ids:selected(),...targetData()});}));
      controls.addEventListener('input',()=>editing=true);controls.addEventListener('change',()=>editing=true);
      const adjustments=document.createElement('details');adjustments.className='issue-adjustments';
      const actionTitle=document.createElement('summary');actionTitle.textContent='调整问题与事件归属';adjustments.append(actionTitle,controls);
      row.append(adjustments);list.append(row);
    });
  }
  async function refresh(force=false) {
    const selected=session();if(!selected || busy)return;
    if(shownSession!==selected){editing=false;shownSession=selected;force=true;}
    if(editing && !force)return;
    const token=++serial;
    try{const [data,pending]=await Promise.all([request(`${api}/issues?session_id=${encodeURIComponent(selected)}`),request(`${api}/unassigned?session_id=${encodeURIComponent(selected)}`)]);if(token!==serial || session()!==selected)return;items=data;unassigned=pending;el('issueCount').textContent=`· ${items.length}`;render();}
    catch(e){if(token===serial && session()===selected)notice(e.message);}
  }
  async function closeSession(id,quiet=false) {
    if(!id)return;
    try{await request(`${api}/sessions/${encodeURIComponent(id)}/close`,{});if(!quiet && session()===id)notice('已检查历史排障轮次，待整理问题已排队；可以继续对话。');}
    catch(e){if(!quiet && session()===id)notice(e.message);}
    await refresh();
  }
  async function organize() {
    const selected=session();if(!selected){notice('发送首条消息后才能整理经验');return;}
    const b=el('issuesOrganize');if(b.disabled)return;b.disabled=true;notice('正在安排会话经验整理…');
    try{await request(`${api}/sessions/${encodeURIComponent(selected)}/organize`,{});if(session()===selected){notice('已排队。只整理尚有新增内容的问题，草稿仍需审核发布。');window.PilotKnowledge?.resumeCards(true);}}
    catch(e){if(session()===selected)notice(e.message);}
    finally{b.disabled=false;}
    await refresh();
  }
  function init() {
    el('issuesRefresh').addEventListener('click',()=>{editing=false;refresh(true);});el('issuesOrganize').addEventListener('click',organize);el('issuesClose').addEventListener('click',()=>closeSession(session()));
    el('sessionId').addEventListener('change',()=>refresh(true));el('sessionIssues').addEventListener('toggle',()=>{if(el('sessionIssues').open)refresh();});
    setInterval(()=>{if(!document.hidden && el('sessionIssues').open)refresh();},5000);refresh();
  }
  function reset(){serial++;items=[];unassigned=[];editing=false;shownSession='';}
  window.PilotIssues={init,refresh,organize,closeSession,reset};
})();
