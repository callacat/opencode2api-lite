# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与[语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### 新增

- **Android：后台被系统冻结的检测 + ColorOS 加白引导（提示 + 跳转入口）**（CMP-236，真机 OnePlus PKG110 / ColorOS / Android 15 定性）。ColorOS Hans 会把退到后台的 App 整个 uid 的 cgroup 冻结，App 自己的线程连同代理子进程 `liboc2api.so` 一起停摆 —— 内核仍 LISTEN、8000 端口在，但用户态不调度，面板 `/health` 超时、按钮全「保存失败」。
  - 判据只用 App 内可观测的替代信号，**不读** `/sys/fs/cgroup` 下 uid 对应的 `cgroup.freeze`（非 root 读不到）：心跳断档（本进程长时间没被调度）+ 设备是醒着的（`uptimeMillis` 在涨，排除息屏/Doze 整机休眠）+ 断档落在一段后台期内。阈值 15s，与「连续 3 次探活失败」同一量级。判定逻辑抽成纯 Java 的 `FreezeDetector`，JVM 单测（`FreezeDetectorTest`，10 条）钉住「断档+醒着=冻结」「断档+睡着≠冻结」「没进后台≠冻结」三条分支。
  - 面板离线横幅区分两种失败：**请求超时**（端口在但不回包）⇒ 明确提示「疑似被系统后台冻结」并给出 ColorOS 加白路径；拒连/其它 ⇒ 维持原有「代理未运行或无响应」。新增「去加白」按钮（`intent://` 唤起 App 打开应用详情页）。面板「保存失败」toast 在超时场景同样升级成冻结专属文案。
  - App 侧（`ProxyService`）判出疑似冻结时记状态（`prefs`）+ 日志页留痕；用户把 App 切回前台时给原生提示，附「去加白」按钮跳到 `Settings.ACTION_APPLICATION_DETAILS_SETTINGS`（不硬编 ColorOS 私有 intent，各版本 action 不稳；文案指路到「设置 → 应用管理 → oc2api → 耗电管理 → 耗电行为控制 →「完全允许后台行为」→ 弹窗「允许」」，老马真机实测有效）。
  - 配置区新增「冻结自检」入口：点一下即可看当前是否疑似被冻结 / 加白是否生效（上次退后台未检出冻结 = 加白已生效或未触发）。

### 修复

- **面板：代理没跑时点「保存全部」只报「保存失败: Failed to fetch」，用户以为配置写错了**（CMP-248）。保存的网络层失败（fetch 被拒 / 超时中断）现在明确提示「代理未运行或服务不可达，请回 App 确认代理已启动」；服务端返回非 2xx（如配置写盘失败）仍原样显示后端返回的具体错误，两类失败不再混成一句。

- **UA 兜底地板 `2.1.1` → `2.1.3`**（CMP-252，承接 CMP-213）。oraarm 现网 v1.3.1 启动日志报 `[告警] UA 兜底地板已陈旧 baked=2.1.1 live=2.1.2`；实施时（2026-10-23）现查官方更新服务 `https://opencode.ai/update/api/latest/cli`，live 已是 **2.1.3**（npm 分发件 `@opencode/cli`），地板一并抬到 2.1.3。它只是兜底地板（官方更新服务不可达时才用到），运行时 UA 仍跟随 live，出口语义不变。`main_test.go` 的「兜底地板不低于当前上游」断言同步抬到 2.1.3；假上游版本改用 `2.1.99`，下次抬地板不用再改这几条用例。

## [v1.3.2] - 2026-10-22

### 修复

- **Android：代理「活着、端口在听、但不服务」，看门狗救不回来**（CMP-215，真机 PKG110 定性）。真机实测：`/proc/net/tcp` 里 `127.0.0.1:8000` 处于 LISTEN、`ps` 里 `liboc2api.so` 进程在、CPU 0%、`/proc/<pid>/status` 为 `S (sleeping)`，但 `/health` 20s 超时（`curl` 返回 000 且不是拒连）—— 新连接被 accept 后无人处理。`supervise()` 只在 `process.waitFor()` 返回时重启，这种「进程在但不干活」的状态永远等不到 exit，于是面板所有 fetch 挂住，用户看到「点任何按钮都崩 / 保存失败 / 很久才能再打开」。
  - 看门狗新增**探活自愈**：每 5s 探一次 `127.0.0.1:<port>/health`（启动后 15s 宽限），连续 3 次失败 ⇒ 判定卡死并杀掉子进程，由原退避逻辑重启。判据：把子进程冻住后**不手动点停止→启动**，1 分钟内自愈。
  - 杀进程用 `destroy()`→ 等 2s → `destroyForcibly()` 升级：真机那种卡死的进程连信号都不处理，只发 SIGTERM 会一直挂着（用 `SIGSTOP` 冻进程实测：SIGTERM 只是排队不生效，SIGKILL 才行）。
  - 探活本身抽成纯 Java 的 `HealthProbe`，JVM 单测覆盖「正常 200 / accept 了但不响应 / 端口关闭 / 非 200」四种；摘掉读超时，「accept 了但不响应」那条会挂住。

- **面板：点主题按钮把「检查更新」按钮的文案改成 🌙/☀**（CMP-215）。主题按钮与「检查更新」按钮共用 `class="theme-toggle"`，而 `toggleTheme()` 用 `document.querySelector('.theme-toggle')` 取**第一个**匹配 —— HTML 里第一个是「检查更新」按钮，于是每次切主题改的都是它的文案（样式也一起串）。现在主题按钮独占 `id="themeBtn"`、按 id 定位，「检查更新」按钮改用普通面板按钮样式，两者不再互相影响。设备自带 Chrome 与桌面浏览器均已复现并验证。

- **面板：点「退出」整页跳走，面板被"打没了"**（CMP-215）。「退出」原本是 `<form method="post" action="/logout"><button type="submit">`，点它 = 整页导航（设备浏览器上表现为 CDP 报 `Execution context was destroyed`），与真机「面板崩了、要多次刷新才能再打开」形状一致。现在改成 JS 提交 + 二次确认，退出后跳登录页；服务端 `/logout` 语义（POST 才真正注销会话）一字未动。

- **面板：代理被系统杀掉时所有按钮都失败、页面看着像死了**（CMP-215）。面板每 5s 轮询 `/api/stats`，服务一挂所有 fetch 立刻失败，每个按钮都报「保存失败」，要反复刷新才能等到服务回来。现在连续 3 次失败就**停掉 5s 轮询**，显示「代理未运行或无响应」；同时留一条 **15s 低频重试**，代理恢复（看门狗拉起 / 用户重启）后自动接回，不用手动刷新。会话失效（302 到 /login）单独提示，不误报成「代理未运行」。

- **面板请求没有超时，服务「连得上但不回包」时永远挂着**（CMP-215，运维 CMP-216 真机实测指出）。真机那种卡死是「端口在 LISTEN、TCP 连得上、但永不回包」——这种形态下浏览器 `fetch` **不会 reject**，于是「连续 N 次失败」的离线条永远不触发，用户看到的是按钮一直转、「检查更新」永远停在「检查中…」。现在面板所有请求都走带 `AbortController` 的 `fetchT`（读 8s / 写 12s），超时中断并按失败计数；实测把代理冻住（`SIGSTOP`）后，不点任何按钮，面板照样进离线态并显示「代理未运行或无响应」，解冻后自动接回。

- **面板：新增「重启代理」入口**（CMP-215）。代理卡死时面板自己毫无办法（它就住在那个进程里），以前只能让用户回 App 手点「停止→启动」。现在离线横幅在 Android 上多一个「重启代理」按钮，走 `intent://…;S.oc2api_restart=1;end` 唤起 App，App 收到就停服务再拉起（`MainActivity.handleRestartIntent`），用户不用回 App 找按钮。

- **面板：百度隧道的两个密码列分不清**（CMP-215，真机踩到）。出口表里「密码」与「Auth」两列挨着，真机上用户把**面板密码**填进了 `password`、`auth`（X-T5-Auth）留空 ⇒ 百度隧道必然 403，但面板毫无提示。现在列名改成 **「SOCKS5 密码」/「百度 X-T5-Auth」**，百度行里密码框提示「（百度隧道不用填）」、auth 框提示「X-T5-Auth（必填）」并在缺失时标红；表格下方有醒目提示「有 N 条百度隧道没填 X-T5-Auth：隧道一定 403。X-T5-Auth 不是 SOCKS5 密码」，保存时也会在 toast 里点名。

- **Android：自更新失败把系统英文串直接甩给用户**（CMP-215）。`PackageInstaller.EXTRA_STATUS_MESSAGE` 的原文（如 `INSTALL FAILED ABORTED: User rejected permissions`）直接进版本行，用户看不懂也不知道该做什么。现在常见失败码翻成人话 + 下一步指引（存储不足 / 降级被拒 / 未授予「安装未知应用」权限并给出设置路径），认不出的码保留原文但补上「（系统码 status=N）」便于排查。

- **Android：覆盖安装后第一次打开 App 挂着上一次的「安装未完成」**（CMP-215）。安装结果广播用的 PendingIntent 不是 one-shot 且从不取消，旧会话的结果可能迟到写进当前界面；同时代码里没有任何标记能区分「当前这次安装」与「上一次」。现在安装时记下会话 id，回调按会话 id 对号入座，对不上的一律不写界面；启动时还会清掉已经没有对应会话的标记 —— 全新安装（无历史会话）打开 App 不再显示「安装未完成」。

## [v1.3.1] - 2026-10-21

### 修复

- **Android：进程被杀后 App 仍显示「运行中」，实际没有任何服务**（CMP-206）。覆盖安装、force-stop、系统回收都会杀掉 app 进程，但 `SharedPreferences` 里的 `state=running` 会留下来；`MainActivity` 直接照它渲染，于是按钮是「停止」、状态写「运行中 http://127.0.0.1:8000/」，而 `ss -ltn` 无 8000、`ps` 无 `liboc2api.so`、`dumpsys activity services` 无服务 —— 面板所有请求自然失败，用户看到的是「保存失败 / 页面不刷新」。redroid（Android 14 / x86_64）上覆盖安装 6/6 轮稳定复现；另一台 PHM110 模拟器同样复现，与容器/存储实现无关，只取决于启动那一刻 prefs 里是否残留 `running`。决定性判据：挂掉那次启动时 prefs 文件时间戳（纳秒级）与 `ServiceRecord` 均未变化，说明服务从未创建、根本没有发生 exec。
  - 根因既不是「exec 失败被吞」（`pb.start()` 抛异常会被捕获、置 `error` 并按 `BACKOFF_MS` 重试），也不是安装器解压 jniLibs 的时序（覆盖安装后立即启动子进程 5/5 成功）。
  - 新增 `ProxyService.isAlive()`（同进程静态标志）；`MainActivity` 启动时若 prefs 说在跑、服务却已不在，就按上次意图重新拉起服务，不再渲染假「运行中」。
  - 看守线程改为捕获 `Exception`（原来只捕 `IOException`），运行时异常不再静默杀死看门狗；启动失败 / 子进程退出会同时写日志页（`LogBuffer`）、常驻通知与状态详情，日志区不再空白。
  - 「打开面板」在代理未运行时提示「代理未运行，请先点启动」，不再打开必然连不上的页面。

## [v1.3.0] - 2026-10-21

### 新增

- **新增 `baidu_connect` 出口（百度 CONNECT 隧道，CMP-197）**：出口列表新增第三种类型，面板可选（出口表新增「类型」下拉、Auth、Connect Host），配置持久化 `kind` / `auth` / `connect_host` 三个字段，`Kind` 省略即 SOCKS5、老配置行为与 `active_socks5` 完全不变。
  - 拨号器 `baiduConnectDial()` 与 `socks5Dial()` 并列，只由 `newSocks5HTTPClient()` 一处分派；失败走 `endpointError()`，现有 429/403 自愈路径原样生效，CONNECT 非 200 报错带状态行、不静默回落直连。
  - 日志 label 固定 `baidu (baidu://<node>)`，与面板 name 无关，`oc2api-watch.py` 的 `exit=` 判据零改动即可比对 `active_socks5`。
  - 凭据 `auth` 只进 `config.json`，面板为 password 输入、日志/报错不打印。回归：`TestBaiduConnectDial`（线协议 + 响应缓冲 + 403）、`TestSocks5ExitLabelBaidu`、`TestConfigPostPreservesBaiduFields`（面板 GET→POST 不丢三字段）。
  - 已知限制见 README：按目标域白名单放行、未压测速率/并发、`auth` 可能轮换、共享出口 IP 可能被上游判滥用流量。

- **Android APK 形态（CMP-111）**：同仓新增 `android/` Gradle 子工程与 `.github/workflows/android-apk.yml`，把代理以静态二进制形式打进 APK，手机上本地跑，WebView 直接复用自带管理面板，无需服务器。
  - 代理源码**一行业务逻辑未动**；Android 侧只做「拉起进程 + 看守重启 + 显示面板」。
  - 打包路径：`CGO_ENABLED=1 GOOS=android GOARCH=arm64|amd64` NDK 交叉编译 → `jniLibs/<abi>/liboc2api.so`（lib 前缀 + .so 后缀，系统安装时才会解压到 `nativeLibraryDir`，那是 Android 10+ W^X 下少数允许 exec 的目录）。
  - 前台服务用 `specialUse` 而非 `dataSync`：后者在 Android 15+ 受 6h/24h 总时长上限，到点会被系统杀掉。
  - 子进程看守：`START_STICKY` + 退出后 1s/2s/4s/8s/15s/30s 退避重启，活过 30s 清零退避。
  - 产物：`oc2api-android-<version>.apk`（arm64-v8a + x86_64），CI 内 `apksigner verify` + `sha256sum`，tag 推送时附到对应 Release。

- **Android GUI 美化（CMP-118）**：面板移出 app，改用系统浏览器打开；原 WebView 位置改日志视图；UI 美化（Material 3 深浅色主题、卡片分组、8dp 栅格、带色状态标识）；GUI 显示版本号（来自 git tag，单一来源）。
  - `activity_main.xml` + 新建 `res/values/themes.xml`：Material3 DayNight 主题，跟随系统深浅色。
  - `MainActivity.java`：删内嵌 WebView，「打开面板」按钮改 `ACTION_VIEW` 调系统浏览器；GUI 显示 `versionName`。
  - `ProxyService.java`：Go 进程日志写内存环形缓冲（最多 1000 行），UI 读取显示。
  - `AndroidManifest.xml`：补 `<queries>` 包可见性，允许调用系统浏览器打开 `http://127.0.0.1`。
  - 不迁 Compose，保持 Java + XML；日志缓冲有上限（1000 行/256KB），禁无界 List。

### 修复


- **403 `FreeTierError` 只换 key 不换出口，整类出口被拒时结构上救不回来**（CMP-130）。
  被拒的对象是出口时，「只换 key」这条路原地打转：CMP-128 实测 mihomo 两个节点池
  3/3 全 403、同刻同形状直连 7/7 全 200；现网日志 k31→k35 五把 key 全 403 即此形态
  —— 判据是出口，不在 key。
  - 新增 `rotateSocks5OnFreeTier403`：switch 模式复用 429 的轮转游标换腿；裸地址模式
    只有一条腿、无从轮转，**内存回落直连**（不落 `config.json`、重启即回原状，保留
    可回滚路径）。
  - `[free tier rejected]` 行新增 `exits=` 字段：本轮真实打过的出口标签序列。单元素 =
    单出口（换 key 无望）；多个不同元素仍全 403 = 整类出口被拒。
  - 反向用例钉住边界：**非** `FreeTierError` 签名的 403 单发透传，不重试、不换 key、
    不触发任何出口动作（CMP-109 结论的保护性反例）。

- **上游黑洞时进程假死：HTTP 客户端零超时 + 无界重试**（CMP-119）。`Transport` 零值 =
  无限等，补 `Dial` / `TLSHandshake` / `ResponseHeader` 三个超时；`fetchOCVersion` 的
  `context.Background()` 被 `sync.Once` 焊死，启动期改用带超时 context；`client.Do`
  错误分支从无条件 `continue` 改为有界重试 + 退避。

- **SSE 客户端断开后 handler 不退出，fd 与 goroutine 漏到整机停摆**（CMP-119）。真机
  症状是「端口仍 LISTEN + 进程还在 + CPU 静止 + 所有路由（含 `/health`）零响应」：
  两个 SSE handler 都丢弃写错误，客户端（面板刷新/切页）已走仍一路读到上游 EOF，
  上游流永不结束时就是永不返回，反复几次把 fd 吃光 → accept 停摆，curl 与裸 `nc`
  都只见 000。OpenAI 与 Claude 两处循环改为写错误即置 `clientGone` 退出，由调用点的
  defer 关闭上游 body。

- **`:latest` 镜像里的版本号不再是 `dev`（CMP-157）**。`:latest` 改为只由 release tag 产出，
  main 分支改打 `:dev`（二进制版本号仍注入 `dev`，未改 `VERSION` 计算）。
  - 现场根因不是 tag 推送没带 `latest`：v1.2.1 的 tag run（39124414931）**确实推了**
    `latest@sha256:10ac393a`，与 `1.2.1` 同 digest；而同一 commit 的 main run（39124415286）
    **晚 44s** 推 `latest@sha256:c909b297`（dev 版）把它覆盖了 —— 两 run 相隔 1s 并发，
    `concurrency` 按 ref 分组互不取消，谁后推完谁赢。
  - 因此 main 与 tag 同秒推送的既有发版习惯下，`:latest` 是掷硬币；让 main 不碰 `latest`
    才能结构性保证「latest = 最新 release」。

- **`Endpoint is unavailable` 不再被当限流连打 5~6 把凭据**（CMP-129）。该 body 上游用
  **429 与 403 两种状态码**回同一句话（CMP-86 修的是 403 形态，本次现场是 429 形态），
  但 429 形态落进「429 一律轮转」的窄判据：每个请求**换 5~6 把凭据连打**
  （现网实测 k1→k16 逐字同一句），且**不进熔断**，下一发照旧重来一遍。
  - 抽出 `isEndpointUnavailableUpstream(status, body)` 作**唯一**判定函数，
    `isRetryableQuotaUpstream` 的 403 分支改为调它 —— CMP-86 的判据表测试原样通过，
    不复制第二份判断。状态码只认 429/403 两种实测形态；仍只认 message、不认整个
    `type:"server_error"`（同 type 下 `Model is unavailable` 是模型已下线的永久错误）。
  - 该形态**先于** `advanceOCKey` 判定：单发只打一次上游、不换 key、不换出口，
    记一次**按模型熔断**后立即回客户端。熔断复用 CMP-83 的同一张表与阈值
    （threshold 3 / window 10min / hold ≤15min），只给条目多带 `clientStatus/clientBody`。
  - **对客户端的形态刻意与真配额可区分**（行为变更，注意）：
    - 真配额耗尽：**429** + `FreeUsageLimitError` + `Retry-After`（**原样不动**）
    - 端点不可用：**503** + `EndpointUnavailableError` + **不带** `Retry-After`
    理由：429 + 空 `Retry-After` 会让客户端当成「立刻可重试」而形成打转；而上游连配额窗口
    都没给，重发同一请求不会更快成功。熔断期满了自动放行去探活，上游一恢复即自愈。
  - 前提核验（2026-10-17 现网实测，非推测）：同一把凭据、同一镜像、串行 10 发单变量对照 ——
    直连腿 200×4 后转本形态；`__round_robin__` over `mihomo:20819` 腿经 mihomo 日志确认
    落 **10 个不同出口节点**、零 200。⇒ 换出口这条真修法在本形态上不成立。
  - CMP-86 原先让 403 形态「换出口/换凭据重试到预算用尽」，本卡按同一判定把它并入同一条
    熔断快速失败路径，两个状态码形态不再走两套行为；相关行为用例已按新行为改写
    （`TestIsRetryableQuotaUpstream` 判据表未动，原样通过）。
  - 单测：429 形态不断 key 且回 503、熔断打开后不打上游、403 形态同路径、流式入口同路径、
    以及「真配额 vs 端点不可用两形态可区分」的对照用例。

- **403 `FreeTierError` 不进自愈重试，单把失效凭据能卡死整机**（CMP-110，根因来自 CMP-109 分诊）。
  CMP-109 现场（2026-10-17 14:12–14:16 CST）：现网五个免费层模型同一时刻**全灭**，
  而容器 `healthy`、`/health=200`。受控金丝雀同窗口交错复现：池里 `k1/k2/k10` 回 200、
  `k43/k86` 稳定回 403 `FreeTierError`；「会话新鲜度」与「出口」两条假设各被单变量实验证伪。
  ⇒ 上游按**凭据**回这道门。原实现只在 `429` 时轮换凭据，403 落透传分支 —— 粘滞 key
  一旦撞上失效集合就整机 403，池中 80+ 把可用凭据全部闲置，且**不自愈**。
  - 新增窄判据 `isFreeTier403`：`403 && body 含 "FreeTierError"`，与 CMP-86 的
    `isRetryableQuotaUpstream`（429 / 403+`Endpoint is unavailable`）并列，两个入口
    （`callOpenCodeAPI` / `callOpenCodeAPIStream`）都纳入重试，预算沿用现有
    `socks5RateLimitAttemptCount()` + `keyPoolExtraAttempts()`，不引入无界重试。
  - **只换 key，不跟换出口/换会话**：那两条已被 CMP-109 实验 2/3 证伪，跟着换只会白烧额度。
  - 不宽成整个 `type:"server_error"`：同 type 下 `Model is unavailable` 是模型已下线的
    永久错误（实测 400），拖进重试同样白烧。试满后仍把**上游原始 403 + body** 透传。
  - 可观测性：`[free tier rejected]` 行补 `key_id=`（与 429 行对齐）—— 此前这道 403
    不带 key，排查时无法分辨「粘滞 key 撞上失效集合」与「门整体失效」。
- **403 形态的瞬时上游错误不进自愈重试，且零日志**（CMP-86，缺口来自 CMP-46 定位）。
  上游把 `Endpoint is unavailable`（`type:"server_error"`）有时回 429、有时回 403，
  body 逐字相同。429 形态本来就换出口/换凭据重试；403 形态却直接落透传分支 ——
  不换出口、不换凭据，且因 `[upstream error]` 只在 `-debug` 下打而**完全不可观测**。
  出口一旦抽到持续报错的节点，钉死模式下会无限期返回 403
  （CMP-46 实测：20818 腿连打 4 发全 403，同一时刻 20819 腿 2/2 全 200）。
  - 新增窄判据 `isRetryableQuotaUpstream`：`429 || (403 && body 含 "Endpoint is unavailable")`。
    **只认 message**，不认整个 `type:"server_error"` —— 同 type 下还有
    `Model is unavailable`（模型已下线，实质永久错误，实测回 400），拖进重试只会
    白烧凭据与出口；400 本轮不纳入。
  - 两个入口（`callOpenCodeAPI` / `callOpenCodeAPIStream`）改判据；循环体复用现有动作
    （`advanceOCKey` + `rotateSocks5OnRateLimit`），重试上限沿用
    `socks5RateLimitAttemptCount()` + `keyPoolExtraAttempts()`，不引入无界重试；
    配额类判据只在 429 下生效（403 形态没有「额度耗尽」语义，永远换 key）；
    试满后仍把**上游原始 status/body** 透传客户端（不改写成 502）。
  - 可观测性（独立缺口）：未进任何重试分支的上游非 2xx 一律**无条件**记一行
    （status + body），不再受 `-debug` 约束 —— 生产默认不开 `-debug`，此前这类错误
    在日志里完全不可见。
- **免费层全量 403 FreeTierError（2026-10-14 上游二次收紧）**：上游把免费层
  （Bearer public）从 `/zen` 整体迁到 `/inference`，并把门禁从「会话 ID 结构校验」
  升级为「按官方 2.x 客户端指纹逐字段校验」。同日 jasonxu114514/opencode2api#83、
  xbzbing/opencode2dsh#2、decolua/9router#4842 等多个下游独立复现并修复，
  本仓按其已验证的形状镜像（分歧处以 83 号单变量消融为准）：
  - 端点：匿名车道（`Bearer public`，含无 key_pool 的全部流量）改打
    `https://opencode.ai/inference/v1/*`；keyed（oc_sk_*）车道保留在 `/zen`。
    `fetchModels` 同步迁到 `/inference/v1/models`。
  - UA：由 `opencode/<ver> ai-sdk/... runtime/...` 改为三段式
    `opencode/latest/2.0.29/cli`（裸版本号与旧式 UA 都直接 403）。
  - 头：补 `x-opencode-session-id`、`x-opencode-client: cli`、
    `x-opencode-project`（裸 40hex）；去掉 `x-session-id`；不发送
    `x-opencode-request`（该头现身即拒）。`x-session-affinity`/`X-Session-Id` 保留。
  - 工具：官方认识的工具名必须携带真实定义（`opencode_tools.json`，抄自官方
    2.x 客户端，经 #83 抓包固化）——同名桩/截断描述都会被拒。核心五件改为
    shell/edit/glob/grep/read（2.x 命名），缺失补齐、同名桩整体替换；
    不认识的名字原样透传。`tool_choice:"none"` 会被拒，发送前剔除。
  - 会话 ID 反转时间戳编码（v1.0.3 引入）仍是 `ses_*` 值的必要形状，保留不变。
- `ocFreeTierHint` 诊断文案更新为 2026-10-14 版门禁清单，避免下次 403 再被
  旧的两道门描述误导。

### 测试

- CMP-119 回归四条：`TestFetchModelsDoesNotHangForever`、
  `TestHealthOutlivesBlackholedUpstream`、`TestUpstreamDialErrorRetryIsBounded`、
  `TestClaudeStreamStopsWhenClientDisconnects`（最后一条去掉修复即 5.00s 超时失败，
  已做变异验证）。
- CMP-130 回归：`TestFreeTier403RotatesExitOrFallsBackToDirect` 三条子用例，加两入口
  （`callOpenCodeAPI` / `callOpenCodeAPIStream`）「形状类 403 单发透传」反向断言。
- `TestFreeTierToolsGate` 契约更新：核心五件必须补齐、同名桩替换为真实定义、
  未知名保留（新增 shell 桩替换断言）。
- `TestIsRetryableQuotaUpstream` / `TestEndpointUnavailable403SamePath` /
  `TestEndpointUnavailableStreamSamePath` / `TestForbiddenNonTransientNotRetried`：钉住
  403 瞬时类换出口/换凭据重试到预算用尽且透传上游原始 403（非流式与流式两个入口各一条）、
  403 `FreeTierError` 与 400 `Model is unavailable` 都不重试；429 行为由既有
  `TestQuotaClass429StopsAtOneUpstreamCall` / `TestCredential429StillRotates` 回归。
  已做变异验证：摘掉 403 那一臂后前三条用例转红。
  （CMP-129 起 `TestTransient403RetriesThenPassesThrough` / `TestTransient403RetriesStream`
  已按新行为改写为同名的 `TestEndpointUnavailable403SamePath` /
  `TestEndpointUnavailableStreamSamePath`：403 形态不再轮转，改走熔断快速失败回 503。）

## [v1.2.5] - 2026-10-20

### 修复

- **容器日志时间漂 8 小时（CMP-190）**：runtime 镜像只有 `alpine + ca-certificates`、没有 zoneinfo，`TZ=Asia/Shanghai` 设了也无效，Go 的 `time.Local` 退回 UTC。二进制改为内嵌 `time/tzdata`，`main()` 启动时 `configureTimeLocal`：TZ 为空默认 `Asia/Shanghai`，TZ 有值按 TZ 走（含 POSIX `:` 前缀）；Dockerfile 同时补 `tzdata` 与 `ENV TZ=Asia/Shanghai`，容器内 `date` 也一致。单测 `TestConfigureTimeLocal` 钉住默认 / 覆盖 / 前缀三种情况。附带效果：每日统计的日切点随 `time.Local` 走（容器里原先是 UTC 00:00 ⇔ CST 08:00 切，现在按 CST 00:00 切）。

- **Android 首装无默认模型别名，任何模型调用 403**（CMP-189）：随包 `android/app/src/main/assets/config.json`
  的 `model_alias` 是空对象，而别名表为空即拒绝一切模型（`未在别名列表中配置`），
  于是新装用户必须先开面板手填别名才能用。现按现网 oraarm 的实际取值预置 7 条
  （`target_model` 带 `-free` 后缀），**只在首次启动落盘**，已装用户升级不覆盖其自有配置。

### 变更

- **UA 兜底地板 `2.0.32` → `2.1.1`（CMP-190）**：官方更新服务当前给 2.1.1（提卡时是 2.1.0，执行当晚上游又抬了一版），地板落后会打「UA 兜底地板已陈旧」告警。新增 `TestUAFloorStaleWarning` 钉住「live > 地板必须告警、live == 地板不告警」；README 写明抬地板触发条件（告警出现 ⇒ 抄 live 版本 + 同步 CHANGELOG 与断言）。

### 新增

- **合并到 main 自动出 Android 滚动开发包（CMP-190）**：`android-apk.yml` 增加 `push: branches: [main]`（`paths` 与 PR 过滤一致，纯文档提交不触发全量 NDK 构建）；main 构建 `versionName=dev.<run_number>`、`versionCode=<run_number>`，产物固定名 `oc2api-android-dev.apk` 覆盖预发布 `android-dev`（`prerelease=true`，release body 记 `commit` 与构建时间）。`v*` tag 路径语义不变：正式 Release 仍是 `oc2api-android-<version>.apk`，两者互不覆盖。

## [v1.2.4] - 2026-10-20

### 修复

- **运行时「内存回落直连」会被续命落盘 / 面板保存写回 `config.json`**（CMP-161）。
  403 整类被拒后回落直连只改内存，但 `configPostBody.merge()` 与 `GET /api/config`
  都从内存现值回填 `active_socks5` 后整份 `saveConfig()`，于是「重启即回原状」在
  「config 可写 + 配了 `oc_session_refresh_cmd`」的部署下失效——回落态跨重启存活。
  现在两处回填都改读 **config.json 磁盘现值**（磁盘读不到/解析失败才沿用内存兜底），
  运行时回落状态只留内存、不参与持久化。回归：`TestRuntimeFallbackNotPersistedByRefreshOrPanel`
  同时钉住续命落盘与面板 GET→POST 两条旁路。

### 构建 / CI

- **`workflow_dispatch` 在非 main 分支推镜像失败**（CMP-160）：新增可选输入 `image_tag`
  （小写、非法字符转 `-`、截 128 字符），留空则只构建不推送并打 notice；main 推送
  `:dev`、tag 推送 semver+latest 的语义未动。

### Android

- **自更新下载支持走代理**（CMP-159）：国网直连 GitHub 会卡死约 150s 后失败，Updater
  改为按配置走代理并在失败时快速报错，不再干等。

## [v1.0.4] - 2026-10-14

### 修复

- **会话续命每 30 分钟把 `config.json` 整份覆盖，出口配置被抹成零值**（现网复发，2026-10-14 14:18）。
  这是上面那条「出口配置被静默清空」的**另一条独立写路径**，v1.0.3 的修复没覆盖到，
  于是在生产上又发了一次：13:37:35 / 13:47:59 / 14:18:08 三次。
  - 根因在 `refreshOCSessionOnce`：它自己拼一个只填 4 个键的 `AppConfig` 字面量去
    `saveConfig`，没填的键按零值整份覆盖 —— `socks5_proxies` / `active_socks5` / `key_pool`
    一起消失。`model_alias` / `reasoning_effort_map` 是当时唯一还在字面量里的两个键，
    正好与现网残留的键完全对上，反向印证。
  - 危险在于**潜伏期**：运行中的容器启动时已把配置读进内存，日志里的 `exit=` 全程还是
    `OC出口 (mihomo:20819)`，健康检查一路报绿。故障要到下一次重启（发版 / 崩溃 / 宿主重启）
    才显形，那时 28882 已经变成 `exit=direct` 直连出墙 —— 与 CMP-38 同一形状的事故。
  - 改法不新增状态：落盘改走 `configPostBody.merge()`，复用 POST /api/config 那条
    「省略即沿用现值」的保护，本次只提交新的 `oc_session_id`，其余字段从内存现值回填。
    另两处 `saveConfig` 调用点（面板 POST、启动落盘）本来就是全量的，不受影响。
  - 自检 21 → 22。新增 `TestRefreshOCSessionKeepsProxyAndKeyPool` 打在
    「续命落盘后从磁盘读回」这个真实边界上，钉住 `active_socks5` / `socks5_proxies` /
    `key_pool` / `model_alias` 都不丢；已做变异验证：退回 4 字段字面量该用例转红，
    报错正是 `active_socks5="" socks5_proxies=[]`、`key_pool=<nil>`。

- **出口配置被静默清空，导致服务绕过 mihomo 直连出墙**（现网事故，2026-10-14）。
  `config.json` 里的 `socks5_proxies` / `active_socks5` 不知何时被清成空，容器日志随即
  从 `exit=OC出口 (mihomo:20819)` 变成 `exit=direct` —— 全量绕开代理直连，请求来源 IP
  与地理身份一起变了，而服务「看起来完全健康」。
  - 根因在 `POST /api/config`：请求体解到普通值时，「键缺失」与「显式置空」是同一个零值，
    服务端区分不了，于是按全量覆盖写回。v1.0.0 与 v1.0.3 先后给 `oc_session_id`、`key_pool`
    补了「省略即沿用」，**出口配置这两个字段一直没有同等待遇** —— 任何只带部分字段的回传
    （旧版面板页、脚本、别的客户端）都会把它们连同 `model_alias`、`reasoning_effort_map`
    一起清空。后两个比出口配置更隐蔽：`normalizeConfig` 先把 nil 补成空 map，
    `applyConfig` 里的 `!= nil` 保护因此完全失效。
  - 请求体这四个字段（外加既有的 `oc_session_id` / `key_pool`）统一改用指针，合进
    `configPostBody.merge()`：键缺失一律沿用现值，显式给出（哪怕是空数组 / 空串）才是「清空」。
    沿用走现有全局快照（`configMu` / `socks5Mu` / `currentKeyPool`），无新增状态。
  - 落盘那份同样扛得住重启 —— 只护内存不护文件的话，下次容器启动照样读回空配置。
  - 自检 19 → 21。`TestConfigPostOmittedFieldsKeepCurrent` 打的是真的
    `adminConfigHandler`（解码 + 合并 + 落盘全链路），不是把 body 结构体外抄一遍；
    已做变异验证：去掉沿用保护该用例如期转红。`TestConfigPostExplicitEmptyStillClears`
    守住反向 —— 显式置空必须真的清掉，否则这条保护会把出口代理变成不可撤销的。

## [v1.0.3] - 2026-10-14

### 新增

- **key_pool 多凭据轮转**：恢复上游 fork 重写时丢掉的凭据池能力。免费层限流维度实测为
  **（模型 × 凭据）**，单凭据的日预算就是整条链路的日预算；此前鉴权写死 `Bearer public`，
  等于把凭据维度钉死在 1，`429 FreeUsageLimitError` 无从消除。配置新增 `key_pool`
  （`enabled` / `strategy` / `max_retries` / `keys`），由 `buildOCRequest` 这唯一鉴权出口选凭据。
  - `strategy` 支持 `sticky`（按 `x-session-id` 粘住一个凭据，中途不换——每次换凭据上游都当
    新身份，可能反而多吃掉一份配额窗口）与 `round_robin`（每个请求推进游标，把负载摊到全池）。
  - 凭据额度耗尽（HTTP 429）时自动换下一条重试，上限 `max_retries`（默认 4）。
    **按状态码判定而不是匹配错误文案**：实测 49 条耗尽凭据里 45 条返回的是
    `{"error":{"type":"server_error","message":"...Endpoint is unavailable."}}`，
    只有 1 条是 `FreeUsageLimitError`，只认文案会漏掉绝大多数耗尽信号。
  - **未启用 / 池空 / 池内无启用凭据时一律回落到 `Bearer public`**，鉴权头与引入本字段之前
    逐字节一致，未配置 `key_pool` 的部署行为不变。
  - 面板保存配置时若请求体未带 `key_pool`，沿用现值而非清空：面板没有 key 管理 UI，
    不做「省略即沿用」的话运营点一次保存就会把整池凭据静默清空（与 v1.0.0 修过的
    `oc_session_id` 同一类问题）。
  - 诊断：429 日志行新增 `key_id=` 与 `next_key_id=`（对应老容器的 `msg=upstream_attempt ...
    key_id=kNN`），启用凭据池时另在启动与热更时打一行凭据数/策略，运营只凭日志即可确认轮转在工作。
- `keys` 条目兼容老容器 `config.example.json` 记录的两种写法：完整对象，或直接写字符串。
  缺 `id` 的条目按数组位置补 `k1..kN`（老容器的 `key_id` 就是位置号）。
  重复凭据在加载时去重——同一凭据出现两次不会多拿一份配额，只会白占轮转位。
- 自检 11 → 19 个用例。最关键的一条是 `TestKeyPoolAuthFallsBackToPublic`：
  5 种未启用/空池/全停用形态下鉴权头必须仍是 `Bearer public`，守住「不配 key_pool 就完全不变」
  这条底线。其余覆盖 sticky 粘住、轮转摊开、归一化去重补号、字符串简写、面板省略即沿用、
  重试次数上界。

### 修复

- **免费层 403 会话门：会话 ID 必须是反转时间戳，纯随机一律 403**。上游 2026-10-11 起
  （zen 路由改薄代理、免费层门禁下沉到私有 inference 服务）校验 session ID 的**语义**而
  非格式：前 12 位小写 hex 必须是 `~(ts_ms*0x1000 + counter)` 的低 48 位
  （对齐上游 `packages/schema/src/identifier.ts`，session 降序、message 升序）。
  原来的纯随机生成必然触发 `403 FreeTierError("OpenCode's free tier can only be used
  from within OpenCode")`，对全部免费模型生效。已照参考仓 `newOCShapeID` 移植
  （进程级 `ocIDMu / ocIDLastMilli / ocIDCounter`，同一毫秒内 counter 自增、跨毫秒归零）。
  背景实验矩阵见参考仓 `docs/labs/2026-10-13-free-tier-fingerprint-refresh.md`。
  - `oc_session_id` 注入能力**保留**（手动指定的会话优先，对「会话 sticky」有独立价值），
    但不再是能否服务的前提。
  - 未配置会话时不再返回 `503 OCSessionMissing`，改为自动生成一个进程内稳定的会话。
    相应的 `ocSessionGuard` / `ocSessionMissingBody` 与三个钉住 503 行为的用例一并删除
    —— 留着会变成永不触发的死路径，外加一句「所有请求将返回 503」的假告警。
  - 自检新增两条：`TestNewOCSessionIDShape`（形状正则，及格线）与
    `TestNewOCSessionIDDescending`（**语义**判据：反推前 12 位必须落在当前时间窗内）。
    后者做过阴性对照——把实现换回纯随机后形状用例仍绿、语义用例立刻变红，正是它防的退化。
  - 实现注记：`ts*0x1000` 会溢出 48 位（1.79e12 × 4096 ≈ 7.3e15 > 2^48），
    所以 ID 里携带的是**模 2^36 毫秒**的时间戳，这套编码约每 2.18 年回绕一次。

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
