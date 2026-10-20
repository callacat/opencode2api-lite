package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// TestParseOCUpdateAPIVersion 钉住权威版本源的解析：2.x 线只在官方更新服务
// （npm 分发件 @opencode/cli），registry.npmjs.org/opencode-ai 与 GitHub releases
// 都只给 1.19.x —— 拉错包会让 UA 变旧、直撞 floor 全量 403（2026-10-17 实测）。
func TestParseOCUpdateAPIVersion(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"优先 npm 分发件", `{"artifacts":[
			{"distribution":"aur","version":"2.0.33","active":true},
			{"distribution":"npm","version":"2.0.32","active":true}]}`, "2.0.32"},
		{"无 npm 则取 active", `{"artifacts":[
			{"distribution":"aur","version":"2.0.31","active":true},
			{"distribution":"opencode","version":"9.9.9","active":false}]}`, "2.0.31"},
		{"非 JSON", `<html>404</html>`, ""},
		{"空 artifacts", `{"artifacts":[]}`, ""},
	}
	for _, c := range cases {
		if got := parseOCUpdateAPIVersion([]byte(c.body)); got != c.want {
			t.Errorf("%s: parseOCUpdateAPIVersion = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestAcceptUAVersion 钉住丢弃规则：取不到、或取到比兜底地板还旧的版本，
// 一律不采用 —— 陈旧 UA 会吃 426，从不主动发。
func TestAcceptUAVersion(t *testing.T) {
	cases := []struct {
		ver  string
		want bool
	}{
		{"", false},
		{"1.19.3", false},        // npm 旧包线，低于地板
		{"2.0.31", false},        // 低于地板
		{ocUABakedVersion, true}, // 等于地板
		{"2.0.33", true},
	}
	for _, c := range cases {
		if got := acceptUAVersion(c.ver); got != c.want {
			t.Errorf("acceptUAVersion(%q) = %v, want %v", c.ver, got, c.want)
		}
	}
}

// TestUAVersionFollowsUpdateAPI 端到端（假上游，不打真实外网）：
// 取到版本 → UA 拼成 opencode/latest/<版本>/cli；
// 取不到 / 取到比地板旧 → 回退或保留兜底地板，绝不发更旧的版本号。
func TestUAVersionFollowsUpdateAPI(t *testing.T) {
	ocOnce.Do(func() {}) // 挡掉 initOCSession 里的真实外网探测

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("mode") {
		case "ok":
			fmt.Fprint(w, `{"artifacts":[{"distribution":"npm","version":"2.0.33","active":true}]}`)
		case "stale":
			fmt.Fprint(w, `{"artifacts":[{"distribution":"npm","version":"1.19.3","active":true}]}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	reset := func(t *testing.T, mode string) {
		t.Helper()
		old := ocUpdateAPIURL
		ocUpdateAPIURL = srv.URL + "/cli?mode=" + mode
		ocUAValue = atomic.Value{}
		t.Cleanup(func() { ocUpdateAPIURL = old })
	}

	t.Run("取到版本即拼装", func(t *testing.T) {
		reset(t, "ok")
		if got := ocClientUA(); got != "opencode/latest/"+ocUABakedVersion+"/cli" {
			t.Fatalf("冷缓存应先回兜底地板，得到 %q", got)
		}
		refreshOCUA()
		if got, want := ocClientUA(), "opencode/latest/2.0.33/cli"; got != want {
			t.Fatalf("UA 应拼成 %q，得到 %q", want, got)
		}
	})

	t.Run("取不到则保留当前", func(t *testing.T) {
		reset(t, "ok")
		refreshOCUA()
		ocUpdateAPIURL = srv.URL + "/down"
		refreshOCUA()
		if got, want := ocClientUA(), "opencode/latest/2.0.33/cli"; got != want {
			t.Fatalf("刷新失败应保留当前 UA，得到 %q", got)
		}
	})

	t.Run("比地板旧则丢弃", func(t *testing.T) {
		reset(t, "ok")
		refreshOCUA()
		ocUpdateAPIURL = srv.URL + "/stale"
		refreshOCUA()
		if got, want := ocClientUA(), "opencode/latest/2.0.33/cli"; got != want {
			t.Fatalf("低于地板的版本应被丢弃，得到 %q", got)
		}
	})

	t.Run("兜底地板不低于当前上游", func(t *testing.T) {
		reset(t, "down")
		if compareVersion(ocUABakedVersion, "2.0.32") < 0 {
			t.Fatalf("兜底地板 %s 低于当前上游 2.0.32，取不到版本时会全量 426", ocUABakedVersion)
		}
		if got, want := ocClientUA(), buildOCUA(ocUABakedVersion); got != want {
			t.Fatalf("冷启动兜底 UA 应为 %q，得到 %q", want, got)
		}
	})
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
	// 所以前 12 位里的时间戳是**模 2^36 毫秒**的。这套编码每 ~2.18 年回绕一次，
	// 因此「距 now 多久」只能在模 2^36 的环上量，不能用线性加减还原。
	// （先前按 now 线性加减还原的写法在 tsMod 越过 now%tsWrap 时会多减一次回绕，
	//  反推出 now-2^36+k、差值 ≈2^36 而误报——生成 50 个 ID 只要跨过回绕
	//  边界就会红，约 1/3 概率复现。）
	const tsWrap = int64(1) << 36
	nowMod := int64(now % uint64(tsWrap))
	for i, d := range decoded {
		// 把 delta 归一化到 (-2^35, 2^35]：过半个周期就说明走的是反方向，补一个周期。
		delta := int64(d>>12) - nowMod
		if delta > tsWrap/2 {
			delta -= tsWrap
		} else if delta < -tsWrap/2 {
			delta += tsWrap
		}
		// 容忍 ±60s：进程内时钟与用例读取时刻有差，且上游容忍度实测 ≥7 天。
		if diff := delta; diff > 60_000 || diff < -60_000 {
			t.Fatalf("第 %d 个 ID 反推出的时间戳距当前 %dms（模 2^36 = %dms），不是 descending 编码（纯随机会长这样）",
				i, diff, int64(d>>12))
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

// TestRefreshOCSessionKeepsProxyAndKeyPool 钉住续命写回 config.json 时不丢出口配置。
// 2026-10-14 14:18 实测复发：续命自己拼一个 4 字段 AppConfig 落盘，少填的键按零值
// 把整份文件覆盖掉，socks5_proxies/active_socks5/key_pool 一并消失。运行中的服务照旧
// 走 OC出口、健康检查全绿，直到某次重启才退化成 exit=direct 直连出墙。
func TestRefreshOCSessionKeepsProxyAndKeyPool(t *testing.T) {
	oldPath := configPath
	configPath = filepath.Join(t.TempDir(), "config.json")
	defer func() {
		configPath = oldPath
		applyConfig(AppConfig{Socks5Proxies: []Socks5Proxy{}, ActiveSocks5: ""})
		ocRefreshCmd = ""
	}()

	const fresh = "ses_0123456789abABCDEFGHIJKLMN"
	applyConfig(AppConfig{
		ModelAlias:    map[string]ModelAlias{"step-5-preview": {TargetModel: "step-5-preview-free"}},
		Socks5Proxies: []Socks5Proxy{{Addr: "mihomo:20819", Name: "OC出口"}},
		ActiveSocks5:  "mihomo:20819",
		OCSessionID:   testSession,
		KeyPool:       &KeyPoolConfig{Enabled: true, Keys: []KeyEntry{{ID: "k1", Key: "oc_sk_test"}}},
	})
	ocRefreshCmd = "echo 'message=created id=" + fresh + "'"
	if !refreshOCSessionOnce() {
		t.Fatal("命令输出含合法 ses_* 时应刷新成功")
	}

	saved := loadConfig(configPath)
	if saved.OCSessionID != fresh {
		t.Errorf("续命本意就是换会话，oc_session_id=%q 期望 %q", saved.OCSessionID, fresh)
	}
	if saved.ActiveSocks5 != "mihomo:20819" || len(saved.Socks5Proxies) != 1 {
		t.Errorf("续命把出口配置写丢了：active_socks5=%q socks5_proxies=%+v",
			saved.ActiveSocks5, saved.Socks5Proxies)
	}
	if saved.KeyPool == nil || len(saved.KeyPool.Keys) != 1 {
		t.Errorf("续命把 key_pool 写丢了：%+v", saved.KeyPool)
	}
	if len(saved.ModelAlias) != 1 {
		t.Errorf("续命把 model_alias 写丢了：%v", saved.ModelAlias)
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

// TestFreeTierToolsGate 钉住免费层门禁的工具侧契约（2026-10-14 版）：
//
//   - 核心五件 shell/edit/glob/grep/read 必须补齐（缺任一件即 403）；
//   - 官方认识的名字（如 shell）若客户端给了桩定义，会被真实定义替换——
//     描述子串匹配、参数逐字节相等的要求下，桩本身就是 403 信号；
//   - 官方不认识的名字（get_weather / bash 等）原样保留（bash 在 2.x 改名
//     shell，门禁按认识的名字校验，bash 不在其列则按未知工具放行）。
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
		{"客户端已自带 bash 但缺 core", map[string]any{
			"tools": []Tool{{Type: "function", Function: ToolFunction{Name: "bash"}}},
		}, []string{"bash"}},
	}

	for _, c := range cases {
		ensureFreeTierTools(c.body)
		got := names(c.body)
		for _, core := range []string{"shell", "edit", "glob", "grep", "read"} {
			if !got[core] {
				t.Errorf("%s: 补齐后缺核心工具 %q，实际 %v", c.name, core, got)
			}
		}
		// 客户端自带的工具不能被核心五件挤掉（历史上真出过：模型抱怨「看不到 get_weather」）
		for _, k := range c.keep {
			if !got[k] {
				t.Errorf("%s: 补齐后丢了客户端原有工具 %q，实际 %v", c.name, k, got)
			}
		}
	}

	// 同名桩必须被真实定义替换：上游对 description 做子串匹配、对 parameters 逐字节比对。
	stub := map[string]any{
		"tools": []Tool{{Type: "function", Function: ToolFunction{
			Name: "shell", Description: "stub", Parameters: map[string]any{"type": "object"},
		}}},
	}
	ensureFreeTierTools(stub)
	var description string
	switch tools := stub["tools"].(type) {
	case []any:
		for _, raw := range tools {
			m, _ := raw.(Tool)
			if m.Function.Name == "shell" {
				description = m.Function.Description
			}
		}
	}
	if genuine, ok := genuineToolDef("shell"); !ok || description != genuine.Description {
		t.Errorf("shell 桩应被替换为真实定义，得到 %q", description)
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

// TestConfigPostOmittedFieldsKeepCurrent 打的是真的 adminConfigHandler（含解码 +
// 合并 + 落盘），不是把 body 结构体外抄一遍。请求体省略的字段必须沿用现值：
// 少这层保护时，面板或脚本一次只带部分字段的回传就会把出口配置静默清空，
// config.json 里没了 socks5_proxies，容器重启后即 exit=direct 直连出墙。
func TestConfigPostOmittedFieldsKeepCurrent(t *testing.T) {
	old := configPath
	configPath = filepath.Join(t.TempDir(), "config.json")
	defer func() { configPath = old }()

	applyConfig(AppConfig{
		ModelAlias:         map[string]ModelAlias{"step-5-preview": {TargetModel: "step-5-preview-free"}},
		ReasoningEffortMap: map[string]string{"think": "high"},
		Socks5Proxies:      []Socks5Proxy{{Addr: "mihomo:20819", Name: "OC出口"}},
		ActiveSocks5:       "mihomo:20819",
		OCSessionID:        testSession,
	})
	post := func(body string) {
		t.Helper()
		w := httptest.NewRecorder()
		adminConfigHandler(w, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("POST %s = %d: %s", body, w.Code, w.Body.String())
		}
	}
	// 只带一个字段，其余全部省略 —— 面板没有 key 管理 UI、回传体残缺时就是这个形状
	post(`{"oc_session_id":"` + testSession + `"}`)

	socks5Mu.RLock()
	proxies, active := socks5Proxies, activeSocks5
	socks5Mu.RUnlock()
	if active != "mihomo:20819" || len(proxies) != 1 || proxies[0].Addr != "mihomo:20819" {
		t.Errorf("出口配置被静默清空：active_socks5=%q proxies=%+v", active, proxies)
	}
	configMu.RLock()
	alias, effort := modelAlias, reasoningEffortMap
	configMu.RUnlock()
	if len(alias) != 1 || len(effort) != 1 {
		t.Errorf("model_alias/reasoning_effort_map 被静默清空：alias=%v effort=%v", alias, effort)
	}
	// 落盘那份同样不能少：容器重启读的就是它，内存里保住不代表下次启动也保住
	saved := map[string]any{}
	if data, err := os.ReadFile(configPath); err != nil {
		t.Fatalf("读回落盘配置失败: %v", err)
	} else if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("落盘配置不是合法 JSON: %v", err)
	}
	if saved["active_socks5"] != "mihomo:20819" {
		t.Errorf("落盘 active_socks5=%v，出口配置没能扛过重启", saved["active_socks5"])
	}
	if list, _ := saved["socks5_proxies"].([]any); len(list) != 1 {
		t.Errorf("落盘 socks5_proxies=%v，出口配置没能扛过重启", saved["socks5_proxies"])
	}
}

// 沿用保护不能反过来越权：显式传空就是运营在面板上按了「清空」，必须真的清掉，
// 否则这条保护就把出口代理变成不可撤销的了。
func TestConfigPostExplicitEmptyStillClears(t *testing.T) {
	old := configPath
	configPath = filepath.Join(t.TempDir(), "config.json")
	defer func() { configPath = old }()

	applyConfig(AppConfig{
		ModelAlias:    map[string]ModelAlias{"step-5-preview": {TargetModel: "step-5-preview-free"}},
		Socks5Proxies: []Socks5Proxy{{Addr: "mihomo:20819", Name: "OC出口"}},
		ActiveSocks5:  "mihomo:20819",
		OCSessionID:   testSession,
	})
	w := httptest.NewRecorder()
	adminConfigHandler(w, httptest.NewRequest(http.MethodPost, "/api/config",
		strings.NewReader(`{"socks5_proxies":[],"active_socks5":""}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d: %s", w.Code, w.Body.String())
	}

	socks5Mu.RLock()
	proxies, active := socks5Proxies, activeSocks5
	socks5Mu.RUnlock()
	if active != "" || len(proxies) != 0 {
		t.Errorf("显式置空应真的清空：active_socks5=%q proxies=%+v", active, proxies)
	}
	// 显式给了 model_alias 就该覆盖；没给的那次（上一个用例）不该影响这里
	configMu.RLock()
	alias := modelAlias
	configMu.RUnlock()
	if len(alias) != 1 {
		t.Errorf("显式置空出口时不该顺带清掉 model_alias，得到 %v", alias)
	}
	applyConfig(AppConfig{OCSessionID: testSession})
}

// TestRuntimeFallbackNotPersistedByRefreshOrPanel 钉住 CMP-161：403 整类被拒后的
// 内存回落直连不得被「续命落盘」或「面板 GET→POST」旁路写进 config.json，否则
// 「重启即回原状」失效，回落态会跨重启存活。
func TestRuntimeFallbackNotPersistedByRefreshOrPanel(t *testing.T) {
	old := configPath
	configPath = filepath.Join(t.TempDir(), "config.json")
	defer func() {
		configPath = old
		ocRefreshCmd = ""
		applyConfig(AppConfig{Socks5Proxies: []Socks5Proxy{}, ActiveSocks5: ""})
	}()

	const exit = "mihomo:20819"
	persisted := AppConfig{
		Socks5Proxies: []Socks5Proxy{{Addr: exit, Name: "OC出口"}},
		ActiveSocks5:  exit,
		OCSessionID:   testSession,
	}
	if err := saveConfig(configPath, persisted); err != nil {
		t.Fatalf("准备磁盘配置失败: %v", err)
	}
	applyConfig(persisted)
	ocRefreshCmd = "echo 'message=created id=ses_0123456789abABCDEFGHIJKLMN'"

	// 模拟 403 整类被拒：内存回落直连，磁盘保持原出口。
	rotateSocks5OnFreeTier403(context.Background(), "OC出口 ("+exit+")")
	socks5Mu.RLock()
	active := activeSocks5
	socks5Mu.RUnlock()
	if active != "" {
		t.Fatalf("回落应只改内存，activeSocks5=%q", active)
	}

	// 续命落盘不得顺带把回落态写进去。
	if !refreshOCSessionOnce() {
		t.Fatal("续命应成功")
	}
	if got := loadConfig(configPath).ActiveSocks5; got != exit {
		t.Errorf("续命把回落态写进了 config.json：active_socks5=%q，期望 %q", got, exit)
	}

	// 面板「GET 回来原样 POST」：回读必须是磁盘值，保存后也不得变。
	w := httptest.NewRecorder()
	adminConfigHandler(w, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/config = %d", w.Code)
	}
	panelBody := w.Body.String()
	var readBack map[string]any
	if err := json.Unmarshal([]byte(panelBody), &readBack); err != nil {
		t.Fatalf("GET /api/config 不是合法 JSON: %v", err)
	}
	if readBack["active_socks5"] != exit {
		t.Errorf("面板回读应是磁盘值 %q，得到 %v", exit, readBack["active_socks5"])
	}
	w = httptest.NewRecorder()
	adminConfigHandler(w, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(panelBody)))
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/config = %d: %s", w.Code, w.Body.String())
	}
	if got := loadConfig(configPath).ActiveSocks5; got != exit {
		t.Errorf("面板保存把回落态写进了 config.json：active_socks5=%q，期望 %q", got, exit)
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

// ======================== 配额类 429（CMP-83） ========================
//
// 上游对「免费层模型日配额耗尽」回 429 + body.type=FreeUsageLimitError，Retry-After
// 精确指向次日 00:00 UTC（CMP-48 实测 94/94）。卡面判据 1/3：这类 429 不能换 key
// 换出口连试 5 次 —— 实测 5 凭据 × 2 出口 100% 仍 429，只把首字节延迟从 ~1s 拉到
// 8–22s。下面三个用例分别钉住：配额类只打 1 次上游、凭据级仍然轮转（反向证据，
// 命中 429 的 31 发里 27 发靠轮转救回，不能被这次改动误删）、同模型连撞即熔断。

const quotaTestBody = `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Rate limit exceeded. Please try again later."}}`

// fakeUpstream 把两条上游基址指到本地假上游并数命中次数。
// ocInferenceBase/ocZenBase 是包级 var 而非 const，就是为了这条缝（test-only 改动）。
func fakeUpstream(t *testing.T, handler http.HandlerFunc) (*atomic.Int64, func()) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	oldInference, oldZen := ocInferenceBase, ocZenBase
	ocInferenceBase, ocZenBase = srv.URL, srv.URL
	return &hits, func() {
		ocInferenceBase, ocZenBase = oldInference, oldZen
		srv.Close()
	}
}

// quotaCase 备好「池里有 6 条凭据 ⇒ 凭据级 429 本可换 4 次、共打 5 次上游」的前提。
func quotaCase(t *testing.T) {
	t.Helper()
	// 挡掉 initOCSession 里对 registry.npmjs.org 的版本探测：用例不该依赖外网。
	ocOnce.Do(func() {})
	quotaBreakerMu.Lock()
	quotaBreakers = map[string]*quotaBreakerEntry{}
	quotaBreakerMu.Unlock()
	applyConfig(AppConfig{OCSessionID: testSession, KeyPool: testPool("sticky",
		"oc_sk_a", "oc_sk_b", "oc_sk_c", "oc_sk_d", "oc_sk_e", "oc_sk_f")})
	t.Cleanup(func() { applyConfig(AppConfig{}) })
	if got := keyPoolExtraAttempts(); got == 0 {
		t.Fatalf("用例前提不成立：期望池内容许多试，得到 %d", got)
	}
}

func writeQuota429(w http.ResponseWriter, retryAfter string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", retryAfter)
	w.WriteHeader(http.StatusTooManyRequests)
	w.Write([]byte(quotaTestBody))
}

// TestQuotaClass429StopsAtOneUpstreamCall 判据 1/3：配额类 429 单发只打一次上游，
// 不换 key 换出口，并把上游 429 + Retry-After 原样透传。
func TestQuotaClass429StopsAtOneUpstreamCall(t *testing.T) {
	quotaCase(t)
	hits, restore := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeQuota429(w, "33553") // 到次日 00:00 UTC 的剩余秒数（CMP-48 实测形态）
	})
	defer restore()

	body, status, hdr, err := callOpenCodeAPI(context.Background(),
		map[string]any{"model": "space-bunny-free", "stream": true}, "space-bunny-free")
	if err == nil {
		t.Fatal("配额类 429 应以失败返回，得到 nil error")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("配额类 429 只该打 1 次上游，实际 %d 次（判据 1/3 回归）", got)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("应透传上游 429，得到 %d", status)
	}
	if got := hdr.Get("Retry-After"); got != "33553" {
		t.Fatalf("Retry-After 应原样透传，得到 %q", got)
	}
	if !strings.Contains(string(body), "FreeUsageLimitError") {
		t.Fatalf("应透传上游错误体，得到 %s", body)
	}
}

// TestCredential429StillRotates 是判据 1 的反向对照：没有 FreeUsageLimitError 指纹、
// Retry-After 只有 1s 的 429 属凭据级/瞬时类，必须继续换凭据重试到预算用尽。
func TestCredential429StillRotates(t *testing.T) {
	quotaCase(t)
	hits, restore := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"slow down"}}`))
	})
	defer restore()

	if _, status, _, _ := callOpenCodeAPI(context.Background(),
		map[string]any{"model": "space-bunny-free"}, "space-bunny-free"); status != http.StatusTooManyRequests {
		t.Fatalf("凭据级 429 试满后仍应回 429，得到 %d", status)
	}
	want := int64(keyPoolDefaultRetries + 1) // 首试 + max_retries 次换凭据
	if got := hits.Load(); got != want {
		t.Fatalf("凭据级 429 应轮转重试到预算用尽（%d 次上游），实际 %d 次", want, got)
	}
}

// TestQuotaBreakerFastFailsAfterThreshold 判据 2：同模型连撞到阈值后，
// 后续请求连上游都不打，直接回 429 + 缓存的 Retry-After。
func TestQuotaBreakerFastFailsAfterThreshold(t *testing.T) {
	quotaCase(t)
	hits, restore := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeQuota429(w, "33553")
	})
	defer restore()

	for i := 1; i <= quotaBreakerThreshold; i++ {
		if _, status, _, _ := callOpenCodeAPI(context.Background(),
			map[string]any{"model": "space-bunny-free"}, "space-bunny-free"); status != http.StatusTooManyRequests {
			t.Fatalf("第 %d 发撞配额 429，应回 429，得到 %d", i, status)
		}
	}
	if got := hits.Load(); got != int64(quotaBreakerThreshold) {
		t.Fatalf("熔断前每发应各打 1 次上游，实际 %d 次", got)
	}

	before := hits.Load()
	body, status, hdr, err := callOpenCodeAPI(context.Background(),
		map[string]any{"model": "space-bunny-free"}, "space-bunny-free")
	if got := hits.Load(); got != before {
		t.Fatalf("熔断打开后不该再打上游，多打了 %d 次", got-before)
	}
	if err == nil || status != http.StatusTooManyRequests {
		t.Fatalf("熔断快失败应回 429 + error，得到 status=%d err=%v", status, err)
	}
	if got := hdr.Get("Retry-After"); got != "33553" {
		t.Fatalf("熔断快失败应带最后一次观测到的 Retry-After，得到 %q", got)
	}
	if !strings.Contains(string(body), "FreeUsageLimitError") {
		t.Fatalf("熔断快失败的 body 应与上游同形，得到 %s", body)
	}
}

// ======================== 瞬时上游错误的 403 形态（CMP-86） ========================
//
// 上游把 `Endpoint is unavailable` 有时回 429、有时回 403，body 逐字相同（CMP-46）。
// 403 形态原先落透传分支：不换出口、不换凭据、且零日志 —— 出口抽到持续报错的节点
// 会永久卡死。判据必须只认 message：同一个 type:"server_error" 下 `Model is unavailable`
// 是模型已下线的永久错误，拖进重试只会白烧额度。
//
// CMP-129 起本节的行为用例改道：两个形态统一归入「端点/模型级不可用」→ 按模型熔断
// 快速失败（见下一节）。但**判定函数本身没变**，`TestIsRetryableQuotaUpstream` 仍是
// CMP-86 留下的判据表，原样通过。

const transientBody = `{"error":{"type":"server_error","message":"Upstream request failed: Endpoint is unavailable."}}`
const modelGoneBody = `{"error":{"type":"server_error","message":"Model is unavailable."}}`

func TestIsRetryableQuotaUpstream(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"429 一律重试", 429, `{"error":{"message":"slow down"}}`, true},
		{"403 + Endpoint is unavailable（瞬时）", 403, transientBody, true},
		{"403 + FreeUsageLimitError（不是瞬时类；配额语义只认 429）", 403, quotaTestBody, false},
		{"403 + 其他 body", 403, `{"error":{"message":"forbidden"}}`, false},
		{"400 + Model is unavailable（模型已下线，永久错误）", 400, modelGoneBody, false},
		{"200 正常响应", 200, `{"ok":true}`, false},
	}
	for _, c := range cases {
		if got := isRetryableQuotaUpstream(c.status, []byte(c.body)); got != c.want {
			t.Errorf("%s：期望 %v，得到 %v", c.name, c.want, got)
		}
	}
}

func writeTransient403(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	w.Write([]byte(transientBody))
}

func writeTransient429(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	w.Write([]byte(transientBody))
}

// ======================== 端点/模型级不可用（CMP-129） ========================
//
// 上游把同一份 `Endpoint is unavailable` body 用 429/403 两种状态码回过
// （CMP-46 记录 403 形态，CMP-129 现场是 429 形态），所以两个形态走同一个判定函数。
//
// 为什么不再轮转（2026-10-17 现网实测，非推测）：
//   - 现网同一时刻 k1→k16 逐字同一句，连打 16 把凭据零变化；
//   - 同一把 key 在 10 个**不同** OC 出口节点上也零救回（direct 腿 200×4 后转本形态，
//     OC 腿 10 发 403/真配额混杂、零 200）—— 换出口这条真修法在本形态上不成立。
// ⇒ 单发只打一次上游、记按模型熔断；回 503 EndpointUnavailableError，与配额耗尽的
//   429 FreeUsageLimitError 可区分（后者带 Retry-After）。429 + 空 Retry-After 会让
//   客户端立刻重试打转，这是本形态必须单独成型的理由。

// TestEndpointUnavailable429StopsAtOneUpstreamCall 判据 1：429 形态单发只打一次上游、
// 不换 key、不进「换出口重试」，并回可与真配额区分的 503。
func TestEndpointUnavailable429StopsAtOneUpstreamCall(t *testing.T) {
	quotaCase(t)
	hits, restore := fakeUpstream(t, writeTransient429)
	defer restore()

	body, status, hdr, err := callOpenCodeAPI(context.Background(),
		map[string]any{"model": "space-bunny-free", "stream": true}, "space-bunny-free")
	if err == nil {
		t.Fatal("端点不可用应以失败返回，得到 nil error")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("端点不可用只该打 1 次上游（不轮转凭据），实际 %d 次", got)
	}
	if k, ok := pickOCKey(testSession); !ok || k.id != "k1" {
		t.Fatalf("不该轮转凭据，会话应仍绑在 k1，得到 %q", k.id)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("应回 503（与真配额 429 可区分），得到 %d", status)
	}
	if got := hdr.Get("Retry-After"); got != "" {
		t.Fatalf("端点不可用不该带 Retry-After（空值会让客户端打转），得到 %q", got)
	}
	if !strings.Contains(string(body), "EndpointUnavailableError") {
		t.Fatalf("body 应是端点不可用形态，得到 %s", body)
	}
}

// TestEndpointUnavailableOpensBreaker 判据 3：同模型连撞到阈值后熔断，
// 后续请求连上游都不打，快失败仍是 503 形态（不是配额的 429）。
func TestEndpointUnavailableOpensBreaker(t *testing.T) {
	quotaCase(t)
	hits, restore := fakeUpstream(t, writeTransient429)
	defer restore()

	for i := 1; i <= quotaBreakerThreshold; i++ {
		if _, status, _, _ := callOpenCodeAPI(context.Background(),
			map[string]any{"model": "space-bunny-free"}, "space-bunny-free"); status != http.StatusServiceUnavailable {
			t.Fatalf("第 %d 发撞端点不可用，应回 503，得到 %d", i, status)
		}
	}
	if got := hits.Load(); got != int64(quotaBreakerThreshold) {
		t.Fatalf("熔断前每发应各打 1 次上游，实际 %d 次", got)
	}

	before := hits.Load()
	body, status, _, err := callOpenCodeAPI(context.Background(),
		map[string]any{"model": "space-bunny-free"}, "space-bunny-free")
	if got := hits.Load(); got != before {
		t.Fatalf("熔断打开后不该再打上游，多打了 %d 次", got-before)
	}
	if err == nil || status != http.StatusServiceUnavailable {
		t.Fatalf("熔断快失败应回 503 + error，得到 status=%d err=%v", status, err)
	}
	if !strings.Contains(string(body), "EndpointUnavailableError") {
		t.Fatalf("熔断快失败应是端点不可用形态，得到 %s", body)
	}
}

// TestEndpointUnavailable403SamePath 判据 3 的 403 侧：CMP-86 当初让 403 形态进轮转，
// 现按同一判定归入同一条熔断快速失败路径（两个状态码形态不再走两套行为）。
func TestEndpointUnavailable403SamePath(t *testing.T) {
	quotaCase(t)
	hits, restore := fakeUpstream(t, writeTransient403)
	defer restore()

	_, status, _, _ := callOpenCodeAPI(context.Background(),
		map[string]any{"model": "space-bunny-free"}, "space-bunny-free")
	if got := hits.Load(); got != 1 {
		t.Fatalf("403 形态同样只该打 1 次上游，实际 %d 次", got)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("403 形态应回 503，得到 %d", status)
	}
}

// TestEndpointUnavailableStreamSamePath：判据在流式入口同样生效（两处是各自独立的循环）。
func TestEndpointUnavailableStreamSamePath(t *testing.T) {
	quotaCase(t)
	hits, restore := fakeUpstream(t, writeTransient429)
	defer restore()

	rc, status, _, _ := callOpenCodeAPIStream(context.Background(),
		map[string]any{"model": "space-bunny-free"}, "space-bunny-free")
	defer rc.Close()
	if got := hits.Load(); got != 1 {
		t.Fatalf("流式入口也只该打 1 次上游，实际 %d 次", got)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("流式入口应回 503，得到 %d", status)
	}
	b, _ := io.ReadAll(rc)
	if !strings.Contains(string(b), "EndpointUnavailableError") {
		t.Fatalf("流式入口应是端点不可用形态，得到 %s", b)
	}
}

// TestQuotaAndEndpointUnavailableAreDistinguishable 判据 2 + 4：同一个模型、同一个
// 熔断计数下，两种 429 必须回出**可区分**的东西 —— 真配额 429 FreeUsageLimitError，
// 端点不可用 503 EndpointUnavailableError。改一个判据不能把另一个砸掉。
func TestQuotaAndEndpointUnavailableAreDistinguishable(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantBody   string
	}{
		{"真配额 429（CMP-83 路径，回归）", http.StatusTooManyRequests, quotaTestBody, http.StatusTooManyRequests, "FreeUsageLimitError"},
		{"端点不可用 429（CMP-129 路径）", http.StatusTooManyRequests, transientBody, http.StatusServiceUnavailable, "EndpointUnavailableError"},
	}
	for _, c := range cases {
		quotaCase(t)
		hits, restore := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(c.status)
			w.Write([]byte(c.body))
		})
		body, status, _, err := callOpenCodeAPI(context.Background(),
			map[string]any{"model": "space-bunny-free", "stream": true}, "space-bunny-free")
		restore()
		if err == nil || status != c.wantStatus {
			t.Errorf("%s：期望 status=%d + error，得到 status=%d err=%v", c.name, c.wantStatus, status, err)
		}
		if !strings.Contains(string(body), c.wantBody) {
			t.Errorf("%s：body 期望含 %s，得到 %s", c.name, c.wantBody, body)
		}
		if got := hits.Load(); got != 1 {
			t.Errorf("%s：两种形态都只该打 1 次上游，实际 %d 次", c.name, got)
		}
	}
}

// TestForbiddenNonTransientNotRetried：403 里只认 `Endpoint is unavailable`，
// `Model is unavailable`（模型已下线）重试无意义。
// 注：`403 FreeTierError` 原在此列，CMP-110 依据 CMP-109 的受控实验改判为凭据级门，
// 已移到下面的「凭据级 403 FreeTierError」一节。
func TestForbiddenNonTransientNotRetried(t *testing.T) {
	quotaCase(t)
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"400 Model is unavailable（模型已下线）", http.StatusBadRequest, modelGoneBody},
	}
	for _, c := range cases {
		hits, restore := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			w.Write([]byte(c.body))
		})
		_, status, _, err := callOpenCodeAPI(context.Background(),
			map[string]any{"model": "space-bunny-free"}, "space-bunny-free")
		restore()
		if got := hits.Load(); got != 1 {
			t.Errorf("%s：不该重试，实际打了 %d 次上游", c.name, got)
		}
		if err == nil || status != c.status {
			t.Errorf("%s：应透传上游 %d，得到 status=%d err=%v", c.name, c.status, status, err)
		}
	}
}

// ======================== 凭据级 403 FreeTierError（CMP-110） ========================
//
// CMP-109 现场（2026-10-17 14:12–14:16 CST）：现网五个免费层模型同一时刻全灭，
// 而容器 healthy、/health=200。受控金丝雀同窗口交错复现：池里 k1/k2/k10 回 200、
// k43/k86 稳定回 403 FreeTierError；会话新鲜度与出口两条假设各被单变量实验证伪。
// ⇒ 上游按**凭据**回这道门。原实现只认 429，403 落透传分支 —— 粘滞 key 撞上失效
// 集合就整机 403，池中其余凭据全闲置且不自愈。
// 下面钉住三件事：换 key 后能救回、预算用尽仍透传上游原始响应、429 配额路径不回归
// （配额 429 的回归在上一节 TestQuotaClass429StopsAtOneUpstreamCall）。

const freeTier403Body = `{"error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used with the official client."}}`

const okSSE = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"x\"," +
	"\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"pong\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"x\"," +
	"\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]," +
	"\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

func TestIsFreeTier403(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"403 + FreeTierError", 403, freeTier403Body, true},
		{"403 + Endpoint is unavailable（瞬时类，走另一条路）", 403, transientBody, false},
		{"403 + 其他 body", 403, `{"error":{"message":"forbidden"}}`, false},
		{"429 + FreeUsageLimitError（配额类）", 429, quotaTestBody, false},
		{"400 + Model is unavailable（永久错误）", 400, modelGoneBody, false},
	}
	for _, c := range cases {
		if got := isFreeTier403(c.status, []byte(c.body)); got != c.want {
			t.Errorf("%s：期望 %v，得到 %v", c.name, c.want, got)
		}
	}
}

func writeFreeTier403(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	w.Write([]byte(freeTier403Body))
}

// TestFreeTier403RotatesKeyThenSucceeds 是本卡的核心用例：失效 key 打头时，
// 第二发必须**换到另一把凭据**并且请求最终成功 —— 这正是 CMP-109 现场缺的自愈。
func TestFreeTier403RotatesKeyThenSucceeds(t *testing.T) {
	quotaCase(t)
	var mu sync.Mutex
	var auths []string
	hits, restore := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		n := len(auths)
		mu.Unlock()
		if n == 1 {
			writeFreeTier403(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(okSSE))
	})
	defer restore()

	body, status, _, err := callOpenCodeAPI(context.Background(),
		map[string]any{"model": "space-bunny-free", "stream": true}, "space-bunny-free")
	if err != nil || status != http.StatusOK {
		t.Fatalf("换 key 后应成功，得到 status=%d err=%v body=%s", status, err, body)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("应以第 2 发成功，实际打了 %d 次上游", got)
	}
	if len(auths) != 2 || auths[0] == auths[1] {
		t.Fatalf("第二发必须换到另一把凭据，实际 Authorization=%v", auths)
	}
	if !strings.Contains(string(body), "pong") {
		t.Fatalf("应还原出上游内容，得到 %s", body)
	}
}

// TestFreeTier403RetriesStream：判据在流式入口同样生效（两处是各自独立的循环）。
func TestFreeTier403RetriesStream(t *testing.T) {
	quotaCase(t)
	hits, restore := fakeUpstream(t, writeFreeTier403)
	defer restore()

	rc, status, _, _ := callOpenCodeAPIStream(context.Background(),
		map[string]any{"model": "space-bunny-free"}, "space-bunny-free")
	defer rc.Close()
	if got := hits.Load(); got != int64(keyPoolDefaultRetries+1) {
		t.Fatalf("流式入口的 403 FreeTierError 也应换 key 重试到预算用尽（%d 次上游），实际 %d 次",
			keyPoolDefaultRetries+1, got)
	}
	if status != http.StatusForbidden {
		t.Fatalf("流式入口试满后应透传上游原始 403，得到 %d", status)
	}
	b, _ := io.ReadAll(rc)
	if !strings.Contains(string(b), "FreeTierError") {
		t.Fatalf("流式入口应透传上游原始错误体，得到 %s", b)
	}
}

// TestFreeTier403PassesThroughWhenBudgetExhausted：池里全是失效凭据时，
// 试满重试预算后仍把**上游原始 403 + body** 透传（不得改写成 502、不得无限重试）。
func TestFreeTier403PassesThroughWhenBudgetExhausted(t *testing.T) {
	quotaCase(t)
	hits, restore := fakeUpstream(t, writeFreeTier403)
	defer restore()

	body, status, _, err := callOpenCodeAPI(context.Background(),
		map[string]any{"model": "space-bunny-free"}, "space-bunny-free")
	if got, want := hits.Load(), int64(keyPoolDefaultRetries+1); got != want {
		t.Fatalf("应换 key 试满预算（%d 次上游），实际 %d 次", want, got)
	}
	if err == nil || status != http.StatusForbidden {
		t.Fatalf("试满后应回上游原始 403 + error，得到 status=%d err=%v", status, err)
	}
	if !strings.Contains(string(body), "FreeTierError") {
		t.Fatalf("应透传上游原始错误体，得到 %s", body)
	}
}

// TestFreeTier403RotatesExitOrFallsBackToDirect 是本卡的核心用例（CMP-130）：
// 403 FreeTierError **不能只换 key**。CMP-128 实测被拒的是「整类出口」（mihomo 两个
// 节点池 3/3 全 403，同刻直连 7/7 全 200），此时换 key 结构上无效 —— 现网日志
// k31→k32→k33→k34→k35 五把 key 全 403 即此形态。修法是 403 也换出口：
//   - 裸地址模式只有一条腿、无从轮转 → 内存回落直连（不落 config，重启即回原状）；
//   - switch 模式复用 429 的轮转游标换到下一条腿；
//   - 已经在直连上则不动（没有可回落的地方）。
func TestFreeTier403RotatesExitOrFallsBackToDirect(t *testing.T) {
	saveSocks := func() (string, []Socks5Proxy) {
		socks5Mu.RLock()
		defer socks5Mu.RUnlock()
		return activeSocks5, append([]Socks5Proxy(nil), socks5Proxies...)
	}
	restore := func(active string, proxies []Socks5Proxy) {
		socks5Mu.Lock()
		defer socks5Mu.Unlock()
		activeSocks5, socks5Proxies = active, proxies
		atomic.StoreUint32(&socks5RateLimitIndex, 0)
	}
	oldActive, oldProxies := saveSocks()
	defer restore(oldActive, oldProxies)

	proxies := []Socks5Proxy{{Name: "OC出口", Addr: "mihomo:20819"}, {Name: "NVIDIA出口", Addr: "mihomo:20818"}}

	t.Run("裸地址模式回落直连", func(t *testing.T) {
		restore("mihomo:20819", proxies)
		rotateSocks5OnFreeTier403(context.Background(), "OC出口 (mihomo:20819)")
		socks5Mu.RLock()
		got := activeSocks5
		socks5Mu.RUnlock()
		if got != "" {
			t.Fatalf("整类出口被拒后应回落直连（activeSocks5 置空），实际 %q", got)
		}
	})

	t.Run("已在直连上不动", func(t *testing.T) {
		restore("", proxies)
		rotateSocks5OnFreeTier403(context.Background(), "direct")
		socks5Mu.RLock()
		got := activeSocks5
		socks5Mu.RUnlock()
		if got != "" {
			t.Fatalf("已经在直连上应保持不变，实际 %q", got)
		}
	})

	t.Run("switch 模式换到下一条腿", func(t *testing.T) {
		for _, mode := range []string{socks5RateLimitSwitch, socks5RateLimitSwitchNoDirect} {
			restore(mode, proxies)
			atomic.StoreUint32(&socks5RateLimitIndex, 0)
			// 用 socks5ExitLabelLocked 取标签：它与生产日志 `exit=` / `[rate-limit proxy switch]`
			// 那一行同源，是「这一发实际走了哪条腿」的权威口径。
			// (__rate_limit_switch__ 的 idx 0 是 direct、no_direct 的 idx 0 是第 1 个代理。)
			socks5Mu.RLock()
			first := socks5ExitLabelLocked(0, mode == socks5RateLimitSwitch)
			socks5Mu.RUnlock()
			rotateSocks5OnFreeTier403(context.Background(), first)
			if nextIdx := int(atomic.LoadUint32(&socks5RateLimitIndex)); nextIdx != 1 {
				t.Errorf("%s：轮转游标应从 0 推到 1，实际 %d", mode, nextIdx)
			}
			socks5Mu.RLock()
			next := socks5ExitLabelLocked(int(atomic.LoadUint32(&socks5RateLimitIndex)), mode == socks5RateLimitSwitch)
			socks5Mu.RUnlock()
			if next == first {
				t.Errorf("%s：轮转后应换到另一条腿，实际仍是 %q", mode, next)
			}
		}
	})
}

// 注：「只换 key、不换出口」这条策略已随 CMP-130 撤销（403 现在也换出口，
// 见 TestFreeTier403RotatesExitOrFallsBackToDirect）。出口真正连通后的端到端行为
// 由假上游金丝雀按出口判 403/200 覆盖 —— 单测里 switch 模式会把请求交给
// SOCKS5 dialer，假上游够不着。

// TestHasNewerVersion 钉住 tag 归一化：CI 注入的 version 与 GitHub tag_name 都是 tag，
// 但只要有任一侧漏了 "v" 前缀，versionNumbers 逐段只吃数字字符就会把 "v1.0.4" 读成
// [0,0,4]，与 [1,0,4] 不等 —— 同版本会被误报「有更新」、低版本也可能被当成新版。
func TestHasNewerVersion(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v1.0.4", "v1.0.5", true},
		{"v1.0.4", "v1.0.4", false},
		{"1.0.4", "v1.0.4", false},
		{"v1.0.4", "1.0.4", false},
		{"v1.0.4", "v1.0.3", false},
		{"v1.0.4", "1.0.0", false}, // 不归一化会读成 false positive
		{"v1.0.9", "v1.1.0", true},
		{"dev", "v1.0.5", false},
		{"", "v1.0.5", false},
		{"v1.0.4", "", false},
	}
	for _, c := range cases {
		if got := hasNewerVersion(c.current, c.latest); got != c.want {
			t.Errorf("hasNewerVersion(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

// TestAdminVersionHandler 钉住面板契约：当前版本 + 最新版本 + 是否有更新 + release 链接。
func TestAdminVersionHandler(t *testing.T) {
	oldFetch, oldVersion := fetchLatestRelease, version
	defer func() { fetchLatestRelease, version = oldFetch, oldVersion }()
	version = "v1.0.4"
	fetchLatestRelease = func(context.Context) (latestRelease, error) {
		return latestRelease{Tag: "v1.0.5", URL: "https://example.invalid/releases/tag/v1.0.5"}, nil
	}
	releaseMu.Lock()
	releaseCached = false
	releaseMu.Unlock()

	rec := httptest.NewRecorder()
	adminVersionHandler(rec, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 应 200，得到 %d", rec.Code)
	}
	var st versionStatus
	if err := json.NewDecoder(rec.Body).Decode(&st); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if st.Version != "v1.0.4" || st.Latest != "v1.0.5" || !st.HasUpdate || st.ReleaseURL == "" || st.CheckedAt == "" {
		t.Fatalf("版本自检响应不符: %+v", st)
	}

	rec2 := httptest.NewRecorder()
	adminVersionHandler(rec2, httptest.NewRequest(http.MethodPost, "/api/version", nil))
	if rec2.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 应 405，得到 %d", rec2.Code)
	}
}

// TestVersionCheckCache 钉住「连点只打一次 GitHub」：未认证 api.github.com 限
// 60 次/小时/IP，缓存失效的话运营多点几下就能把额度打满（然后面板长期「检查失败」）。
func TestVersionCheckCache(t *testing.T) {
	oldFetch, oldTTL := fetchLatestRelease, releaseCheckTTL
	defer func() { fetchLatestRelease, releaseCheckTTL = oldFetch, oldTTL }()
	releaseCheckTTL = 30 * time.Minute

	var calls int32
	fetchLatestRelease = func(context.Context) (latestRelease, error) {
		atomic.AddInt32(&calls, 1)
		return latestRelease{Tag: "v1.0.5", URL: "https://example.invalid/rel"}, nil
	}
	releaseMu.Lock()
	releaseCached = false
	releaseMu.Unlock()

	for i := 0; i < 5; i++ {
		if got := checkLatestVersion(context.Background()); got.Latest != "v1.0.5" || got.Error != "" {
			t.Fatalf("第 %d 次查询异常: %+v", i, got)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("TTL 内连查 5 次应只出站 1 次，实际 %d 次", n)
	}

	// TTL 过期后应重新出站
	releaseMu.Lock()
	releaseAt = time.Now().Add(-31 * time.Minute)
	releaseMu.Unlock()
	checkLatestVersion(context.Background())
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("TTL 过期后应重新出站，实际累计 %d 次", n)
	}

	// 失败结果同样进缓存：否则每次面板加载都会去撞一个连不通的 GitHub
	fetchLatestRelease = func(context.Context) (latestRelease, error) {
		atomic.AddInt32(&calls, 1)
		return latestRelease{}, fmt.Errorf("boom")
	}
	releaseMu.Lock()
	releaseCached = false
	releaseMu.Unlock()
	for i := 0; i < 3; i++ {
		if st := checkLatestVersion(context.Background()); st.Error != "检查失败" || st.HasUpdate {
			t.Fatalf("失败应静默降级为「检查失败」: %+v", st)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("失败结果也应缓存（3 次查询共 1 次出站），实际累计 %d 次", n)
	}
}

// ======================== 上游黑洞不拖死整机（CMP-119） ========================
//
// 现场（真机 PKG110 / Android 15）：进程存活、端口 LISTEN，但 /、/health、
// /api/config 全部无响应，持续 ≥9 分钟。根因有三条互相独立的「无上界等待」，
// 每条都由下面一个用例钉住；/health 与上游解耦由 TestHealthOutlivesBlackholedUpstream
// 直接判。

// blackholeUpstream 起一个「accept 之后永不回应」的上游，返回它的基址。
// 这正是现场形态：TCP 连得上，连接一直挂着，但一个字节都不回 ——
// 比 RST 难缠得多，因为内核不会替你把 socket 收掉。
func blackholeUpstream(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		// held 让已 accept 的连接保持可达：net.Conn 有 finalizer 会关 fd，
		// 不攥住的话一次 GC 就能把「黑洞」变成 RST，用例前提当场失效。
		// 声明在闭包内、只写不读，没有跨 goroutine 数据竞争。
		var held []net.Conn
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c) // 故意既不写也不关：等客户端自己的超时来收尸
		}
	}()
	return "http://" + ln.Addr().String()
}

// pointUpstreamAt 把两条上游基址指到指定地址并在用例结束后复位。
func pointUpstreamAt(t *testing.T, base string) {
	t.Helper()
	oldInf, oldZen := ocInferenceBase, ocZenBase
	ocInferenceBase, ocZenBase = base, base
	t.Cleanup(func() { ocInferenceBase, ocZenBase = oldInf, oldZen })
}

// TestFetchModelsDoesNotHangForever 钉住根因之一：模型目录拉取过去不带任何
// context、传输层也没有超时，上游黑洞时永久阻塞。它在启动路径上（go
// initializeOpenCode），并且经 initOCSession 挂在 sync.Once 上 —— 一旦卡死，
// 之后每一次业务请求都在 Once 上排队等它，整机停摆且不重启不恢复。
func TestFetchModelsDoesNotHangForever(t *testing.T) {
	ocOnce.Do(func() {}) // 挡掉对 registry.npmjs.org 的真实探测，用例不该依赖外网
	pointUpstreamAt(t, blackholeUpstream(t))

	old := upstreamModelsTimeout
	upstreamModelsTimeout = 2 * time.Second
	defer func() { upstreamModelsTimeout = old }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := fetchModels(); err == nil {
			t.Error("黑洞上游不该返回成功")
		}
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("fetchModels 在黑洞上游上永久阻塞（旧实现无任何超时，卡在这里）")
	}
}

// TestHealthOutlivesBlackholedUpstream 是本卡的主判据：上游黑洞、
// 且有一个 goroutine 正卡在上游调用上时，/health 必须照样秒回 200。
// 走真实 TCP 而不是直接调 handler —— 要证明的是「整机还能服务」，不是
// 「这个 handler 能返回」。
func TestHealthOutlivesBlackholedUpstream(t *testing.T) {
	ocOnce.Do(func() {})
	pointUpstreamAt(t, blackholeUpstream(t))

	old := upstreamModelsTimeout
	upstreamModelsTimeout = 5 * time.Second
	defer func() { upstreamModelsTimeout = old }()

	// 生产注册方式：同一个 mux、同一个 healthHandler。
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 先让一个上游调用真的挂住，复现「有在途请求卡死」的现场。
	stuck := make(chan struct{})
	go func() { defer close(stuck); fetchModels() }()
	select {
	case <-stuck:
		t.Fatal("fetchModels 立即返回了，用例前提不成立")
	case <-time.After(500 * time.Millisecond):
	}

	client := &http.Client{Timeout: 3 * time.Second}
	for i := 1; i <= 3; i++ {
		start := time.Now()
		resp, err := client.Get(srv.URL + "/health")
		if err != nil {
			t.Fatalf("上游黑洞时 /health 第 %d 次请求失败: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("上游黑洞时 /health 应仍为 200，得到 %d", resp.StatusCode)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("上游黑洞时 /health 第 %d 次用了 %s，探针不该被上游拖慢", i, d)
		}
	}
}

// TestUpstreamDialErrorRetryIsBounded 钉住根因之二：client.Do 出网络层错误时，
// 重试循环原来是无条件 continue —— 不查 attempts、不退避、不打日志。上游一挂
// 就变成满 CPU 死循环，且日志里一个字都看不到。必须有限次后放弃并返回。
func TestUpstreamDialErrorRetryIsBounded(t *testing.T) {
	ocOnce.Do(func() {})
	quotaBreakerMu.Lock()
	quotaBreakers = map[string]*quotaBreakerEntry{}
	quotaBreakerMu.Unlock()
	applyConfig(AppConfig{OCSessionID: testSession})
	t.Cleanup(func() { applyConfig(AppConfig{}) })
	// 端口 1 必然连不上：每次 client.Do 立刻返回错误，正好压这条分支。
	pointUpstreamAt(t, "http://127.0.0.1:1")

	done := make(chan struct{})
	go func() {
		defer close(done)
		callOpenCodeAPI(context.Background(),
			map[string]any{"model": "cmp119-probe"}, "cmp119-probe")
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("上游 dial 持续失败时应尽快放弃（旧实现是无限热重试，永远不返回）")
	}
}

// goneWriter 模拟客户端已经消失的下游：每次写都返回 EPIPE 类的错误。
type goneWriter struct{ h http.Header }

func (g *goneWriter) Header() http.Header       { return g.h }
func (g *goneWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
func (g *goneWriter) WriteHeader(int)           {}

// TestClaudeStreamStopsWhenClientDisconnects 钉住根因之三：SSE handler 原来
// 丢弃写错误，客户端（面板刷新/切页）走了以后 handler 仍一路读到上游 EOF，
// goroutine 与上游连接一起泄漏。面板反复刷新累积起来，fd 被吃光后 accept
// 停摆 —— 端口还 LISTEN、进程还在、CPU 静止，但所有 HTTP 请求零响应。
// 上游流永不结束时，旧实现在这条用例上就是永久不返回。
func TestClaudeStreamStopsWhenClientDisconnects(t *testing.T) {
	pr, pw := io.Pipe()
	upstreamClosed := make(chan struct{})
	// 上游无限流：只有 handler 自己退出才会关掉读端。
	go func() {
		defer close(upstreamClosed)
		chunk := `data: {"choices":[{"delta":{"content":"x"}}]}` + "\n"
		for {
			if _, err := pw.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}()

	// body 一旦被 Close，writer 侧立刻拿到 ErrClosedPipe —— 用来证明上游连接被释放。
	body := &closeNotifyReader{Reader: pr, closed: make(chan struct{})}
	w := &goneWriter{h: http.Header{}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// 生产调用点就是 defer upResp.Close() + claudeStreamHandler(...)：
		// handler 返回 ⇒ 上游连接才被释放。
		defer body.Close()
		claudeStreamHandler(w, body, "cmp119-probe", "cmp119-probe", "test")
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("客户端已断开，claudeStreamHandler 仍不返回（写错误被丢弃 → goroutine 与上游连接泄漏）")
	}
	select {
	case <-body.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("handler 返回后上游 body 应被关闭，实际没关（连接泄漏）")
	}
	pw.Close()
	<-upstreamClosed
}

type closeNotifyReader struct {
	io.Reader
	closed chan struct{}
	once   sync.Once
}

func (c *closeNotifyReader) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// TestShape403DoesNotRotateExitOrRetry（CMP-130 独立验收·反向用例）：
// 非 FreeTierError 签名的 403（形状类/其它类）必须单发透传：不重试、不换 key、
// 不触发任何出口动作。只有 403+FreeTierError 才允许进 rotateSocks5OnFreeTier403 ——
// 这是 CMP-109 结论（换出口救不了非出口类 403）的保护性反例。
func TestShape403DoesNotRotateExitOrRetry(t *testing.T) {
	quotaCase(t)
	const shapeBody = `{"error": {"type": "permission_error", "message": "request shape rejected by upstream"}}`

	// 判据层：形状类 403 不得被归入任何「换出口 / 进轮转分支」的通道。
	if isFreeTier403(http.StatusForbidden, []byte(shapeBody)) {
		t.Fatalf("形状类 403 不得被 isFreeTier403 命中")
	}
	if isRetryableQuotaUpstream(http.StatusForbidden, []byte(shapeBody)) {
		t.Fatalf("形状类 403 不得被 isRetryableQuotaUpstream 命中（否则会进轮转分支）")
	}

	hits, restore := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(shapeBody))
	})
	defer restore()

	body, status, _, err := callOpenCodeAPI(context.Background(),
		map[string]any{"model": "space-bunny-free"}, "space-bunny-free")
	if got := hits.Load(); got != 1 {
		t.Fatalf("形状类 403 应单发透传（不重试），实际打了 %d 次上游", got)
	}
	if status != http.StatusForbidden || err == nil {
		t.Fatalf("应透传上游 403 + error，得到 status=%d err=%v", status, err)
	}
	if !strings.Contains(string(body), "shape rejected") {
		t.Fatalf("应透传上游原始错误体，得到 %s", body)
	}

	// 流式入口同源：同一签名在 stream 循环里同样单发透传。
	hits2, restore2 := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(shapeBody))
	})
	defer restore2()
	rc, status2, _, _ := callOpenCodeAPIStream(context.Background(),
		map[string]any{"model": "space-bunny-free"}, "space-bunny-free")
	b2, _ := io.ReadAll(rc)
	rc.Close()
	if got := hits2.Load(); got != 1 {
		t.Fatalf("流式入口的形状类 403 也应单发透传，实际 %d 次", got)
	}
	if status2 != http.StatusForbidden || !strings.Contains(string(b2), "shape rejected") {
		t.Fatalf("流式入口应透传 403+原始体，得到 status=%d body=%s", status2, b2)
	}
}
