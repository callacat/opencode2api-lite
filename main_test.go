package main

import (
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
