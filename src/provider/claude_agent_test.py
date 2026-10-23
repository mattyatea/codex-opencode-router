import contextlib
import importlib.util
import io
import json
import pathlib
import tempfile
import unittest
from unittest.mock import patch

from claude_agent_sdk import AssistantMessage, ResultMessage, StreamEvent, TextBlock, ToolUseBlock

spec = importlib.util.spec_from_file_location("bridge", pathlib.Path(__file__).with_name("claude_agent.py"))
bridge = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bridge)


def result(**kwargs):
    return ResultMessage(subtype="success", duration_ms=1, duration_api_ms=1,
                         is_error=False, num_turns=1, session_id="test", **kwargs)


class ClaudeBridgeTest(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.session_root = directory.name

    async def test_catalog_uses_each_resolved_models_sdk_context_limits(self):
        limits = {
            "claude-sonnet-5-5": {"maxTokens": 1000000, "rawMaxTokens": 1000000, "autoCompactThreshold": 967000},
            "claude-haiku-5-5": {"maxTokens": 150000, "rawMaxTokens": 200000, "autoCompactThreshold": 117000},
        }
        limits["claude-opus-4-6"] = limits["claude-sonnet-5-5"]
        hidden = ["claude-haiku-4-5-20251001", "claude-sonnet-5", "claude-opus-5",
                  "claude-fable-5", "claude-opus-4-8", "claude-opus-4-7", "claude-sonnet-4-6"]
        queried = []

        class Client:
            def __init__(self, *, options):
                self.model = options.model

            async def __aenter__(self):
                return self

            async def __aexit__(self, *args):
                pass

            async def get_server_info(self):
                return {"models": [{"resolvedModel": model} for model in [*limits, *hidden, next(iter(limits))]]}

            async def get_context_usage(self):
                queried.append(self.model)
                return limits[self.model]

        output = io.StringIO()
        with patch.object(bridge, "ClaudeSDKClient", Client), contextlib.redirect_stdout(output):
            await bridge.discover_models()
        models = json.loads(output.getvalue())
        self.assertEqual(queried, list(limits))
        self.assertEqual([model["id"] for model in models], list(limits))
        for model in models:
            usage = limits[model["id"]]
            self.assertEqual(model["context_window"], usage["maxTokens"])
            self.assertEqual(model["max_context_window"], usage["rawMaxTokens"])
            self.assertEqual(model["auto_compact_token_limit"], usage["autoCompactThreshold"])

    async def test_codex_tool_call_and_result_round_trip(self):
        tool = {"type": "function", "name": "exec_command", "parameters": {
            "type": "object", "properties": {"cmd": {"type": "string"}}, "required": ["cmd"]}}
        request = {"model": "sonnet", "instructions": "System instructions", "input": "Run pwd",
                   "tools": [tool, {"type": "custom", "name": "apply_patch", "format": {"type": "grammar"}}],
                   "_thread_id": "thread-1", "_session_root": self.session_root}

        async def first_query(*, prompt, options):
            self.assertEqual(options.tools, [])
            self.assertEqual(options.setting_sources, [])
            self.assertTrue(options.strict_mcp_config)
            self.assertEqual(options.max_turns, 1)
            self.assertIn("System instructions", options.system_prompt)
            self.assertEqual((await anext(prompt))["message"]["content"], [{"type": "text", "text": "Run pwd"}])
            self.assertIsNone(options.resume)
            decision = await options.hooks["PreToolUse"][0].hooks[0]({}, None, {})
            self.assertEqual(decision["hookSpecificOutput"]["permissionDecision"], "defer")
            with self.assertRaisesRegex(RuntimeError, "executed by Codex"):
                await bridge.never_execute({})
            yield AssistantMessage(model="sonnet", content=[
                ToolUseBlock(id="call_1", name="mcp__codex__" + bridge.tool_alias("exec_command"), input={"cmd": "pwd"}),
                ToolUseBlock(id="call_2", name="mcp__codex__" + bridge.tool_alias("apply_patch"), input={"input": "*** Begin Patch\n*** End Patch"}),
            ])
            yield result(usage={"input_tokens": 10, "output_tokens": 5, "cache_read_input_tokens": 2})

        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            await bridge.run(request, query_fn=first_query)
        events = [json.loads(line) for line in output.getvalue().splitlines()]
        self.assertEqual(events[0]["delta"]["tool_calls"][0]["function"]["name"], "exec_command")
        self.assertTrue(events[1]["custom"])
        self.assertEqual(events[-1]["usage"]["prompt_tokens"], 12)
        request["tools"] = []
        request["input"] = [
            {"type": "additional_tools", "role": "developer", "tools": [tool]},
            {"role": "user", "content": "Run pwd"},
            {"type": "function_call", "call_id": "call_1", "name": "exec_command", "arguments": '{"cmd":"pwd"}'},
            {"type": "custom_tool_call", "call_id": "call_2", "name": "apply_patch", "input": "*** Begin Patch\n*** End Patch"},
            {"type": "function_call_output", "call_id": "call_1", "output": "/workspace"},
            {"type": "custom_tool_call_output", "call_id": "call_2", "output": "Done"},
        ]

        async def second_query(*, prompt, options):
            self.assertEqual(options.resume, "test")
            self.assertIn("codex", options.mcp_servers)
            blocks = (await anext(prompt))["message"]["content"]
            self.assertEqual(blocks, [
                {"type": "tool_result", "tool_use_id": "call_1", "content": "/workspace"},
                {"type": "tool_result", "tool_use_id": "call_2", "content": "Done"},
            ])
            yield StreamEvent(uuid="event", session_id="test", event={"type": "content_block_delta", "delta": {"type": "text_delta", "text": "/workspace"}})
            yield AssistantMessage(model="sonnet", content=[
                TextBlock(text="/workspace"),
                ToolUseBlock(id="call_3", name="mcp__codex__" + bridge.tool_alias("exec_command"), input={"cmd": "ls"}),
            ])
            yield result()

        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            await bridge.run(request, query_fn=second_query)
        events = [json.loads(line) for line in output.getvalue().splitlines()]
        self.assertEqual([e["delta"]["content"] for e in events if "content" in e.get("delta", {})], ["/workspace"])
        self.assertEqual(events[1]["delta"]["tool_calls"][0]["function"]["name"], "exec_command")

    async def test_additional_tool_redefinition_and_tool_choice_none(self):
        old = {"type": "function", "name": "lookup", "parameters": {"type": "object", "properties": {}}}
        new = {**old, "description": "updated lookup"}
        namespace = lambda tool: {"type": "namespace", "name": "functions", "tools": [tool]}
        tools, names = bridge.codex_tools([namespace(old), namespace(new)])
        self.assertEqual(len(tools), 1)
        self.assertIn("updated lookup", tools[0].description)
        self.assertEqual(next(iter(names.values()))["namespace"], "functions")

        async def inspect(*, prompt, options):
            self.assertEqual(options.mcp_servers, {})
            self.assertEqual((await anext(prompt))["message"]["content"], [{"type": "text", "text": "hello"}])
            yield result()

        with contextlib.redirect_stdout(io.StringIO()):
            await bridge.run({"model": "sonnet", "tool_choice": "none", "input": [
                {"type": "additional_tools", "role": "developer", "tools": [namespace(new)]},
                {"role": "user", "content": "hello"},
            ]}, query_fn=inspect)

    async def test_session_isolation_rewind_and_failed_turn(self):
        request = {"model": "claude-sonnet-5-5", "input": "hello", "_thread_id": "one", "_session_root": self.session_root}
        async def initial(**kwargs):
            yield AssistantMessage(model=request["model"], content=[TextBlock(text="Hi")])
            yield result()
        with contextlib.redirect_stdout(io.StringIO()):
            await bridge.run(request, query_fn=initial)
        continued = [{"role": "user", "content": "hello"}, {"role": "assistant", "content": "Hi"}, {"role": "user", "content": "again"}]
        for changed in ({"_thread_id": "two"}, {"model": "claude-opus-5-5"}, {"input": "rewritten"}, {"instructions": "new system"},
                        {"input": continued + [{"role": "assistant", "content": "Other model answer"}, {"role": "user", "content": "continue"}]}):
            candidate = {**request, "input": continued, **changed}
            system, history = bridge.conversation(candidate)
            # Restore the original metadata because entering a request invalidates it.
            saved = {p: p.read_bytes() for p in pathlib.Path(self.session_root).rglob("state.json")}
            with bridge.session_state(candidate, system, history) as (_, resume, _, _):
                self.assertIsNone(resume)
            for path, data in saved.items():
                path.write_bytes(data)
        request["input"] = continued
        async def failed(*, prompt, options):
            self.assertEqual(options.resume, "test")
            raise RuntimeError("cancelled upstream")
            yield
        with self.assertRaisesRegex(RuntimeError, "cancelled upstream"):
            await bridge.run(request, query_fn=failed)
        system, history = bridge.conversation(request)
        with bridge.session_state(request, system, history) as (_, resume, _, _):
            self.assertIsNone(resume)

    async def test_errors_do_not_become_successful_responses(self):
        async def failed_query(**kwargs):
            yield AssistantMessage(model="sonnet", content=[TextBlock(text="Not logged in")], error="authentication_failed")

        with self.assertRaisesRegex(RuntimeError, "authentication_failed: run .*codex-go-router claude login"):
            await bridge.run({"model": "sonnet", "input": "hello"}, query_fn=failed_query)
        with self.assertRaisesRegex(ValueError, "text input only"):
            bridge.conversation({"input": [{"role": "user", "content": [{"type": "input_image", "image_url": "example"}]}]})

    async def test_subscription_environment_and_namespaced_schema(self):
        tools, names = bridge.codex_tools([{"type": "namespace", "name": "functions", "tools": [
            {"type": "function", "name": "exec", "parameters": {"type": "object", "properties": {"code": {"type": "string"}}}}
        ]}])
        self.assertEqual(tools[0].input_schema["properties"]["code"], {"type": "string"})
        self.assertEqual(names["mcp__codex__" + bridge.tool_alias("exec", "functions")]["namespace"], "functions")

        async def inspect_environment(**kwargs):
            import os
            self.assertNotIn("ANTHROPIC_API_KEY", os.environ)
            self.assertNotIn("ANTHROPIC_BASE_URL", os.environ)
            yield result()

        with patch.dict("os.environ", {"ANTHROPIC_API_KEY": "unused", "ANTHROPIC_BASE_URL": "unused"}):
            with contextlib.redirect_stdout(io.StringIO()):
                await bridge.run({"model": "sonnet", "input": "hello"}, query_fn=inspect_environment)


if __name__ == "__main__":
    unittest.main()
