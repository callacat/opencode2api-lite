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
}
