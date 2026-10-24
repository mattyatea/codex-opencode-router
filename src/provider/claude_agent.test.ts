import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { beforeEach, test } from 'node:test';
import * as bridge from './claude_agent.ts';
import type { ControlQuery, InputItem, Payload, RunQuery } from './claude_agent.ts';

type QueryParams = Parameters<RunQuery>[0];

test('request input waits for asynchronous chunks and preserves UTF-8', async () => {
  const bytes = Buffer.from(JSON.stringify({ model: 'sonnet', input: 'こんにちは' }));
  async function* chunks() {
    for (const byte of bytes) {
      await new Promise((resolve) => setImmediate(resolve));
      yield Buffer.from([byte]);
    }
  }
  assert.deepEqual(await bridge.readPayload(chunks()), { model: 'sonnet', input: 'こんにちは' });
});

test('agent messages preserve readable content and routing context', () => {
  const [, history] = bridge.conversation({ input: [{
    type: 'agent_message', author: '/root', recipient: '/root/worker',
    content: [{ type: 'input_text', text: 'Run the task' }, { type: 'encrypted_content' }],
  }] });
  assert.deepEqual(history, [{
    role: 'user', content: 'Agent message from /root to /root/worker:\nRun the task',
  }]);
});
// Mocks yield only the message fields the bridge reads.
const mock = (fn: (params: QueryParams) => AsyncGenerator<object>) => fn as unknown as RunQuery;
const firstPrompt = async ({ prompt }: QueryParams) => (await prompt[Symbol.asyncIterator]().next()).value.message.content;

let sessionDir: string;
beforeEach(() => {
  sessionDir = fs.mkdtempSync(path.join(os.tmpdir(), 'claude-bridge-test-'));
});

const result = (extra: object = {}) => ({ type: 'result', subtype: 'success', is_error: false, session_id: 'test', usage: {}, ...extra });
const assistant = (content: object[], extra: object = {}) => ({ type: 'assistant', parent_tool_use_id: null, message: { content, stop_reason: null }, ...extra });
const statePath = () => path.join(sessionDir, 'state.json');

async function captured(fn: () => Promise<unknown>) {
  const chunks: string[] = [];
  const write = process.stdout.write;
  process.stdout.write = ((chunk: unknown) => { chunks.push(String(chunk)); return true; }) as typeof process.stdout.write;
  try {
    await fn();
  } finally {
    process.stdout.write = write;
  }
  return chunks.join('').split('\n').filter(Boolean).map((line) => JSON.parse(line));
}

test('catalog uses each resolved model\'s SDK context limits', async () => {
  const limits: Record<string, { maxTokens: number; rawMaxTokens: number; autoCompactThreshold: number }> = {
    'claude-sonnet-5-5': { maxTokens: 1000000, rawMaxTokens: 1000000, autoCompactThreshold: 967000 },
    'claude-haiku-5-5': { maxTokens: 150000, rawMaxTokens: 200000, autoCompactThreshold: 117000 },
  };
  limits['claude-opus-4-6'] = limits['claude-sonnet-5-5'];
  const hidden = ['claude-haiku-4-5-20251001', 'claude-sonnet-5', 'claude-opus-5',
    'claude-fable-5', 'claude-opus-4-8', 'claude-opus-4-7', 'claude-sonnet-4-6'];
  const ids = Object.keys(limits);
  const queried: string[] = [];
  const queryFn = (({ options }: QueryParams) => ({
    supportedModels: async () => [...ids, ...hidden, ids[0]].map((resolvedModel) => ({ resolvedModel })),
    getContextUsage: async () => { queried.push(options.model!); return limits[options.model!]; },
    close() {},
  })) as unknown as ControlQuery;
  const [models] = await captured(() => bridge.discoverModels(queryFn));
  assert.deepEqual(queried, ids);
  assert.deepEqual(models.map((model: { id: string }) => model.id), ids);
  for (const model of models as { id: string; input_modalities: string[]; context_window: number; max_context_window: number; auto_compact_token_limit: number }[]) {
    const usage = limits[model.id];
    assert.deepEqual(model.input_modalities, ['text', 'image']);
    assert.equal(model.context_window, usage.maxTokens);
    assert.equal(model.max_context_window, usage.rawMaxTokens);
    assert.equal(model.auto_compact_token_limit, usage.autoCompactThreshold);
  }
  assert.equal(bridge.modelLabel('claude-sonnet-5-5'), 'CC/Sonnet 5.5');
});

test('Codex tool call and result round trip', async () => {
  const tool = { type: 'function', name: 'exec_command', parameters: { type: 'object', properties: { cmd: { type: 'string' } }, required: ['cmd'] } };
  const request: Payload = {
    model: 'sonnet', instructions: 'System instructions', input: 'Run pwd',
    tools: [tool, { type: 'custom', name: 'apply_patch', format: { type: 'grammar' } }], _session_dir: sessionDir,
  };
  const firstQuery = mock(async function* (params) {
    const { options } = params;
    assert.deepEqual(options.tools, []);
    assert.deepEqual(options.settingSources, []);
    assert.equal(options.strictMcpConfig, true);
    assert.equal(options.maxTurns, 1);
    assert.match(options.systemPrompt as string, /System instructions/);
    assert.deepEqual(await firstPrompt(params), [{ type: 'text', text: 'Run pwd' }]);
    assert.equal(options.resume, undefined);
    const decision = await options.hooks!.PreToolUse![0].hooks[0]({} as never, undefined, {} as never);
    assert.deepEqual(decision, { hookSpecificOutput: { hookEventName: 'PreToolUse', permissionDecision: 'defer' } });
    await assert.rejects(bridge.neverExecute(), /executed by Codex/);
    yield assistant([
      { type: 'tool_use', id: 'call_1', name: 'mcp__codex__' + bridge.toolAlias('exec_command'), input: { cmd: 'pwd' } },
      { type: 'tool_use', id: 'call_2', name: 'mcp__codex__' + bridge.toolAlias('apply_patch'), input: { input: '*** Begin Patch\n*** End Patch' } },
    ]);
    yield result({ usage: { input_tokens: 10, output_tokens: 5, cache_read_input_tokens: 2 } });
  });
  let events = await captured(() => bridge.run(request, firstQuery));
  assert.equal(events[0].delta.tool_calls[0].function.name, 'exec_command');
  assert.equal(events[1].custom, true);
  assert.equal(events.at(-1).usage.prompt_tokens, 12);

  const input: InputItem[] = [
    { type: 'additional_tools', role: 'developer', tools: [tool] },
    { role: 'user', content: 'Run pwd' },
    { type: 'function_call', call_id: 'call_1', name: 'exec_command', arguments: '{"cmd":"pwd"}' },
    { type: 'custom_tool_call', call_id: 'call_2', name: 'apply_patch', input: '*** Begin Patch\n*** End Patch' },
    { type: 'function_call_output', call_id: 'call_1', output: '/workspace' },
    { type: 'custom_tool_call_output', call_id: 'call_2', output: 'Done' },
  ];
  request.tools = [];
  request.input = input;
  const secondQuery = mock(async function* (params) {
    const { options } = params;
    assert.equal(options.resume, 'test');
    assert.ok('codex' in options.mcpServers!);
    assert.deepEqual(await firstPrompt(params), [
      { type: 'tool_result', tool_use_id: 'call_1', content: '/workspace' },
      { type: 'tool_result', tool_use_id: 'call_2', content: 'Done' },
    ]);
    yield { type: 'stream_event', parent_tool_use_id: null, event: { type: 'content_block_delta', delta: { type: 'text_delta', text: '/workspace' } } };
    yield assistant([{ type: 'text', text: '/workspace' }, { type: 'tool_use', id: 'call_3', name: 'exec_command', input: { cmd: 'ls' } }]);
    yield result({ subtype: 'error_max_turns', is_error: true, errors: [] });
    throw new Error('maximum turns');
  });
  events = await captured(() => bridge.run(request, secondQuery));
  assert.deepEqual(events.filter((e) => 'content' in (e.delta ?? {})).map((e) => e.delta.content), ['/workspace']);
  assert.equal(events[1].delta.tool_calls[0].function.name, 'exec_command');
  assert.equal(fs.existsSync(statePath()), false);

  input.push(
    { role: 'assistant', content: '/workspace' },
    { type: 'function_call', call_id: 'call_3', name: 'exec_command', arguments: '{"cmd":"ls"}' },
    { type: 'function_call_output', call_id: 'call_3', output: 'file.txt' },
  );
  const recovered = mock(async function* (params) {
    assert.equal(params.options.resume, undefined);
    const content = await firstPrompt(params);
    assert.match(content, /file\.txt/);
    assert.doesNotMatch(content, /No such tool/);
    yield result();
  });
  await captured(() => bridge.run(request, recovered));
});

test('additional tool redefinition and tool_choice none', async () => {
  const old = { type: 'function', name: 'lookup', parameters: { type: 'object', properties: {} } };
  const updated = { ...old, description: 'updated lookup' };
  const namespace = (tool: bridge.ToolDefinition) => ({ type: 'namespace', name: 'functions', tools: [tool] });
  const [tools, names] = bridge.codexTools([namespace(old), namespace(updated)]);
  assert.equal(tools.length, 1);
  assert.match(tools[0].description, /updated lookup/);
  assert.equal(Object.values(names)[0].namespace, 'functions');
  const inspect = mock(async function* (params) {
    assert.deepEqual(params.options.mcpServers, {});
    assert.deepEqual(await firstPrompt(params), [{ type: 'text', text: 'hello' }]);
    yield result();
  });
  await captured(() => bridge.run({ model: 'sonnet', tool_choice: 'none', input: [
    { type: 'additional_tools', role: 'developer', tools: [namespace(updated)] },
    { role: 'user', content: 'hello' },
  ] }, inspect));
});

test('images survive initial, resumed, and bootstrapped turns', async () => {
  const image = { type: 'input_image', image_url: 'data:image/png;base64,aW1hZ2U=' };
  const content = [{ type: 'input_text', text: 'Describe' }, image];
  const expected = bridge.multimodalContent(content) as object[];
  const input: InputItem[] = [{ role: 'user', content }];
  const request: Payload = { model: 'sonnet', input, _session_dir: sessionDir };
  const initial = mock(async function* (params) {
    assert.equal(params.options.resume, undefined);
    assert.deepEqual(await firstPrompt(params), expected);
    yield assistant([{ type: 'text', text: 'Seen' }]);
    yield result();
  });
  await captured(() => bridge.run(request, initial));
  input.push({ role: 'assistant', content: 'Seen' }, { role: 'user', content });
  const resumed = mock(async function* (params) {
    assert.equal(params.options.resume, 'test');
    assert.deepEqual(await firstPrompt(params), expected);
    yield result();
  });
  await captured(() => bridge.run(request, resumed));
  const [, history] = bridge.conversation(request);
  const before = bridge.historyHashes(history);
  const blocks = bridge.promptContent(history, true) as { type: string; text?: string }[];
  assert.deepEqual(blocks.filter((b) => b.type === 'image'), [expected[1], expected[1]]);
  assert.doesNotMatch(blocks[0].text!, /aW1hZ2U=/);
  assert.deepEqual(bridge.historyHashes(history), before);
  const [, results] = bridge.conversation({ input: [{ type: 'function_call_output', call_id: 'image-call', output: content }] });
  assert.deepEqual(bridge.promptContent(results), [{ type: 'tool_result', tool_use_id: 'image-call', content: expected }]);
});

test('tool names resolve only available, unambiguous tools', () => {
  const tool = { type: 'function', name: 'exec_command' };
  const [, names] = bridge.codexTools([{ type: 'namespace', name: 'functions', tools: [tool] }]);
  for (const name of ['exec_command', 'functions.exec_command', 'mcp__codex__' + bridge.toolAlias('exec_command', 'functions')]) {
    assert.equal(bridge.resolveTool(name, names).namespace, 'functions');
  }
  const [, ambiguous] = bridge.codexTools(['functions', 'other'].map((name) => ({ type: 'namespace', name, tools: [tool] })));
  for (const [name, available] of [['exec_command', ambiguous], ['missing', names], ['exec_command', {}]] as const) {
    assert.throws(() => bridge.resolveTool(name, available), /unavailable or ambiguous/);
  }
  assert.equal(bridge.resolveTool('other.exec_command', ambiguous).namespace, 'other');
});

test('rewritten history and failed turns do not resume', async () => {
  const request: Payload = { model: 'claude-sonnet-5-5', input: 'hello', _session_dir: sessionDir };
  const initial = mock(async function* () {
    yield assistant([{ type: 'text', text: 'Hi' }]);
    yield result();
  });
  await captured(() => bridge.run(request, initial));
  const continued: InputItem[] = [{ role: 'user', content: 'hello' }, { role: 'assistant', content: 'Hi' }, { role: 'user', content: 'again' }];
  for (const changed of [{ input: 'rewritten' }, { instructions: 'new system' },
    { input: [...continued, { role: 'assistant', content: 'Other model answer' }, { role: 'user', content: 'continue' }] }]) {
    const candidate = { ...request, input: continued, ...changed };
    const [system, history] = bridge.conversation(candidate);
    // Restore the saved state because opening a session invalidates it.
    const saved = fs.readFileSync(statePath());
    assert.equal(bridge.openSession(candidate, system, history).resume, null);
    fs.writeFileSync(statePath(), saved);
  }
  request.input = continued;
  const failed = mock(async function* (params) {
    assert.equal(params.options.resume, 'test');
    throw new Error('cancelled upstream');
  });
  await assert.rejects(captured(() => bridge.run(request, failed)), /cancelled upstream/);
  const [system, history] = bridge.conversation(request);
  assert.equal(bridge.openSession(request, system, history).resume, null);
});

test('errors do not become successful responses', async () => {
  const failedQuery = mock(async function* () {
    yield assistant([{ type: 'text', text: 'Not logged in' }], { error: 'authentication_failed' });
  });
  await assert.rejects(captured(() => bridge.run({ model: 'sonnet', input: 'hello' }, failedQuery)),
    /authentication_failed: run .*codex-go-router claude login/);
  assert.throws(() => bridge.conversation({ input: [{ role: 'user', content: [{ type: 'input_image', image_url: 'example' }] }] }), /images require/);
});

test('deferred Codex tool calls complete even when the SDK sets is_error', async () => {
  const tool = { type: 'function', name: 'probe', parameters: { type: 'object', properties: {} } };
  const deferredQuery = mock(async function* () {
    yield assistant([{ type: 'tool_use', id: 'call_1', name: 'mcp__codex__' + bridge.toolAlias('probe'), input: {} }]);
    yield result({ is_error: true, result: '', stop_reason: 'tool_deferred', terminal_reason: 'tool_deferred' });
  });
  const events = await captured(() => bridge.run({ model: 'sonnet', input: 'probe', tools: [tool] }, deferredQuery));
  assert.equal(events.at(-1).type, 'complete');

  for (const terminal of ['tool_deferred', 'api_error']) {
    const failedQuery = mock(async function* () {
      yield result({ is_error: true, result: '', stop_reason: terminal, terminal_reason: terminal });
    });
    await assert.rejects(captured(() => bridge.run({ model: 'sonnet', input: 'probe' }, failedQuery)),
      new RegExp('Claude SDK request failed: ' + terminal));
  }
});

test('unavailable deferred resume rebuilds once from Codex tool results', async () => {
  const tool = { type: 'function', name: 'probe', parameters: { type: 'object', properties: {} } };
  const request: Payload = { model: 'sonnet', input: 'probe', tools: [tool], _session_dir: sessionDir };
  await captured(() => bridge.run(request, mock(async function* () {
    yield assistant([{ type: 'tool_use', id: 'call_1', name: 'mcp__codex__' + bridge.toolAlias('probe'), input: {} }]);
    yield result({ stop_reason: 'tool_deferred', terminal_reason: 'tool_deferred' });
  })));
  request.input = [
    { role: 'user', content: 'probe' },
    { type: 'function_call', name: 'probe', call_id: 'call_1', arguments: '{}' },
    { type: 'function_call_output', call_id: 'call_1', output: 'already executed' },
  ];
  let attempts = 0;
  const resumeQuery = mock(async function* (params) {
    attempts++;
    if (attempts === 1) {
      assert.equal(params.options.resume, 'test');
      yield result({ is_error: true, result: '', terminal_reason: 'tool_deferred_unavailable' });
    } else {
      assert.equal(params.options.resume, undefined);
      assert.match(await firstPrompt(params), /already executed/);
      yield assistant([{ type: 'text', text: 'OK' }]);
      yield result();
    }
  });
  const events = await captured(() => bridge.run(request, resumeQuery));
  assert.equal(attempts, 2);
  assert.equal(events.filter((event) => event.type === 'complete').length, 1);
  assert.equal(events[0].delta.content, 'OK');
});

test('subscription environment and namespaced schema', async () => {
  const [tools, names] = bridge.codexTools([{ type: 'namespace', name: 'functions', tools: [
    { type: 'function', name: 'exec', parameters: { type: 'object', properties: { code: { type: 'string' } } } },
  ] }]);
  assert.deepEqual((tools[0].inputSchema as { properties: object }).properties, { code: { type: 'string' } });
  assert.equal(names['mcp__codex__' + bridge.toolAlias('exec', 'functions')].namespace, 'functions');
  process.env.ANTHROPIC_API_KEY = 'unused';
  process.env.ANTHROPIC_BASE_URL = 'unused';
  try {
    const inspectEnvironment = mock(async function* ({ options }) {
      assert.equal(options.env!.ANTHROPIC_API_KEY, undefined);
      assert.equal(options.env!.ANTHROPIC_BASE_URL, undefined);
      yield result();
    });
    await captured(() => bridge.run({ model: 'sonnet', input: 'hello' }, inspectEnvironment));
  } finally {
    delete process.env.ANTHROPIC_API_KEY;
    delete process.env.ANTHROPIC_BASE_URL;
  }
});
