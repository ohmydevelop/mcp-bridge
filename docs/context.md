# 上下文与实测

通过 `bridge_save_tool` 发布 Python 工具，传入 `name`、`description`、`input_schema`、`uses:["qa/failure_summary"]` 和下列 `script`。脚本只返回五个 ID。`dry_run:true` 校验语法、入口和依赖。

```python
def run(args, ctx):
    rows = ctx.call_tool("qa/failure_summary", {"limit": 1000})["rows"]
    return [{"id": r["id"]} for r in rows[:5]]
```

每次执行启动独立的 Linux 用户、网络与 PID 命名空间，仅挂载只读 Python 运行库和临时目录。父进程通过管道代理 `uses` 中的调用并检查当前用户权限。一个脚本最多运行 30 秒、占用 256 MiB 地址空间、调用 50 次上游，最终结果上限 1 MiB。运行和预检需要 `bwrap`、支持非特权用户命名空间的内核、`/usr/bin/python3` 和 `libseccomp.so.2`。预检只解析脚本，不执行用户代码。部署不支持隔离时会返回错误。

```sh
make check         # vet、race、两个 Bridge 互联、旧客户端、恢复与幂等投递
make check-app     # 需要 Chromium，使用本地 MCP Apps 测试宿主
make capture       # 需要 Codex CLI，使用隔离 HOME 和回环模型端点
make release
(cd dist && sha256sum -c SHA256SUMS)
```

`bin/trace/request-1.json` 包含实际 CLI 发出的第一轮模型请求和工具定义；`request-2.json` 包含 `function_call_output`。测试端点返回确定性的工具调用，因而这项测试测量的是上下文构建与序列化，并未测量模型选择工具的质量。

### 一次 Codex 会话如何构建请求

源码定位在 Codex 主分支的 `codex-rs/core/src/session/mod.rs` 中的 `build_initial_context_with_world_state`，以及 `codex-rs/core/src/client.rs` 中的 `build_responses_request`。后者从 Prompt 取格式化历史，并依模型配置生成工具描述。下面对应此次普通 Responses 请求；Responses Lite 在该版本源码中另有 `AdditionalTools` 分支。

| 步骤 | 线上消息的位置 | 本次捕获 |
|---|---|---|
| 装配提示词与 Skill 目录 | `input[]` 内带 role 的 message | 首轮含 developer 指令、用户输入及可用 Skill 信息。完整的上游 QA Skill 通过显式工具调用按需读取。 |
| 注册可用工具 | 顶层 `tools[]` | shell 是 `function/exec_command`；Bridge 是名为 `mcp__bridge` 的 namespace，其中包含 `call_tool` 的参数定义。测试配置只向模型暴露该 Bridge 入口。 |
| 发起调用 | `function_call` | 本地模型端点指定 namespace、name、call_id 和 JSON 字符串 arguments。Codex 执行 MCP `tools/call`。 |
| 返回数据并再次请求 | 后续 `input[]` | `function_call_output.call_id` 与调用匹配。输出含 CLI 包装文本和投影后的 JSON。区别上下文成本时应分别看工具目录和每次返回体。 |

对照样本位于相邻的 `session-trace/`，其中的原始 MCP JSON-RPC、模型的两次请求及 SSE 响应可以按调用顺序对读。当前主分支快照是 `af7194230970`，实际会话采集使用已安装的 Codex CLI 0.166.0。

### 工具路由对照

基线使用提交 `3cb306f`。两个版本各连接同一 QA 测试服务，导出管理员实际看到的工具目录。Codex 使用 `gpt-6.2-sol` 对每条中英文请求各调用一次，记录首次工具名并校验其入参 schema。旧版的原生 QA 工具和读取 Skill 文件的专用工具都计为有效入口。

| 一次采样 | 基线 | 重构后 |
|---|---:|---:|
| 管理员工具目录（含 QA） | 46 | 17 |
| 首次选中可处理请求的工具 | 8/8 | 8/8 |
| 首次调用参数通过对应 schema | 8/8 | 8/8 |
| 搜索结果前三项包含目标工具 | 3/4 | 4/4 |

四条检索词为“失败用例”、`failure_summary`、`qa/failure_summary` 和 `QA failure`。新版 QA 来源配置了中文别名。此样本显示目录缩小后覆盖了相同意图，中文别名补齐了一条检索。样本不足以推断总体命中率变化。首次调用通过回放服务器记录，实际执行结果由集成测试与请求捕获单独验证。

复测命令（Python 依赖 `fastjsonschema`，Codex 需可访问模型）：

```sh
make build
python3 tests/routing_eval.py /path/to/baseline/mcp-bridge --model gpt-6.2-sol --output bin/trace/routing-eval.json
```
