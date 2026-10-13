const state = {
  streaming: true,
  analysisMode: 'standard',
  sending: false,
  overviewLoading: false,
  toastTimer: null,
  pendingHitl: null,
};

const tabButtons = Array.from(document.querySelectorAll('.tab-btn'));
const panelChat = document.getElementById('panel-chat');
const panelObservability = document.getElementById('panel-observability');
const panelKnowledge = document.getElementById('panel-knowledge');

const healthDot = document.getElementById('healthDot');
const healthText = document.getElementById('healthText');

const userIdInput = document.getElementById('userId');
const sessionIdInput = document.getElementById('sessionId');
const streamModeInput = document.getElementById('streamMode');
const chatDurationInput = document.getElementById('chatDuration');
const analysisModeInputs = Array.from(document.querySelectorAll('input[name="analysisMode"]'));
const composerHint = document.getElementById('composerHint');
const newSessionBtn = document.getElementById('newSessionBtn');
const loadHistoryBtn = document.getElementById('loadHistoryBtn');
const clearLocalBtn = document.getElementById('clearLocalBtn');

const chatForm = document.getElementById('chatForm');
const queryInput = document.getElementById('queryInput');
const sendBtn = document.getElementById('sendBtn');
const cancelBtn = document.getElementById('cancelBtn');
const chatMessages = document.getElementById('chatMessages');

const uploadForm = document.getElementById('uploadForm');
const docTitleInput = document.getElementById('docTitle');
const docCategoryInput = document.getElementById('docCategory');
const docContentInput = document.getElementById('docContent');

const searchForm = document.getElementById('searchForm');
const searchQueryInput = document.getElementById('searchQuery');
const searchResults = document.getElementById('searchResults');
const searchHint = document.getElementById('searchHint');

const overviewForm = document.getElementById('overviewForm');
const obsServiceInput = document.getElementById('obsService');
const obsMetricQueryInput = document.getElementById('obsMetricQuery');
const obsDurationInput = document.getElementById('obsDuration');
const obsLogLevelInput = document.getElementById('obsLogLevel');
const obsSummaryCards = document.getElementById('obsSummaryCards');
const alertsList = document.getElementById('alertsList');
const metricsChart = document.getElementById('metricsChart');
const logsList = document.getElementById('logsList');
const alertsMeta = document.getElementById('alertsMeta');
const metricsMeta = document.getElementById('metricsMeta');
const logsMeta = document.getElementById('logsMeta');
const logsSummary = document.getElementById('logsSummary');
const hitlApproval = document.getElementById('hitlApproval');
const hitlReason = document.getElementById('hitlReason');
const hitlCheckpoint = document.getElementById('hitlCheckpoint');
const hitlApproveBtn = document.getElementById('hitlApproveBtn');
const hitlRejectBtn = document.getElementById('hitlRejectBtn');

const toast = document.getElementById('toast');
const pageTitle = document.getElementById('pageTitle');
const pageDescription = document.getElementById('pageDescription');
const quickPromptButtons = Array.from(document.querySelectorAll('.quick-prompt'));

const pageMeta = {
  chat: {
    title: '智能对话',
    description: '与 Pilot 协作分析问题，保留完整排障上下文。',
  },
  observability: {
    title: '可观测性',
    description: '聚合告警、指标与日志，快速感知服务运行状态。',
  },
  knowledge: {
    title: '知识库',
    description: '沉淀排障经验，并通过混合检索持续复用。',
  },
};

function randomId(prefix) {
  return `${prefix}_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 7)}`;
}

function parseJsonSafe(text) {
  try {
    return JSON.parse(text);
  } catch (_error) {
    return null;
  }
}

function escapeHtml(text) {
  return String(text)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

function renderInlineMarkdown(text) {
  if (!text) {
    return '';
  }

  const codeSnippets = [];
  const tokenized = text.replace(/`([^`\n]+)`/g, (_match, code) => {
    const token = `@@CODE_${codeSnippets.length}@@`;
    codeSnippets.push(`<code>${escapeHtml(code)}</code>`);
    return token;
  });

  let html = escapeHtml(tokenized);
  html = html.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
  html = html.replace(/\*([^*]+)\*/g, '<em>$1</em>');
  html = html.replace(/~~([^~]+)~~/g, '<del>$1</del>');

  codeSnippets.forEach((snippet, idx) => {
    html = html.replace(`@@CODE_${idx}@@`, snippet);
  });

  return html;
}

function normalizeMarkdownText(text) {
  // Preserve Markdown syntax and code verbatim; only normalize line endings.
  return String(text || '').replace(/\r\n?/g, '\n');
}

function renderMarkdownFallback(text) {
  const normalized = normalizeMarkdownText(text);
  if (!normalized.trim()) {
    return '';
  }

  const lines = normalized.split('\n');
  const htmlParts = [];

  let inCodeBlock = false;
  let codeLang = '';
  let codeLines = [];
  let paragraphLines = [];
  let inUnorderedList = false;
  let inOrderedList = false;
  let tableLines = [];

  function closeLists() {
    if (inUnorderedList) {
      htmlParts.push('</ul>');
      inUnorderedList = false;
    }
    if (inOrderedList) {
      htmlParts.push('</ol>');
      inOrderedList = false;
    }
  }

  function flushParagraph() {
    if (!paragraphLines.length) {
      return;
    }
    const paragraphHtml = renderInlineMarkdown(paragraphLines.join('\n')).replace(/\n/g, '<br>');
    htmlParts.push(`<p>${paragraphHtml}</p>`);
    paragraphLines = [];
  }

  function splitTableRow(line) {
    let row = line.trim();
    if (row.startsWith('|')) {
      row = row.slice(1);
    }
    if (row.endsWith('|')) {
      row = row.slice(0, -1);
    }
    return row.split('|').map((cell) => cell.trim());
  }

  function isTableDivider(line) {
    const trimmed = line.trim();
    if (!trimmed) {
      return false;
    }
    return /^[:\-|\s]+$/.test(trimmed) && trimmed.includes('-');
  }

  function flushTable() {
    if (!tableLines.length) {
      return;
    }

    if (tableLines.length >= 2 && isTableDivider(tableLines[1])) {
      const headers = splitTableRow(tableLines[0]);
      const bodyRows = tableLines.slice(2).map((row) => splitTableRow(row));

      const headHtml = headers.map((cell) => `<th>${renderInlineMarkdown(cell)}</th>`).join('');
      const bodyHtml = bodyRows
        .map((cells) => `<tr>${cells.map((cell) => `<td>${renderInlineMarkdown(cell)}</td>`).join('')}</tr>`)
        .join('');

      htmlParts.push(`<table><thead><tr>${headHtml}</tr></thead><tbody>${bodyHtml}</tbody></table>`);
    } else {
      paragraphLines = paragraphLines.concat(tableLines);
      flushParagraph();
    }

    tableLines = [];
  }

  for (const rawLine of lines) {
    const line = rawLine.trimEnd();
    const fenceMatch = line.match(/^```(\w+)?\s*$/);

    if (fenceMatch) {
      flushParagraph();
      closeLists();

      if (!inCodeBlock) {
        inCodeBlock = true;
        codeLang = (fenceMatch[1] || '').toLowerCase();
        codeLines = [];
      } else {
        const languageClass = codeLang ? ` class="language-${escapeHtml(codeLang)}"` : '';
        htmlParts.push(`<pre><code${languageClass}>${escapeHtml(codeLines.join('\n'))}</code></pre>`);
        inCodeBlock = false;
        codeLang = '';
        codeLines = [];
      }
      continue;
    }

    if (inCodeBlock) {
      codeLines.push(rawLine);
      continue;
    }

    if (line.includes('|')) {
      flushParagraph();
      closeLists();
      tableLines.push(line);
      continue;
    }
    flushTable();

    if (!line.trim()) {
      flushParagraph();
      closeLists();
      continue;
    }

    const headingMatch = line.match(/^(#{1,6})\s+(.+)$/);
    if (headingMatch) {
      flushParagraph();
      closeLists();
      const level = headingMatch[1].length;
      htmlParts.push(`<h${level}>${renderInlineMarkdown(headingMatch[2])}</h${level}>`);
      continue;
    }

    const blockquoteMatch = line.match(/^>\s?(.*)$/);
    if (blockquoteMatch) {
      flushParagraph();
      closeLists();
      htmlParts.push(`<blockquote>${renderInlineMarkdown(blockquoteMatch[1])}</blockquote>`);
      continue;
    }

    const unorderedMatch = line.match(/^\s*[-*+]\s+(.+)$/);
    if (unorderedMatch && unorderedMatch[1].trim()) {
      flushParagraph();
      if (inOrderedList) {
        htmlParts.push('</ol>');
        inOrderedList = false;
      }
      if (!inUnorderedList) {
        htmlParts.push('<ul>');
        inUnorderedList = true;
      }
      htmlParts.push(`<li>${renderInlineMarkdown(unorderedMatch[1])}</li>`);
      continue;
    }

    const orderedMatch = line.match(/^\s*\d+\.\s+(.+)$/);
    if (orderedMatch && orderedMatch[1].trim()) {
      flushParagraph();
      if (inUnorderedList) {
        htmlParts.push('</ul>');
        inUnorderedList = false;
      }
      if (!inOrderedList) {
        htmlParts.push('<ol>');
        inOrderedList = true;
      }
      htmlParts.push(`<li>${renderInlineMarkdown(orderedMatch[1])}</li>`);
      continue;
    }

    if (inUnorderedList || inOrderedList) {
      closeLists();
    }
    paragraphLines.push(line);
  }

  flushTable();
  flushParagraph();
  closeLists();

  if (inCodeBlock) {
    const languageClass = codeLang ? ` class="language-${escapeHtml(codeLang)}"` : '';
    htmlParts.push(`<pre><code${languageClass}>${escapeHtml(codeLines.join('\n'))}</code></pre>`);
  }

  return htmlParts.join('');
}

function renderMarkdown(text) {
  const normalized = normalizeMarkdownText(text);
  if (!normalized.trim()) {
    return '';
  }

  if (window.marked && typeof window.marked.parse === 'function'
      && window.DOMPurify && typeof window.DOMPurify.sanitize === 'function') {
    if (typeof window.marked.setOptions === 'function') {
      window.marked.setOptions({
        gfm: true,
        breaks: true,
      });
    }

    const rawHtml = window.marked.parse(normalized);
    return window.DOMPurify.sanitize(rawHtml, {
      USE_PROFILES: { html: true },
    });
  }

  return renderMarkdownFallback(text);
}

function updateChatViewport(update, forceFollow = false) {
  const previousTop = chatMessages.scrollTop;
  // Decide before rendering: a new chunk may add more than one viewport of text.
  const follow = forceFollow
    || chatMessages.scrollHeight - chatMessages.clientHeight - previousTop <= 24;
  update();
  chatMessages.scrollTop = follow ? chatMessages.scrollHeight : previousTop;
}

function setMessageContent(contentNode, content) {
  const html = renderMarkdown(content);
  const update = () => {
    contentNode.innerHTML = html;
    if (contentNode.parentElement?.classList.contains('msg-bubble')) {
      contentNode.parentElement.hidden = !String(content || '').trim();
    }
  };
  if (chatMessages.contains(contentNode)) {
    updateChatViewport(update);
  } else {
    update();
  }
}

function createStreamRenderer(contentNode) {
  let pendingText = null;
  let frameId = null;

  function flush() {
    if (frameId !== null) {
      cancelAnimationFrame(frameId);
      frameId = null;
    }
    if (pendingText !== null) {
      const text = pendingText;
      pendingText = null;
      setMessageContent(contentNode, text);
    }
  }

  return {
    schedule(text) {
      pendingText = text;
      if (frameId === null) {
        frameId = requestAnimationFrame(flush);
      }
    },
    flush,
  };
}

function showToast(message, type = 'info') {
  let display = String(message || '');
  if (type === 'error') {
    if (/402|Insufficient Balance/.test(display)) display = '模型服务余额不足，请检查模型账户。';
    else if (/NodeRunError|node path|tool_steps|stack trace/i.test(display)) display = '执行未成功，请展开执行过程查看错误详情。';
    else if (display.length > 180) display = display.slice(0, 180) + '…';
  }
  toast.textContent = display;
  toast.classList.remove('info', 'success', 'error', 'show');
  toast.classList.add(type, 'show');

  if (state.toastTimer) {
    clearTimeout(state.toastTimer);
  }

  state.toastTimer = setTimeout(() => {
    toast.classList.remove('show');
  }, 2400);
}

function switchTab(target) {
  if (window.PilotKnowledge?.canNavigate && !window.PilotKnowledge.canNavigate(false)) return;
  tabButtons.forEach((btn) => {
    btn.classList.toggle('active', btn.dataset.tab === target);
  });

  panelChat.classList.toggle('active', target === 'chat');
  panelObservability.classList.toggle('active', target === 'observability');
  panelKnowledge.classList.toggle('active', target === 'knowledge');

  const meta = pageMeta[target] || pageMeta.chat;
  pageTitle.textContent = meta.title;
  pageDescription.textContent = meta.description;
  document.title = `${meta.title} · Pilot`;
  document.getElementById('chatTools').hidden = target !== 'chat';
  window.PilotKnowledge?.setLayout(target);
  window.scrollTo({top:0,behavior:'instant'});

  if (target === 'observability') {
    loadOverview();
  }
  if (target === 'knowledge') window.PilotKnowledge?.refresh();
}

function setHealth(online, text) {
  healthDot.classList.toggle('online', online);
  healthDot.classList.toggle('offline', !online);
  healthText.textContent = text;
}

async function checkHealth() {
  try {
    const response = await fetch('/api/v1/health', { cache: 'no-store' });
    if (!response.ok) {
      throw new Error(`HTTP ${response.status}`);
    }
    const payload = await response.json();
    const status = payload?.data?.status;
    if (status === 'degraded') {
      setHealth(true, '服务降级可用');
      return;
    }
    if (status === 'not_ready') {
      setHealth(false, '服务未就绪');
      return;
    }
    setHealth(true, '服务在线');
  } catch (_error) {
    setHealth(false, '服务离线');
  }
}

function formatTime() {
  const now = new Date();
  return now.toLocaleTimeString('zh-CN', { hour12: false });
}

function appendMessage(role, content) {
  const msg = document.createElement('article');
  msg.className = `msg msg-${role}`;

  const row = document.createElement('div');
  row.className = 'msg-row';

  const avatar = document.createElement('div');
  avatar.className = `msg-avatar msg-avatar-${role}`;
  avatar.textContent = role === 'user' ? 'U' : role === 'assistant' ? 'AI' : 'SYS';

  const body = document.createElement('div');
  body.className = 'msg-body';

  const meta = document.createElement('div');
  meta.className = 'msg-meta';
  const roleText = role === 'user' ? '你' : role === 'assistant' ? 'Pilot' : '系统';
  meta.textContent = `${roleText} · ${formatTime()}`;

  const bubble = document.createElement('div');
  bubble.className = 'msg-bubble';

  const contentNode = document.createElement('div');
  contentNode.className = 'msg-content';
  setMessageContent(contentNode, content);

  bubble.appendChild(contentNode);
  body.appendChild(meta);
  body.appendChild(bubble);
  row.appendChild(avatar);
  row.appendChild(body);
  msg.appendChild(row);

  updateChatViewport(() => chatMessages.appendChild(msg), role === 'user');

  return { messageNode: msg, contentNode, bodyNode: body };
}

function attachFeedbackControls(message, interactionId, session) {
  if (!message || !message.bodyNode || !interactionId || !session) return;
  const controls = document.createElement('div');
  controls.className = 'feedback-controls';
  controls.setAttribute('aria-label', '回答反馈');

  const improvement = document.createElement('textarea');
  improvement.rows = 1;
  improvement.maxLength = 2000;
  improvement.placeholder = '可选：哪里需要改进？';
  improvement.setAttribute('aria-label', '反馈改进建议');

  const submit = async (vote) => {
    controls.setAttribute('aria-busy', 'true');
    try {
      const { response, data } = await fetchJson('/api/v1/chat/feedback', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          interaction_id: interactionId,
          user_id: session.userId,
          session_id: session.sessionId,
          vote,
          improvement: improvement.value.trim(),
        }),
      });
      if (!response.ok || !data || data.success !== true) {
        throw new Error(getErrorMessage(data, response, '反馈保存失败'));
      }
      controls.replaceChildren(document.createTextNode(vote === 'up' ? '已记录有帮助' : '已记录改进反馈'));
      showToast('反馈已保存', 'success');
    } catch (error) {
      controls.setAttribute('aria-busy', 'false');
      showToast(error.message, 'error');
    }
  };

  ['up', 'down'].forEach((vote) => {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'feedback-btn';
    button.textContent = vote === 'up' ? '有帮助' : '需改进';
    button.setAttribute('aria-label', vote === 'up' ? '给回答点赞' : '给回答点踩');
    button.addEventListener('click', () => submit(vote));
    controls.appendChild(button);
  });
  controls.appendChild(improvement);
  const fold = document.createElement('details');
  fold.className = 'feedback-fold';
  const heading = document.createElement('summary'); heading.textContent = '回答反馈';
  fold.append(heading, controls);
  updateChatViewport(() => message.bodyNode.appendChild(fold));
}

function waitForHitlDecision(meta) {
  if (state.pendingHitl) {
    state.pendingHitl.resolve(false);
  }
  hitlReason.textContent = meta.reason || '宽时间窗或高成本操作需要审批。';
  hitlCheckpoint.textContent = `checkpoint: ${meta.checkpoint_id}`;
  hitlCheckpoint.hidden = true;
  hitlApproval.hidden = false;
  hitlApproveBtn.disabled = false;
  hitlRejectBtn.disabled = false;
  hitlApproveBtn.focus();
  return new Promise((resolve) => {
    state.pendingHitl = { checkpointId: meta.checkpoint_id, resolve };
  });
}

function resolveHitlDecision(approve) {
  if (!state.pendingHitl) return;
  const pending = state.pendingHitl;
  state.pendingHitl = null;
  hitlApproveBtn.disabled = true;
  hitlRejectBtn.disabled = true;
  hitlApproval.hidden = true;
  pending.resolve(approve);
}

function setSending(sending) {
  state.sending = sending;
  sendBtn.disabled = sending || state.historyLoading;
  sendBtn.textContent = sending ? '发送中...' : '发送';
  cancelBtn.hidden = !sending;
  [clearLocalBtn, userIdInput, sessionIdInput, ...analysisModeInputs].forEach(node => { node.disabled = sending; });
  window.PilotWorkspace?.status();
}

function getSessionParams() {
  const userId = userIdInput.value.trim();
  const sessionId = sessionIdInput.value.trim();

  if (!userId || !sessionId) {
    showToast('请先填写用户 ID 和会话 ID', 'error');
    return null;
  }
  try { localStorage.setItem('pilot-session', JSON.stringify({userId, sessionId})); } catch {}
  return { userId, sessionId };
}

async function fetchJson(url, options = {}, timeoutMs = 20000) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);

  try {
    const response = await fetch(url, {
      ...options,
      signal: controller.signal,
    });

    const raw = await response.text();
    return {
      response,
      raw,
      data: raw ? parseJsonSafe(raw) : null,
    };
  } catch (error) {
    if (error && error.name === 'AbortError') {
      throw new Error('请求超时，请稍后重试');
    }
    throw error;
  } finally {
    clearTimeout(timer);
  }
}

function getErrorMessage(data, response, fallback) {
  if (data && typeof data.message === 'string' && data.message.trim()) {
    return data.message;
  }
  if (response && !response.ok) {
    return `${fallback}: HTTP ${response.status}`;
  }
  return fallback;
}

function createPendingAssistant() {
  const message = appendMessage('assistant', '');
  message.execution = new window.PilotExecution(message, {
    update: updateChatViewport,
    following: () => chatMessages.scrollHeight - chatMessages.clientHeight - chatMessages.scrollTop <= 24,
  });
  return message;
}

async function sendChatSync(query, session, assistant = null) {
  const { response, data } = await fetchJson('/api/v1/chat', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      user_id: session.userId,
      session_id: session.sessionId,
      query,
      duration_minutes: Number(chatDurationInput.value) || 30,
    }),
  }, 310000);

  if (!response.ok || !data || data.success !== true) {
    throw new Error(getErrorMessage(data, response, '请求失败'));
  }

  const message = assistant || createPendingAssistant();
  setMessageContent(message.contentNode, data.data || '');
  message.execution.finish('completed', '回答完成');
  attachFeedbackControls(message, response.headers.get('X-Pilot-Interaction-ID'), session);
  window.PilotKnowledge?.attach(message,response.headers.get('X-Pilot-Run-ID'),session,[],true);
}

function updateAnalysisMode(mode) {
  state.analysisMode = mode === 'multi-agent' ? 'multi-agent' : 'standard';
  const multiAgentMode = state.analysisMode === 'multi-agent';
  streamModeInput.disabled = multiAgentMode;
  if (multiAgentMode) {
    streamModeInput.checked = false;
    state.streaming = false;
    composerHint.innerHTML = '接口：<code>/api/v1/chat/multi-agent</code>';
    return;
  }
  state.streaming = streamModeInput.checked;
  composerHint.innerHTML = '接口：<code>/api/v1/chat</code> 与 <code>/api/v1/chat/stream</code>';
}

function extractSSEPayload(block) {
  const lines = block.split('\n');
  const payloadLines = [];

  lines.forEach((line) => {
    if (line.startsWith('data:')) {
      let data = line.slice(5);
      // SSE 规范允许 data: 后跟一个空格，这里仅去掉一个空格，保留其余缩进。
      if (data.startsWith(' ')) {
        data = data.slice(1);
      }
      payloadLines.push(data);
    }
  });

  return payloadLines.join('\n');
}

function extractSSEEvent(block) {
  const typeLine = block.split('\n').find(line => line.startsWith('event:'));
  return { type: typeLine ? typeLine.slice(6).trim() : 'message', data: extractSSEPayload(block) };
}

async function handleSend(event) {
  event.preventDefault();

  if (state.sending || state.historyLoading) {
    return;
  }

  const query = queryInput.value.trim();
  if (!query) {
    showToast('请输入问题后再发送', 'error');
    return;
  }

  if (!sessionIdInput.value.trim()) sessionIdInput.value = randomId('session');
  const session = getSessionParams();
  if (!session) {
    return;
  }

  appendMessage('user', query);
  window.PilotWorkspace?.sent(query);
  const assistant = createPendingAssistant();
  queryInput.value = '';

  setSending(true);
  state.runningSession = session.sessionId;
  try {
    if (state.analysisMode === 'multi-agent') {
      await window.PilotChat.send(query, session, assistant, 'multi-agent');
    } else if (state.streaming) {
      await window.PilotChat.send(query, session, assistant, 'stream');
    } else {
      await sendChatSync(query, session, assistant);
    }
  } catch (error) {
    assistant.execution.finish('failed', error.message);
    showToast(error.message, 'error');
  } finally {
    state.runningSession = null;
    setSending(false);
  }
}

async function loadHistory() {
  if (state.sending && state.activeRun?.session.sessionId === sessionIdInput.value.trim()) return;
  if (!sessionIdInput.value.trim()) { window.PilotWorkspace?.status(); return; }
  const session = getSessionParams();
  if (!session) {
    return;
  }

  const revision = window.PilotWorkspace?.revision();
  const valid = () => window.PilotWorkspace?.revision() === revision && sessionIdInput.value.trim() === session.sessionId;
  state.historyLoading = true; sendBtn.disabled = true; window.PilotWorkspace?.status();
  try {
    const { response, data } = await fetchJson(
      `/api/v1/chat/history?user_id=${encodeURIComponent(session.userId)}&session_id=${encodeURIComponent(session.sessionId)}`,
      { cache: 'no-store' }
    );

    if (!response.ok || !data || data.success !== true) {
      throw new Error(getErrorMessage(data, response, '加载历史失败'));
    }

    if (!valid()) return;
    const history = Array.isArray(data.data) ? data.data : [];

    const count = await window.PilotChat.history(session, history, valid);
    if (!valid()) return;
    window.PilotWorkspace?.remember(history.find(m=>m.role==='user')?.content || '');
    showToast(`已加载 ${count} 条历史记录`, 'success');
  } catch (error) {
    if (valid()) showToast(error.message, 'error');
  } finally {
    if (valid()) {state.historyLoading=false;sendBtn.disabled=state.sending;window.PilotWorkspace?.status();}
  }
}

function makeEmptyCard(text) {
  const node = document.createElement('div');
  node.className = 'empty';
  node.textContent = text;
  return node;
}

function renderSearchResults(items) {
  searchResults.innerHTML = '';

  if (!items.length) {
    searchResults.appendChild(makeEmptyCard('没有检索到结果。'));
    if (searchHint) {
      searchHint.textContent = '没有命中结果，请尝试更换关键词或缩短查询语句。';
    }
    return;
  }

  items.forEach((item, idx) => {
    const card = document.createElement('div');
    card.className = 'result-item';

    const head = document.createElement('div');
    head.className = 'result-head';

    const rank = document.createElement('span');
    rank.className = 'result-rank';
    rank.textContent = String(idx + 1);

    const title = document.createElement('div');
    title.className = 'result-title';
    title.textContent = (item.title || '(无标题)') + (item.section_path ? ` · ${item.section_path}` : '');

    const tag = document.createElement('span');
    tag.className = 'result-tag';
    tag.textContent = item.category || 'uncategorized';

    const content = document.createElement('div');
    content.className = 'result-content';
    content.textContent = truncateText(item.content || '', 240);

    head.appendChild(rank);
    head.appendChild(title);
    head.appendChild(tag);
    card.appendChild(head);
    card.appendChild(content);

    searchResults.appendChild(card);
  });
}

function truncateText(text, maxLen) {
  if (text.length <= maxLen) {
    return text;
  }
  return `${text.slice(0, maxLen)}...`;
}

async function handleUpload(event) {
  event.preventDefault();

  const payload = {
    title: docTitleInput.value.trim(),
    category: docCategoryInput.value.trim(),
    content: docContentInput.value,
  };

  const markdownFile = document.getElementById('docFile')?.files[0];
  if (!payload.title || !payload.category || (!markdownFile && !payload.content.trim())) {
    showToast('标题、分类、内容都不能为空', 'error');
    return;
  }

  const submitBtn = uploadForm.querySelector('button[type="submit"]');
  submitBtn.disabled = true;
  submitBtn.textContent = '上传中...';

  try {
    let requestOptions;
    if (markdownFile) {
      const form = new FormData();
      form.append('file', markdownFile);
      form.append('title', payload.title);
      form.append('category', payload.category);
      requestOptions = { method: 'POST', body: form };
    } else {
      requestOptions = { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) };
    }
    const { response, data } = await fetchJson('/api/v1/knowledge/upload', requestOptions, 120000);

    if (!response.ok || !data || data.success !== true) {
      throw new Error(getErrorMessage(data, response, '上传失败'));
    }

    showToast(`上传成功，已建立 ${data.data?.chunk_count || 0} 个检索片段`, 'success');
    uploadForm.reset();
    docContentInput.required = true;
  } catch (error) {
    showToast(error.message, 'error');
  } finally {
    submitBtn.disabled = false;
    submitBtn.textContent = '上传到知识库';
  }
}

async function handleSearch(event) {
  event.preventDefault();

  const query = searchQueryInput.value.trim();
  if (!query) {
    showToast('请输入检索词', 'error');
    return;
  }

  const submitBtn = searchForm.querySelector('button[type="submit"]');
  submitBtn.disabled = true;
  submitBtn.textContent = '搜索中...';
  if (searchHint) {
    searchHint.textContent = `正在检索 "${query}" ...`;
  }

  try {
    const { response, data } = await fetchJson('/api/v1/knowledge/search', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ query }),
    });

    if (!response.ok || !data || data.success !== true) {
      throw new Error(getErrorMessage(data, response, '检索失败'));
    }

    const items = Array.isArray(data.data) ? data.data : [];
    renderSearchResults(items);
    if (searchHint) {
      searchHint.textContent = `关键词 "${query}" 返回 ${items.length} 条结果`;
    }
    showToast(`检索完成，共 ${items.length} 条`, 'success');
  } catch (error) {
    if (searchHint) {
      searchHint.textContent = '检索失败，请检查服务或调整关键词后重试。';
    }
    showToast(error.message, 'error');
  } finally {
    submitBtn.disabled = false;
    submitBtn.textContent = '搜索';
  }
}

function renderSummaryCards(summary) {
  const cards = [
    { label: '活跃告警', value: summary.alerts_active ?? 0 },
    { label: '告警总数', value: summary.alerts_total ?? 0 },
    { label: '指标点数', value: summary.metrics_points ?? 0 },
    {
      label: '最新指标值',
      value:
        typeof summary.metrics_latest === 'number'
          ? Number(summary.metrics_latest).toFixed(4)
          : '0.0000',
    },
  ];

  obsSummaryCards.innerHTML = '';
  cards.forEach((item, idx) => {
    const node = document.createElement('div');
    node.className = `summary-card summary-card-${idx + 1}`;

    const label = document.createElement('div');
    label.className = 'summary-label';
    label.textContent = item.label;

    const value = document.createElement('div');
    value.className = 'summary-value';
    value.textContent = String(item.value);

    node.appendChild(label);
    node.appendChild(value);
    obsSummaryCards.appendChild(node);
  });
}

function severityClass(severity) {
  const normalized = (severity || '').toLowerCase();
  if (normalized === 'critical') return 'sev-critical';
  if (normalized === 'warning') return 'sev-warning';
  return 'sev-info';
}

function renderAlerts(alerts) {
  alertsMeta.textContent = `${alerts.length} 条`;
  alertsList.innerHTML = '';

  if (!alerts.length) {
    alertsList.appendChild(makeEmptyCard('当前没有告警数据。'));
    return;
  }

  alerts.forEach((alert) => {
    const node = document.createElement('div');
    node.className = 'obs-item';

    const head = document.createElement('div');
    head.className = 'obs-item-head';

    const title = document.createElement('span');
    title.className = 'obs-item-title';
    title.textContent = alert.name || 'Unnamed Alert';

    const level = (alert.severity || 'info').toLowerCase();
    const levelChip = document.createElement('span');
    levelChip.className = `chip ${severityClass(level)}`;
    levelChip.textContent = level;

    head.appendChild(title);
    head.appendChild(levelChip);

    const startedAt = alert.start_time
      ? new Date(alert.start_time).toLocaleString('zh-CN', { hour12: false })
      : '-';

    const meta = document.createElement('div');
    meta.className = 'obs-item-meta';
    meta.textContent = `状态: ${alert.status || 'unknown'} · 时间: ${startedAt}`;

    const desc = document.createElement('div');
    desc.className = 'result-content';
    desc.textContent = alert.description || '无描述';

    node.appendChild(head);
    node.appendChild(meta);
    node.appendChild(desc);

    alertsList.appendChild(node);
  });
}

function renderLogs(logs, summary = {}) {
  const original = Number(summary.original_count ?? logs.length);
  const returned = Number(summary.returned_count ?? logs.length);
  const omitted = Number(summary.omitted_count ?? Math.max(0, original - returned));
  logsMeta.textContent = `${returned}/${original} 条`;
  if (logsSummary) {
    const stateText = summary.truncated ? `已按输出预算截断，省略 ${omitted} 条或部分正文。` : '结果未截断。';
    const levels = Object.entries(summary.level_counts || {}).map(([level, count]) => `${level} ${count}`).join(' · ');
    logsSummary.textContent = `${stateText}${levels ? ` 级别聚合：${levels}` : ''}`;
  }
  logsList.innerHTML = '';

  if (!logs.length) {
    logsList.appendChild(makeEmptyCard('当前没有日志数据（日志查询功能可继续对接真实日志源）。'));
    return;
  }

  logs.forEach((log) => {
    const node = document.createElement('div');
    node.className = 'obs-item';

    const head = document.createElement('div');
    head.className = 'obs-item-head';

    const service = document.createElement('span');
    service.className = 'obs-item-title';
    service.textContent = log.service || 'unknown-service';

    const level = document.createElement('span');
    const normalizedLevel = (log.level || 'info').toLowerCase() === 'warning' ? 'warn' : (log.level || 'info').toLowerCase();
    level.className = `chip log-${['debug', 'info', 'warn', 'error', 'fatal'].includes(normalizedLevel) ? normalizedLevel : 'info'}`;
    level.textContent = normalizedLevel;

    head.appendChild(service);
    head.appendChild(level);

    const timestamp = log.timestamp
      ? new Date(log.timestamp).toLocaleString('zh-CN', { hour12: false })
      : '-';

    const meta = document.createElement('div');
    meta.className = 'obs-item-meta';
    meta.textContent = timestamp;

    const message = document.createElement('div');
    message.className = 'result-content';
    message.textContent = log.message || '(empty log message)';

    node.appendChild(head);
    node.appendChild(meta);
    node.appendChild(message);

    logsList.appendChild(node);
  });
}

function renderMetricsChart(metrics) {
  metricsMeta.textContent = `${metrics.length} 点`;

  if (!metrics.length) {
    metricsChart.innerHTML = '';
    return;
  }

  const width = 1000;
  const height = 280;
  const pad = 24;

  const values = metrics.map((m) => Number(m.value) || 0);
  const min = Math.min(...values);
  const max = Math.max(...values);
  const span = Math.max(max - min, 1e-6);

  const xFor = (idx) =>
    pad + (idx / Math.max(values.length - 1, 1)) * (width - pad * 2);
  const yFor = (val) => height - pad - ((val - min) / span) * (height - pad * 2);

  const linePoints = values.map((v, i) => `${xFor(i)},${yFor(v)}`).join(' ');
  const areaPoints = [
    `${xFor(0)},${height - pad}`,
    ...values.map((v, i) => `${xFor(i)},${yFor(v)}`),
    `${xFor(values.length - 1)},${height - pad}`,
  ].join(' ');

  const gridLines = [0, 1, 2, 3, 4]
    .map((i) => {
      const y = pad + (i / 4) * (height - pad * 2);
      return `<line class="chart-grid-line" x1="${pad}" y1="${y}" x2="${width - pad}" y2="${y}" />`;
    })
    .join('');

  metricsChart.innerHTML = `
    <defs>
      <linearGradient id="metricAreaGradient" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0%" stop-color="rgba(16, 198, 170, 0.35)" />
        <stop offset="100%" stop-color="rgba(16, 198, 170, 0.03)" />
      </linearGradient>
    </defs>
    ${gridLines}
    <polygon class="chart-area" points="${areaPoints}" fill="url(#metricAreaGradient)" />
    <polyline class="chart-path" points="${linePoints}" />
  `;
}

function setOverviewLoading(loading) {
  state.overviewLoading = loading;
  const submitBtn = overviewForm.querySelector('button[type="submit"]');
  submitBtn.disabled = loading;
  submitBtn.textContent = loading ? '加载中...' : '刷新看板';
  panelObservability.classList.toggle('panel-loading', loading);
  panelObservability.setAttribute('aria-busy', String(loading));
}

async function loadOverview(event) {
  if (event) {
    event.preventDefault();
  }

  if (state.overviewLoading) {
    return;
  }

  const payload = {
    service: obsServiceInput.value.trim() || 'prometheus',
    metric_query: obsMetricQueryInput.value.trim() || 'up',
    duration_minutes: Number(obsDurationInput.value) || 60,
    log_level: obsLogLevelInput.value || 'all',
    log_limit: 100,
    log_output_rune_budget: 12000,
  };

  setOverviewLoading(true);

  try {
    const { response, data } = await fetchJson('/api/v1/observability/overview', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });

    if (!response.ok || !data || data.success !== true || !data.data) {
      throw new Error(getErrorMessage(data, response, '看板加载失败'));
    }

    const { summary = {}, alerts = [], metrics = [], logs = [], log_summary: logSummary = {} } = data.data;
    renderSummaryCards(alerts.length || metrics.length || logs.length ? summary : {});
    renderAlerts(Array.isArray(alerts) ? alerts : []);
    renderMetricsChart(Array.isArray(metrics) ? metrics : []);
    renderLogs(Array.isArray(logs) ? logs : [], logSummary);
  } catch (error) {
    showToast(error.message, 'error');
  } finally {
    setOverviewLoading(false);
  }
}

function bindEvents() {
  cancelBtn.addEventListener('click', () => window.PilotChat.cancel());
  hitlApproveBtn.addEventListener('click', () => resolveHitlDecision(true));
  hitlRejectBtn.addEventListener('click', () => resolveHitlDecision(false));
  tabButtons.forEach((btn) => {
    btn.addEventListener('click', () => switchTab(btn.dataset.tab));
  });

  streamModeInput.addEventListener('change', () => {
    state.streaming = streamModeInput.checked;
  });

  analysisModeInputs.forEach((input) => {
    input.addEventListener('change', () => {
      if (input.checked) {
        updateAnalysisMode(input.value);
      }
    });
  });

  newSessionBtn.addEventListener('click', () => {
    window.PilotWorkspace.select('');
  });
  sessionIdInput.addEventListener('change', () => window.PilotWorkspace.select(sessionIdInput.value.trim(), true));

  loadHistoryBtn.addEventListener('click', () => window.PilotWorkspace.history());

  clearLocalBtn.addEventListener('click', () => {
    chatMessages.querySelectorAll('.msg').forEach(node => node.remove());
    showToast('已清空', 'info');
  });

  chatForm.addEventListener('submit', handleSend);
  uploadForm.addEventListener('submit', handleUpload);
  document.getElementById('docFile')?.addEventListener('change', (event) => {
    const file = event.target.files[0];
    docContentInput.required = !file;
    if (file && !docTitleInput.value.trim()) docTitleInput.value = file.name.replace(/\.(md|markdown)$/i, '');
  });
  searchForm.addEventListener('submit', handleSearch);
  overviewForm.addEventListener('submit', loadOverview);

  quickPromptButtons.forEach((button) => {
    button.addEventListener('click', () => {
      queryInput.value = button.dataset.query || '';
      queryInput.focus();
      queryInput.setSelectionRange(queryInput.value.length, queryInput.value.length);
    });
  });

  queryInput.addEventListener('keydown', (event) => {
    if (event.key === 'Enter' && !event.shiftKey && !event.isComposing) {
      event.preventDefault();
      chatForm.requestSubmit();
    }
  });
  window.PilotWorkspace.init();
}

async function bootstrap() {
  let rememberedSession = null;
  try { rememberedSession = JSON.parse(localStorage.getItem('pilot-session')); } catch {}
  userIdInput.value = rememberedSession?.userId || randomId('user');
  sessionIdInput.value = rememberedSession?.sessionId || '';
  const selectedMode = analysisModeInputs.find((input) => input.checked);
  updateAnalysisMode(selectedMode ? selectedMode.value : 'standard');

  sendBtn.disabled=true;
  try { await window.PilotKnowledge.init(); }
  catch(error) { appendMessage('system',`账号会话准备失败：${error.message}。恢复服务后请刷新页面。`);checkHealth();return; }
  bindEvents();
  sendBtn.disabled=false;
  if(sessionIdInput.value) getSessionParams();
  window.PilotChat.restoreActive().then(restored => { if (!restored && rememberedSession?.userId===userIdInput.value && !state.sending) loadHistory(); });

  checkHealth();
  setInterval(checkHealth, 15000);
  setInterval(() => {
    if (panelObservability.classList.contains('active')) {
      loadOverview();
    }
  }, 30000);
}

bootstrap();
