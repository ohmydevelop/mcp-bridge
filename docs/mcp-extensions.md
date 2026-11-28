# 通过 Bridge 使用 MCP 扩展

客户端使用本机 stdio 或 Streamable HTTP 连接 Bridge，先调用 `server/discover`。Bridge 按当前账号和已连接的上游返回能力。本文以 MCP 基础协议 `2026-07-28`、Skills beta、Tasks 同版扩展，以及 Files 实验扩展的初始修订为基线。

## 能力声明

支持交互、后台任务和文件的客户端，在相关请求的 `params._meta` 中发送以下元数据。客户端应按实际支持的能力填写。在 `tools/list` 中也声明 Files，客户端才能发现带原始文件参数 schema 的工具。Bridge 转发调用时按上游能力协商。

```json
{
  "io.modelcontextprotocol/protocolVersion": "2026-07-28",
  "io.modelcontextprotocol/clientInfo": {"name": "example-client", "version": "1"},
  "io.modelcontextprotocol/clientCapabilities": {
    "elicitation": {"form": {}, "url": {}},
    "extensions": {
      "io.modelcontextprotocol/tasks": {},
      "io.modelcontextprotocol/files": {"upload": {}, "download": {}}
    }
  }
}
```

## Skills

用 `skills/list` 发现入口，用 `skills/get` 获取单个 Skill 的当前清单，再用 `resources/read` 读取所需文件。静态清单按大小和 SHA-256 校验每次读取的内容。动态 Skill 声明 `"resources":"dynamic"`，读取入口时核对 YAML frontmatter。用 `resources/directory/read` 列出直接子项，动态目录要求上游也支持该方法。

上游使用部分清单时，可以直接访问已知入口。其 Bridge URI 形如 `skill://SOURCE/upstream/ENCODED/NAME/SKILL.md`，其中 ENCODED 为上游完整入口 URI 的无填充 base64url，NAME 为 Skill 名称。每次读取都会重新检查来源权限。[Skills 规范](https://skills.extensions.modelcontextprotocol.io/specification/beta/skills)

## Tasks 与 Elicitation

调用 `call_tool` 后根据 `resultType` 决定下一步。

| resultType | 客户端操作 |
| --- | --- |
| complete | 读取工具结果及 isError |
| task | 保存 Bridge 返回的 taskId，按 pollIntervalMs 调用 tasks/get，也可以通过 subscriptions/listen 的 notifications.taskIds 订阅状态 |
| input_required | 将 inputRequests 中的表单或 URL 交给用户，以相同工具名和参数、requestState 及 inputResponses 再次调用 |
| upload_required | 完成所有指定的上传，再以相同工具名、参数和 requestState 重试一次 |

任务运行中出现 `status: input_required` 时，用 `tasks/update` 提交回答。用 `tasks/cancel` 请求取消，最终状态以查询或通知为准。HTTP 上的三个 `tasks/*` 方法都要把 taskId 放入 `Mcp-Name` 头。后台 Python 编排可等待上游任务完成；需要人工确认时会结束调用并尝试取消任务。[Tasks 规范](https://tasks.extensions.modelcontextprotocol.io/specification/2026-07-28/tasks) · [Elicitation 规范](https://modelcontextprotocol.io/specification/2026-07-28/client/elicitation)

声明 Tasks 的客户端执行 `run_trigger_now` 可直接取得任务句柄，查询最终的 CLI、终端或 webhook 投递回执。预览保留同步行为。Bridge 将任务句柄绑定到账号；保留数据目录和主密钥后可以跨 Bridge 重启继续查询。上游任务还取决于原服务保留状态，修改来源配置会使旧的上游句柄失效。

## 文件

客户端按工具 schema 的 `x-mcp-file` 选择文件。小文件可在 `maxInlineBytes` 总预算内用 base64 内联。Bridge 公布可见文件上游共同支持的上限，最多 4 MiB。带外传输先发送文件元数据，再按目标发送原始字节。下载目标位于最终结果的 `_meta["io.modelcontextprotocol/files"].downloads`。

- 上游返回带签名的 HTTPS URL 且使用 `credentials: omit` 时，客户端直接访问存储。
- 上游返回相对路径或同源授权目标时，HTTP 客户端会取得 Bridge 自己的相对路径。在默认入口下，字节请求发送到 `/mcp/files/{token}` 并附当前账号的 Bridge 凭证。Bridge 使用来源配置的 Authorization 访问需要鉴权的上游目标。
- 本机 stdio 客户端要使用这类中继目标，部署时设置 `BRIDGE_PUBLIC_URL` 为可访问的 HTTPS 地址，并将 `/mcp-file-link/{token}` 路由到同一 daemon。客户端使用返回的短期签名链接。纯 stdio 上游需自行提供签名 HTTPS 目标。
- 中继完整传输时在数据目录的 `transfers/` 中暂存字节，检查大小和声明的摘要，处理结束后删除。启动时清理中断留下的临时文件。单次中继传输上限为 1 TiB，实际可用空间取决于部署。上传的最终入库、操作幂等和上游任务保留由提供该工具的标准 MCP Server 负责。

Files 仍为实验扩展。Claude 托管客户端还对连接器目录和传输地址有单独要求。[Files 草案](https://github.com/modelcontextprotocol/experimental-ext-file-uploads/blob/main/specification/draft/file-uploads.md) · [Claude 文件对接条件](https://claude.com/docs/connectors/building/files)

## 上游 OAuth

使用 `bridge_create_source` 创建 HTTP 来源。下面是需要用户在浏览器授权的示例。将示例地址和客户端 ID 换成上游注册信息。

```json
{
  "source": {
    "id": "devboard",
    "type": "streamable-http",
    "url": "https://devboard.example/mcp",
    "oauth": {
      "mode": "authorization_code",
      "issuer": "https://login.example",
      "client_id": "bridge-client",
      "scopes": ["issues.read"]
    }
  }
}
```

部署时将 `BRIDGE_PUBLIC_URL` 设为 Bridge 对浏览器可见的基址。预注册客户端的回调地址为该基址加 `/oauth/callback`。上游支持 Client ID Metadata Document 时，可以省略预注册信息，只填 `mode`，并使用 HTTPS 基址。Bridge 在 `/oauth/client.json` 发布客户端元数据。

创建结果为 `authorization_required` 时，由来源所有者调用 `bridge_begin_oauth({"source_id":"devboard"})`，打开返回的 `authorization_url`。授权完成后 Bridge 立即连接上游，刷新令牌随来源加密保存。用 `bridge_get_status` 检查 `ready`。来源的访问授权决定哪些 Bridge 账号能使用这份上游身份。

| 运行身份 | `source.oauth` 字段 |
| --- | --- |
| 用户浏览器授权 | `mode: authorization_code`。预注册时填写 `issuer`、`client_id`，机密客户端另填 `client_secret` |
| 服务账号 | `mode: client_credentials`，填写 `issuer`、`client_id`、`client_secret` 和所需 `scopes` |
| 企业身份 | `mode: enterprise`，填写上游 `issuer`、`client_id` 和企业 `idp_issuer`、`idp_client_id`，依注册信息提供客户端密钥，再提供企业 IdP 签发的 `id_token` |

本机管理员持有的企业来源还可以通过 `BRIDGE_OIDC_TOKEN_FILES` 绑定令牌文件，例如 `{"devboard":"/run/credentials/devboard.id-token"}`。在这种配置下省略 `id_token`，由身份提供方或部署系统更新文件。Bridge 读取断言，向 IdP 换取 ID-JAG，再向上游授权服务器换取访问令牌。

## 服务发现与 MCP Apps

配置 `BRIDGE_PUBLIC_URL` 后，Bridge 在 `/.well-known/ai-catalog.json` 发布目录，在 `/mcp/server-card` 发布自己的 Server Card。反向代理应将两个路径转发到同一个实例。本机调用 `bridge_discover_server` 并传入域名的 HTTPS URL 可列出卡片；条目提供卡片 URL 时，把该 URL 再传给同一工具即可读取端点，用选中的 `remotes[].url` 创建来源。

客户端通过 `tools/list` 获取 App 工具，并读取工具元数据中的 UI 资源 URI。聚合入口会重写 URI，以区分来自不同来源的界面。多个 App 使用相同的回调工具名，或 App 内部依赖上游原始名称时，将客户端连接到 `/mcp/apps/{source_id}`。这个 HTTP 入口保留该来源的工具名和 UI 资源 URI，沿用 Bridge 的账号和来源访问控制。

## 资源、补全与通知

从 `resources/templates/list` 取得模板，填入参数后通过 `resources/read` 读取。通过 `completion/complete` 请求补全时，使用 Bridge 列出的模板或提示词名称。上游支持订阅时，客户端可以订阅资源更新。工具、资源和提示词目录收到变更通知后会刷新，定期轮询用于补偿遗漏的通知。

普通工具调用可附进度标识，客户端收到的进度沿用自己的标识。客户端取消正在进行的请求时，Bridge 将取消传至上游。目录结果采用账号私有缓存，TTL 为 5 秒。内容读取的缓存 TTL 为 0。触发器任务在触发器有效期间没有固定到期时间，删除后按保留策略计算到期时间。

## 验证

运行 `make check` 执行 Go 竞态检查及协议、授权、触发器和脚本集成测试，运行 `make check-app` 在 Chromium 里验证 Bridge 自带界面和上游 App 的工具回调。单独验证协议和授权可运行 `make build`，再分别执行 `python3 tests/standard_relay.py`、`python3 tests/source_oauth.py`。
