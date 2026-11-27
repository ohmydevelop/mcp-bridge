# 数据流与协议边界

```text
旧 Agent 的 tools/list、tools/call      支持扩展的 Agent       MCP Apps 宿主
                 │                      │                      │
                 └────────────── 身份与权限检查 ─────────────────┘
                                      │
                 业务与管理工具 / 原生扩展方法
                                      │
           source 目录、技能 manifest、Python 工具与事件流
                 │                    │                   │
         插件或直接配置的 MCP       SQLite WAL         投递 worker
        stdio / HTTP / 旧 SSE       状态与游标            │
                                                        ▼
                                               Agent 自动化 webhook
```

| 身份 | 公开入口 |
|---|---|
| 所有 Agent（业务操作） | `search_tools`、`call_tool`、`bridge_execute_script` |
| 所有 Agent（Skill 与事件） | `search_skills`、`get_skill`、`search_event_types`、`save_trigger`、`run_trigger_now`、`list_triggers`、`get_trigger`、`delete_trigger` |
| 所有 Agent（管理操作） | `bridge_create_source`、`bridge_update_source`、`bridge_delete_source`、`bridge_get_tool`、`bridge_save_tool`、`bridge_delete_tool`、`bridge_save_destination`、`bridge_delete_destination`、`bridge_get_status`、`bridge_list_sources`；以 `serve -app` 启用 `bridge_open_app` |
| 生产者（1） | `publish_event`。凭证绑定一个来源，仅可发送该来源声明的事件 |

`tools/list` 按身份返回固定目录。发现业务工具用 `search_tools`，包括上游和已发布的 `bridge.builtin/<name>` 工具。结果提供 `tool_id`、说明、输入 schema；`call_tool` 再次检查调用者对来源和工具的权限。三种搜索默认每页 5 项，将返回的非零 `next_offset` 作为下一次的 `offset`。在来源的 `aliases` 中按 `tool:名称`、`skill:名称`、`event:名称` 配置中英文别名和问法。精确限定 ID 优先，其次为名称或别名精确、部分匹配、描述匹配，同分按 ID 排序。

`bridge_create_source` 接受直接配置或本地 `package_path`。Agent Plugins 和 MCPB 包会被校验、快照并创建来源。预检返回 `schema`、`servers`、`missing`；自动选择唯一服务。使用 `bridge_get_status({"source_id":"qa"})` 读取脱敏实例详情与版本，使用 `bridge_update_source` 提交局部修改。对象字段递归合并，数组整体替换，传入 `null` 删除指定字段。无参数的 `bridge_get_status` 还返回已发布工具及 webhook 的 ID 和版本号，供更新和删除使用。后台维护来源连接，MCP App 调用相同管理工具。原生 Skills、Events 请求继续通过协议方法访问。

线协议使用 MCP 日期版本。当前原生路径以 `2026-07-28` 和各扩展名称为准，兼容测试涵盖 `2025-06-18`。实现参考 [Events beta](https://github.com/modelcontextprotocol/ext-events/tree/c77b843/specification/beta)、[Skills beta](https://github.com/modelcontextprotocol/ext-skills/tree/c0705f9/specification/beta)、[Agent Plugins](https://agent-plugins.org/) 和 [MCPB](https://github.com/modelcontextprotocol/mcpb/tree/70fe3b3)。这里的 agent 管理工具名、source ID、流程定义和自动化 webhook 信封均属于 Bridge API。正式协议全量一致性及各商业宿主兼容性尚未认证。

### 普通 CLI 转成工具

在标准 `mcp.json` 中将 command 配置为 Bridge 二进制。下面发布 `inspect`，工具调用参数以一个 JSON 对象写入 QA 程序的 stdin。固定 argv 保留参数边界；stdout 的 JSON 对象成为 structuredContent，其他文本成为 text content。超时、失败退出或超出字节上限会返回工具错误。

```json
{"$schema":"https://agent-plugins.org/schemas/1.0.0/mcp.schema.json","mcpServers":{"qa":{"type":"stdio","command":"/usr/local/bin/mcp-bridge","args":["cli","-name","inspect","-max-output","65536","--","/opt/qa/bin/qa","inspect","--json"]}}}
```
