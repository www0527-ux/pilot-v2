import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {runInNewContext} from 'node:vm';
import test from 'node:test';
const stateSource = readFileSync(new URL('./agent-session.js',import.meta.url),'utf8');
const transport = readFileSync(new URL('./agent-chat.js',import.meta.url),'utf8');
const app = readFileSync(new URL('./app.js',import.meta.url),'utf8');
function context() {
  const ctx={window:{},TextDecoder};
  runInNewContext(stateSource,ctx);
  const helpers = app.slice(app.indexOf('function parseJsonSafe('), app.indexOf('function escapeHtml('))
    + app.slice(app.indexOf('function extractSSEPayload('), app.indexOf('async function handleSend('))
    + app.slice(app.indexOf('function getErrorMessage('), app.indexOf('function createPendingAssistant('));
  const readFunction = transport.slice(transport.indexOf('async function read('), transport.indexOf('async function consume('));
  runInNewContext(helpers + readFunction, ctx);
  return ctx;
}
function session(change) {const ctx=context();return new ctx.window.PilotAgentSession('run',change)}
const event=(sequence,kind,status,extra={})=>({run_id:'run',sequence,kind,status,...extra});
function answer(run,start=1) {
  run.accept(event(start,'answer','started'));run.accept(event(start+1,'answer','delta',{text:'## 结论\n\n正常。'}));
  run.accept(event(start+2,'answer','ended'));run.accept(event(start+3,'run','completed'));
}
test('no-tool answers enter a declared phase and complete only after answer end',()=>{
  const run=session();run.accept(event(1,'answer','started'));assert.equal(run.phase,'final_streaming');
  run.accept(event(2,'answer','delta',{text:'首块'}));assert.equal(run.text,'首块');assert.equal(run.terminal,false);
  run.accept(event(3,'answer','ended'));assert.equal(run.terminal,false);
  run.accept(event(4,'run','completed'));assert.equal(run.phase,'completed');
});
for(const mode of ['single tool','serial tools','parallel tools','partial failure','retry','multiple rounds']) {
  test(`${mode}: execution never appears in answer text`,()=>{
    const run=session();let seq=1;
    const count=mode==='single tool'?1:3;
    for(let i=0;i<count;i++)run.accept(event(seq++,'tool','running',{id:`t${i}`,name:'query_logs',summary:'内部执行说明',round:i+1}));
    for(let i=count-1;i>=0;i--)run.accept(event(seq++,'tool',i===0&&mode==='partial failure'?'failed':'completed',{id:`t${i}`,name:'query_logs',summary:'工具结果'}));
    if(mode==='retry'){run.accept(event(seq++,'tool','failed',{id:'retry1',name:'query_logs'}));run.accept(event(seq++,'tool','completed',{id:'retry2',name:'query_logs'}));}
    assert.equal(run.text,'');assert.equal(run.phase,'executing');assert.equal(run.terminal,false);
    run.accept(event(seq++,'model','running',{id:'m',summary:'继续分析'}));assert.equal(run.phase,'executing');
    answer(run,seq);assert.equal(run.text,'## 结论\n\n正常。');
  });
}
test('out-of-order first delta waits for answer start, and replay cannot duplicate text',()=>{
  const run=session();const first=event(2,'answer','delta',{text:'你好'});
  run.accept(first);assert.equal(run.text,'');run.accept(event(1,'answer','started'));assert.equal(run.text,'你好');
  run.accept(first);assert.equal(run.text,'你好');run.accept(event(4,'run','completed'));assert.equal(run.terminal,false);
  run.accept(event(3,'answer','ended'));assert.equal(run.phase,'completed');
});
test('early success and unclassified final deltas are protocol errors',()=>{
  assert.throws(()=>session().accept(event(1,'run','completed')),/回答流结束/);
  assert.throws(()=>session().accept(event(1,'answer','delta',{text:'bad'})),/缺少开始/);
});
for(const phase of ['executing','final_streaming'])for(const status of ['failed','cancelled']) {
  test(`${status} during ${phase} is terminal and cannot be overwritten`,()=>{
    const run=session();let seq=1;if(phase==='final_streaming'){run.accept(event(seq++,'answer','started'));run.accept(event(seq++,'answer','delta',{text:'已接收'}));}
    run.accept(event(seq++,'run',status));const before=run.text;
    run.accept(event(seq++,'answer','delta',{text:'迟到'}));run.accept(event(seq++,'run','completed'));
    assert.equal(run.phase,status);assert.equal(run.text,before);
  });
}
test('explicitly returning to tools preserves existing answer and later streams append',()=>{
  const run=session();run.accept(event(1,'answer','started'));run.accept(event(2,'answer','delta',{text:'第一部分'}));
  run.accept(event(3,'run','executing'));assert.equal(run.text,'第一部分');run.accept(event(4,'tool','running',{id:'t'}));
  run.accept(event(5,'answer','started'));run.accept(event(6,'answer','delta',{text:'，第二部分'}));
  run.accept(event(7,'answer','ended'));run.accept(event(8,'run','completed'));assert.equal(run.text,'第一部分，第二部分');
});
test('approval interruption has its own terminal state and no manufactured answer',()=>{
  const run=session();run.accept(event(1,'result','interrupted',{result:{metadata:{checkpoint_id:'checkpoint'}}}));
  run.accept(event(2,'run','interrupted'));assert.equal(run.phase,'interrupted');assert.equal(run.text,'');assert.equal(run.result.metadata.checkpoint_id,'checkpoint');
});
test('history reconstruction has identical process events and answer without executing tools',()=>{
  const first=session();first.accept(event(1,'tool','completed',{id:'t',name:'query_logs'}));answer(first,2);
  const restored=session();first.events.forEach(e=>restored.accept(e));assert.equal(restored.text,first.text);assert.equal(restored.phase,'completed');assert.equal(restored.events.length,first.events.length);
});
test('SSE decoder accepts split UTF-8, CRLF and lifecycle plus first delta in one packet',async()=>{
  const ctx=context(),run=new ctx.window.PilotAgentSession('run');
  const events=[event(1,'tool','completed',{id:'t',summary:'不可进入正文'}),event(2,'answer','started'),event(3,'answer','delta',{text:'你好，世界'}),event(4,'answer','ended'),event(5,'run','completed')];
  const raw=events.map(e=>`event: agent\r\ndata: ${JSON.stringify(e)}\r\n\r\n`).join('')+'data: [DONE]\r\n\r\n';
  const bytes=new TextEncoder().encode(raw);let i=0;
  const response={ok:true,headers:{get:()=> 'text/event-stream'},body:{getReader:()=>({async read(){return i<bytes.length?{value:bytes.slice(i,++i),done:false}:{done:true}},releaseLock(){}})}};
  await ctx.read(response,run);assert.equal(run.text,'你好，世界');assert.equal(run.phase,'completed');
});
test('a truncated SSE connection retains partial final text for replay',async()=>{
  const ctx=context(),run=new ctx.window.PilotAgentSession('run');let sent=false;
  const raw=[event(1,'answer','started'),event(2,'answer','delta',{text:'部分回答'})].map(e=>`event: agent\ndata: ${JSON.stringify(e)}\n\n`).join('');
  const response={ok:true,headers:{get:()=> 'text/event-stream'},body:{getReader:()=>({async read(){if(sent)return{done:true};sent=true;return{value:new TextEncoder().encode(raw),done:false}},releaseLock(){}})}};
  await assert.rejects(ctx.read(response,run),/连接中断/);assert.equal(run.text,'部分回答');assert.equal(run.terminal,false);
});
test('startup failures preserve the API error and HTTP fallback', async () => {
  const ctx = context();
  for (const [body, message] of [[JSON.stringify({message:'模型服务暂时不可用'}), /模型服务暂时不可用/], ['Bad Gateway', /HTTP 502/]]) {
    await assert.rejects(ctx.read({ok:false, status:502, text:async()=>body}, session()), message);
  }
});
