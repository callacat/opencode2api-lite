# opencode2api-lite

把 **OpenCode Zen** 免费模型服务转成标准 API 的轻量代理：对外暴露 **OpenAI Chat Completions / OpenAI Responses / Anthropic Messages** 三种协议，免 API key、单文件 Go 实现、零第三方依赖，内置管理面板与 SOCKS5 出口调度。

```text
客户端（OpenAI / Anthropic SDK、Claude Code、各类面板）
        │
        ▼
opencode2api-lite  :8000
  ├─ /v1/chat/completions   OpenAI 协议
  ├─ /v1/responses          OpenAI Responses 协议
  ├─ /v1/messages           Anthropic 协议
  └─ /                      管理面板（模型别名 / 出口代理 / 用量统计）
        │   模型别名解析 + 客户端身份伪装 + 免费额度工具补齐
        ▼
https://opencode.ai/zen/v1/...     （可经 SOCKS5 出口）
```

> [!WARNING]
> 管理面板默认**不启用登录验证**（`-password` 留空）。对外暴露时务必设置密码，或用防火墙限制来源——面板可读写全部配置。
>
> 本项目为第三方代理实现，与 OpenCode 官方无关，仅供学习研究使用；请遵守上游服务条款，风险自担。

## 特性

- **三协议入口**：`/v1/chat/completions`（流式 / 非流式——非流式请求由代理以上游流式发出并本地聚合还原）、`/v1/responses`、`/v1/messages`（Anthropic，Claude Code 等可直连）
- **模型别名**：客户端模型名 → 上游 zen 模型名映射；未在别名表中配置的模型一律 403，防误用
- **多模态**：别名可配 `multimodal_model`，带图片的请求自动切换上游模型
- **SOCKS5 出口调度**：直连 / 固定代理 / 轮询 / 429 限速自动切换（含/不含直连两种模式）
- **百度 CONNECT 隧道出口**：第三种出口类型 `baidu_connect`，国内网络经 `cloudnproxy.baidu.com` 的 CONNECT 隧道出网（面板可选，作保命通道）
- **身份自动刷新**：启动时从 npm 拉取 opencode 最新版本号填入 UA（低于上游门槛自动抬升），每日统计重置时同步刷新 session / project
- **管理面板**：模型别名、推理强度映射、出口代理在线编辑（保存即热加载）；用量统计按模型/按日；上游模型列表；会话手动刷新
- **单文件零依赖**：仅 Go 标准库，`go build` 直接出二进制，交叉编译全平台

## 快速开始

### 二进制

```bash
# 从 Releases 下载对应平台产物（校验见 sha256sums.txt）
./opencode2api-lite-<version>-linux-amd64 -port 8000 -password <面板密码>
```

### Docker

```bash
docker run -d --name opencode2api-lite \
  -p 8000:8000 \
  -v "$PWD/data:/data" \
  ghcr.io/callacat/opencode2api-lite:latest \
  -password your-password
```

Compose 见 [`docker-compose.example.yml`](docker-compose.example.yml)；镜像支持 `linux/amd64` 与 `linux/arm64`。

日志时间默认按 `Asia/Shanghai` 输出：时区库已内嵌进二进制（`time/tzdata`），镜像与 Android
都不需要额外装 tzdata；需要别的时区时用 `TZ=UTC` 等环境变量覆盖即可。

### 验证

```bash
# 上游模型列表
curl -s http://127.0.0.1:8000/v1/models

# OpenAI 协议（模型名需先在 config.json / 面板中配置别名）
curl -s http://127.0.0.1:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"big-pickle","messages":[{"role":"user","content":"hi"}],"stream":true}'

# Anthropic 协议（Claude Code：把 ANTHROPIC_BASE_URL 指到本服务即可）
curl -s http://127.0.0.1:8000/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{"model":"big-pickle","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}'
```

## 配置

首次启动会在工作目录生成 `config.json`（缺失字段自动补默认值并回写）：

```json
{
  "model_alias": {
    "big-pickle": { "target_model": "big-pickle" },
    "claude-sonnet-4-5": { "target_model": "big-pickle", "multimodal_model": "mimo-v2.5" }
  },
  "reasoning_effort_map": { "big-pickle": "medium" },
  "socks5_proxies": [
    { "addr": "127.0.0.1:7890", "name": "local" },
    {
      "addr": "baidu://cloudnproxy.baidu.com:443",
      "name": "baidu",
      "kind": "baidu_connect",
      "auth": "",
      "connect_host": "sptest.baidu.com"
    }
  ],
  "active_socks5": ""
}
```

| 字段 | 说明 |
| --- | --- |
| `model_alias` | 客户端模型名 → `target_model`（上游 zen 模型）。**未列入的一律 403**；`target_model` 留空时取别名同名；兼容旧写法 `"别名": "目标模型"` |
| `multimodal_model` | 可选，带图片的请求改用该上游模型 |
| `reasoning_effort_map` | 可选，按模型指定 reasoning effort |
| `socks5_proxies` | 出口列表。SOCKS5 项：`addr`（`host:port`）/ `username` / `password` / `name`；百度隧道项：`addr`（`baidu://host[:port]`，缺省端口 443）/ `name` / `kind` / `auth` / `connect_host` |
| `kind` | 出口类型：省略或 `"socks5"` = SOCKS5（老配置行为不变）；`"baidu_connect"` = 百度 CONNECT 隧道 |
| `auth` | 百度隧道的 `X-T5-Auth` 值。**凭据**：只写在 `config.json`，不要提交进仓库；面板里是 `type=password` 输入框，日志与报错不会打印它 |
| `connect_host` | 百度隧道 CONNECT 的 `Host` 头，默认 `sptest.baidu.com` |
| `active_socks5` | 出口选择：`""` 直连；`"127.0.0.1:7890"` 指定代理（百度项填 `"baidu://cloudnproxy.baidu.com:443"`）；`"__round_robin__"` 每请求轮询；`"__rate_limit_switch__"` 遇 429 轮换（含直连）；`"__rate_limit_switch_no_direct__"` 同上但只用代理 |
| `oc_session_id` | 免费层必需的**真实会话 ID**（`ses_` 开头 + 26 位）。上游 2026-10 起校验会话存在性，编造的 `ses_*` 一律 403；未配置时代理返回 `503 OCSessionMissing`（而不是静默 403）。也可用环境变量 `OC_SESSION_ID` 注入。**注意：会话有效并不等于能过**——请求体的 `tools` 还必须同时含 `bash` 与 `read`（缺任一同样 403，由代理自动补齐，勿在客户端精简掉）。两道门都失效时，上游只回一个不区分来源的 `403 FreeTierError` |
| `oc_session_refresh_cmd` | 可选。定期执行的命令，从其输出里解析新的 `ses_*` 自动续命（默认每 30 分钟，`OC_SESSION_REFRESH_INTERVAL` 可调）。上游无匿名会话注册端点，会话只能由 OpenCode 客户端产生，有效期未公开量化 |
| `key_pool` | 可选，多凭据轮转。免费层限流维度是**（模型 × 凭据）**，单凭据的日预算就是整条链路的日预算，配上池子才能把总额度按凭据数放大。`enabled` 关闭 / `keys` 为空时鉴权回落到 `Bearer public`，行为与不配这一项完全一致。`strategy`：`sticky`（默认，按 `x-session-id` 粘住一条，中途不换）或 `round_robin`（每请求推进游标摊开负载）。`max_retries` 是凭据额度耗尽（429）后额外换几条重试，默认 4。`keys` 每条可写 `{"id","key","weight","enabled"}` 或直接写字符串；缺 `id` 按数组位置补 `k1..kN`，重复凭据加载时去重。凭据属敏感信息，不要提交进仓库 |

### 百度 CONNECT 隧道的已知限制

- **按目标域白名单放行**：隧道只对百度放行的域名可用，未放行的域会在 CONNECT 或 TLS 阶段失败。实测 `api.github.com` 曾 TLS 失败（国内 Android 自更新下载会失败），2026-10-21 复测又可通——白名单会变，**不要把它当稳定通道**。
- **未压测速率/并发**：只作保命通道，出现主出口全挂时临时顶上；吞吐与并发能力未量化。
- **`X-T5-Auth` 可能被封或轮换**：失效后隧道侧会回 403/407，代理按普通出口失败处理（走 429/403 自愈路径）；换值在面板改即可，不需要发版。
- **上游可能按共享出口 IP 判「滥用流量」**：百度隧道出口是共享 IP，2026-10-21 实测 opencode.ai 对聊天接口返回 `403 FreeTierError ... abusive traffic from this IP`（同刻直连 200），隧道本身的 CONNECT/TLS/HTTP 均正常，被拦的是上游的免费层门禁。

## HTTP 接口

| 端点 | 方法 | 说明 |
| --- | --- | --- |
| `/v1/chat/completions` | POST | OpenAI 协议，流式 / 非流式 |
| `/v1/responses` | POST | OpenAI Responses 协议 |
| `/v1/messages` | POST | Anthropic Messages 协议 |
| `/v1/models` | GET | 上游模型列表 |
| `/health` | GET | 存活检查，返回 `OK` |
| `/` | GET | 管理面板（`-password` 非空时需登录） |
| `/login` `/logout` | GET / POST | 面板登录 / 登出 |
| `/api/config` | GET / POST | 读取 / 保存配置（保存即热加载） |
| `/api/stats` | GET / DELETE | 用量统计 / 清零 |
| `/api/models` | GET | 上游全量模型列表（面板下拉用） |
| `/api/reload` | POST | 刷新会话身份并重拉模型列表 |

## 命令行参数

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-port` | `8000` | 监听端口 |
| `-listen` | 空 | 监听地址。**留空 = 绑全网卡**（与历史行为一致，容器靠 `ports:` 映射进来就靠它）；写 `127.0.0.1` 只绑回环；写非回环地址**必须同时设 `-password`**，否则拒绝启动 |
| `-config` | `config.json` | 配置文件路径 |
| `-password` | 空 | 管理面板密码；留空则不启用登录验证 |
| `-debug` | `false` | 调试日志 |

监听地址也可用环境变量 `OC_LISTEN_ADDR` 或配置里的 `listen_addr` 指定，优先级
`-listen > OC_LISTEN_ADDR > config.listen_addr`；三者都空则沿用历史行为（`":"+port`）。

> Android app 走的是另一条路：它**显式传** `-listen 127.0.0.1`（局域网开关打开才传 `0.0.0.0`
> 并强制面板密码），所以手机上默认只对本机可见，而服务器部署的默认行为一字未变。
> 本地 app 的安全默认由调用方承担，不改库的默认值——已有部署（含现网容器）升级后不受影响。

## Android

手机上本地跑代理，界面直接复用自带管理面板（WebView），不需要服务器。

### 安装与启动

1. 从 [Releases](../../releases) 下载 `oc2api-android-<version>.apk` 安装
   （`arm64-v8a` 真机 / `x86_64` 模拟器都在同一个包里）。
2. 打开 app → 设端口（默认 8000）→ 点 **启动**。
3. 常驻通知出现即代理已就绪；下方 WebView 会加载面板，地址是
   `http://127.0.0.1:8000/`。面板能直接读写配置（会话、别名、出口、凭据池）。

### 给手机内其它 app 用

把目标 app 的 base_url 指到本机：

```
http://127.0.0.1:8000/v1/chat/completions   # OpenAI 协议
http://127.0.0.1:8000/v1/messages           # Anthropic 协议（Claude Code）
```

两点容易踩：

- 目标 app 自己也要允许到 `127.0.0.1` 的**明文 HTTP**（Android 9+ 默认禁明文）。
  本 app 已用 `network_security_config.xml` 按域放行，但那只管本 app。
- 本机回环地址只有同机 app 能访问，别的设备要走局域网模式。

### 局域网共享（默认关闭）

局域网模式打开后代理绑 `0.0.0.0`，同一 Wi-Fi 下的设备就能用
`http://<手机IP>:8000/v1/...`。

> ⚠️ **面板密码在局域网模式下是强制的**（app 与代理侧各拦一次）：面板能改上游会话与
> 凭据池，无密码地暴露到局域网等于把凭据送人。默认关闭时只绑 `127.0.0.1`，
> 外部设备一律连不上。

### 长时后台

系统进入 Doze 会掐掉应用的网络访问，代理就接不上上游。app 里有个
**忽略电池优化** 按钮，装机后点一次；否则「关屏 30 分钟后仍可用」只在插着电时成立。

### 滚动开发包

合并到 main 会自动构建一次 APK 并覆盖预发布 [`android-dev`](../../releases/tag/android-dev)：
`versionName` 为 `dev.<run_number>`（App 内可见），asset 名固定为 `oc2api-android-dev.apk`，
release body 记录对应 `commit` 与构建时间。正式版仍只由 `v*` tag 产出，两者互不覆盖。

### 构建（新 ABI / 自定义）

发布由 CI 完成：`.github/workflows/android-apk.yml` 先用 NDK
`CGO_ENABLED=1 GOOS=android` 交叉编译出 `liboc2api.so`，再 `gradle assembleRelease`
签名出包。本机不需要 Go / NDK / Gradle。

签名：CI 优先用仓库 secrets 里的 `ANDROID_KEYSTORE_B64` / `STORE_PASSWORD` /
`KEY_ALIAS` / `KEY_PASSWORD`；没有就临时自签一份，**签名身份每次运行都不同，
不同次构建的 APK 不能互相覆盖安装** —— 要长期分发请在 secrets 里配一份固定 keystore。
keystore 与 APK 一律不入库。

## 构建

```bash
go build -o opencode2api-lite .     # 本机构建
go vet ./... && go test ./...       # 检查 + 单测
```

发布由 GitHub Actions 完成：

- [`build-release.yml`](.github/workflows/build-release.yml)：推送 `v*` tag → 5 平台二进制 + `sha256sums.txt` → GitHub Release（也支持手动触发验证）
- [`docker-ghcr.yml`](.github/workflows/docker-ghcr.yml)：构建 `linux/amd64` + `linux/arm64` 镜像推送 GHCR（main → `dev`，tag → 语义化版本 + `latest`；
  `workflow_dispatch` 可传 `image_tag` 为任意分支构建可测镜像，留空则只构建不推送）
- [`android-apk.yml`](.github/workflows/android-apk.yml)：推送 `v*` tag → NDK 交叉编译 + Gradle 出签名 APK → artifact（tag 时附到 Release）；
  合并到 main 也会自动出包（`versionName=dev.<run_number>`），滚动覆盖预发布 `android-dev`（不进正式 Release）

## 版本与变更记录

- **变更明细**：[`CHANGELOG.md`](CHANGELOG.md)（按版本分节，本版改了什么看这里）
- **产物与校验**：[Releases](../../releases)（各平台二进制、APK、`sha256sums.txt`）
- **版本自证**：二进制版本号由 CI 按 tag 注入，`./opencode2api-lite-<version> -version` 打印的值应与 tag 一致
- **UA 兜底地板**：`main.go` 里的 `ocUABakedVersion` 是官方更新服务不可达时的下限（低于上游 floor 会全量 426）。
  启动日志出现 `[告警] UA 兜底地板已陈旧 baked=<地板> live=<新版本>` 时，把 live 版本抄进 `ocUABakedVersion`，
  同步 CHANGELOG 并抬 `main_test.go` 里「兜底地板不低于当前上游」的断言——这条告警就是抬地板的唯一触发信号。

## 提交约定

- **行为面改动**（flag / 配置键 / HTTP 端点 / 默认值 / 依赖）必须在**同一 commit** 内同步更新 README 与 CHANGELOG；不许「先合并，文档以后再补」。
- **发版时三处版本号同源**：CHANGELOG 段落、README 引用的产物名、Release notes —— 任一不一致视为发版未完成。
- 判断口径：问一句「新人只看这个仓库，能不能跑起来、知不知道这版改了什么」；答否就是文档没跟上。

## License

[MIT](LICENSE)
