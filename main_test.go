package main

import (
	"encoding/json"
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

// TestOCSessionInjection 钉住 2026-10 上游校验变更后的会话选取逻辑：
// 注入的真实会话必须被原样使用，未注入时才退回编造（且编造值仍须格式合规）。
// 上游已从「格式校验」改为「会话存在性校验」，编造值必然 403 FreeTierError。
func TestOCSessionInjection(t *testing.T) {
	const real = "ses_ecbad4897ffe524uCAgXevWcMc"

	applyConfig(AppConfig{OCSessionID: real})
	if got := ocCurrentSessionID(); got != real {
		t.Fatalf("注入真实会话后应原样使用，得到 %s", got)
	}
	if !hasInjectedOCSession() {
		t.Error("已注入会话时 hasInjectedOCSession 应为 true")
	}

	applyConfig(AppConfig{OCSessionID: ""})
	if hasInjectedOCSession() {
		t.Error("清空注入后 hasInjectedOCSession 应为 false")
	}
	fallback := ocCurrentSessionID()
	if fallback == real {
		t.Error("未注入时不应回落到上一次注入的会话")
	}
	if len(fallback) != len("ses_")+26 {
		t.Errorf("兜底会话长度应为 %d，得到 %d (%s)", len("ses_")+26, len(fallback), fallback)
	}
	if !strings.HasPrefix(fallback, "ses_") {
		t.Errorf("兜底会话应保留 ses_ 前缀，得到 %s", fallback)
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
