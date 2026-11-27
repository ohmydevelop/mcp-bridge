# 用 QA 事件触发自动化

1. 管理员在部署环境的 `BRIDGE_WEBHOOK_ORIGINS` 中配置接收端 origin，在 `BRIDGE_TOKENS` 中为 Agent 和生产者配置独立凭证。在 `BRIDGE_PROFILES` 中将生产者绑定到 `qa`。
2. 调用 `bridge_create_source`，例如 `{"source":{"id":"qa","type":"package","grants":["reader"],"events":{"test_run.failed":""},"aliases":{"event:test_run.failed":["支付项目测试失败"]}},"package_path":"/absolute/path/to/testdata/qa"}`。空字符串代表由生产者主动上报，配置为工具名则由后台调用该轮询工具。轮询工具接收 cursor 和 arguments，返回 events、cursor、hasMore 和 nextPollMs。
3. 用 `bridge_save_destination` 登记目标，例如 `{"destination":{"id":"review","url":"https://agent.example/hooks/qa","description":"QA 自动化","grants":["reader"]},"revision":0}`。敏感凭证可用 `credential` 引用服务端的 `BRIDGE_CRED_<名称>`。
4. 普通 Agent 调用 `search_event_types({"query":"支付项目测试失败"})`，再用 `save_trigger` 保存下方定义。唯一可用目标会自动选中；若返回 `destination_required`，用返回的候选 ID 重试。成功时返回 trigger_id、目标和 revision。
5. 生产者调用 `publish_event({"event_id":"qa-731","event_name":"test_run.failed","data":{"project":"payment"}})`。源身份来自凭证。收到 webhook 的 Agent 用 `get_skill` 获取说明，用 `call_tool` 执行业务工具。

```json
{"trigger_id":"qa-review","when":{"type":"event","source_id":"qa","event_name":"test_run.failed","filter":{"project":"payment"}},"skill_ref":"skill://qa/triage-failure/SKILL.md"}
```

`list_triggers({})` 列出流程，`get_trigger({"trigger_id":"qa-review"})` 可查看最近 50 次投递。用 `save_trigger({"trigger_id":"qa-review","revision":1,"disabled":true})` 停用并保留历史。流程绑定的来源、事件名、接收模式以及已建立的 push/webhook 订阅过滤条件需要通过新流程 ID 更换。

兼容自动化收到 JSON 对象，字段为 `delivery_id`、`flow_id`、`source_id`、`event`、`skill_ref`。接收端按 `Idempotency-Key` 去重，再用 `get_skill` 获取说明。目的端配置 credential 引用后，`X-Bridge-Signature` 是以 secret 为键、对原始请求体做 HMAC-SHA256 得到的 Base64 值。2xx 表示接收端已接单，Agent 的业务执行状态由其自动化系统记录。网络中断、408、429、5xx 会重试，最多十次。重试耗尽后可通过流程详情检查失败记录。远端可能在回复前已处理事件，端到端语义为至少一次。

原生 Events webhook 使用独立的 Standard Webhooks 签名和 challenge，携带 `webhook-id`、`webhook-timestamp`、`webhook-signature`、`X-MCP-Subscription-Id`。入站地址为 `/callback/<flow-id>`。旧 Agent 只需实现其已有的自动化接收接口。

### 回放一次由事件触发的工具改进

`tests/qa_replay.py` 在临时目录启动真实 Bridge、QA MCP 来源和本地 webhook。任务是返回机器人配置中的三个开关。示例的基线为每个字段读取一次完整快照。QA 适配器在每次读取时记录快照摘要和字节数，调用方记录总耗时。Bridge 在一次组合工具调用内按上游工具名及参数统计成功的只读调用。相同读取发生两次及以上时，Bridge 自动产生管理员可见的 `bridge.builtin` 来源事件 `tool.repeated_read`，接收端根据通知中的 `skill_ref` 读取 Skill，再启动 Codex 生成 Python 实现。脚本以独立工具名发布到临时 Bridge，不替换现有部署。

订阅时可用 `filter: {"tool_id":"bridge.builtin/bot_flags"}` 指定维护目标。事件携带版本、总调用数、成功只读调用数、重复次数和执行耗时。仅识别上游声明 `annotations.readOnlyHint: true` 的读取；分页参数不同算作不同读取。顶层直接读取的任务归组见下文。相同工具版本在同一个 UTC 自然小时合并为一个事件。原始参数和返回值不会进入维护 webhook。回放适配器另外统计快照摘要和读取字节，供独立验证。

```sh
make build
python3 tests/qa_replay.py --output bin/trace/qa-replay.json
# 使用真实 QA 数据时，从当前 shell 安全输入凭证
read -rs QA_API_KEY && export QA_API_KEY
python3 tests/qa_replay.py --live --output bin/trace/qa-live-replay.json
unset QA_API_KEY
```

两种模式均需可用的 Codex CLI、模型访问权限及 Bridge Python 隔离运行时。默认使用本地样本；live 模式通过 `X-KOALA-TOKEN` 只读调用一次 [QA bot API](https://qa.chaitin.cn/swagger/index.html#/bot/get_bot)，仅保留三个开关。模型输入包含 Skill、基线和执行统计。运行报告包含候选代码、每例调用次数、读取字节、耗时和快照摘要。字节数统计本地重放的 JSON 大小。真实快照与缺字段、真假混合、显式 null、上游报错样本逐一双跑；验证器检查两版结果、独立预期和实测调用次数。耗时仅用于诊断。成功时报告写入指定路径。

### 同一任务内的直接调用

Agent 或宿主在任务开始时生成一个随机标识，同一任务内的每次直接读取使用同一个 `task_scope`。它是 Bridge 的可选调用参数，可跨 MCP 连接复用。示例参数：

```json
{"tool_id":"qa/get_bot","arguments":{},"task_scope":"qa-flags-9ea18062"}
```

Bridge 将已认证身份、任务标识、上游工具名和规范化参数作为计数键，在实际调用上游的位置计数。直接调用和 Python 组合工具读取同一个上游工具时共享计数，外层组合调用不额外计数。仅计入成功完成且有只读标注的调用。第二次相同读取触发内置事件 `task.repeated_read`；后续相同读取沿用同一个事件 ID。首次读取与第二次读取最多间隔 24 小时。SQLite 保存计数，重启后仍能关联；过期计数在后续任务读取时清理。省略任务标识时，Bridge 尝试从 Codex 请求元数据提取当前轮次。共用本机管理员 stdio 的客户端也共用身份，需要各自生成新的任务标识。

Agent 为目标 Agent 的 webhook 设置流程：来源 `bridge.builtin`，事件 `task.repeated_read`，过滤条件例如 `{"actor":"qa-reader","tool_id":"qa/get_bot"}`，并绑定 `optimize-bot-read` Skill。Webhook 接收的是身份、任务标识、工具名与触发时的计数。Agent 的自动化接收端负责将任务标识映射到自己的任务记录；示例的三字段业务契约来自 Skill。接收端生成并发布独立候选，在冻结的输入上与原始的一系列直接调用逐一对照。

```sh
make build
python3 tests/qa_replay.py --direct --output bin/trace/qa-direct-task.json
```

### 现有 stdio 客户端自动带入任务标识

当宿主为每个业务任务独立启动一次 MCP 子进程时，在 MCP 配置中设置：

```toml
[mcp_servers.bridge]
command = "/absolute/path/to/mcp-bridge"
args = ["stdio", "-one-task-per-process"]
```

该代理每次启动生成一个随机 scope，在转发普通 `call_tool` 时补入。两个同时运行的任务即使都用本机 stdio 身份，也有独立的计数。调用方显式提供的 `task_scope` 优先。若一个任务可能重启子进程，启动器可以改为在该任务子进程的环境中设置同一个 `BRIDGE_TASK_SCOPE`；每个新任务生成新值，不要将固定值写入共享配置。

当宿主跨任务复用一个 MCP 子进程时，保持默认 `args = ["stdio"]`。如果宿主没有逐次传递任务边界，Bridge 无法从相同的 JSON-RPC 调用判断是同一任务重复读取还是两个任务各读取一次。缺少有效任务标识或完整 Codex 元数据时，直接调用正常执行，不参与跨请求计数。共享 stdio 身份的授权规则保持原状。

用 `python3 tests/qa_replay.py --stdio-task --output bin/trace/qa-stdio-task.json` 验证。示例交错调用两个任务专用子进程和一个无标识的长连接，随后从收到的 webhook 执行 Skill 并回放。

### Codex 按轮次自动归组

Codex 使用普通 `args = ["stdio"]` 即可。Bridge 首先使用显式 `task_scope`，否则从 `params._meta` 取 `threadId`（或 `x-codex-turn-metadata.thread_id`）和 `x-codex-turn-metadata.turn_id`。若两个线程字段同时提供，值必须一致。有效的自动 scope 为 `codex:<thread_id>:<turn_id>`。计数键仍包含 Bridge 认证的身份、工具和参数；客户端提供的元数据仅用于关联。若需要把多轮计入同一个业务任务，可显式覆盖 scope。

```sh
make build
python3 tests/qa_replay.py --codex-turns --output bin/trace/qa-codex-turns.json
```

加 `--live` 可复用前述 QA 凭证读取一次真实快照。测试运行一个 Codex app-server、两个线程和五个轮次，从上游日志核对实际读取次数，从流程查询核对投递数，并将 webhook 中的 scope 与 app-server 返回的线程及轮次 ID 比对。随后通过 Skill 生成候选并在五组相同输入上回放。

### 验证原线程能否发现新工具

`python3 tests/qa_replay.py --adoption --output bin/trace/qa-adoption.json` 保持同一 Codex app-server 和原业务线程存活，从启动时就向它开放 `search_tools` 与 `call_tool`。候选通过五组回放后，测试用相同脚本发布正式业务名和可搜索的中英文描述、别名，并移除候选名。发布和回放发生在临时 Bridge 中。

回放验证器在全部样本通过后，将原始重复读取 webhook 的 `delivery_id` 作为 `origin_delivery_id` 传给正式的 `bridge_save_tool`。候选发布及 dry run 均不带这个字段。Bridge 从已存储的触发记录还原目标线程和轮次，在同一个 SQLite 事务中写入正式工具与一条定向的 `catalog.updated` 投递。它沿用原始投递的 webhook 地址标识；其他流程不会收到这条通知。版本冲突会回滚整个事务。

接收端根据事件中的 `actor`、`thread_id`、`turn_id` 与原触发记录核对，按 `delivery_id` 去重，再通过管理该线程的 Codex app-server 调用 `thread/inject_items`，写入目录更新消息，指明新编排所属的 `bridge.builtin` 来源，具体工具由原线程检索选择。接收端在注入成功后才确认 webhook。QA 宿主实现见 `tests/qa_replay.py` 和 `tests/codex_turn_replay.py`。独立部署需要能访问原 app-server 的 webhook 接收端，Bridge 自身没有桌面客户端的线程控制连接。

实测对原线程和未通知的对照线程发送完全相同的业务问题。报告的 `adoption.notification` 记录注入回执，`origin` 和 `control` 记录原始问题、选用工具及参数、上游读取次数和回答。两条用户消息都不包含工具名或检索指令。

### 第二层实验：改进验证用例

`tests/qa_process_eval.py` 使用上一轮真实 QA 回放报告及三批实际执行的校准工具记录，邀请 Codex 提议额外输入。校准工具含两种人为植入的缺陷和一个正确实现。提供给模型的是旧验证和后续执行的输入、期望、实际结果与读取数。模型只能提议测试输入；独立评分器计算期望值，保留原来的读取预算和错误约定。

两个流程接收相同的下一批候选：两个新缺陷实现和两个正确实现。旧流程使用原始五例，新流程额外采用 AI 提议的用例。每个候选只有验证失败时才获得一次模型修复机会，修复后重新运行对应流程的验证。预先固定的 24 个盲测配置不用于生成提议或修复反馈；提议与盲测恰好重合的输入不计分。报告列出原始集合的 SHA-256、剔除数量、逐候选分数和实际修复脚本。

```sh
make build
python3 tests/qa_process_eval.py --archive bin/trace/qa-auto-adoption.json --output bin/trace/qa-process-eval.json
python3 tests/qa_replay.py --direct --output bin/trace/qa-next-round.json
```

第一条 Python 命令需要已经生成的 live 报告；归档快照如与冻结的 QA 示例不同，可用 `--snapshot` 指定对应的三个开关。第二条使用普通本地样本和真实 Codex。已验收的 AI 提议保存在 `tests/qa_bot_validation_cases.json`，后续常规回放自动加载。本实验固定优化 QA 字段投影；独立样本来自该契约允许的布尔值、缺字段和 null 的组合。

### 常驻的第二层维护 Agent

宿主上的 QA 记录库保存实际调用输出、契约期望值、读取次数以及该轮使用的验证集版本。常规 QA runner 按 `QA_VALIDATION_DB` 打开它，并将同一候选的回放及后续执行记录一起提交。其他 QA 执行器可在获得相同投影和独立期望值后调用 `Journal.record`。只有参与记录的 QA 工作流才有这些业务级结果。

在 Bridge 守护进程启动环境里将 `BRIDGE_WEBHOOK_ORIGINS` 配置为 `http://127.0.0.1:8787`，`BRIDGE_CRED_QA_REVIEW` 配置为双方共享的随机密钥，保留现有主密钥及已有来源。接收端通过 HMAC 校验投递。在仓库根目录构建二进制后，启动维护进程：

```sh
export QA_VALIDATION_DB="$HOME/.mcp-bridge/qa-maintenance/qa.sqlite"
export QA_REVIEW_HOOK_KEY="$BRIDGE_CRED_QA_REVIEW"
python3 examples/qa-maintenance/worker.py --db "$QA_VALIDATION_DB" --state "$HOME/.mcp-bridge" --port 8787
```

普通 QA runner 也需继承同一个 `QA_VALIDATION_DB`。维护进程注册轮询源 `qa-replay-log` 和流程 `qa-review`。Bridge 轮询已提交的执行记录，以 `qa.replay.completed` 通知 worker；webhook 只含运行标识和版本。接收端先入库再应答，重投按运行标识去重。每个记录库运行一个 worker。挂起的工作重启后继续，连续三次处理异常会记为 failed；可在 SQLite 的 jobs 与 failures 表查询。不要更换已注册的同名 webhook 端口或记录库路径而不更新对应 Bridge 配置。

发现已通过回放的候选随后违反契约时，worker 读取当前版本最近的执行记录，按本地 `review-validation` Skill 调用 Codex 生成测试输入，再在独立临时 Bridge 上运行配对盲测。每轮在生成提议前固定盲测集，并排除与提议重复的输入。当前 QA 专用挑战库包含两个针对空字段的缺陷和两个正确实现。晋级条件为：至少一项盲测分数提高，新流程的所有候选均通过验证及全部盲测，正确对照没有被误拦。只有仍对应当前版本的工作可以原子追加用例；下一轮读取新版本。没有证据、不满足门槛或处理失败均保留原验证集。

从实际执行到下一轮的端到端验收：
```sh
make build
python3 tests/qa_event_review.py --output bin/trace/qa-event-review.json
```
该测试启动真实 Bridge、持久队列、独立 worker 和真实 Codex，运行一次健康样例及一次故障注入，然后重启 worker，运行普通 QA 回放。它检查 Bridge 投递、盲测报告和下一轮实际使用的 suite_revision，不需要单独启动评审实验。

### 单个失败事件贯穿两层改进

`tests/qa_incident.py` 仅使用本次运行的数据。它用 `QA_API_KEY` 只读获取线上三个开关，以“一次读取”为验收预算执行旧的三次读取流程。预算失败后，生产者发送带本次 run_id 的 `test_run.failed`。接收该 webhook 后才启动排查线程；失败摘要来自本次断言。为复现问题，第一次排查明确要求重放三次读取。最终采用阶段的用户问题只问三个开关。

为让第二层有可判定的盲区，验收在当次模型生成的 v1 周围加一个显式的兼容性故障注入：字段齐全且主字段显式为 null 时丢失其余开关。旧验证集和影子执行均实际调用同一 v1。worker 只能从本次记录取得证据。新版验证集重新运行 v1 并捕获缺陷后，维护模型修复它；只有修复版本通过新版验证和影子样本才按原始重复读取的 delivery_id 正式发布。注入仅在隔离验收中发生。最终是否采用根据原线程实际的 call_tool 参数判断。

```sh
make build
read -rs QA_API_KEY && export QA_API_KEY
python3 tests/qa_incident.py --output bin/trace/qa-incident-e2e.json
unset QA_API_KEY
```

JSON 保存执行阶段、候选代码、盲测、投递、SQLite 事件及原线程决策；同名 JSONL 记录维护端发出的 MCP 工具调用、Codex 已完成的工具调用及 webhook 收件。两个文件共享 run_id。未完成或原线程没有采用时保留部分记录且 passed 为 false。


### 将维护建议反馈到原 Codex 线程

运行 `python3 tests/qa_natural.py --followup --codex /path/to/codex --output bin/trace/qa-feedback.json` 可观察同一线程中的两次自然业务请求。该示例使用环境变量 `QA_API_KEY` 访问 QA。第一轮将 Codex 会话保存在本次实验的临时目录，第二轮由 app-server 恢复同一线程。

QA 接收端验证 Bridge webhook 的 HMAC 签名，再由维护 Agent 生成面向调用者的建议。宿主调用 `examples/qa-maintenance/thread_feedback.py` 的 `deliver_review`，用已验证事件中的身份、线程和轮次匹配实际调用记录，通过 `thread/inject_items` 写入建议。收到 RPC 确认后才发送第二轮业务问题。宿主分别记录注入回执、两轮 MCP 调用、真实上游响应和答案。第一轮没有重复读取时直接执行第二轮，回执为空。该示例覆盖单次运行中的路由，长期运行的宿主应保存投递回执以处理 webhook 重试。

### 在自然重复读取的线程中随机比较反馈

用 `python3 tests/qa_feedback_trial.py --codex /path/to/codex --output bin/trace/TRIAL` 启动新实验，凭据与自然业务实验相同。每个线程独占 Bridge、Codex 会话和调用日志。首轮只发送交接问题，签名 webhook 和原线程调用记录共同确认入组资格。实验开始前固定随机种子、每对线程的分配顺序、维护建议、入组目标和筛查上限；并发线程通过 SQLite 事务领取下一个名额。对照组恢复同一个线程，直接发送第二次业务问题；投递组在两步之间定向注入预先固定的维护建议。临时工作目录清理后会保留每个线程的实验记录。

用 `python3 tests/qa_feedback_report.py bin/trace/TRIAL` 导出隐去分组和样本编号的最终回答与实时配置。按回答是否正确陈述三个开关填写 JSON 布尔评分表，再传入 `--ratings ratings.json` 生成逐轮调用表和分组结果。主要指标要求第二轮正常结束，只有一次实际 QA 请求，该请求成功且最终回答正确。未读取当前配置即使回答正确也不计成功。每对入组线程各有一个投递、一个对照；双侧随机化检验只使用完整分配对，在两臂结果不一致的对上计算精确二项概率。
