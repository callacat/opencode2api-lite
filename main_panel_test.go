package main

import (
	"strings"
	"testing"
)

// CMP-215：面板 HTML/JS 的三条回归护栏。
// 真机上验收靠人工点按钮，这里钉住最容易再犯的三处：
// 主题按钮串图标（两个按钮共用 class + querySelector 取第一个匹配）、
// 退出按钮整页导航（form submit）、服务不可达时轮询不停。

// tagWithID 返回带指定 id 的那个标签的原文（找不到返回空串）。
func tagWithID(html, id string) string {
	i := strings.Index(html, `id="`+id+`"`)
	if i < 0 {
		return ""
	}
	start := strings.LastIndex(html[:i], "<")
	end := strings.Index(html[i:], ">")
	if start < 0 || end < 0 {
		return ""
	}
	return html[start : i+end+1]
}

func TestAdminPanelThemeToggleDoesNotBleed(t *testing.T) {
	// 主题按钮独占 id，JS 按 id 定位：不再依赖「第一个 .theme-toggle 是谁」。
	if !strings.Contains(adminHTML, `id="themeBtn"`) {
		t.Fatal("面板主题按钮缺 id=\"themeBtn\"")
	}
	if !strings.Contains(adminHTML, `getElementById('themeBtn')`) {
		t.Fatal("toggleTheme 未按 id 定位主题按钮")
	}
	if strings.Contains(adminHTML, `querySelector('.theme-toggle')`) {
		t.Fatal("面板 JS 仍用 querySelector('.theme-toggle')：与「检查更新」按钮共用 class 时会串图标")
	}
	// 「检查更新」按钮不能再带 theme-toggle，否则样式也串。
	tag := tagWithID(adminHTML, "verCheckBtn")
	if tag == "" {
		t.Fatal("面板里找不到 verCheckBtn 按钮")
	}
	if strings.Contains(tag, "theme-toggle") {
		t.Fatalf("「检查更新」按钮仍带 theme-toggle class，样式会串：%s", tag)
	}
}

func TestAdminPanelLogoutIsNotFormSubmit(t *testing.T) {
	// form submit = 点一下整页导航，页面上其它交互全丢（CMP-215 真机症状）。
	if strings.Contains(adminHTML, `<form method="post" action="/logout"`) {
		t.Fatal("面板「退出」仍是 form submit：点它会整页导航")
	}
	if !strings.Contains(adminHTML, "function doLogout()") {
		t.Fatal("面板缺少 doLogout()")
	}
	// 退出前要有二次确认，避免误触。
	if !strings.Contains(adminHTML, "confirm('确认退出登录？')") {
		t.Fatal("面板「退出」没有二次确认")
	}
}

func TestAdminPanelOfflineState(t *testing.T) {
	// 服务不可达时：停轮询 + 明确提示 + 给重连入口。
	for _, want := range []string{"function stopPolling()", "function startPolling()", "function retryConnect()", "代理未运行"} {
		if !strings.Contains(adminHTML, want) {
			t.Fatalf("面板缺少 %q：服务不可达时没有停轮询/提示/重连入口", want)
		}
	}
	// 轮询必须走可停的 startPolling，而不是裸 setInterval。
	// 轮询必须走可停的 startPolling，而不是裸 setInterval。
	tail := adminHTML[strings.Index(adminHTML, "function fmt(n)"):]
	if !strings.Contains(tail, "startPolling();") {
		t.Fatal("面板启动时没有走 startPolling()：轮询停不下来")
	}
	if strings.Contains(tail, "setInterval(") {
		t.Fatal("面板仍有裸 setInterval 轮询：失败时停不下来")
	}
	// 代理被系统杀掉后会被看门狗拉起来，所以停掉 5s 轮询的同时要留一条低频重试，
	// 否则代理恢复了面板也不会自己接回去（CMP-215 真机定性后补的）。
	if !strings.Contains(adminHTML, "function startRetry()") || !strings.Contains(adminHTML, "function stopRetry()") {
		t.Fatal("面板缺少低频重试：停了 5s 轮询后代理恢复也接不回来")
	}
	if !strings.Contains(adminHTML, "setInterval(loadStats,15000)") {
		t.Fatal("低频重试的间隔不是 15s")
	}
	// 「重启代理」入口：面板跑在系统浏览器里，代理卡死时只能唤起 App 去重启。
	if !strings.Contains(adminHTML, `id="restartProxyBtn"`) {
		t.Fatal("面板离线横幅里没有「重启代理」入口")
	}
	if !strings.Contains(adminHTML, "intent://#Intent;component=com.callacat.oc2api/.MainActivity;S.oc2api_restart=1;end") {
		t.Fatal("「重启代理」入口没有指向 App 的重启 intent")
	}
	// 真机 PKG110 那种「连得上但不回包」的形态下，浏览器 fetch 不会自己 reject；
	// 没有超时就会永远挂着，离线判定根本触发不了（运维在 CMP-216 实测指出）。
	if !strings.Contains(adminHTML, "function fetchT(") || !strings.Contains(adminHTML, "AbortController") {
		t.Fatal("面板没有带超时的 fetchT：服务不回包时请求会永远挂着")
	}
	if !strings.Contains(adminHTML, "await fetchT('/api/stats',8000)") {
		t.Fatal("/api/stats 轮询没走 fetchT 超时")
	}
	// 所有面板请求都该走 fetchT，别再留裸 fetch（写操作更要防按钮一直转）。
	if strings.Contains(adminHTML, "await fetch('/api/") {
		t.Fatal("还有裸 fetch 打面板 API：服务不回包时会永远挂着")
	}
}

// 百度隧道的两个密码列挨着，用户会把面板密码填到 SOCKS5 密码列里
// （真机就因此少了 X-T5-Auth，隧道必然 403）——列名与提示必须能一眼分清。
func TestAdminPanelBaiduAuthColumn(t *testing.T) {
	for _, want := range []string{"SOCKS5 密码", "百度 X-T5-Auth", "X-T5-Auth（必填）", "function renderSocks5Warn()"} {
		if !strings.Contains(adminHTML, want) {
			t.Fatalf("面板缺少 %q：百度隧道的 X-T5-Auth 与 SOCKS5 密码分不清", want)
		}
	}
}

// CMP-236：后台被冻结（ColorOS Hans 冻结 uid cgroup）时面板要给出明确提示 + 加白入口。
func TestAdminPanelFreezeHint(t *testing.T) {
	for _, want := range []string{"疑似被系统后台冻结", "耗电管理", "耗电行为控制", "完全允许后台行为", "去加白"} {
		if !strings.Contains(adminHTML, want) {
			t.Fatalf("面板缺少 %q：后台被冻结时没有给出 ColorOS 加白指引", want)
		}
	}
	if !strings.Contains(adminHTML, `id="whitelistBtn"`) {
		t.Fatal("面板没有「去加白」入口按钮")
	}
	if !strings.Contains(adminHTML, "intent://#Intent;component=com.callacat.oc2api/.MainActivity;S.oc2api_whitelist=1;end") {
		t.Fatal("「去加白」入口没有指向 App 的 intent")
	}
	// 超时（端口在但不回包）才判「疑似被冻结」；拒连是另一回事，不能混。
	if !strings.Contains(adminHTML, "freezeSuspect") && !strings.Contains(adminHTML, "请求超时") {
		t.Fatal("面板没有把「请求超时」当成冻结信号")
	}
	if !strings.Contains(adminHTML, "保存失败：代理疑似被系统后台冻结") {
		t.Fatal("保存失败没有升级成冻结专属文案")
	}
}
