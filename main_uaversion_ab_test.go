package main

// UA 版本号单变量金丝雀：同一个请求体（走真实业务路径 ensureFreeTierTools）、
// 同一个 session，只改 UA 里的 <version> 段。
//
// 默认不跑（会打真实上游）；显式给版本列表才跑：
//
//	OC2API_UA_AB=2.0.29,2.0.32 go test -run TestRealUpstreamUAVersionAB -v .
//
// 出口也是变量时可一起扫（每个出口各跑一遍全部版本，串行，避免并发自伤）；
// 值 "direct" 表示直连，其余当作 socks5 addr（容器内用 mihomo:20819，宿主机用 127.0.0.1:20819）：
//
//	OC2API_UA_AB=2.0.29,2.0.32 OC2API_AB_EXITS=direct,127.0.0.1:20819 go test -run ... -v .
//
// OC2API_AB_ECHO=1 时每个出口先打一次 IP 回显，记下该出口的实际出口 IP。
import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRealUpstreamUAVersionAB(t *testing.T) {
	vers := os.Getenv("OC2API_UA_AB")
	if vers == "" {
		t.Skip("设 OC2API_UA_AB=<版本逗号分隔> 才打真实上游")
	}
	model := os.Getenv("OC2API_AB_MODEL")
	if model == "" {
		model = "space-bunny-free"
	}
	exits := os.Getenv("OC2API_AB_EXITS")
	if exits == "" {
		exits = "direct"
	}
	bodyMap := map[string]any{
		"model":    model,
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	// 与 callOpenCodeAPI 同一条路径：免费层门禁要求核心五件工具带官方真实定义。
	ensureFreeTierTools(bodyMap)
	body, err := json.Marshal(bodyMap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	initOCSession()
	for _, ex := range strings.Split(exits, ",") {
		ex = strings.TrimSpace(ex)
		if ex == "" {
			continue
		}
		socks5Mu.Lock()
		if ex == "direct" {
			activeSocks5, socks5Proxies = "", nil
		} else {
			socks5Proxies = []Socks5Proxy{{Addr: ex, Name: ex}}
			activeSocks5 = ex
		}
		socks5Mu.Unlock()
		if os.Getenv("OC2API_AB_ECHO") == "1" {
			t.Logf("出口 %-20s 出口 IP: %s", ex, probeEgressIP())
		}
		for _, v := range strings.Split(vers, ",") {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			req, _, err := buildOCRequest(ctx, body)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("User-Agent", "opencode/latest/"+v+"/cli")
			client, _, err := getHTTPClient(ctx)
			if err != nil {
				t.Fatalf("http client: %v", err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Errorf("出口 %s UA %s: transport error: %v", ex, v, err)
				cancel()
				continue
			}
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
			resp.Body.Close()
			snippet := strings.ReplaceAll(string(raw), "\n", " ")
			if len(snippet) > 200 {
				snippet = snippet[:200]
			}
			t.Logf("出口 %-20s UA %-8s -> HTTP %d | %s", ex, v, resp.StatusCode, snippet)
			cancel()
		}
	}
}

// probeEgressIP 用当前选中的出口打一次回显服务，记录实际出口 IP。
func probeEgressIP() string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.ipify.org", nil)
	client, sel, err := getHTTPClient(ctx)
	if err != nil {
		return "client error: " + err.Error()
	}
	resp, err := client.Do(req)
	if err != nil {
		return "(" + sel.Label + ") transport error: " + err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
	return "(" + sel.Label + ") " + strings.TrimSpace(string(b))
}
