package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
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

// TestOCSessionInjection 钉住 2026-10 上游改会话存在性校验后的选取契约：
// 注入的真实会话被原样使用；未注入时**不编造**（返回空串，由 callOpenCodeAPI 转 503），
// 因为编造值必然 403 FreeTierError，而 403 对调用方毫无信息量。
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
	if got := ocCurrentSessionID(); got != "" {
		t.Errorf("未注入时不应编造会话（会必然 403），得到 %q", got)
	}
}

// TestOCSessionMissingBody 确认 503 响应体可读：调用方要能一眼看出该配什么。
func TestOCSessionMissingBody(t *testing.T) {
	var resp struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(ocSessionMissingBody(), &resp); err != nil {
		t.Fatalf("503 响应体不是合法 JSON: %v", err)
	}
	if resp.Error.Type != "OCSessionMissing" {
		t.Errorf("错误类型 = %q, 期望 OCSessionMissing", resp.Error.Type)
	}
	if !strings.Contains(resp.Error.Message, "oc_session_id") {
		t.Errorf("错误信息应点名要配的字段，得到 %q", resp.Error.Message)
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

// TestSessionGuardBothEntryPoints 钉住「未配置真实会话就不打上游」这条契约在**两个**入口都成立。
//
// 曾经只有非流式入口有这道闸口，流式请求会退回编造会话、静默拿到 403 FreeTierError，
// 运维看到 403 会误判成「会话过期」，排查方向被带偏。
func TestSessionGuardBothEntryPoints(t *testing.T) {
	// 预置 sync.Once，跳过 initOCSession 里的版本探测网络请求，让用例纯离线。
	ocOnce.Do(func() {})

	applyConfig(AppConfig{})
	defer applyConfig(AppConfig{})

	body, status, _, err := callOpenCodeAPI(context.Background(), map[string]any{}, "big-pickle")
	assertMissing := func(what string, body []byte, status int, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: 未配置会话时应返回错误", what)
		}
		if status != http.StatusServiceUnavailable {
			t.Errorf("%s: 状态码 = %d, 期望 503", what, status)
		}
		if !strings.Contains(string(body), "OCSessionMissing") {
			t.Errorf("%s: 响应体应含 OCSessionMissing，得到 %q", what, string(body))
		}
	}
	assertMissing("非流式入口", body, status, err)

	stream, sStatus, _, sErr := callOpenCodeAPIStream(context.Background(), map[string]any{}, "big-pickle")
	if stream != nil {
		defer stream.Close()
		got, _ := io.ReadAll(stream)
		assertMissing("流式入口", got, sStatus, sErr)
	} else {
		t.Fatal("流式入口未配置会话时应返回非 nil 的响应体")
	}

	// 配了真实会话后闸口必须放行（否则修复过头，把正常请求也挡了）
	applyConfig(AppConfig{OCSessionID: "ses_ecbad4897ffe524uCAgXevWcMc"})
	if b, err := ocSessionGuard(); err != nil || b != nil {
		t.Errorf("已配置会话时闸口应放行，得到 body=%s err=%v", string(b), err)
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
