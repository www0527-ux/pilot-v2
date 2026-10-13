/* Local view navigation; server run identity and SSE remain in PilotChat. */
(() => {
  const el = id => document.getElementById(id);
  const views = new Map();
  let shown = '', revision = 0;
  const indexKey = () => `pilot-history:${el('userId').value}`;
  const index = () => { try { return JSON.parse(localStorage.getItem(indexKey())) || []; } catch { return []; } };
  function remember(title = '') {
    const id = el('sessionId').value.trim(); if (!id) return;
    const rows = index(), old = rows.find(r => r.id === id);
    const row = {id, title: title.slice(0, 60) || old?.title || '排障会话', at: new Date().toISOString()};
    try { localStorage.setItem(indexKey(), JSON.stringify([row, ...rows.filter(r => r.id !== id)].slice(0, 60))); } catch {}
  }
  function status() {
    const current = el('sessionId').value.trim();
    const running = state.activeRun?.session.sessionId || state.runningSession;
    const other = running && running !== current;
    el('sessionStatus').textContent = state.historyLoading ? '加载历史…' : state.sending ? (other ? '另一会话正在运行 · 返回历史可查看' : 'Agent 正在执行') : current ? '会话已就绪' : '新会话 · 发送后开始';
    el('chatMessages').classList.toggle('is-empty', !el('chatMessages').children.length);
  }
  function clearView() {
    revision++;
    if (shown) {
      const fragment = document.createDocumentFragment();
      const scroll=el('chatMessages').scrollTop;
      while (el('chatMessages').firstChild) fragment.append(el('chatMessages').firstChild);
      views.set(shown, {fragment, scroll});
    } else el('chatMessages').replaceChildren();
    el('sessionIssues').open = false; el('issueList').replaceChildren(); el('issueCount').textContent = '';
    el('issueNotice').textContent = ''; el('hitlApproval').hidden = true;
    el('sessionSettings').open = false; el('queryInput').value = '';
  }
  async function select(id, reload = false) {
    el('sessionId').value=shown;
    if (state.pendingHitl) { showToast('请先处理当前运行的审批，再切换会话', 'info'); return false; }
    if (!window.PilotKnowledge?.canNavigate()) return false;
    const previous = shown;
    clearView(); shown = id; el('sessionId').value = id;
    state.historyLoading = false;
    el('sendBtn').disabled = state.sending;
    try { localStorage.setItem('pilot-session', JSON.stringify({userId:el('userId').value, sessionId:id})); } catch {}
    const cached = views.get(id);
    const running = (state.activeRun?.session.sessionId || state.runningSession) === id;
    if (cached && (!reload || running)) {
      el('chatMessages').append(cached.fragment); el('chatMessages').scrollTop = cached.scroll;
    } else if (id) { await loadHistory(); }
    window.PilotIssues?.reset(); if (id) window.PilotIssues?.refresh(true);
    window.PilotKnowledge?.resumeCards();
    status(); el('queryInput').focus(); return previous !== id || Boolean(id);
  }
  function sent(title) { shown = el('sessionId').value.trim(); remember(title); status(); }
  function history() {
    const list = el('historyList'); list.replaceChildren();
    const rows = index();
    rows.forEach(row => {
      const button = document.createElement('button'); button.type = 'button'; button.className = 'history-row';
      const title = document.createElement('strong'), meta = document.createElement('span');
      title.textContent = row.title; meta.textContent = new Date(row.at).toLocaleString('zh-CN');
      button.append(title, meta); button.addEventListener('click', async () => { el('historyDialog').close(); await select(row.id); }); list.append(button);
    });
    if (!rows.length) list.textContent = '本浏览器尚无会话记录。';
    el('historySessionInput').value = ''; el('historyDialog').showModal();
  }
  function init() {
    shown = el('sessionId').value.trim();
    el('historyClose').addEventListener('click', () => el('historyDialog').close());
    el('historyForm').addEventListener('submit', async e => { e.preventDefault(); const id=el('historySessionInput').value.trim(); if(!id)return; el('historyDialog').close(); await select(id,true); });
    el('loadSessionIdBtn').addEventListener('click', () => select(el('sessionId').value.trim(),true));
    document.querySelectorAll('[data-copy]').forEach(b => b.addEventListener('click', async () => {
      const value=el(b.dataset.copy).value; if(!value){showToast('发送首条消息后生成会话 ID');return;}
      try { await navigator.clipboard.writeText(value); showToast('已复制'); } catch { showToast('复制失败，可以选中输入框内容手动复制','error'); }
    }));
    document.addEventListener('click', e => { if(!el('sessionSettings').contains(e.target)) el('sessionSettings').open=false; });
    document.addEventListener('keydown', e => {if(e.key==='Escape')el('sessionSettings').open=false;});
    const observer=new MutationObserver(status); observer.observe(el('chatMessages'),{childList:true});
    status();
  }
  window.PilotWorkspace = {init,select,history,sent,status,remember,revision:()=>revision,current:id=>id===el('sessionId').value.trim()};
})();
