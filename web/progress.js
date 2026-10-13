/* A compact, grouped trail; technical data uses textContent only. */
(() => {
  const names = {
    query_alerts: '查询告警信息', query_metrics: '分析性能指标', query_logs: '查询异常日志',
    query_traces: '检查请求链路', search_knowledge: '检索排障知识', get_service_topology: '检查服务依赖',
    get_current_time: '确认排障时间', analyze_anomaly: '分析指标异常', correlate_signals: '关联观测证据',
    triage: '制定排查计划', evidence: '采集运行证据', knowledge: '检索排障知识', synthesis: '整理诊断信息',
  };
  const order = Object.keys(names);
  const make = (tag, cls, text) => { const node = document.createElement(tag); node.className = cls; if (text !== undefined) node.textContent = text; return node; };
  const seconds = ms => `${(Math.max(0, ms) / 1000).toFixed(1)} 秒`;
  const stopped = new Set(['completed', 'failed', 'cancelled', 'interrupted', 'rejected', 'processing']);
  const label = item => item.kind === 'model' ? (item.name === 'execution' ? '执行流程' : '分析请求与证据') : names[item.name] || '执行查询';
  class ExecutionView {
    constructor(message, options) {
      this.options = options; this.status = 'executing'; this.started = performance.now();
      this.groups = new Map(); this.steps = new Map(); this.calls = new Map(); this.processElapsed = null;
      this.statusNode = make('div', 'agent-status'); this.statusNode.setAttribute('role', 'status');
      this.indicator = make('span', 'execution-indicator'); this.indicator.setAttribute('aria-hidden', 'true');
      this.heading = make('span', 'agent-status-text', '正在分析请求');
      this.elapsed = make('span', 'execution-elapsed', '0.0 秒'); this.elapsed.setAttribute('aria-hidden', 'true');
      this.statusNode.append(this.indicator, this.heading, this.elapsed);
      this.panel = make('details', 'execution-panel'); this.panel.open = true;
      const summary = make('summary', 'execution-heading'); this.summary = make('span', 'execution-title', '执行过程');
      summary.append(this.summary, make('span', 'execution-chevron', '⌄'));
      this.list = make('div', 'execution-list'); this.list.setAttribute('aria-label', '执行步骤');
      this.empty = make('div', 'execution-empty', '正在准备必要的上下文。'); this.list.append(this.empty);
      this.panel.append(summary, this.list); this.notice = make('div', 'agent-notice'); this.notice.hidden = true;
      options.update(() => {
        const body = message.contentNode.parentElement;
        message.bodyNode.insertBefore(this.statusNode, body); message.bodyNode.insertBefore(this.panel, body);
        message.bodyNode.insertBefore(this.notice, body); body.hidden = true;
      });
      this.timer = setInterval(() => { this.elapsed.textContent = seconds(performance.now() - this.started); }, 500);
      this.refresh();
    }
    handle(event) {
      if (!event) return;
      if (event.kind === 'run') { this.setPhase(event.status === 'running' ? 'executing' : event.status, event.summary); return; }
      if (event.kind === 'answer') { if (event.status === 'started') this.setPhase('final_streaming'); return; }
      if (event.kind === 'notice') { this.options.update(() => { this.notice.textContent = event.summary; this.notice.hidden = false; }); return; }
      if (!['model', 'role', 'tool'].includes(event.kind) || !event.id) return;
      this.options.update(() => {
        const previous = this.calls.get(event.id);
        if (previous && previous.status !== 'running' && event.status === 'running') return;
        const item = { ...previous, ...event, firstSequence: previous?.firstSequence || event.sequence || 0 }; this.calls.set(event.id, item);
        const groupID = item.group_id || (item.kind === 'role' ? item.name : 'investigation');
        let group = this.groups.get(groupID);
        if (!group) {
          const node = make('div', 'execution-group');
          node.append(make('div', 'execution-group-title', ({triage:'计划与分工', evidence:'服务观测', knowledge:'知识检索', synthesis:'综合诊断', investigation:'收集与分析证据'})[groupID] || '执行步骤'));
          const rows = make('div', 'execution-group-rows'); node.append(rows); this.list.append(node);
          group = { node, rows, order: groupID === 'triage' ? 0 : groupID === 'investigation' || groupID === 'evidence' ? 1 : groupID === 'knowledge' ? 2 : 3 };
          this.groups.set(groupID, group);
          [...this.groups.values()].sort((a, b) => a.order - b.order).forEach(value => this.list.append(value.node));
        }
        const key = `${groupID}:${item.kind}:${item.kind === 'model' ? 'analysis' : item.name}`;
        let step = this.steps.get(key);
        if (!step) {
          const node = make('details', 'execution-step'); const summary = make('summary', 'execution-step-heading');
          const icon = make('span', 'execution-step-icon'); icon.setAttribute('aria-hidden', 'true');
          const title = make('span', 'execution-step-title', label(item)); const timing = make('span', 'execution-step-time');
          summary.append(icon, title, timing);
          const description = make('div', 'execution-step-description');
          const technical = make('details', 'execution-technical'); technical.append(make('summary', 'execution-technical-heading', '查看技术详情'));
          const detail = make('div', 'execution-step-detail'); technical.append(detail); node.append(summary, description, technical); group.rows.append(node);
          step = { node, icon, timing, description, detail, calls: new Map(), order: item.kind === 'model' ? -2 : item.kind === 'role' ? -1 : order.indexOf(item.name) };
          this.steps.set(key, step);
          [...this.steps.values()].filter(value => value.node.parentElement === group.rows).sort((a, b) => a.order - b.order).forEach(value => group.rows.append(value.node));
        }
        step.calls.set(item.id, item);
        const calls = [...step.calls.values()].sort((a, b) => (a.round || 0) - (b.round || 0) || a.firstSequence - b.firstSequence);
        const active = calls.some(value => value.status === 'running'); const last = calls.at(-1); const status = active ? 'running' : last.status;
        const failures = calls.filter(value => value.status === 'failed').length;
        step.node.dataset.status = status;
        step.icon.textContent = active ? '' : status === 'failed' ? '!' : status === 'skipped' || status === 'interrupted' ? '–' : '✓';
        step.timing.textContent = active ? '进行中' : seconds(calls.reduce((sum, value) => sum + (value.duration_ms || 0), 0));
        step.description.textContent = failures && status === 'completed' ? `已恢复 · ${calls.length} 次调用` : status === 'failed' ? '本步骤未成功，详情中保留了错误信息。' : last.summary || '执行完成';
        step.detail.replaceChildren();
        calls.forEach((call, index) => {
          const entry = make('section', 'execution-call');
          entry.append(make('div', 'execution-call-title', `调用 ${index + 1}${call.round ? ` · 第 ${call.round} 轮` : ''} · ${call.status} · ${seconds(call.duration_ms || 0)}`));
          entry.append(make('pre', '', `名称：${call.name || ''}\n${call.summary || ''}`));
          if (call.parameters && Object.keys(call.parameters).length) entry.append(make('pre', '', `参数\n${JSON.stringify(call.parameters, null, 2)}`));
          if (call.result !== undefined && call.result !== null) entry.append(make('pre', '', `工具结果\n${typeof call.result === 'string' ? call.result : JSON.stringify(call.result, null, 2)}`));
          else if (call.result_preview) entry.append(make('pre', '', `结果预览\n${call.result_preview}`)); step.detail.append(entry);
        });
        this.empty.hidden = true; this.refresh();
      });
    }
    refresh() {
      this.statusNode.dataset.status = this.status; this.panel.dataset.status = this.status;
      const count = this.steps.size;
      this.summary.textContent = `${this.status === 'completed' ? '已完成排障分析' : this.processElapsed !== null ? '执行记录' : '执行过程'}${count ? ` · ${count} 个步骤` : ''}${this.processElapsed !== null ? ` · ${seconds(this.processElapsed)}` : ''}`;
      if (this.status !== 'executing') return;
      const active = [...this.calls.values()].filter(item => item.status === 'running'); const tools = active.filter(item => item.kind === 'tool'); const role = active.find(item => item.kind === 'role');
      this.heading.textContent = tools.length ? `${label(tools[0])}${tools.length > 1 ? ` · ${tools.length} 项并行` : ''}` : role ? label(role) : '正在分析请求';
    }
    setPhase(phase, summary = '') {
      if (stopped.has(this.status)) return;
      const previous = this.status; this.status = phase;
      this.options.update(() => {
        if (phase === 'final_streaming' && previous !== phase) { this.processElapsed = performance.now() - this.started; this.panel.open = false; }
        if (phase === 'executing' && previous === 'final_streaming') { this.processElapsed = null; this.panel.open = true; }
        this.heading.textContent = summary || (phase === 'final_streaming' ? '正在生成最终回答' : '正在分析请求');
        if (stopped.has(phase)) {
          this.panel.open = false;
          clearInterval(this.timer); this.elapsed.textContent = seconds(performance.now() - this.started);
          if (phase === 'failed') {
            const detail=make('details','execution-error'),heading=make('summary','','查看错误详情');
            detail.append(heading,make('pre','',summary || '执行失败'));this.list.append(detail);
            this.heading.textContent=/402|Insufficient Balance/.test(summary)?'模型服务余额不足':/429/.test(summary)?'模型服务请求过于频繁':/timeout|超时/i.test(summary)?'执行超时，可查看详情后重试':'执行失败，可展开过程查看详情';
          }
          this.indicator.textContent = phase === 'completed' ? '✓' : phase === 'interrupted' ? 'Ⅱ' : phase === 'cancelled' || phase === 'rejected' ? '–' : '!';
          for (const step of this.steps.values()) { if (step.node.dataset.status === 'running') { step.node.dataset.status = 'interrupted'; step.icon.textContent = '–'; step.timing.textContent = '已停止'; } }
          this.empty.textContent = '本次请求没有执行步骤。';
        }
        this.refresh();
      });
    }
    finish(status = 'completed', summary = '') { this.setPhase(status, summary || ({completed:'已完成', failed:'执行失败', cancelled:'已取消', interrupted:'等待审批'})[status] || '已停止'); }
    restore(events) {
      events.forEach(event => this.handle(event)); this.options.update(() => { this.panel.open = false; }); clearInterval(this.timer);
      if (events.length > 1) {
        const start = new Date(events[0].timestamp);
        const boundary = events.find(event => event.kind === 'answer' && event.status === 'started') || events.at(-1);
        this.processElapsed = new Date(boundary.timestamp) - start;
        this.elapsed.textContent = seconds(new Date(events.at(-1).timestamp) - start); this.refresh();
      }
    }
    dispose() { clearInterval(this.timer); }
    resume(createdAt) {
      clearInterval(this.timer);
      const at = new Date(createdAt).getTime();
      if (Number.isFinite(at)) this.started = performance.now() - Math.max(0, Date.now() - at);
      this.timer = setInterval(() => { this.elapsed.textContent = seconds(performance.now() - this.started); }, 500);
    }
  }
  window.PilotExecution = ExecutionView;
})();
