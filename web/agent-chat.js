/* Task event transport, reconnect, cancellation, and history replay. */
(() => {
  let active = null;
  const key = 'pilot-active-run';
  const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
  const ownership = session => `user_id=${encodeURIComponent(session.userId)}&session_id=${encodeURIComponent(session.sessionId)}`;
  const remember = value => { try { value ? localStorage.setItem(key, JSON.stringify(value)) : localStorage.removeItem(key); } catch {} };
  const saved = () => { try { return JSON.parse(localStorage.getItem(key)); } catch { return null; } };
  async function read(response, run) {
    if (!response.ok) {
      const error = new Error(getErrorMessage(parseJsonSafe(await response.text()), response, '执行请求失败'));
      error.noRun = true; throw error;
    }
    if (!(response.headers.get('Content-Type') || '').includes('text/event-stream')) throw new Error('服务没有返回 Agent 事件流，请检查服务版本');
    const reader = response.body.getReader(); const decoder = new TextDecoder(); let buffer = ''; let doneSignal = false;
    try {
      while (!doneSignal) {
        const chunk = await reader.read(); if (chunk.done) break;
        buffer = (buffer + decoder.decode(chunk.value, { stream:true })).replace(/\r\n/g, '\n');
        let index;
        while ((index = buffer.indexOf('\n\n')) !== -1) {
          const block = buffer.slice(0, index); buffer = buffer.slice(index + 2);
          const event = extractSSEEvent(block); if (!event.data) continue;
          if (event.data === '[DONE]') { doneSignal = true; break; }
          if (event.type !== 'agent') throw new Error('收到缺少阶段标识的 Agent 事件');
          run.accept(parseJsonSafe(event.data));
        }
      }
      if (!doneSignal || !run.terminal || run.waiting.size) throw new Error('事件连接中断，正在恢复执行记录');
    } finally { reader.releaseLock(); }
  }
  async function consume(initial, run, session, message, controller) {
    let response = initial;
    for (let attempt = 0; ; attempt++) {
      try {
        if (!response) response = await fetch(`/api/v1/chat/runs/${run.id}/events?${ownership(session)}&after=${run.sequence}`, { cache:'no-store', signal:controller.signal });
        await read(response, run); return;
      } catch (error) {
        if (controller.signal.aborted) throw error;
        if (error.noRun || attempt >= 3 || /格式|所属|缺少|标记|版本/.test(error.message)) throw error;
        message.execution.handle({kind:'notice', summary:'连接中断，正在续接；已接收的内容会保留。'});
        await delay([300, 800, 1500][attempt]); response = null;
      }
    }
  }
  async function one(query, session, message, mode, resume, replay = null) {
    const id = replay?.id || crypto.randomUUID(); const controller = new AbortController();
    const renderer = createStreamRenderer(message.contentNode);
    const run = new window.PilotAgentSession(id, (event, current) => {
      message.execution.handle(event);
      if (event.kind === 'answer' && ['delta','replace'].includes(event.status)) renderer.schedule(current.text);
      if (event.kind === 'run' && current.terminal) renderer.flush();
    });
    message.agentSession = run;
    run.message = message;
    active = {id, session, controller, run, message}; state.activeRun = active;
    remember({id, query, session, mode});
    try {
      let response = null;
      if (!replay) {
        const endpoint = mode === 'resume' ? '/api/v1/chat/multi-agent/resume' : mode === 'multi-agent' ? '/api/v1/chat/multi-agent' : '/api/v1/chat/stream';
        response = await fetch(endpoint, {
          method:'POST', headers:{'Content-Type':'application/json'}, signal:controller.signal,
          body:JSON.stringify({ user_id:session.userId, session_id:session.sessionId, query, duration_minutes:Number(chatDurationInput.value) || 30,
            include_progress:true, run_id:id, ...(resume || {}) }),
        });
      }
      await consume(response, run, session, message, controller);
      if (run.phase === 'completed') attachFeedbackControls(message, id, session);
      if (run.terminal) window.PilotKnowledge?.attach(message,id,session,run.events,true);
      if (run.terminal) remember(null);
      return run;
    } catch (error) {
      if (controller.signal.aborted) {
        message.execution.finish('cancelled', '已取消'); remember(null); return run;
      }
      message.execution.finish('failed', error.message);
      if (error.noRun) remember(null);
      throw error;
    } finally {
      renderer.flush(); if (active?.id === id) { active = null; state.activeRun = null; }
    }
  }
  async function send(query, session, message, mode) {
    return continueApproval(await one(query, session, message, mode), session);
  }
  async function continueApproval(run, session) {
    while (run.phase === 'interrupted' && run.result?.metadata?.checkpoint_id) {
      if (!window.PilotWorkspace.current(session.sessionId)) return run;
      const meta = run.result.metadata; const approve = await waitForHitlDecision(meta);
      if (run.message) run.message.execution.heading.textContent = approve ? '审批已通过，已继续执行' : '审批已拒绝';
      const resumed = createPendingAssistant();
      run = await one('继续已审批的排障', session, resumed, 'resume', {checkpoint_id:meta.checkpoint_id, approve, parent_run_id:run.id});
    }
    return run;
  }
  async function cancel() {
    if (state.pendingHitl) { resolveHitlDecision(false); return; }
    const current = active; if (!current) return;
    const response = await fetch(`/api/v1/chat/runs/${current.id}/cancel?${ownership(current.session)}`, {method:'POST'});
    if (!response.ok) { showToast('取消请求失败，请重试', 'error'); return; }
    const result = await response.json(); if (result.data !== 'cancelled') return;
    // Server records cancellation first, then local reads stop. A late token
    // cannot overwrite either server or client terminal state.
    current.run.phase = 'cancelled'; current.controller.abort(); current.message.execution.finish('cancelled', '已取消'); remember(null);
  }
  async function restoreActive() {
    const value = saved(); if (!value || state.sending) return false;
    const session = getSessionParams(); if (!session || session.userId !== value.session?.userId || session.sessionId !== value.session?.sessionId) return;
    appendMessage('user', value.query); const message = createPendingAssistant(); setSending(true);
    try { await continueApproval(await one(value.query, session, message, value.mode, null, value), session); }
    catch (error) { showToast(error.message, 'error'); }
    finally { setSending(false); }
    return true;
  }
  async function history(session, legacy, isCurrent = () => true) {
    const response = await fetch(`/api/v1/chat/runs?${ownership(session)}`, {cache:'no-store'});
    const data = parseJsonSafe(await response.text());
    if (!response.ok || !data?.success) throw new Error(getErrorMessage(data, response, '执行历史读取失败'));
    if (!isCurrent()) return 0;
    chatMessages.querySelectorAll('.msg').forEach(node => { node.remove(); });
    const runs = data.data || []; const ids = new Set(runs.map(run => run.id));
    const continued = new Set(runs.map(run => run.parent_run_id).filter(Boolean));
    const entries = legacy.filter(item => !ids.has(item.run_id)).map(item => ({at:item.timestamp, message:item}));
    runs.forEach(run => entries.push({at:run.created_at, run})); entries.sort((a,b) => new Date(a.at) - new Date(b.at));
    let reconnect = null;
    for (const entry of entries) {
      if (entry.message) appendMessage(['user','assistant'].includes(entry.message.role) ? entry.message.role : 'system', entry.message.content || '');
      else {
        const record = entry.run; appendMessage('user', record.query); const message = createPendingAssistant();
        const model = new window.PilotAgentSession(record.id); (record.events || []).forEach(event => model.accept(event));
        message.agentSession = model; model.message = message; message.execution.restore(record.events || []); setMessageContent(message.contentNode, record.answer || '');
        if (['executing','running','final_streaming'].includes(record.status)) reconnect={record,message};
        if (record.status === 'interrupted' && continued.has(record.id)) message.execution.heading.textContent = '审批已处理，执行已继续';
        if (record.status === 'completed') attachFeedbackControls(message, record.id, session);
        if (['completed','failed','cancelled','interrupted'].includes(record.status)) window.PilotKnowledge?.attach(message,record.id,session,record.events || []);
        if (record.status === 'interrupted' && !continued.has(record.id) && model.result?.metadata?.checkpoint_id) {
          const button = document.createElement('button'); button.className = 'btn btn-subtle'; button.textContent = '继续处理审批';
          button.addEventListener('click', async () => {
            if (state.sending) return; button.disabled = true; setSending(true);
            try { await continueApproval(model, session); } catch (error) { showToast(error.message, 'error'); }
            finally { setSending(false); }
          }); message.bodyNode.append(button);
        }
      }
    }
    if (reconnect && !active && !state.sending && isCurrent()) {
      const {record,message}=reconnect;message.execution.resume(record.created_at);setSending(true);
      // Reattach via GET events; never POST the user's question again.
      continueApprovalPromise(record,session,message);
    }
    return entries.length;
  }
  async function continueApprovalPromise(record,session,message) {
    try {await continueApproval(await one(record.query,session,message,record.mode,null,record),session);}
    catch(error){showToast(error.message,'error');}
    finally{setSending(false);}
  }
  window.PilotChat = {send, cancel, history, restoreActive};
})();
