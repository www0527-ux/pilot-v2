/* Private experience jobs, review drawer, and published-version references. */
(() => {
  const labels = {pending:'待审核',approved:'已通过',rejected:'已拒绝',draft:'草稿',published:'已发布',archived:'已归档',unverified:'未验证',partially_verified:'部分验证',verified:'已验证'};
  const names = ['title','service','phenomenon','root_cause','solution','validation','conditions','verification','cause_status'];
  const api = '/api/v1/experiences';
  const byId = id => document.getElementById(id);
  const button = (text, fn, primary = false) => { const b = document.createElement('button'); b.type='button'; b.className=`btn ${primary?'btn-main':'btn-subtle'}`; b.textContent=text; b.addEventListener('click',fn); return b; };
  const stamp = time => time ? new Date(time).toLocaleString() : '—';
  let automatic = false, tab = 'pending', page = 1, total = 0, current = null, baseline = '', busy = false, generation = 0, previousFocus = null, listGeneration = 0, versionCache = [], editing = false, layout = 'chat';
  const jobs = new Map();
  async function request(path, body) {
    const response = await fetch(path, {cache:'no-store', ...(body ? {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)} : {})});
    let data; try { data=await response.json(); } catch { throw new Error('服务没有返回有效数据'); }
    if (!response.ok || !data.success) { const error=new Error(data.message || `请求失败：HTTP ${response.status}`); error.status=response.status; throw error; }
    return data.data;
  }
  function content() {
    const form=byId('experienceEditor'), result={};
    names.forEach(name => { result[name]=form.elements.namedItem(name).value; });
    result.tags=form.elements.namedItem('tags').value.split(/[,，]/).map(s=>s.trim()).filter(Boolean);
    return result;
  }
  function editorState() { return JSON.stringify({content:content(),note:byId('experienceNote').value}); }
  function dirty() { return current && current.version.review_status==='pending' && editorState() !== baseline; }
  function markDirty() { byId('experienceDirty').hidden=!dirty(); }
  function error(message) { const node=byId('experienceDrawerError'); node.textContent=message || ''; node.hidden=!message; }
  function closeNow() { generation++; byId('experienceDrawer').hidden=true; document.body.classList.remove('review-open'); current=null; editing=false; previousFocus?.isConnected && previousFocus.focus({preventScroll:true}); }
  function close() { if (busy) return; if (dirty()) byId('experienceLeave').hidden=false; else closeNow(); }
  function canNavigate(closePanel = true) {
    if (busy) { showToast('审核正在提交，请稍候'); return false; }
    if (dirty()) { byId('experienceLeave').hidden=false; showToast('请先保存或放弃审核修改，再切换','info'); return false; }
    if (closePanel) closeNow(); return true;
  }
  function setLayout(target) {
    layout=target;
    const drawer=byId('experienceDrawer');
    (target==='knowledge'?byId('knowledgeDetailMount'):byId('chatReviewMount')).append(drawer);
    drawer.classList.toggle('is-inline',target==='knowledge');
    if(target==='observability')closeNow();
    document.body.classList.toggle('review-open',target==='chat' && !drawer.hidden);
    modal();
  }
  function modal(){byId('experienceDrawer').setAttribute('aria-modal',String(layout==='chat' && matchMedia('(max-width:1199px)').matches));}
  function setEditing(value) {
    editing=value;
    byId('experienceEditor').hidden=!value; byId('experienceReading').hidden=value;
    byId('experienceEdit').textContent=value?'返回阅读':'编辑内容';
  }
  function reading(d) {
    const node=byId('experienceReading');node.replaceChildren();
    const title=document.createElement('h3');title.textContent=d.version.title;node.append(title);
    const meta=document.createElement('p');meta.className='experience-meta';
    meta.textContent=`${d.version.service || '未指定服务'} · ${(d.tags || []).join('、') || '无标签'} · 根因${({unknown:'未知',hypothesis:'待验证',confirmed:'已确认'})[d.version.cause_status] || '未知'}`;node.append(meta);
    [['phenomenon','问题现象'],['root_cause','排查与根因分析'],['solution','解决方案'],['validation','验证结果与依据'],['conditions','适用条件']].forEach(([field,label])=>{
      const section=document.createElement('section'),heading=document.createElement('h4'),body=document.createElement('div');
      heading.textContent=label;body.className='msg-content';body.innerHTML=renderMarkdown(d.version[field] || '尚未记录');section.append(heading,body);node.append(section);
    });
  }
  function render(d, versions = []) {
    if (versions.length) versionCache=versions;
    else if (!current || current.entry.id!==d.entry.id) versionCache=[];
    versionCache=versionCache.filter(item=>item.version.number!==d.version.number).concat(d).sort((a,b)=>b.version.number-a.version.number);
    current=d; error(''); byId('experienceLeave').hidden=true; byId('experienceDirty').hidden=true;
    const editable=d.version.review_status==='pending' && d.version.number===d.entry.latest_version && d.entry.lifecycle!=='archived';
    byId('experienceEdit').hidden=!editable; reading(d); setEditing(false);
    const form=byId('experienceEditor');
    names.forEach(name=>{ const input=form.elements.namedItem(name); input.value=d.version[name] || ''; input.disabled=!editable; });
    form.elements.namedItem('tags').value=(d.tags || []).join(', '); form.elements.namedItem('tags').disabled=!editable;
    byId('experienceNote').value=d.version.review_note || ''; byId('experienceNote').disabled=d.entry.lifecycle==='archived';
    baseline=editorState();
    byId('experienceDrawerTitle').textContent=editable?'审核排障经验':'排障经验详情';
    byId('experienceDrawerMeta').textContent=`v${d.version.number} · ${labels[d.version.review_status]} · ${labels[d.entry.lifecycle]} · ${labels[d.version.verification]}`;
    byId('experienceSaveStatus').textContent=d.version.reviewed_at?`审核于 ${stamp(d.version.reviewed_at)}`:`更新于 ${stamp(d.version.updated_at)}`;
    if (d.entry.archive_reason) byId('experienceSaveStatus').textContent=`${d.entry.lifecycle==='archived'?'归档原因':'上次归档原因'}：${d.entry.archive_reason}`;
    const select=byId('experienceVersion'); select.replaceChildren();
    versionCache.forEach(item=>{ const option=document.createElement('option'); option.value=item.version.number; option.textContent=`v${item.version.number} · ${labels[item.version.review_status]}`; option.selected=item.version.number===d.version.number; select.append(option); });
    const evidence=byId('experienceEvidence'); evidence.replaceChildren();
    (d.sources || []).forEach(s=>{const p=document.createElement('p');p.textContent=`来源问题 ${s.issue_id} · ${s.action} · ${s.reason || ''}`;evidence.append(p);(s.run_ids?.length?s.run_ids:[s.run_id]).forEach(id=>evidence.append(button(`查看来源 Run ${id}`,()=>source(id,s.session_id))));});
    if (!(d.evidence || []).length) {const p=document.createElement('p');p.textContent='没有可引用的工具证据。该案例中的根因和验证结论需要另行确认。';evidence.append(p);}
    (d.evidence || []).forEach(e=>{ const section=document.createElement('details'), label=document.createElement('summary'), pre=document.createElement('pre'); label.textContent=`${e.tool} · ${stamp(e.observed_at)} · ${e.status}`; pre.textContent=`证据 ID：${e.id}\n${JSON.stringify(e.result,null,2)}`; section.append(label,pre); evidence.append(section); });
    document.querySelectorAll('.experience-row').forEach(row=>row.classList.toggle('selected',row.dataset.entryId===d.entry.id));
    const actions=byId('experienceActions'); actions.replaceChildren();
    if (d.entry.lifecycle==='archived' && d.entry.published_version) {
      const hint=document.createElement('span');hint.className='experience-meta';hint.textContent=`恢复已审核的 v${d.entry.published_version}，不会发布待审核修订。`;
      actions.append(hint,button('恢复发布',()=>submit('restore'),true));
    }
    else if (editable) actions.append(button('拒绝收录',()=>submit('reject')),button('保存草稿',()=>submit('save')),button('审核通过并发布',()=>submit('publish'),true));
    else if (d.entry.lifecycle==='published') {
      if (d.version.number===d.entry.latest_version && d.version.review_status!=='pending') actions.append(button('创建修订版本',()=>submit('revise'),true));
      else if (d.entry.latest_version!==d.version.number) actions.append(button('查看最新修订',()=>open(d.entry.id)));
      actions.append(button('归档',()=>submit('archive')));
    }
    if(layout==='knowledge')window.scrollTo({top:0,behavior:'instant'});
  }
  async function open(id, version = 0) {
    if (busy) return;
    if (dirty()) { byId('experienceLeave').hidden=false; showToast('请先保存或放弃当前修改，再查看其他知识','error'); return; }
    const token=++generation; previousFocus=document.activeElement;
    current=null;editing=false;byId('experienceEditor').hidden=true;byId('experienceReading').replaceChildren();byId('experienceEdit').hidden=true;byId('experienceDirty').hidden=true;byId('experienceLeave').hidden=true;
    byId('experienceDrawer').hidden=false;setLayout(layout);error(''); byId('experienceSaveStatus').textContent='加载中…';
    byId('experienceDrawerTitle').textContent='经验详情';byId('experienceDrawerMeta').textContent='';byId('experienceEvidence').replaceChildren();
    byId('experienceActions').replaceChildren();
    try { const [d, versions]=await Promise.all([request(`${api}/${encodeURIComponent(id)}${version?`?version=${version}`:''}`),request(`${api}/${encodeURIComponent(id)}/versions`)]); if(token!==generation)return; render(d,versions); byId('experienceClose').focus({preventScroll:true}); }
    catch(e) { if(token===generation){error(e.message);byId('experienceSaveStatus').textContent='加载失败';} }
  }
  async function submit(action, closeAfter = false) {
    if (!current || busy) return;
    if (['save','publish'].includes(action) && !byId('experienceEditor').reportValidity()) return;
    if (action==='archive' && !byId('experienceNote').value.trim()) { error('请填写归档原因'); byId('experienceNote').focus(); return; }
    busy=true; error(''); byId('experienceSaveStatus').textContent='提交中…';
    const disabled=Array.from(byId('experienceDrawer').querySelectorAll('button,input,textarea,select')).map(node=>[node,node.disabled]); disabled.forEach(([node])=>node.disabled=true);
    let saved=null;
    try { saved=await request(`${api}/${encodeURIComponent(current.entry.id)}`,{...content(),action,revision:current.entry.revision,note:byId('experienceNote').value}); }
    catch(e) { error(e.message+(e.status===409?'。你的编辑仍保留，请复制需要保留的内容后重新打开最新版本。':'')); byId('experienceSaveStatus').textContent='未保存'; }
    finally { disabled.forEach(([node,old])=>node.disabled=old); busy=false; }
    if (saved) { render(saved); showToast(action==='restore'?`已恢复发布 v${saved.entry.published_version}，待审核修订不会自动发布`:action==='publish'?'已发布；验证状态保持独立':'操作已保存','success'); refresh(); refreshCards(current.entry.source_run_id); if(closeAfter)closeNow(); }
  }
  async function refresh() {
    const token=++listGeneration; const filter=new FormData(byId('experienceFilters'));
    const query=new URLSearchParams({tab,page:String(page)}); for(const [key,value] of filter) if(value)query.set(key,value);
    const list=byId('experienceList'); list.setAttribute('aria-busy','true');
    try {
      const data=await request(`${api}?${query}`); if(token!==listGeneration)return; total=data.total;
      byId('experienceCounts').textContent=`待审核 ${data.counts.pending}　·　已发布 ${data.counts.published}　·　已归档 ${data.counts.archived}`;
      list.replaceChildren(); if(!data.items.length)list.textContent='当前筛选条件下没有经验。完成排障后可手动整理，或开启自动整理。';
      data.items.forEach(d=>{ const row=document.createElement('button'); row.type='button'; row.className='experience-row';row.dataset.entryId=d.entry.id;row.classList.toggle('selected',current?.entry.id===d.entry.id); const title=document.createElement('strong'), meta=document.createElement('span'), source=document.createElement('span'); title.textContent=d.version.title; meta.textContent=`${d.version.service || '未指定服务'} · ${labels[d.version.review_status]} · v${d.version.number}`; source.textContent=`更新于 ${stamp(d.entry.updated_at)}`; source.className='experience-meta'; row.append(title,meta,source); row.addEventListener('click',()=>open(d.entry.id)); list.append(row); });
      byId('experiencePage').textContent=`第 ${page} 页 · ${total} 条`; byId('experiencePrev').disabled=page===1; byId('experienceNext').disabled=page*30>=total;
    } catch(e) { if(token===listGeneration)list.textContent=e.message; }
    finally { if(token===listGeneration)list.setAttribute('aria-busy','false'); }
  }
  function refreshCards(runID) { const watch=jobs.get(runID); if(watch)updateCard(watch); }
  async function updateCard(watch) {
    if(!watch.message.bodyNode.isConnected) return;
    const node=watch.node; if(!node)return;
    try {
      const job=await request(`${api}/jobs/${encodeURIComponent(watch.id)}`);
      if(!watch.message.bodyNode.isConnected || !window.PilotWorkspace.current(watch.session.sessionId))return;
      window.PilotIssues?.refresh();
      if(['pending','running','waiting_snapshot'].includes(job.status)) {
        node.hidden=watch.dismissed || (!job.manual && !(automatic && job.tracked));
        node.replaceChildren(document.createTextNode(job.status==='running'?'正在整理排障经验…':'经验整理已排队，关闭页面后也可继续查看。'));
        if(watch.attempts++<90)watch.timer=setTimeout(()=>updateCard(watch),2000);
        else node.append(button('刷新整理状态',()=>{watch.attempts=0;updateCard(watch);}));
        return;
      }
      node.replaceChildren();
      if(job.status==='failed') { node.hidden=watch.dismissed;node.append(document.createTextNode('经验整理未成功'),button('重试整理',()=>trigger(watch)));const details=document.createElement('details'),summary=document.createElement('summary'),pre=document.createElement('pre');summary.textContent='查看错误详情';pre.textContent=job.error || '请重试';details.append(summary,pre);node.append(details);return; }
      if(job.status==='no_draft') { node.hidden=true;return; }
      const data=await request(`${api}?run_id=${encodeURIComponent(watch.id)}&tab=all`);
      if(!watch.message.bodyNode.isConnected || !window.PilotWorkspace.current(watch.session.sessionId))return;
      if(!data.items.length) { node.hidden=true;return; }
      node.hidden=watch.dismissed;
      node.append(document.createTextNode(`已整理 ${data.items.length} 条经验 · ${data.items.filter(d=>d.version.review_status==='pending').length} 条待审核`));
      data.items.forEach(d=>node.append(button(d.version.review_status==='pending'?`审核经验 · ${d.version.title}`:`查看经验 · ${d.version.title}`,()=>open(d.entry.id))));
      node.append(button('稍后处理',()=>{watch.dismissed=true;node.hidden=true;try{localStorage.setItem(`pilot-experience-dismissed:${watch.id}`,'1');}catch{};}));
      refresh();
    } catch(e) {
      if(!watch.message.bodyNode.isConnected || !window.PilotWorkspace.current(watch.session.sessionId))return;
      if(e.status===404) {
        if(automatic && watch.fresh && watch.attempts++<8) { node.textContent='正在准备经验整理任务…';watch.timer=setTimeout(()=>updateCard(watch),1500);return; }
        node.hidden=true;return;
      }
      node.replaceChildren(document.createTextNode(e.message),button('重新加载',()=>updateCard(watch)));
    }
  }
  async function trigger(watch) {
    if(watch.triggering)return;watch.triggering=true;watch.dismissed=false;watch.node.hidden=false;watch.node.textContent='创建整理任务…';
    try { await request(`${api}/jobs/${encodeURIComponent(watch.id)}`,{});watch.attempts=0;watch.fresh=true;updateCard(watch); }
    catch(e) {watch.node.replaceChildren(document.createTextNode(e.message),button('重试',()=>trigger(watch)));}
    finally {watch.triggering=false;}
  }
  function attach(message,id,session,events=[],fresh=false) {
    if(!id || !message?.bodyNode || message.knowledgeAttached)return;message.knowledgeAttached=true;
	message.messageNode.dataset.runId=id;
    const refs=new Set();
    events.filter(e=>e.kind==='knowledge_reference' && e.result?.entry_id).forEach(e=>{ const ref=e.result,key=`${ref.entry_id}:${ref.version}`;if(refs.has(key))return;refs.add(key);const card=document.createElement('div');card.className='experience-card';card.append(document.createTextNode(`检索历史经验 · ${ref.title} · v${ref.version}`),button('查看经验',()=>open(ref.entry_id,ref.version)));message.bodyNode.append(card); });
    const node=document.createElement('div');node.className='experience-card';node.setAttribute('aria-live','polite');node.hidden=true;
    message.bodyNode.append(node);
    const old=jobs.get(id);if(old?.timer)clearTimeout(old.timer);
    const watch={id,session,message,node,attempts:0,fresh,dismissed:false};try{watch.dismissed=localStorage.getItem(`pilot-experience-dismissed:${id}`)==='1';}catch{}jobs.set(id,watch);updateCard(watch);
  }
  async function source(runID,session) {
    if(!current)return; if(dirty()){byId('experienceLeave').hidden=false;return;}
    const d=current;
    runID=typeof runID==='string'?runID:d.entry.source_run_id;session=session || d.entry.session_id;
    try {
      const records=await request(`/api/v1/chat/runs?session_id=${encodeURIComponent(session)}`);
      if(current!==d || busy)return;
      if(!records.some(r=>r.id===runID)){error('原始会话记录已清理或超出历史加载范围；此处仍保留当时的证据快照。');return;}
    } catch(e) {if(current===d)error(e.message);return;}
    if(current!==d || busy)return;
    closeNow();switchTab('chat');await window.PilotWorkspace.select(session,true);
    const target=document.querySelector(`[data-run-id="${runID}"]`);
    target?.scrollIntoView({block:'center'});
    showToast(`来源 Run：${runID}`,'success');
  }
  async function init() {
    const account=await request('/api/v1/account/session',{});
    userIdInput.value=account.user_id;userIdInput.readOnly=true;
    const settings=await request(`${api}/settings`);automatic=settings.automatic;byId('experienceAutomatic').checked=automatic;
    byId('experienceAutomatic').addEventListener('change',async e=>{const checked=e.target.checked;e.target.disabled=true;try{const saved=await request(`${api}/settings`,{automatic:checked});automatic=saved.automatic;showToast(automatic?'问题确认解决后自动提炼草稿，仍需人工发布':'问题进度继续整理，知识草稿可手动触发','success');}catch(err){e.target.checked=automatic;showToast(err.message,'error');}finally{e.target.disabled=false;}});
    byId('experienceFilters').addEventListener('submit',e=>{e.preventDefault();page=1;refresh();});
    document.querySelectorAll('[data-experience-tab]').forEach(b=>b.addEventListener('click',()=>{tab=b.dataset.experienceTab;page=1;document.querySelectorAll('[data-experience-tab]').forEach(n=>n.classList.toggle('active',n===b));refresh();}));
    byId('experiencePrev').addEventListener('click',()=>{if(page>1){page--;refresh();}});byId('experienceNext').addEventListener('click',()=>{if(page*30<total){page++;refresh();}});
    byId('experienceClose').addEventListener('click',close);byId('experienceEditor').addEventListener('input',markDirty);byId('experienceEditor').addEventListener('submit',e=>{e.preventDefault();submit('save');});
    byId('experienceEdit').addEventListener('click',()=>{if(editing && dirty()){showToast('请先保存修改，再返回阅读');return;}setEditing(!editing);});
    byId('experienceNote').addEventListener('input',markDirty);
    byId('experienceVersion').addEventListener('change',e=>{if(dirty()){e.target.value=current.version.number;byId('experienceLeave').hidden=false;return;}open(current.entry.id,Number(e.target.value));});
    document.querySelectorAll('[data-leave]').forEach(b=>b.addEventListener('click',()=>{if(b.dataset.leave==='save')submit('save',true);else if(b.dataset.leave==='discard')closeNow();else byId('experienceLeave').hidden=true;}));
    byId('experienceSource').addEventListener('click',source);
    document.addEventListener('keydown',e=>{if(e.key==='Escape' && !byId('experienceDrawer').hidden){e.preventDefault();close();}});
    matchMedia('(max-width:1199px)').addEventListener('change',modal);
    document.addEventListener('keydown',e=>{
      const drawer=byId('experienceDrawer');if(e.key!=='Tab' || drawer.hidden || drawer.getAttribute('aria-modal')!=='true')return;
      const nodes=Array.from(drawer.querySelectorAll('button,input,select,textarea,summary')).filter(n=>!n.disabled && n.getClientRects().length);
      const first=nodes[0],last=nodes.at(-1);if(!first)return;
      if(e.shiftKey && (document.activeElement===first || !drawer.contains(document.activeElement))){e.preventDefault();last.focus();}
      else if(!e.shiftKey && (document.activeElement===last || !drawer.contains(document.activeElement))){e.preventDefault();first.focus();}
    });
    window.addEventListener('beforeunload',e=>{if(dirty()){e.preventDefault();e.returnValue='';}});
    setLayout('chat');refresh();window.PilotIssues?.init();return account;
  }
  function resumeCards(manual=false){jobs.forEach(w=>{if(w.message.bodyNode.isConnected){clearTimeout(w.timer);if(manual){w.dismissed=false;w.attempts=0;try{localStorage.removeItem(`pilot-experience-dismissed:${w.id}`);}catch{}}updateCard(w);}});}
  window.PilotKnowledge={init,refresh,attach,open,canNavigate,setLayout,resumeCards};
})();
