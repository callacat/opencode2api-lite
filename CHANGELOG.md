# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与[语义化版本](https://semver.org/lang/zh-CN/)。

## [v1.0.2] - 2026-10-13

### 修复

- **流式请求绕过会话检查**：v1.0.1 只给非流式入口加了「未配置真实会话就返回 503」的闸口，
  `callOpenCodeAPIStream` 漏了同一道检查——未配置会话时它会带着编造的 `ses_*` 真的打上游、
  静默收到 `403 FreeTierError`，正是本次要消灭的排查盲区。现两个入口共用 `ocSessionGuard`。
  阴性对照：拆掉流式入口的闸口后 `TestSessionGuardBothEntryPoints` 立即变红，并复现出真实的
  403 FreeTierError 响应体。

### 新增

- 上游回 `403 FreeTierError` 时**无条件**记一行 `[free tier rejected]` 诊断（不再受 `-debug`
  约束，此前生产默认根本看不到这类 403），并点名免费层的两道门：会话存在性、`tools` 的
  bash+read。上游对两者返回的是同一个 403，不点名就会把排查引向错误的那一道。
- 自检 9 → 11 个用例，新增两个针对上述两点的判据：
  `TestFreeTierToolsGate`（四种 tools 形态下 bash+read 必须齐全，且不丢客户端原有工具）、
  `TestSessionGuardBothEntryPoints`（流式与非流式两个入口都必须在缺会话时 503）。
  两者都做过阴性对照：删掉 `read` 工具定义 / 拆掉流式闸口，对应用例立刻变红。
- README 配置表补上 `oc_session_id` 与 `oc_session_refresh_cmd` 两项（v1.0.1 漏写），
  并在 `oc_session_id` 一栏点明它是第一道门、`tools` 的 bash+read 是第二道门。

## [v1.0.1] - 2026-10-13

### 修复

- **免费层全量 403 FreeTierError**：上游 2026-10 把 `x-session-id` 的校验从「格式校验」升级为
  「存在性校验」。v1.0.0 依赖 `newOCSessionID()` 凭空编造一个格式合规的 `ses_*`（注释原文：
  「随机编造即可，无需服务端注册」），该假设已失效，编造值一律被拒。
  实测对照矩阵（2026-10-13）确认判定条件是**会话真实存在**：
  content 用 parts 数组还是字符串、read 参数 `file_path` 还是 `filePath`、有无 `stream_options`
  均无影响；携带真实 `oc_sk_*` API key、或补齐 `x-opencode-client/project/request/session/session-id`
  新协议头，也都无法让编造会话通过。
  `tools` 门仍然生效，`ensureFreeTierTools` 必须保留，不可删——它是**与会话相互独立的第二道门**：
  独立复核（同一真实会话、同一出口，只变 tools，n=2/组）——`[bash, read]` → 200、四件套 → 200、
  无 `tools` 字段 → 403、`[bash, glob, grep]`（缺 read）→ 403、`[glob, grep, read]`（缺 bash）→ 403。
  即「会话是好的」不足以过门，`tools` 被精简掉会得到一模一样的 `403 FreeTierError`。

### 新增

- 配置项 `oc_session_id` 与环境变量 `OC_SESSION_ID`：注入 OpenCode 客户端真实创建过的会话。
  **未配置时不再编造**（编造必然 403），直接返回 `503 OCSessionMissing` 并在错误信息里写明
  要配什么——静默的 403 对调用方毫无信息量。
  注意它只是**第一道门**：会话有效之后，请求体的 `tools` 还必须同时含 `bash` 与 `read`，
  缺任一同样是 `403 FreeTierError`（`ensureFreeTierTools` 自动补齐）。
- 配置项 `oc_session_refresh_cmd` 与环境变量 `OC_SESSION_REFRESH_INTERVAL`（默认 30m）：
  代理定期执行该命令，从输出里解析 `ses_[0-9a-f]{12}[A-Za-z0-9]{14}` 并写回内存与 config.json，
  解决会话过期后的续命问题（实测有效期下限 52 分钟，故默认间隔取其一半留一倍余量）。
  典型命令：`opencode run "x" 2>&1 | grep -o 'ses_[0-9a-f]\{12\}[A-Za-z0-9]\{14\}'`
- 管理面板新增「OpenCode 会话 ID」与「会话自动续命命令」两项输入，保存时一并回写。
- 面板回传配置时若请求体省略 `oc_session_id` 会沿用现值——旧版前端/脚本只提交已知字段，
  直接解进新 struct 会把会话静默清空、服务随即退回 403。
- 自检扩充到 9 个用例：`TestOCSessionInjection`（含「未配置不编造」）、
  `TestOCSessionMissingBody`、`TestOCSessionRe`、`TestRefreshOCSessionOnce`、
  `TestNormalizeConfigTrimsSession`、`TestMergeOCSessionID`、`TestConfigPostDecodePresence`。

### 已知限制

- 上游未提供匿名会话注册端点（`zen/v1/session(s)`、`inference/v1/session(s)`、`api/session`
  均 404；客户端二进制中也无注册端点），因此真实会话只能由 OpenCode 客户端产生后注入。
- 会话有效期未量化：实测 2026-10-13 16:13 产生的会话至 17:2x 仍返回 200（下限 ≥52 分钟），
  上限未知。`oc_session_refresh_cmd` 的默认 30 分钟间隔建立在这个下限之上；若实际 TTL
  短于该间隔，需调小 `OC_SESSION_REFRESH_INTERVAL`。
- 会话刷新命令需要能创建真实会话的凭据（如本机 `opencode run` + `auth.json` 里的 oc_sk key），
  该凭据不随本项目分发，部署方需自备。

### 回滚

回滚到 v1.0.0 即恢复旧行为（编造会话、必然 403）。新增字段均为 `omitempty`，
旧版本读取含 `oc_session_id` 的 config.json 会忽略该键，不影响启动。
面板新增输入项在旧版本不存在，忽略即可。

## [v1.0.0] - 2026-09-22

### 新增

- 初始版本：OpenCode Zen → OpenAI Chat Completions / OpenAI Responses / Anthropic Messages 三协议代理
- 模型别名与多模态模型映射（未配置模型一律 403 拦截）
- SOCKS5 出口调度：直连 / 固定代理 / 轮询 / 429 限速自动切换
- 身份刷新：UA 版本从 npm 拉取并保证不低于上游门槛，每日统计重置时同步刷新 session
- 管理面板：配置在线编辑、用量统计（按模型/按日）、上游模型列表、会话手动刷新
- GitHub Actions：5 平台二进制 + `sha256sums.txt` 发 Release；GHCR 多架构镜像（linux/amd64, linux/arm64）
