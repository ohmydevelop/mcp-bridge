# MCP Bridge

- **统一发现** — Agent 通过一个 MCP 入口查找和调用多个服务的工具，也能按需读取 Skill。
- **Python 编排** — Agent 可用脚本组合多个工具，先临时执行，再将同一脚本发布为可复用工具。
- **事件与定时触发** — Bridge 按事件或计划运行任务，将结果投递到 Agent 终端、webhook 或批处理 CLI。

MCP Bridge 支持本机 stdio 和 Streamable HTTP 接入。已有运行中的实例可直接配置客户端。上游授权、Apps、服务发现、客户端能力声明和文件中继部署见 [MCP 协议与扩展配置](docs/mcp-extensions.md)。

Apple Silicon macOS 和 Windows 11 x64 的压缩包提供用户级安装、登录自启动和原生 Python handler。[安装与验收说明](docs/platforms.md)

## 启动本机服务

在 Linux 上使用 Go 1.26，从仓库根目录运行以下命令。首次启动生成主密钥，后续启动复用同一文件。已有部署请沿用原主密钥和数据目录。

```sh
make build
install -d -m 700 "$HOME/.local/state/mcp-bridge"
keyfile="$HOME/.local/state/mcp-bridge/master.key"
(umask 077; set -C; openssl rand -hex 32 > "$keyfile") 2>/dev/null || test -s "$keyfile"
BRIDGE_MASTER_KEY="$(cat "$keyfile")" ./bin/mcp-bridge serve
```

## 配置 MCP 客户端

**本机 stdio**：保持服务运行，以同一用户启动客户端。将以下入口加入客户端配置，按客户端格式填写。将 `command` 替换为 `realpath ./bin/mcp-bridge` 的输出。

```json
{
  "mcpServers": {
    "bridge": {
      "command": "/absolute/path/to/bin/mcp-bridge",
      "args": ["stdio"]
    }
  }
}
```

默认数据目录为 `~/.mcp-bridge`。若服务使用了 `-data DIR`，客户端参数也使用 `["stdio", "-data", "DIR"]`，将 `DIR` 替换为同一绝对路径。

**Streamable HTTP**：客户端使用部署方提供的 `/mcp` URL 和请求头 `Authorization: Bearer <token>`。默认本机地址为 `http://127.0.0.1:8088/mcp`，服务端在启动前通过 `BRIDGE_TOKENS` 设置凭证，例如 `{"agent":"<至少 32 字符的随机令牌>"}`。

连接后调用 `tools/list` 验证接入。Agent 根据返回的工具描述和 `inputSchema` 发现能力，再按任务调用 `tools/call`。
