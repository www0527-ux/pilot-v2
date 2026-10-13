/* Transport-independent state: process events never become answer text. */
(() => {
  const terminal = new Set(['completed', 'failed', 'cancelled', 'interrupted', 'rejected', 'processing']);
  class AgentSession {
    constructor(id, change = () => {}) {
      this.id = id; this.phase = 'executing'; this.text = ''; this.answerEnded = false;
      this.sequence = 0; this.waiting = new Map(); this.events = []; this.change = change; this.result = null;
    }
    accept(event) {
      if (!event || event.run_id !== this.id || !Number.isSafeInteger(event.sequence) || event.sequence < 1) throw new Error('执行事件格式或所属会话不正确');
      if (event.sequence <= this.sequence || terminal.has(this.phase)) return;
      this.waiting.set(event.sequence, event);
      while (this.waiting.has(this.sequence + 1)) {
        const next = this.waiting.get(this.sequence + 1); this.waiting.delete(this.sequence + 1);
        this.apply(next); this.sequence = next.sequence; this.events.push(next); this.change(next, this);
        if (terminal.has(this.phase)) { this.waiting.clear(); break; }
      }
    }
    apply(event) {
      if (event.kind === 'answer') {
        if (event.status === 'started') { this.phase = 'final_streaming'; this.answerEnded = false; }
        else if (event.status === 'delta') {
          if (this.phase !== 'final_streaming' || this.answerEnded) throw new Error('回答增量缺少开始事件或到达已结束的回答');
          this.text += event.text || '';
        } else if (event.status === 'replace') {
          if (this.phase !== 'final_streaming') throw new Error('回答修正缺少开始事件'); this.text = event.text || '';
        } else if (event.status === 'ended') {
          if (this.phase !== 'final_streaming') throw new Error('回答结束事件缺少开始事件'); this.answerEnded = true;
        }
      } else if (event.kind === 'run') {
        if (event.status === 'completed' && (!this.answerEnded || !this.text.trim())) throw new Error('任务在回答流结束前被标记为完成');
        this.phase = event.status;
      } else if (event.kind === 'result') this.result = event.result;
    }
    get terminal() { return terminal.has(this.phase); }
  }
  window.PilotAgentSession = AgentSession;
})();
