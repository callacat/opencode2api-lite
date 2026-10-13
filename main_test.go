package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCompareVersion 覆盖上游版本门槛判定：UA 里的 opencode/<version> 低于
// minOCVersion 时上游返回 426，比较语义错了会在运行期才暴露，这里钉住。
func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.18.31", "1.17.0", 1},
		{"1.17.0", "1.17.0", 0},
		{"1.17", "1.17.0", 0}, // 缺失段按 0 补齐
		{"1.16.9", "1.17.0", -1},
		{"2.0.0", "1.99.99", 1},
	}
	for _, c := range cases {
		if got := compareVersion(c.a, c.b); got != c.want {
			t.Errorf("compareVersion(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestNormalizeOCVersion(t *testing.T) {
	if got := normalizeOCVersion("1.16.0"); got != minOCVersion {
		t.Errorf("低于门槛的版本应抬升到 %s，得到 %s", minOCVersion, got)
	}
	if got := normalizeOCVersion("1.18.31"); got != "1.18.31" {
		t.Errorf("达标版本应原样返回，得到 %s", got)
	}
}

// TestOCSessionInjection 钉住会话选取契约：配了 oc_session_id 就用配置值
// （固定会话对「会话 sticky」有独立价值，且已上线，不退回去）；
// 没配则自动生成反转时间戳会话——上游校验的是 ID 的时间戳结构而非「谁签发过」，
// 自动生成即可过门，不再 503。
func TestOCSessionInjection(t *testing.T) {
	const real = "ses_ecbad4897ffe524uCAgXevWcMc"

	applyConfig(AppConfig{OCSessionID: real})
	if got := ocCurrentSessionID(); got != real {
		t.Fatalf("注入真实会话后应原样使用，得到 %q", got)
	}
	if !hasInjectedOCSession() {
		t.Error("已注入会话时 hasInjectedOCSession 应为 true")
	}

	applyConfig(AppConfig{OCSessionID: ""})
	if hasInjectedOCSession() {
		t.Error("清空注入后 hasInjectedOCSession 应为 false")
	}
	generated := ocCurrentSessionID()
	if generated == "" {
		t.Fatal("未注入时应自动生成会话，不应返回空串（旧行为是 503）")
	}
	if !ocSessionRe.MatchString(generated) {
		t.Errorf("自动生成的会话应符合 ses_+26 位格式，得到 %q", generated)
	}
	// 进程内稳定：同一进程反复取应拿到同一个，避免每请求换一个身份
	if again := ocCurrentSessionID(); again != generated {
		t.Errorf("自动生成的会话应在进程内稳定，得到 %q 与 %q 不一致", again, generated)
	}
}

// TestNewOCSessionIDShape 断言生成值匹配 ^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$ ——
// 纯随机也能通过这条形状正则，所以它只是及格线，不是判据本身。
func TestNewOCSessionIDShape(t *testing.T) {
	for i := 0; i < 200; i++ {
		got := newOCSessionID()
		if !ocSessionRe.MatchString(got) {
			t.Fatalf("第 %d 次生成不符合形状: %q", i, got)
		}
	}
}

// TestNewOCSessionIDDescending 是防退化的关键判据：前 12 位 hex 必须带
// 反转时间戳结构 —— 即 ^(ts_ms*0x1000+counter) 的低 48 位。
// 只断言形状正则的话，把实现换回纯随机照样全绿，而线上会全量 403。
func TestNewOCSessionIDDescending(t *testing.T) {
	// 取当前毫秒基准：生成值的前 12 位应当反推回一个「最近的」时间戳。
	now := uint64(time.Now().UnixMilli())
	var decoded []uint64
	for i := 0; i < 50; i++ {
		id := newOCSessionID()
		var v uint64
		if _, err := fmt.Sscanf(id[4:16], "%012x", &v); err != nil {
			t.Fatalf("前 12 位不是 hex: %q", id)
		}
		// 上游取的是 ^(ts*0x1000+counter) 的**低 48 位**，反推也必须在 48 位宽度内
		// 取反补回来；直接对 64 位取反会把高 16 位补成 1，ts 全是垃圾。
		decoded = append(decoded, (v&0xFFFFFFFFFFFF)^0xFFFFFFFFFFFF)
	}
	// ts*0x1000 已经溢出 48 位（ts_ms≈1.79e12 × 4096 ≈ 7.3e15 > 2^48≈2.8e14），
	// 所以前 12 位里的时间戳是**模 2^36 毫秒**的。2^36 ms ≈ 795 天，
	// 也就是这套编码每 ~2.18 年回绕一次；反推时必须把回绕补回来。
	const tsWrap = uint64(1) << 36
	for i, d := range decoded {
		tsMod := d >> 12
		ts := tsMod + ((now-tsMod)/tsWrap+1)*tsWrap
		if ts > now {
			ts -= tsWrap
		}
		// 反推出的时间戳必须落在「刚才这一小段时间窗」内。
		// 容忍 ±60s：进程内时钟与用例读取时刻有差，且上游容忍度实测 ≥7 天。
		var diff int64
		if int64(ts) > int64(now) {
			diff = int64(ts) - int64(now)
		} else {
			diff = int64(now) - int64(ts)
		}
		if diff > 60_000 {
			t.Fatalf("第 %d 个 ID 反推出的时间戳 %d 距当前 %d 相差 %dms，不是 descending 编码（纯随机会长这样）",
				i, ts, now, diff)
		}
	}
	// 同一毫秒内连续生成的 counter 必须递增，否则 value 会撞车。
	first, second := newOCSessionID(), newOCSessionID()
	if first[:16] == second[:16] {
		t.Errorf("连续两次生成的前 12 位完全相同（%s），counter 未生效", first[:16])
	}
}

// TestOCSessionRe 钉住会话 id 的解析规则：只认规范长度 26 的 ses_*，
// 免得续命命令输出里的噪声被当成会话写进配置。
func TestOCSessionRe(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"created id=ses_ecbad4897ffe524uCAgXevWcMc", "ses_ecbad4897ffe524uCAgXevWcMc"},
		{"ses_aaaaaaaaaaaaa", ""},             // 太短
		{"ses_ZZZZZZZZZZZZZZZZZZZZZZZZZ", ""}, // 前 12 位非十六进制
		{"no session here", ""},
	}
	for _, c := range cases {
		got := ocSessionRe.FindString(c.in)
		if got != c.want {
			t.Errorf("ocSessionRe(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// TestRefreshOCSessionOnce 端到端验证续命：命令输出里的 ses_* 被采纳并写回内存。
// 只测内存态，不碰磁盘上的 config.json。
func TestRefreshOCSessionOnce(t *testing.T) {
	// refreshOCSessionOnce 会写回 config.json，指向临时目录避免污染仓库
	oldPath := configPath
	configPath = filepath.Join(t.TempDir(), "config.json")
	defer func() {
		configPath = oldPath
		applyConfig(AppConfig{})
		ocRefreshCmd = ""
	}()
	applyConfig(AppConfig{OCSessionID: "ses_ecbad4897ffe524uCAgXevWcMc"})

	// 未配置命令时应安静返回 false，不做任何事。
	if refreshOCSessionOnce() {
		t.Error("未配置续命命令时不应刷新成功")
	}

	const fresh = "ses_0123456789abABCDEFGHIJKLMN"
	if len(fresh) != len("ses_")+26 || !ocSessionRe.MatchString(fresh) {
		t.Fatalf("测试常量本身不合法（长度 %d）: %s", len(fresh), fresh)
	}
	ocRefreshCmd = "echo 'message=created id=" + fresh + "'"
	if !refreshOCSessionOnce() {
		t.Fatal("命令输出含合法 ses_* 时应刷新成功")
	}
	if got := ocCurrentSessionID(); got != fresh {
		t.Errorf("续命后应使用新会话，得到 %q", got)
	}
	// 同一个会话再跑一次不算更新。
	if refreshOCSessionOnce() {
		t.Error("输出与现值相同时不应算作刷新")
	}
}

// TestNormalizeConfigTrimsSession 确认配置里的会话 id 会被去空白，
// 避免面板里误粘的换行/空格直接导致上游 403。
func TestNormalizeConfigTrimsSession(t *testing.T) {
	cfg := AppConfig{OCSessionID: "  ses_abc  "}
	normalizeConfig(&cfg)
	if cfg.OCSessionID != "ses_abc" {
		t.Errorf("会话 id 应去空白，得到 %q", cfg.OCSessionID)
	}
}

// TestMergeOCSessionID 钉住面板回传配置的防丢行为：省略 oc_session_id 的请求体
// 不得把现值清空（免费层能否通过上游校验只取决于这一个字段），显式传值才覆盖，
// 显式传空串则允许清空。presence 判定必须由调用方用 *string 完成——
// 直接解到 string 时「省略」与「空」不可区分。
func TestMergeOCSessionID(t *testing.T) {
	const real = "ses_ecbad4897ffe524uCAgXevWcMc"
	applyConfig(AppConfig{OCSessionID: real})
	defer applyConfig(AppConfig{OCSessionID: ""})

	cases := []struct {
		name    string
		present bool
		val     string
		want    string
	}{
		{"省略该键应沿用现值", false, "", real},
		{"显式空串应允许清空", true, "", ""},
		{"显式新值应生效", true, "ses_xyz", "ses_xyz"},
	}
	for _, c := range cases {
		cfg := AppConfig{}
		mergeOCSessionID(&cfg, c.present, c.val)
		normalizeConfig(&cfg)
		if cfg.OCSessionID != c.want {
			t.Errorf("%s: 得到 %q, 期望 %q", c.name, cfg.OCSessionID, c.want)
		}
	}
}

// TestFreeTierToolsGate 钉住免费层的第二道门：请求体的 tools 必须同时含 bash 与 read。
//
// 这道门与 x-session-id 相互独立 —— 会话有效但 tools 被「精简」掉，上游照样回 403
// FreeTierError（2026-10-13 实测：无 tools / 缺 read / 缺 bash 均 403，bash+read 与四件套 200）。
// 修 403 时极易只查会话，故在此把四种 tools 形态都钉住：谁把补齐逻辑改坏，这里就红。
func TestFreeTierToolsGate(t *testing.T) {
	names := func(body map[string]any) map[string]bool {
		got := map[string]bool{}
		for _, raw := range body["tools"].([]any) {
			if n := toolNameOf(raw); n != "" {
				got[n] = true
			}
		}
		return got
	}

	cases := []struct {
		name string
		body map[string]any
		keep []string // 补齐后必须仍然存在的客户端原有工具
	}{
		{"客户端没带 tools", map[string]any{}, nil},
		{"[]Tool 形态（生产实际形态）", map[string]any{
			"tools": []Tool{{Type: "function", Function: ToolFunction{Name: "get_weather"}}},
		}, []string{"get_weather"}},
		{"[]any 形态（旧 JSON 往返路径）", map[string]any{
			"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}},
		}, []string{"get_weather"}},
		{"[]map[string]any 形态（仅扫描兼容，合并不保留）", map[string]any{
			"tools": []map[string]any{{"type": "function", "function": map[string]any{"name": "get_weather"}}},
		}, nil},
		{"客户端已自带 bash 但缺 read", map[string]any{
			"tools": []Tool{{Type: "function", Function: ToolFunction{Name: "bash"}}},
		}, []string{"bash"}},
	}

	for _, c := range cases {
		ensureFreeTierTools(c.body)
		got := names(c.body)
		if !got["bash"] || !got["read"] {
			t.Errorf("%s: 补齐后应同时含 bash 与 read，实际 %v", c.name, got)
		}
		// 客户端自带的工具不能被桩四件套挤掉（历史上真出过：模型抱怨「看不到 get_weather」）
		for _, k := range c.keep {
			if !got[k] {
				t.Errorf("%s: 补齐后丢了客户端原有工具 %q，实际 %v", c.name, k, got)
			}
		}
	}
}

// TestConfigPostDecodePresence 确认 json 解码确实能区分「键缺失」与「显式空串」，
// 这是 mergeOCSessionID 的 present 参数成立的前提。
func TestConfigPostDecodePresence(t *testing.T) {
	decode := func(body string) *string {
		var b struct {
			AppConfig
			OCSessionID *string `json:"oc_session_id"`
		}
		if err := json.Unmarshal([]byte(body), &b); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		return b.OCSessionID
	}
	if p := decode(`{"model_alias":{}}`); p != nil {
		t.Errorf("键缺失时应得 nil，得到 %q", *p)
	}
	if p := decode(`{"model_alias":{},"oc_session_id":""}`); p == nil || *p != "" {
		t.Errorf("显式空串应得非 nil 的空串，得到 %v", p)
	}
	if p := decode(`{"model_alias":{},"oc_session_id":"ses_a"}`); p == nil || *p != "ses_a" {
		t.Errorf("显式新值应原样解出，得到 %v", p)
	}
}

// ======================== key_pool 多凭据轮转 ========================

const testSession = "ses_ecbad4897ffe524uCAgXevWcMc"

func testPool(strategy string, keys ...string) *KeyPoolConfig {
	kp := &KeyPoolConfig{Enabled: true, Strategy: strategy}
	for i, k := range keys {
		kp.Keys = append(kp.Keys, KeyEntry{ID: "k" + string(rune('1'+i)), Key: k})
	}
	return kp
}

// authOf 走真实鉴权出口 buildOCRequest，取出 Authorization 头 —— 断言打在
// 唯一那条出路上，而不是内部辅助函数，否则重构就测不到真东西。
func authOf(t *testing.T) (auth, keyID string) {
	t.Helper()
	req, id, err := buildOCRequest(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatalf("构造上游请求失败: %v", err)
	}
	return req.Header.Get("Authorization"), id
}

// TestKeyPoolAuthFallsBackToPublic 是本卡最重要的一条防退化用例：
// 未启用 / 池为空 / 池里没有可用凭据时，鉴权必须**逐字节**回到 Bearer public。
// 悄悄改成别的东西 = 免费层直接退化成单凭据限流，且没有任何报错。
func TestKeyPoolAuthFallsBackToPublic(t *testing.T) {
	cases := []struct {
		name string
		kp   *KeyPoolConfig
	}{
		{"未配置 key_pool", nil},
		{"启用但池为空", &KeyPoolConfig{Enabled: true, Strategy: "sticky"}},
		{"停用但池里有凭据", &KeyPoolConfig{Enabled: false, Strategy: "sticky",
			Keys: []KeyEntry{{ID: "k1", Key: "oc_sk_a"}}}},
		{"停用且策略为轮转", &KeyPoolConfig{Enabled: false, Strategy: "round_robin",
			Keys: []KeyEntry{{ID: "k1", Key: "oc_sk_a"}}}},
		{"池里凭据全被 enabled:false 停用", &KeyPoolConfig{Enabled: true, Strategy: "sticky",
			Keys: []KeyEntry{{ID: "k1", Key: "oc_sk_a", Enabled: boolPtr(false)}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			applyConfig(AppConfig{OCSessionID: testSession, KeyPool: c.kp})
			auth, keyID := authOf(t)
			if auth != "Bearer public" {
				t.Errorf("应回落到 Bearer public，得到 %q", auth)
			}
			if keyID != "" {
				t.Errorf("未命中池时 keyID 应为空，得到 %q", keyID)
			}
		})
	}
	applyConfig(AppConfig{}) // 复位全局池，别漏给后面的用例
}

// TestKeyPoolAuthUsesPoolKey 钉住正向路径：启用且池非空时，鉴权头用的是池中
// 凭据而不是 public，且 key_id 与该凭据的配置 id 对得上（运营靠它判断轮转）。
func TestKeyPoolAuthUsesPoolKey(t *testing.T) {
	kp := testPool("sticky", "oc_sk_pool_one", "oc_sk_pool_two")
	applyConfig(AppConfig{OCSessionID: testSession, KeyPool: kp})
	auth, keyID := authOf(t)
	if auth != "Bearer oc_sk_pool_one" && auth != "Bearer oc_sk_pool_two" {
		t.Errorf("应使用池内凭据，得到 %q", auth)
	}
	if keyID != "k1" && keyID != "k2" {
		t.Errorf("key_id 应来自配置 id，得到 %q", keyID)
	}
	applyConfig(AppConfig{})
}

// TestKeyPoolStickyPinsSession 覆盖 issue 点名的语义：sticky 下同一会话连发多次
// 必须命中同一个 key —— 中途换 key 会让上游当成新身份，可能反而多吃掉一份配额窗口。
func TestKeyPoolStickyPinsSession(t *testing.T) {
	applyConfig(AppConfig{OCSessionID: testSession,
		KeyPool: testPool("sticky", "oc_sk_one", "oc_sk_two", "oc_sk_three")})
	first, _ := authOf(t)
	for i := 0; i < 5; i++ {
		if got, _ := authOf(t); got != first {
			t.Fatalf("sticky 下第 %d 次请求换了凭据：%q -> %q", i+2, first, got)
		}
	}
	// 额度耗尽后必须换凭据，否则 sticky 会永久钉死在一条死 key 上
	if next := advanceOCKey(testSession); next != "k2" {
		t.Errorf("advance 后应换到 k2，得到 %q", next)
	}
	if got, _ := authOf(t); got == first {
		t.Errorf("advance 之后应换凭据，仍是 %q", got)
	}
	applyConfig(AppConfig{})
}

// TestKeyPoolRoundRobinSpreads 轮转策略要把负载摊开，否则单条凭据先被打空。
func TestKeyPoolRoundRobinSpreads(t *testing.T) {
	applyConfig(AppConfig{OCSessionID: testSession,
		KeyPool: testPool("round_robin", "oc_sk_one", "oc_sk_two", "oc_sk_three")})
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		auth, _ := authOf(t)
		seen[auth] = true
	}
	if len(seen) != 3 {
		t.Errorf("轮转应把 6 次请求摊到 3 条凭据上，实际用到 %d 条", len(seen))
	}
	applyConfig(AppConfig{})
}

// TestNormalizeKeyPool 归一化三件事：重复凭据只留一条（重复项不增加配额，
// 只会白占轮转位 —— 现网 120 条里就有 34 条重复）、缺 id 按位置补 k1..kN、
// 停用项剔除。现网配置后 86 条根本没有 id 字段，补号是诊断行能对上的前提。
func TestNormalizeKeyPool(t *testing.T) {
	kp := &KeyPoolConfig{Enabled: true, Keys: []KeyEntry{
		{ID: "k1", Key: "oc_sk_dup"},
		{Key: "oc_sk_dup"},                          // 重复
		{Key: "oc_sk_b"},                            // 无 id → 按位置补
		{Key: "oc_sk_off", Enabled: boolPtr(false)}, // 停用
		{Key: "  oc_sk_c  "},                        // 带空白
	}}
	normalizeKeyPool(kp)
	if kp.Strategy != "sticky" {
		t.Errorf("策略缺省应为 sticky，得到 %q", kp.Strategy)
	}
	if kp.MaxRetries != keyPoolDefaultRetries {
		t.Errorf("max_retries 缺省应为 %d，得到 %d", keyPoolDefaultRetries, kp.MaxRetries)
	}
	var ids, keys []string
	for _, k := range kp.Keys {
		ids = append(ids, k.ID)
		keys = append(keys, k.Key)
	}
	if got, want := strings.Join(ids, ","), "k1,k3,k5"; got != want {
		t.Errorf("去重/补号后 id 序列 = %q，期望 %q（k2 重复、k4 停用应被剔除）", got, want)
	}
	if got, want := strings.Join(keys, ","), "oc_sk_dup,oc_sk_b,oc_sk_c"; got != want {
		t.Errorf("凭据序列 = %q，期望 %q（空白应被裁掉）", got, want)
	}
}

func boolPtr(b bool) *bool { return &b }

// TestKeyPoolPlainStringShorthand 兼容老容器 config.example.json 记录的写法：
// keys 里可以直接写字符串，不必包成 {"key": ...}。
func TestKeyPoolPlainStringShorthand(t *testing.T) {
	var kp KeyPoolConfig
	if err := json.Unmarshal([]byte(`{"enabled":true,"keys":["oc_sk_a",{"id":"named","key":"oc_sk_b"}]}`), &kp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	normalizeKeyPool(&kp)
	if len(kp.Keys) != 2 {
		t.Fatalf("应解出 2 条凭据，得到 %d", len(kp.Keys))
	}
	if kp.Keys[0].Key != "oc_sk_a" || kp.Keys[0].ID != "k1" {
		t.Errorf("字符串简写应补成 {key, id:k1}，得到 %+v", kp.Keys[0])
	}
	if kp.Keys[1].ID != "named" {
		t.Errorf("显式 id 应保留，得到 %q", kp.Keys[1].ID)
	}
}

// TestConfigPostPreservesKeyPool 钉住面板保存不清空凭据池：面板没有 key 管理 UI，
// 回传体里不会有 key_pool，解码必须能区分「键缺失」与「显式给出」，
// 缺失时 handler 沿用现值 —— 否则运营点一次保存就把整池凭据静默清空。
func TestConfigPostPreservesKeyPool(t *testing.T) {
	decode := func(body string) *KeyPoolConfig {
		var b struct {
			AppConfig
			KeyPool *KeyPoolConfig `json:"key_pool"`
		}
		if err := json.Unmarshal([]byte(body), &b); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		return b.KeyPool
	}
	if p := decode(`{"model_alias":{}}`); p != nil {
		t.Errorf("键缺失时应得 nil（handler 据此沿用现值），得到 %+v", p)
	}
	p := decode(`{"key_pool":{"enabled":false,"keys":[]}}`)
	if p == nil || p.Enabled {
		t.Errorf("显式给出应解出且能表达「停用」，得到 %+v", p)
	}
	p = decode(`{"key_pool":{"enabled":true,"keys":["oc_sk_x"]}}`)
	if p == nil || !p.Enabled || len(p.Keys) != 1 {
		t.Errorf("显式给出应原样解出，得到 %+v", p)
	}
}

// TestKeyPoolExtraAttemptsBound 换凭据的重试次数要有上界：池大不等于可以无限试，
// 每一次 429 重试都要等一个上游往返，放开会把首字节延迟拖到分钟级。
func TestKeyPoolExtraAttemptsBound(t *testing.T) {
	applyConfig(AppConfig{OCSessionID: testSession, KeyPool: testPool("sticky", "oc_sk_a", "oc_sk_b")})
	if got := keyPoolExtraAttempts(); got != 1 {
		t.Errorf("2 条凭据最多换 1 次，得到 %d", got)
	}
	applyConfig(AppConfig{OCSessionID: testSession, KeyPool: testPool("sticky", "oc_sk_a")})
	if got := keyPoolExtraAttempts(); got != 0 {
		t.Errorf("只有 1 条凭据时不应换键，得到 %d", got)
	}
	// 6 条凭据时 max_retries=4 才是约束（否则 n-1 才是约束，见上一断言）
	applyConfig(AppConfig{OCSessionID: testSession, KeyPool: testPool("sticky",
		"oc_sk_a", "oc_sk_b", "oc_sk_c", "oc_sk_d", "oc_sk_e", "oc_sk_f")})
	if got := keyPoolExtraAttempts(); got != keyPoolDefaultRetries {
		t.Errorf("凭据多于 max_retries 时应取 max_retries=%d，得到 %d", keyPoolDefaultRetries, got)
	}
	applyConfig(AppConfig{})
}
