# mini-agent（约 370 行 Go）

一个用来理解 AI Agent 内部结构的最小实现：**一次模型请求 + 工具调用 + 结果回灌 + 会话日志**。

它和 DeepSeek Harness 的对应关系：

| mini-agent | DSH | 说明 |
|---|---|---|
| `Run()` 里的 for 循环 | `ReactLoopAgent.turn()/step()` | 一轮里可以有多步 |
| `messages` 数组 | `session` 日志 + `deriveMessages()` | 这里直接放内存，DSH 从日志派生 |
| `complete()` / `streamComplete()` | `ctx.llm.stream()` | 非流式 / SSE 流式 |
| `runTool()` | `ctx.tools.execute()` + pipeline | 这里没有审批/沙箱/并发 |
| `session.jsonl` | JSONL 会话持久化 | 每步落一行 |

## 运行

先跑假模型，确认循环是通的（不花钱、不需要 key）：

```powershell
chcp 65001   # 让控制台按 UTF-8 显示中文，只需一次
cd D:\Develop\Workspace\AI\mini-agent
go run . -mock
```

再跑真实模型：

```powershell
$env:DEEPSEEK_API_KEY = "sk-你的key"
go run . -task "读取 go.mod，告诉我 module 名和 go 版本"
```

可选环境变量：

```powershell
$env:DEEPSEEK_MODEL = "deepseek-flash"      # 默认
$env:DEEPSEEK_BASE_URL = "https://api.deepseek.com"
```

## 在 VSCode 里调试

1. 需要 Go 扩展 + Delve（两者都已装好；Delve 在 `%USERPROFILE%\go\bin\dlv.exe`）
2. 用 VSCode 打开 `mini-agent` 文件夹，按 `F5`，选 **mini-agent (mock)** / **(DeepSeek)** / **(stream)**
3. 在 `main.go` 的 `Run()` 循环里下断点（比如 `resp, err := a.complete(...)`），用 `F10` 单步、`F5` 继续
4. 想看变量：左侧 **Run and Debug** 面板的 Variables / Call Stack

**调试流式**：`stream.go` 的 `parseStream` 每个 chunk 都会执行，普通断点会停很多次。在断点行右键 → **Add Conditional Breakpoint**，条件写：

```go
payload == "[DONE]"
len(payload) > 400
```

（Delve 的条件表达式支持简单运算，别写函数调用；够用了。）

然后看 `payload`、`ch`、`res.Content`、`calls` 这几个变量，就能看清 SSE 一块块到达的过程。

跑真实模型前，先在你启动 VSCode 的那个终端里设置 key，再启动：

```powershell
$env:DEEPSEEK_API_KEY = "sk-你的key"
code .
```

## 代码结构（就两个文件）

- `main.go`
  - `Run()`：初始化 messages → 循环「请求 → 有 tool_calls 就执行 → 回灌」→ 没有工具就输出并结束
  - `complete()`：POST `/chat/completions`，完全 OpenAI 兼容格式
  - `writeLog()`：每一步追加一行 `session.jsonl`
- `tools.go`
  - `toolDefs()`：三个工具的 JSON Schema（模型看到的就是这个）
  - `readFile()`：限制在工作目录内，支持 `offset`/`limit` 分页，单次返回受 6000 字符预算约束
  - `grepFiles()`：纯 Go 正则搜索，返回 `文件:行号: 内容` 和总匹配数，不经过 shell
  - `runCommand()`：`cmd /C`，30 秒超时，输出截断
- `stream.go`
  - `parseStream()`：解析 SSE（`data:`、空行、`[DONE]`），拼接 `delta.content` 和 `delta.tool_calls`
  - `streamComplete()`：发流式请求、边收边打印、把增量还原成一条完整消息
- `compact.go`
  - `findTailStart()`：选保留尾部，保证不以 `tool` 开头（不拆散工具调用和结果）
  - `compact()`：把较早的消息总结成一条 system 摘要，保留原始任务和最近几条
- `shell_windows.go` / `shell_other.go`：按平台构造 shell 命令

## 实验 1：上下文爆炸（用 `big-file.txt`）

`big-file.txt` 是本地生成的测试日志：**4001 行、367 KB**，最后一行藏着 `FINAL: deployment completed revision=abc123`。它被 `.gitignore` 忽略，不会提交。

按顺序执行，每次记下 `step 1` 的 `prompt_tokens`：

```powershell
# 1) 基线：小文件
go run . -task "读取 go.mod，一句话说明这是什么项目"

# 2) 大文件：默认截断（maxToolChars = 6000）
go run . -task "读取 big-file.txt，用一句话总结它是什么日志"

# 3) 问结尾：模型应该看不到（内容被截断）
go run . -task "读取 big-file.txt，告诉我最后一行是什么"

# 4) 把 tools.go 里的 maxToolChars 改成 60000，重跑第 2 步
# 5) 改回 6000。分页已经实现：先读一次拿到 total_lines，再跳到结尾
go run . -task "读取 big-file.txt 的最后 10 行，告诉我最后一行是什么"
```

记录表（自己填）：

| 实验 | prompt_tokens | 耗时 | 模型能看到结尾吗 |
|---|---|---|---|
| 1. go.mod | | | 不适用 |
| 2. big-file（6000） | | | 不能（只发送前 ~66 行） |
| 4. big-file（60000） | | | 不能（单次仍受字符预算限制，但能看到 total_lines） |
| 5. 分页后（已实现） | | | 能（先读头拿 total_lines，再跳读） |

结论：**读文件工具必须支持分页**（offset/limit），否则要么截断丢信息，要么把整个文件塞进上下文。这就是 DSH 的文件工具带行号范围和结果裁剪的原因。

## 实验 2：流式输出

```powershell
go run . -stream -task "读取 big-file.txt 的最后 10 行，告诉我最后一行是什么"
```

对照观察：

1. 文本是不是逐字出现，首字延迟和总耗时差多少
2. 工具调用那一步没有正文，要等工具结果回来、下一轮才继续出字
3. 不加 `-stream` 再跑一次，对比输出节奏和 `[总用量]`
4. 读 `stream.go` 的 `parseStream()`：`data:` 前缀、空行、`[DONE]`、`finish_reason` 各在哪处理

想看得更细，可以在 `parseStream` 里临时打印每个 chunk 的 `finish_reason` 和 `delta`，观察 `tool_calls` 的 `arguments` 是怎么一块块拼起来的。

## 实验 3：上下文压缩

`-compact-at` 指定触发阈值（`prompt_tokens` 超过它才压缩，`0` 关闭）；`-compact-min` 是旧消息的最小字符数，太短就不值得摘要。

先跑一个小任务：

```powershell
go run . -compact-at 800 -task "读取 big-file.txt 第 1 行，取出 latency 数值 L；再读取第 L 行，取出 latency 数值 M；再读取第 M 行；最后报告这三行的 req 编号。"
```

这会打印：

```
[compaction] 跳过：旧消息只有 130 字符（阈值 1200）
```

**这是正确的**：两小段消息摘要出来反而更长，压缩不划算。

要真正触发，需要每一步都读大段内容，让旧消息足够多：

```powershell
go run . -compact-at 800 -compact-keep 3 -task "读取 big-file.txt 第 1-100 行，取第 1 行的 latency 数值 L1；再读取第 L1 行前后各 50 行，取这一段第 1 行的 latency 数值 L2；再读取第 L2 行前后各 50 行，取这一段第 1 行的 latency 数值 L3；最后报告 L1、L2、L3。"
```

`-compact-keep 3` 表示只保留最近 3 条消息。默认 4 时，多工具步会让“最近 4 条”全落在最后一步，前面没有可压的旧消息，所以不会触发。

后一次读取依赖前一次的结果，所以会顺序走三步，历史里累计约 2 万字符。这时应该看到：

```
[compaction] 消息 8 → 7，摘要 N 字，摘要用量 prompt=... completion=...
```

观察：

1. 压缩发生在哪一步、当时 `prompt_tokens` 多大
2. 压缩后下一次请求的 `prompt_tokens` 降了多少
3. 最终答案是否正确（摘要有没有丢关键信息）
4. `session.jsonl` 里会多出 `"type":"compaction"` 的行

**注意**：如果历史不够长（消息少于 8 条，或旧消息少于 `-compact-min` 字符），会跳过压缩。这是有意的——小历史摘要不划算。

## 七个练习（按顺序做，每个都对应 DSH 的一个设计）

1. **上下文爆炸**：见上一节 `big-file.txt` 实验。
2. **工具报错**：把 `path` 改成不存在的文件，观察模型会不会自己换路径重试；再让它执行一条不存在的命令。
3. **加第三个工具**：`write_file`。体会工具描述（description）写得好不好，直接决定模型会不会用对。
4. **流式**：把 `stream` 改成 `true`，处理 SSE 和 `tool_calls` 增量拼接。这就是 DSH 的 `AssistantStreamAttempt` 在干的事。
5. **历史压缩**：消息超过 20 条时，把前 10 条交给模型总结成一条 system，再继续。这就是 compaction。
6. **加审批**：执行 `run_command` 前打印命令并等待你输入 y/n。这就是 approval。
7. **对照 DSH**：带着这些问题回看 `agent.ts`——
   - 为什么用户输入先进入队列（`agent/inbox/spliced`）而不是直接写 `messages`？
   - 为什么要写 `request/header`（提示词/工具变了怎么办）？
   - 为什么 `tool/call` 和 `tool/result` 要分开写、失败也要补写？

## 成本

默认用 `deepseek-flash`，一次十来步的练习大约几分钱到几毛钱；`-max-steps` 默认 8，防止跑飞。
