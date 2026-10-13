import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { runInNewContext } from 'node:vm';
import { File } from 'node:buffer';

const html = readFileSync(new URL('./index.html', import.meta.url), 'utf8');
const js = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const css = readFileSync(new URL('./styles.css', import.meta.url), 'utf8');
const markdownHelpers = js.slice(js.indexOf('function escapeHtml('), js.indexOf('function setMessageContent('));
const markedSource = readFileSync(new URL('./vendor/marked-18.1.0.umd.js', import.meta.url), 'utf8');

test('knowledge upload sends a Markdown file or preserves pasted code indentation', async () => {
  for (const useFile of [false, true]) {
    const file = new File(['# Guide\n'], 'guide.md', { type: 'text/markdown' });
    let request, resets = 0;
    const submit = { disabled: false, textContent: '' };
    const context = {
      FormData,
      docTitleInput: { value: 'Guide' }, docCategoryInput: { value: 'ops' },
      docContentInput: { value: '    SELECT 1;\n', required: !useFile },
      document: { getElementById: () => ({ files: useFile ? [file] : [] }) },
      uploadForm: { querySelector: () => submit, reset: () => { resets++; } },
      showToast() {}, getErrorMessage() { return 'failed'; },
      async fetchJson(_url, options) { request = options; return { response: { ok: true }, data: { success: true, data: { chunk_count: 2 } } }; },
    };
    runInNewContext(js.slice(js.indexOf('async function handleUpload('), js.indexOf('async function handleSearch(')), context);
    await context.handleUpload({ preventDefault() {} });
    if (useFile) {
      assert.equal(request.body.get('file').name, 'guide.md');
      assert.equal(request.headers, undefined); // Browser must generate the multipart boundary.
    } else {
      assert.equal(JSON.parse(request.body).content, '    SELECT 1;\n');
    }
    assert.equal(resets, 1);
    assert.equal(submit.disabled, false);
    assert.equal(context.docContentInput.required, true);
  }
});

function markdownContext(useMarked = false) {
  const context = { window: {} };
  if (useMarked) {
    runInNewContext(markedSource, context);
    context.window.marked = context.marked;
    // Actual DOM sanitization is checked in the browser; here inspect parser output.
    context.window.DOMPurify = { sanitize: (value) => value };
  }
  runInNewContext(markdownHelpers, context);
  return context;
}

test('Markdown preserves headings, inline symbols, and code before parsing', () => {
  const context = markdownContext();
  const input = '## 结论\n\n- **持续性的慢 + 偶发抖动式 502**，10–24s。\n\n## 证据\n\n1. 错误日志\n\n```python\n## comment\na = 1 + 2\n```';
  assert.equal(context.normalizeMarkdownText(input), input);
  assert.equal(context.normalizeMarkdownText('a\r\nb\rc'), 'a\nb\nc');
  for (const renderer of [context, markdownContext(true)]) {
    const output = renderer.renderMarkdown(input);
    assert.match(output, /<h2>结论<\/h2>/);
    assert.match(output, /<h2>证据<\/h2>/);
    assert.match(output, /<strong>持续性的慢 \+ 偶发抖动式 502<\/strong>/);
    assert.match(output, /## comment\na = 1 \+ 2/);
    assert.equal((output.match(/<li>/g) || []).length, 2);
    assert.doesNotMatch(output, /<li>\s*<\/li>/);
  }
});

test('local Markdown parser renders tables and multiline emphasis', () => {
  const context = markdownContext(true);
  const output = context.renderMarkdown('**持续性的慢\n以及偶发 502**\n\n| 指标 | 状态 |\n| --- | --- |\n| 延迟 | 偏高 |');
  assert.match(output, /<strong>持续性的慢<br>以及偶发 502<\/strong>/);
  assert.match(output, /<table>/);
  assert.match(output, /<th>指标<\/th>/);
  assert.match(output, /<td>偏高<\/td>/);
  assert.doesNotMatch(html, /cdn\.jsdelivr\.net/);
  assert.match(html, /src="\.\/vendor\/marked-18\.1\.0\.umd\.js"/);
  assert.match(html, /src="\.\/vendor\/dompurify-3\.4\.16\.min\.js"/);
});

test('fallback does not mistake emphasis for a list and escapes HTML without sanitizer', () => {
  const context = markdownContext(true);
  delete context.window.DOMPurify;
  const output = context.renderMarkdown('**完整强调**\n\n<img src=x onerror=alert(1)>');
  assert.match(output, /<strong>完整强调<\/strong>/);
  assert.doesNotMatch(output, /<li>|<img/);
  assert.match(output, /&lt;img/);
});

test('stream rendering batches chunks and flushes the final text immediately', () => {
  const frames = new Map();
  const rendered = [];
  let nextId = 0;
  const context = {
    requestAnimationFrame(callback) { frames.set(++nextId, callback); return nextId; },
    cancelAnimationFrame(id) { frames.delete(id); },
    setMessageContent(_node, text) { rendered.push(text); },
  };
  runInNewContext(js.slice(js.indexOf('function createStreamRenderer('), js.indexOf('function showToast(')), context);
  const renderer = context.createStreamRenderer({});
  renderer.schedule('第');
  renderer.schedule('第一段');
  assert.equal(frames.size, 1);
  assert.deepEqual(rendered, []);
  [...frames.values()][0]();
  assert.deepEqual(rendered, ['第一段']);
  renderer.schedule('第一段，最后一段');
  renderer.flush();
  assert.equal(frames.size, 0);
  assert.deepEqual(rendered, ['第一段', '第一段，最后一段']);
  renderer.flush();
  assert.equal(rendered.length, 2);
});

test('HITL and feedback use persistent accessible controls', () => {
  assert.match(html, /id="hitlApproval"[^>]*aria-live="polite"/);
  assert.match(html, /id="hitlApproveBtn"/);
  assert.match(html, /id="hitlRejectBtn"/);
  assert.match(js, /function attachFeedbackControls/);
  assert.match(js, /\/api\/v1\/chat\/feedback/);
  assert.doesNotMatch(js, /window\.(confirm|prompt)\s*\(/);
});

test('log controls share the complete backend vocabulary and summary state', () => {
  for (const level of ['all', 'fatal', 'error', 'warn', 'info', 'debug']) {
    assert.match(html, new RegExp(`<option value="${level}"`));
  }
  assert.match(js, /original_count/);
  assert.match(js, /omitted_count/);
  assert.match(js, /summary\.truncated/);
  for (const level of ['debug', 'info', 'warn', 'error', 'fatal']) {
    assert.match(css, new RegExp(`\\.log-${level}\\s*\\{`));
  }
});

test('responsive and keyboard accessibility invariants are declared once', () => {
  assert.equal((css.match(/:root\s*\{/g) || []).length, 1);
  assert.match(css, /:focus-visible/);
  assert.match(css, /@media \(max-width: 640px\)/);
  assert.match(css, /overflow-x:\s*hidden/);
  assert.match(html, /id="toast"[^>]*role="status"[^>]*aria-live="polite"/);
  assert.match(html, /id="chatMessages"[^>]*aria-live="polite"/);
});
