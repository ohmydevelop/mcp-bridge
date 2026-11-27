# macOS 和 Windows 用户级部署

发布包面向 Apple Silicon macOS 和 Windows 11 x64。Bridge、上游进程、Python handler 和 CLI 投递程序都在本机运行。定时、事件、Webhook 和 CLI 投递可以使用；交互式终端投递（`mcp-bridge run`）目前仅在 Linux 提供。

## 安装

将压缩包与同一发布版本的 `SHA256SUMS` 下载到同一个目录。首次安装会生成主密钥与 HTTP token；每次登录后服务自动启动，监听 `127.0.0.1:8088`。若已有手动启动的实例在使用默认数据目录或端口，请先停止它。已有数据库也需要原始主密钥。

**macOS**，在已登录的用户桌面打开终端：

```sh
shasum -a 256 mcp-bridge-darwin-arm64.tar.gz
# 与 SHA256SUMS 内该文件的摘要核对后解包
tar -xzf mcp-bridge-darwin-arm64.tar.gz
./mcp-bridge-darwin-arm64/macos-service.sh install
"$HOME/Library/Application Support/mcp-bridge/macos-service.sh" status
```

**Windows 11**，在当前用户的 PowerShell 中运行。若下载文件带有互联网标记，请在核对摘要后解除该发布包的阻止，以便本机执行随包脚本。

```powershell
Get-FileHash .\mcp-bridge-windows-amd64.zip -Algorithm SHA256
# 与 SHA256SUMS 内该文件的摘要核对后继续
Unblock-File .\mcp-bridge-windows-amd64.zip
Expand-Archive .\mcp-bridge-windows-amd64.zip .
powershell -NoProfile -ExecutionPolicy RemoteSigned -File .\mcp-bridge-windows-amd64\windows-service.ps1 install
powershell -NoProfile -ExecutionPolicy RemoteSigned -File "$env:LOCALAPPDATA\mcp-bridge\windows-service.ps1" status
```

升级时在新版解压目录中再次执行 `install`。服务使用当前用户主目录下的 `.mcp-bridge`。其中 `master.key` 用于解密配置，`http.token` 用于 HTTP 客户端认证。迁移旧数据库前，将其原始密钥设置为安装环境的 `BRIDGE_MASTER_KEY`。备份应包括数据库和密钥。

## 连接客户端

stdio 客户端的 `command` 使用下列二进制的**完整路径**，`args` 填 `["stdio"]`。客户端不一定会展开 `~`、`$HOME` 或 `%LOCALAPPDATA%`。

| 平台 | 安装后的程序 |
|---|---|
| macOS | `$HOME/Library/Application Support/mcp-bridge/mcp-bridge` |
| Windows | `%LOCALAPPDATA%\mcp-bridge\mcp-bridge.exe` |

Streamable HTTP 客户端使用 `http://127.0.0.1:8088/mcp` 和 `Authorization: Bearer <http.token 中的值>`。本地 stdio 可以通过 `bridge_save_destination` 即时新增 CLI 目标；运行时以字符串数组传递参数，因此路径中的空格和中文无需额外 shell 转义。

## Handler 和服务状态

安装脚本在当前终端运行 `mcp-bridge python-path` 查找可用的 Python 3，并保存成功探测的解释器路径与当前 PATH，供登录启动使用。服务会使用本机解释器启动受限进程。运行 `status` 查看本机接入和 handler 自检结果。没有保存解释器且新装的 Python 位于服务已有 PATH 时，下次执行会自动发现。要切换到新终端中找到的 Python，可运行下列命令。脚本先验证解释器，再原子更新登录服务读取的路径；正在运行的 Bridge 会在下一次执行时使用它，探测失败时保留上次成功的路径。

```sh
"$HOME/Library/Application Support/mcp-bridge/macos-service.sh" refresh-python
```

```powershell
powershell -NoProfile -ExecutionPolicy RemoteSigned -File "$env:LOCALAPPDATA\mcp-bridge\windows-service.ps1" refresh-python
```

Windows 首版使用标准安装布局的 CPython 3.10+。Bridge 将探测到的基础解释器和标准库复制为当前用户的只读运行时视图，位置为 `%LOCALAPPDATA%\MCPBridge\PythonRuntimes`。AppContainer 从该视图启动；虚拟环境会解析到基础解释器，第三方 site-packages 不进入视图。macOS 在本机解释器中加载 Seatbelt 隔离策略。两端均在运行用户脚本前启用隔离，平台不支持时，handler 入口会返回错误。

前台调试时先停止登录服务，再以安装脚本的 `serve` 参数启动。macOS 使用 `launchctl bootout "gui/$(id -u)/net.mcp-bridge.agent"`，Windows 使用 `Stop-ScheduledTask -TaskName "MCP Bridge ($([Security.Principal.WindowsIdentity]::GetCurrent().User.Value))"`。调试完成后再次运行 `install` 恢复自启动。卸载时运行同一脚本的 `uninstall` 操作，数据库和密钥保留在原目录。

## 平台验收

在对应架构的机器上，从仓库运行 `python tests/native_platform.py <解包后的 Bridge 路径>`。它使用临时目录，启动两个实例，验证 HTTP、stdio、原生 handler 回调、定时 CLI 投递的文件内容及回执、热更新和重启恢复。CI 的 Windows 托管作业运行于 Windows Server x64；正式 Windows 11 验收另在专用 Windows 11 x64 运行器执行。登录服务的验收在一次性用户账户里完成安装、状态检查、重新登录、升级和卸载，并核对数据与密钥仍可使用。
