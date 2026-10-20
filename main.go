package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	_ "embed" // opencode_tools.json 真实工具定义
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	// 内嵌 IANA 时区库（约 +450KB）：alpine 运行时镜像与 Android 都没有
	// /usr/share/zoneinfo，Go 的 time.LoadLocation 取不到数据会直接失败，
	// time.Local 退化 UTC，日志时间整体漂 8 小时（CMP-190）。
	_ "time/tzdata"
)

// defaultTimezone 是 TZ 未设置时的默认时区。
const defaultTimezone = "Asia/Shanghai"

// configureTimeLocal 决定进程的 time.Local：TZ 有值就按 TZ 走（显式覆盖），
// 为空则默认上海。不依赖宿主/镜像是否装了 tzdata —— 时区数据已内嵌。
func configureTimeLocal(tz string) {
	if tz == "" {
		tz = defaultTimezone
	}
	// 兼容 POSIX 的 ":Asia/Shanghai" 写法
	if loc, err := time.LoadLocation(strings.TrimPrefix(tz, ":")); err == nil {
		time.Local = loc
	}
}

// newTransport 收口所有上游传输层的超时。CMP-119：这些 Transport 是字面量构造的，
// 没写的字段全是零值，而零值在 net/http 里等于「无限等」—— 只要上游 accept 了连接
// 却不再吐响应头，client.Do 就永久阻塞，调用方的 context 又是 Background
// （fetchOCVersion/fetchModels）或客户端不主动断开的 r.Context()，谁也救不回来。
// 表现就是「进程存活、端口 LISTEN，但整机 HTTP 无响应」。
//
// 三条缺一不可：dial 不设超时（对端黑洞时 SYN 重试两分钟以上才失败）、
// TLS 握手不设超时、响应头不设超时。三条都只覆盖到「响应头到达」为止，
// 之后的正文流不受影响，所以 SSE 流式不会被掐断。
func newTransport(dial func(context.Context, string, string) (net.Conn, error)) *http.Transport {
	if dial == nil {
		d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		dial = d.DialContext
	}
	return &http.Transport{
		DialContext:           dial,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
	}
}

// 直连客户端：不设 Client.Timeout（它会覆盖整个请求、连流式读 body 一起掐），
// 兜底靠 newTransport 里的传输层超时 + 调用方 context。
var httpClient = &http.Client{Transport: newTransport(nil)}

// ======================== SOCKS5 代理 ========================

type Socks5Proxy struct {
	Addr     string `json:"addr"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Name     string `json:"name,omitempty"`

	// Kind 是出口类型：""/"socks5" = SOCKS5，老配置行为不变；"baidu_connect" = 百度 CONNECT 隧道。
	Kind string `json:"kind,omitempty"`
	// Auth 是百度隧道的 X-T5-Auth 值（凭据，只进 config.json，不进仓库/日志）。
	Auth string `json:"auth,omitempty"`
	// ConnectHost 是百度隧道 CONNECT 的 Host 头，默认 sptest.baidu.com。
	ConnectHost string `json:"connect_host,omitempty"`
}

const proxyKindBaidu = "baidu_connect"

// baiduBootstrapHost 是百度隧道 CONNECT 的默认 Host 头（实测值，勿自创）。
const baiduBootstrapHost = "sptest.baidu.com"

// socks5EndpointError 表示 SOCKS5 服务本身无法连接、协商或认证。
// 与 CONNECT 目标失败相区分：前者是代理节点不可用，后者是目标主机不可达。
type socks5EndpointError struct {
	stage string
	err   error
}

func (e *socks5EndpointError) Error() string {
	return fmt.Sprintf("socks5 %s: %v", e.stage, e.err)
}

func (e *socks5EndpointError) Unwrap() error { return e.err }

func endpointError(stage string, err error) error {
	return &socks5EndpointError{stage: stage, err: err}
}

func socks5Dial(proxy Socks5Proxy) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, target string) (net.Conn, error) {
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		conn, err := dialer.DialContext(ctx, "tcp", proxy.Addr)
		if err != nil {
			return nil, endpointError("connect to "+proxy.Addr, err)
		}
		deadline := time.Now().Add(15 * time.Second)
		if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
			deadline = contextDeadline
		}
		conn.SetDeadline(deadline)

		// 认证方法协商
		auth := byte(0x00) // no auth
		if proxy.Username != "" {
			auth = 0x02 // username/password
		}
		if _, err := conn.Write([]byte{0x05, 0x01, auth}); err != nil {
			conn.Close()
			return nil, endpointError("handshake write", err)
		}
		buf := make([]byte, 2)
		if _, err := io.ReadFull(conn, buf); err != nil {
			conn.Close()
			return nil, endpointError("handshake read", err)
		}
		if buf[0] != 0x05 {
			conn.Close()
			return nil, endpointError("handshake", errors.New("not socks5 protocol"))
		}

		// 用户名/密码认证
		if buf[1] == 0x02 {
			if proxy.Username == "" {
				conn.Close()
				return nil, endpointError("authentication", errors.New("server requires auth but no credentials"))
			}
			ulen := len(proxy.Username)
			plen := len(proxy.Password)
			authBuf := make([]byte, 3+ulen+plen)
			authBuf[0] = 0x01
			authBuf[1] = byte(ulen)
			copy(authBuf[2:], proxy.Username)
			authBuf[2+ulen] = byte(plen)
			copy(authBuf[3+ulen:], proxy.Password)
			if _, err := conn.Write(authBuf); err != nil {
				conn.Close()
				return nil, endpointError("auth write", err)
			}
			authResp := make([]byte, 2)
			if _, err := io.ReadFull(conn, authResp); err != nil {
				conn.Close()
				return nil, endpointError("auth read", err)
			}
			if authResp[1] != 0x00 {
				conn.Close()
				return nil, endpointError("authentication", errors.New("auth failed"))
			}
		} else if buf[1] != 0x00 {
			conn.Close()
			return nil, endpointError("authentication", fmt.Errorf("unsupported auth method 0x%02x", buf[1]))
		}

		// CONNECT 请求
		host, portStr, err := net.SplitHostPort(target)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5: invalid target %s: %w", target, err)
		}
		port := 0
		fmt.Sscanf(portStr, "%d", &port)

		req := []byte{0x05, 0x01, 0x00} // VER, CMD=CONNECT, RSV
		ip := net.ParseIP(host)
		if ip != nil {
			if ip4 := ip.To4(); ip4 != nil {
				req = append(req, 0x01) // IPv4
				req = append(req, ip4...)
			} else {
				req = append(req, 0x04) // IPv6
				req = append(req, ip.To16()...)
			}
		} else {
			if len(host) > 255 {
				conn.Close()
				return nil, fmt.Errorf("socks5: hostname too long")
			}
			req = append(req, 0x03) // Domain
			req = append(req, byte(len(host)))
			req = append(req, []byte(host)...)
		}
		req = append(req, byte(port>>8), byte(port))

		if _, err := conn.Write(req); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 connect write: %w", err)
		}

		// 读取响应
		resp := make([]byte, 4)
		if _, err := io.ReadFull(conn, resp); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 connect read: %w", err)
		}
		if resp[1] != 0x00 {
			conn.Close()
			return nil, fmt.Errorf("socks5 connect to target failed: status 0x%02x", resp[1])
		}

		// 读取绑定地址
		switch resp[3] {
		case 0x01: // IPv4
			if _, err := io.ReadFull(conn, make([]byte, 4+2)); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5: read bind ipv4: %w", err)
			}
		case 0x03: // Domain
			dlen := make([]byte, 1)
			if _, err := io.ReadFull(conn, dlen); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5: read bind domain len: %w", err)
			}
			if _, err := io.ReadFull(conn, make([]byte, int(dlen[0])+2)); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5: read bind domain: %w", err)
			}
		case 0x04: // IPv6
			if _, err := io.ReadFull(conn, make([]byte, 16+2)); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5: read bind ipv6: %w", err)
			}
		default:
			conn.Close()
			return nil, fmt.Errorf("socks5: unknown address type 0x%02x", resp[3])
		}

		conn.SetDeadline(time.Time{})
		return conn, nil
	}
}

// bufferedConn 把 CONNECT 响应头读剩下的缓冲字节接回连接。
// TLS 握手从客户端紧跟着开始，吞掉这几个字节就是随机握手失败。
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// baiduNodeAddr 从 addr 取节点地址：去掉 baidu:// 前缀，缺省端口补 443。
func baiduNodeAddr(addr string) (string, error) {
	node := strings.TrimPrefix(strings.TrimSpace(addr), "baidu://")
	if node == "" {
		return "", errors.New("baidu node address is empty")
	}
	if _, _, err := net.SplitHostPort(node); err != nil {
		node = net.JoinHostPort(strings.Trim(node, "[]"), "443")
	}
	return node, nil
}

// baiduConnectDial 走百度 CONNECT 隧道（代理不是 SOCKS5，协议照抄已验证的
// x-tunnel dialBaiduTunnel 实现）。失败一律包 endpointError，复用现有自愈路径。
func baiduConnectDial(proxy Socks5Proxy) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, target string) (net.Conn, error) {
		node, err := baiduNodeAddr(proxy.Addr)
		if err != nil {
			return nil, endpointError("baidu node", err)
		}
		connectHost := strings.TrimSpace(proxy.ConnectHost)
		if connectHost == "" {
			connectHost = baiduBootstrapHost
		}
		// auth/connect_host 来自配置文件（可经面板写入），拼进请求头前挡掉换行注入。
		for _, v := range []string{connectHost, proxy.Auth} {
			if strings.ContainsAny(v, "\r\n") {
				return nil, endpointError("baidu node", errors.New("auth/connect_host 含非法换行"))
			}
		}

		dialer := &net.Dialer{Timeout: 10 * time.Second}
		conn, err := dialer.DialContext(ctx, "tcp", node)
		if err != nil {
			return nil, endpointError("connect to baidu node "+node, err)
		}
		deadline := time.Now().Add(15 * time.Second)
		if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
			deadline = contextDeadline
		}
		conn.SetDeadline(deadline)

		req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nX-T5-Auth: %s\r\n"+
			"User-Agent: okhttp/3.11.0 Dalvik/2.1.0 (Linux; Build/RKQ1.200826.002) baiduboxapp/11.0.5.12 (Baidu; P1 11)\r\n"+
			"Proxy-Connection: keep-alive\r\nConnection: keep-alive\r\n\r\n", target, connectHost, proxy.Auth)
		if _, err := io.WriteString(conn, req); err != nil {
			conn.Close()
			return nil, endpointError("baidu CONNECT write", err)
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			conn.Close()
			return nil, endpointError("read baidu CONNECT response", err)
		}
		// 不碰 resp.Body：隧道 200 响应没有 CL，Body 会一直读到 EOF（即连接关闭）。
		if resp.StatusCode != http.StatusOK {
			conn.Close()
			return nil, endpointError("baidu CONNECT "+target, fmt.Errorf("状态行 %q", resp.Status))
		}
		conn.SetDeadline(time.Time{})
		return &bufferedConn{Conn: conn, r: br}, nil
	}
}

// newSocks5HTTPClient 构建走 SOCKS5 代理的 http.Client。
// 不设 Client.Timeout，请求生命周期由调用方 context 控制；
// 传输层超时与直连侧同源（见 newTransport）。
func newSocks5HTTPClient(proxy Socks5Proxy) *http.Client {
	dial := socks5Dial(proxy)
	if proxy.Kind == proxyKindBaidu {
		dial = baiduConnectDial(proxy)
	}
	return &http.Client{Transport: newTransport(dial)}
}

func closeHTTPClientIdle(client *http.Client) {
	if client == nil {
		return
	}
	if transport, ok := client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}

var (
	socks5Proxies []Socks5Proxy
	activeSocks5  string // 启用的代理 Addr；空表示直连；特殊值表示轮询/429 切换
	socks5Mu      sync.RWMutex
)

const (
	socks5RR                      = "__round_robin__"
	socks5RateLimitSwitch         = "__rate_limit_switch__"
	socks5RateLimitSwitchNoDirect = "__rate_limit_switch_no_direct__"
)

var (
	socks5RRIndex        uint32
	socks5RateLimitIndex uint32 // 0 表示直连，1..n 表示 socks5Proxies[n-1]
)

var (
	socks5Client    *http.Client // 缓存的 SOCKS5 客户端
	socks5ClientKey string       // 地址和凭据共同组成缓存键
)

func socks5ProxyCacheKey(proxy Socks5Proxy) string {
	return proxy.Addr + "\x00" + proxy.Username + "\x00" + proxy.Password +
		"\x00" + proxy.Kind + "\x00" + proxy.Auth + "\x00" + proxy.ConnectHost
}

// socks5ProxyLabel 是日志里的出口标识，同时是巡检脚本的判据：
// oc2api-watch.py 的 exit 正则取 `exit=<name> (<addr>)` 的 addr 组与 active_socks5 对比。
// 百度隧道固定 `baidu (baidu://<node>)`，与面板 name 无关 —— name 带空格会让 addr 组为空，
// 巡检会误判漂移。
func socks5ProxyLabel(proxy Socks5Proxy) string {
	if proxy.Kind == proxyKindBaidu {
		return "baidu (" + proxy.Addr + ")"
	}
	if proxy.Name != "" {
		return proxy.Name + " (" + proxy.Addr + ")"
	}
	return proxy.Addr
}

// proxySelection 描述本次请求选中的出口，仅用于日志标记。
type proxySelection struct {
	Label string
}

func getHTTPClient(ctx context.Context) (*http.Client, proxySelection, error) {
	socks5Mu.Lock()
	if activeSocks5 == "" {
		socks5Mu.Unlock()
		return httpClient, proxySelection{Label: "direct"}, nil
	}

	var proxy Socks5Proxy
	var useRR bool
	label := activeSocks5

	switch activeSocks5 {
	case socks5RR:
		if len(socks5Proxies) == 0 {
			socks5Mu.Unlock()
			return httpClient, proxySelection{Label: "direct"}, nil
		}
		idx := atomic.AddUint32(&socks5RRIndex, 1) % uint32(len(socks5Proxies))
		proxy = socks5Proxies[idx]
		useRR = true
		label = socks5ProxyLabel(proxy)
	case socks5RateLimitSwitch, socks5RateLimitSwitchNoDirect:
		var ok bool
		proxy, ok = currentRateLimitProxyLocked(activeSocks5 == socks5RateLimitSwitch)
		if !ok {
			socks5Mu.Unlock()
			return httpClient, proxySelection{Label: "direct"}, nil
		}
		label = socks5ProxyLabel(proxy)
	default:
		var found bool
		for i := range socks5Proxies {
			if socks5Proxies[i].Addr == activeSocks5 {
				proxy = socks5Proxies[i]
				found = true
				break
			}
		}
		if !found {
			socks5Mu.Unlock()
			return httpClient, proxySelection{Label: "direct"}, nil
		}
		label = socks5ProxyLabel(proxy)
	}

	cacheKey := socks5ProxyCacheKey(proxy)
	if !useRR && socks5Client != nil && socks5ClientKey == cacheKey {
		client := socks5Client
		socks5Mu.Unlock()
		return client, proxySelection{Label: label}, nil
	}

	client := newSocks5HTTPClient(proxy)

	if !useRR {
		closeHTTPClientIdle(socks5Client)
		socks5Client = client
		socks5ClientKey = cacheKey
	}
	socks5Mu.Unlock()
	return client, proxySelection{Label: label}, nil
}

func currentRateLimitProxyLocked(includeDirect bool) (Socks5Proxy, bool) {
	total := len(socks5Proxies)
	if includeDirect {
		total++
	}
	if total <= 0 {
		return Socks5Proxy{}, false
	}
	idx := int(atomic.LoadUint32(&socks5RateLimitIndex)) % total
	if includeDirect && idx == 0 {
		return Socks5Proxy{}, false
	}
	if includeDirect {
		return socks5Proxies[idx-1], true
	}
	return socks5Proxies[idx], true
}

func socks5ExitLabelLocked(idx int, includeDirect bool) string {
	if includeDirect && idx <= 0 {
		return "direct"
	}
	proxyIdx := idx
	if includeDirect {
		proxyIdx = idx - 1
	}
	if proxyIdx < 0 || proxyIdx >= len(socks5Proxies) {
		return "direct"
	}
	proxy := socks5Proxies[proxyIdx]
	return socks5ProxyLabel(proxy)
}

func socks5RateLimitAttemptCount() int {
	socks5Mu.RLock()
	defer socks5Mu.RUnlock()
	switch activeSocks5 {
	case socks5RateLimitSwitch:
		return len(socks5Proxies) + 1
	case socks5RateLimitSwitchNoDirect:
		if len(socks5Proxies) == 0 {
			return 1
		}
		return len(socks5Proxies)
	default:
		return 1
	}
}

func rotateSocks5OnRateLimit(ctx context.Context) {
	socks5Mu.Lock()
	defer socks5Mu.Unlock()
	includeDirect := activeSocks5 == socks5RateLimitSwitch
	if activeSocks5 != socks5RateLimitSwitch && activeSocks5 != socks5RateLimitSwitchNoDirect {
		return
	}
	total := len(socks5Proxies)
	if includeDirect {
		total++
	}
	if total <= 1 {
		if includeDirect {
			log.Printf("%s[rate-limit proxy switch] upstream 429, only direct exit available", labelPrefix(ctx))
		} else {
			log.Printf("%s[rate-limit proxy switch] upstream 429, no SOCKS5 exit available", labelPrefix(ctx))
		}
		return
	}
	oldIdx := int(atomic.LoadUint32(&socks5RateLimitIndex)) % total
	nextIdx := (oldIdx + 1) % total
	atomic.StoreUint32(&socks5RateLimitIndex, uint32(nextIdx))
	closeHTTPClientIdle(socks5Client)
	socks5Client = nil
	socks5ClientKey = ""
	log.Printf("%s[rate-limit proxy switch] upstream 429: %s -> %s", labelPrefix(ctx), socks5ExitLabelLocked(oldIdx, includeDirect), socks5ExitLabelLocked(nextIdx, includeDirect))
}

// rotateSocks5OnFreeTier403 处理「403 FreeTierError」这一路的出口动作。
//
// CMP-109 当年用「20819 与 20818 都 403」否证过「403 换出口有用」，CMP-128 推翻了它：
// 那两条腿同属一个被整类拒绝的代理池，不是真对照；同刻同形状直连 7/7 全 200。
// ⇒ 被拒的是**整类出口**时换 key 结构上无效（日志 k31→k35 五把 key 全 403 即此形态），
// 这一路必须也换出口，否则永久 403 且不自愈。
//
// 两种模式分别处理：
//   - switch 模式（__rate_limit_switch__ / __rate_limit_switch_no_direct__）：
//     复用 429 的同一条轮转游标，下一发换到下一个出口。
//   - 裸地址/其它模式：只有一条腿，无从轮转；该腿已被整类拒绝时直接**内存回落直连**
//     —— 池里唯一还没试过的那条腿（CMP-128 实测直连 7/7 全 200）。只改内存不落
//     config.json，重启即回原状，保留可回滚路径。
func rotateSocks5OnFreeTier403(ctx context.Context, curLabel string) {
	socks5Mu.Lock()
	switch activeSocks5 {
	case socks5RateLimitSwitch, socks5RateLimitSwitchNoDirect:
		socks5Mu.Unlock()
		rotateSocks5OnRateLimit(ctx)
		return
	}
	if activeSocks5 == "" || curLabel == "direct" {
		socks5Mu.Unlock()
		return
	}
	rejected := activeSocks5
	activeSocks5 = ""
	closeHTTPClientIdle(socks5Client)
	socks5Client = nil
	socks5ClientKey = ""
	socks5Mu.Unlock()
	log.Printf("%s[free tier 403] 出口 %s 被整类拒绝，内存回落直连（不落 config.json，重启即回原状）",
		labelPrefix(ctx), rejected)
}

// ======================== 随机 ID ========================

// clientIP 从请求头提取客户端真实公网 IP。
// 走 Cloudflare CDN 时真实 IP 在 CF-Connecting-IP 头中（r.RemoteAddr 是 CF 边缘节点 IP），
// 回退顺序：CF-Connecting-IP → X-Forwarded-For → RemoteAddr。
// reqLabel 生成「#请求序号 ip=客户端IP」形式的标识，用来把并发请求的日志串起来。
// 序号来自 requestCount（每个请求唯一），可与 [访问] 行一一对应。
func reqLabel(cnt int64, r *http.Request) string {
	return fmt.Sprintf("#%d ip=%s", cnt, clientIP(r))
}

// requestLabelKey 用于把「#序号 ip=客户端IP」放进请求 context，
// 让 callOpenCodeAPI / callOpenCodeAPIStream 内部的重试、限流、断开日志也能带上同一个标识。
type requestLabelKey struct{}

func withRequestLabel(ctx context.Context, label string) context.Context {
	return context.WithValue(ctx, requestLabelKey{}, label)
}

func requestLabel(ctx context.Context) string {
	label, _ := ctx.Value(requestLabelKey{}).(string)
	return label
}

// labelPrefix 返回带尾随空格的请求标识（没有则返回空串），方便直接拼在日志消息最前面。
func labelPrefix(ctx context.Context) string {
	if label := requestLabel(ctx); label != "" {
		return label + " "
	}
	return ""
}

func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
		if idx := strings.IndexByte(ip, ','); idx >= 0 {
			ip = strings.TrimSpace(ip[:idx])
		}
		return ip
	}
	if ip := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); ip != "" {
		if idx := strings.IndexByte(ip, ','); idx >= 0 {
			ip = strings.TrimSpace(ip[:idx])
		}
		return ip
	}
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return ip
	}
	return r.RemoteAddr
}

func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = letters[b[i]%byte(len(letters))]
	}
	return string(b)
}

func randomHex(n int) string {
	const hex = "0123456789abcdef"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = hex[b[i]%byte(len(hex))]
	}
	return string(b)
}

// fakeThinkingSignature 生成一个模拟的 thinking 签名。
// Claude 官方 API 要求 thinking 块携带 base64 签名，客户端（Cherry Studio / AI SDK）
// 会做 schema 校验；代理把上游 reasoning_content 转成 thinking 时没有真实签名，
// 这里伪造一个随机 base64 值以通过校验。签名仅用于客户端格式校验，不做任何签名验证。
func fakeThinkingSignature() string {
	b := make([]byte, 48)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ======================== OpenCode 会话 ========================

var (
	ocSessionID  string
	ocProjectID  string
	ocClientVer  string
	ocOnce       sync.Once
	requestCount atomic.Int64
)

// ======================== 上游请求头指纹 ========================
//
// 2026-10-14 上游把免费层（Bearer public）从 /zen 整体迁到 /inference，
// 门禁从「会话 ID 结构校验」升级为「按官方 2.x 客户端指纹逐字段校验」。
// 逐项消融后的形状（jasonxu114514/opencode2api#83、xbzbing/opencode2dsh#2
// 同日独立复现；/zen 老端点对免费层即使逐字节克隆客户端请求也 403）：
//
//  1. 端点：免费层（keyless）走 /inference/v1/*；keyed（oc_sk_*）车道仍在 /zen。
//  2. UA 必须是三段式 "opencode/<channel>/<version>/<name>"；裸版本号或旧
//     "opencode/<ver> (os arch; runtime)" 形都会被拒 → 403 FreeTierError。
//  3. x-opencode-session-id 必须带（值为反转时间戳 ses_*，见 newOCSessionID）；
//     自造的相关头（如 x-opencode-request）现身即 403。
//  4. x-opencode-project 为裸 40 位小写 hex（形如 "prj_*" 的前缀形即拒）。
//  5. tools 里被官方认识的名字必须带真实定义（description 子串匹配、parameters
//     逐字节相等）——占位桩本身就是拒绝信号；核心五件 shell/edit/glob/grep/read
//     由 ensureFreeTierTools 用真实定义补齐、已声明的同名桩替换。
//  6. body 必须 stream:true；非流式客户端由代理聚合成非流式返回，感知不变。
//
// 10-13 时代的要求/容忍项已失效：x-session-id 头、「会话是否真实注册过」。
// 两个上游基址是 var 而非 const：测试要把重试循环指到本地假上游，
// 才能把「配额类 429 打几次上游」这类行为钉住（见 main_test.go）。
var (
	// 免费层（keyless，Bearer public）车道上游
	ocInferenceBase = "https://opencode.ai/inference"
	// keyed（oc_sk_*）车道上游
	ocZenBase = "https://opencode.ai/zen"
)

const (
	// 免费层按 UA 里的版本号对「floor」判定（低于 floor 回 426），而 floor 随每次官方
	// 发版移动 ⇒ 写死的常量会静默腐烂，这正是 2026-10-17 全量 403 FreeTierError 的根因。
	// 因此版本从官方更新服务取，只在下面留一个兜底地板。
	//
	// 兜底地板：随官方发版手工抬。它同时是「取到的版本比它还旧 ⇒ 丢弃」的地板，
	// 陈旧 UA 会吃 426，从不主动发。fetchOCVersion() 拉的 npm 1.x 线不能当这个地板。
	// 抬地板的触发条件：启动日志出现「[告警] UA 兜底地板已陈旧 baked=<地板> live=<新版本>」
	// ⇒ 把 live 版本抄到这里、同步 CHANGELOG，并确认 main_test.go 的「兜底地板不低于
	// 当前上游」断言已跟着抬（CMP-190）。
	ocUABakedVersion = "2.1.1"
)

// ocUpdateAPIURL 是权威版本源：2.x 线只在这里发布（npm 分发件名 @opencode/cli）。
// 实测（2026-10-17）它给 2.0.32，而 registry.npmjs.org/opencode-ai/latest 与
// GitHub releases/latest 都只给 1.19.3 —— 后两者带不动 2.x 线，从它们取会把
// UA 弄得更旧、直接撞 floor。fetchOCVersion() 拉的就是后者，只用于日志展示。
// 是 var 而非 const：单测要把它指到假上游（与 ocInferenceBase 同理）。
var ocUpdateAPIURL = "https://opencode.ai/update/api/latest/cli"

// minOCVersion / defaultOCVersion 只服务于 fetchOCVersion 的版本号下限校正（日志展示用），
// 不再参与任何门禁判定。
const (
	minOCVersion     = "1.17.0"
	defaultOCVersion = "1.18.31"
)

// ocIDTimestamp 返回 (毫秒时间戳, 该毫秒内的序号)。序号在同一毫秒内自增、
// 跨毫秒归零——上游的 shape 校验按 (ts_ms, counter) 复合值取低 48 位，
// 少了这个序号，同一毫秒内并发生成的 ID 会撞出同一个 value。
var (
	ocIDMu        sync.Mutex
	ocIDLastMilli uint64
	ocIDCounter   uint64
)

func ocIDTimestamp() (uint64, uint64) {
	ocIDMu.Lock()
	defer ocIDMu.Unlock()
	now := uint64(time.Now().UnixMilli())
	if now != ocIDLastMilli {
		ocIDLastMilli = now
		ocIDCounter = 0
	}
	ocIDCounter++
	return now, ocIDCounter
}

// newOCSessionID 生成通过上游免费层门禁的 session id（ses_ + 26 位）。
//
// 关键不是「格式像不像」，而是**语义**：上游 2026-10-11 起（zen 路由改薄代理、
// 免费层门禁下沉到私有 inference 服务）校验 session ID 的反转时间戳结构 ——
// 前 12 位小写 hex 必须是 ~(ts_ms*0x1000 + counter) 的低 48 位
// （对齐上游 packages/schema/src/identifier.ts：session 降序、message 升序）。
// 纯随机或升序一律 403 FreeTierError("OpenCode's free tier can only be used
// from within OpenCode")，对全部免费模型生效。
// 背景与实验矩阵见参考仓 docs/labs/2026-10-13-free-tier-fingerprint-refresh.md；
// 2026-09-18 曾实测「仅格式正则校验」而改成纯随机，那个前提已被上游收紧推翻。
func newOCSessionID() string {
	const hexChars = "0123456789abcdef"
	const alnumChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	ts, counter := ocIDTimestamp()
	value := ^(ts*0x1000 + counter) // descending：按位取反
	var b [26]byte
	for i := range 6 {
		byteVal := byte(value >> (40 - 8*i))
		b[i*2] = hexChars[byteVal>>4]
		b[i*2+1] = hexChars[byteVal&0xf]
	}
	var rnd [14]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		// crypto/rand 不可用时退化为 counter 派生序列，仍保持字符集/长度合规。
		// 前 12 位不受影响 —— 门禁只看前 12 位的时间戳结构。
		for i := range rnd {
			rnd[i] = alnumChars[(counter+uint64(i)*31)%uint64(len(alnumChars))]
		}
	} else {
		for i := range rnd {
			rnd[i] = alnumChars[rnd[i]%byte(len(alnumChars))]
		}
	}
	copy(b[12:], rnd[:])
	return "ses_" + string(b[:])
}

// normalizeOCVersion 保证 UA 版本号不低于上游要求的最小值，
// 避免 npm 拉取失败时回退到过旧版本触发 426。
func normalizeOCVersion(version string) string {
	if compareVersion(version, minOCVersion) >= 0 {
		return version
	}
	return minOCVersion
}

// compareVersion 比较点分十进制版本号，逐段比较数值，相等返回 0。
func compareVersion(a, b string) int {
	an, bn := versionNumbers(a), versionNumbers(b)
	for i := range an {
		if an[i] != bn[i] {
			if an[i] < bn[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionNumbers(v string) [3]int {
	var out [3]int
	parts := strings.Split(v, ".")
	for i := 0; i < len(out) && i < len(parts); i++ {
		n := 0
		for _, c := range parts[i] {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out
}

// 上游控制面调用（版本探测、模型目录）的截止时间。做成 var 而非 const 是为了
// 测试把黑洞用例的等待压到秒级 —— 与 ocInferenceBase 同一个理由（见其定义处）。
var (
	upstreamVersionTimeout = 15 * time.Second
	upstreamModelsTimeout  = 30 * time.Second
)

func fetchOCVersion() string {
	// 这一行是 CMP-119 根因的关键：initOCSession 在 sync.Once 里调本函数，
	// 旧实现请求没有 context、传输层也没有任何超时，一次黑洞就把 Once 焊死，
	// 此后每一次 callOpenCodeAPI / callOpenCodeAPIStream / fetchModels
	// 都在 Once 上永久阻塞 —— 整机停摆且不重启不恢复。
	ctx, cancel := context.WithTimeout(context.Background(), upstreamVersionTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://registry.npmjs.org/opencode-ai/latest", nil)
	req.Header.Set("Accept", "application/json")
	client, _, err := getHTTPClient(req.Context())
	if err != nil {
		return defaultOCVersion
	}
	resp, err := client.Do(req)
	if err != nil {
		return defaultOCVersion
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var info struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(body, &info) == nil && info.Version != "" {
		return normalizeOCVersion(info.Version)
	}
	return defaultOCVersion
}

// ocUAValue 缓存发往上游的 UA。冷启动为空 ⇒ ocClientUA() 返回兜底地板。
var ocUAValue atomic.Value // string

// ocClientUA 返回发往上游的三段式 UA：opencode/<channel>/<version>/<client>。
// 版本由官方更新服务解析（见 refreshOCUA），取不到时退回兜底地板。
func ocClientUA() string {
	if v, ok := ocUAValue.Load().(string); ok && v != "" {
		return v
	}
	return buildOCUA(ocUABakedVersion)
}

// buildOCUA 拼出门禁要求的三段式 UA。抽成纯函数是为了 UA 形状可单测。
func buildOCUA(version string) string {
	return "opencode/latest/" + version + "/cli"
}

// fetchOCUpdateAPIVersion 从官方更新服务取 2.x 线最新 CLI 版本；取不到返回 ""。
func fetchOCUpdateAPIVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ocUpdateAPIURL, nil)
	req.Header.Set("Accept", "application/json")
	client, _, err := getHTTPClient(ctx)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return parseOCUpdateAPIVersion(body)
}

// parseOCUpdateAPIVersion 从更新服务响应里挑出版本号：优先 npm 分发件，
// 其次任一 active 的；都不合规返回 ""（调用方保留当前值）。
// 抽成纯函数是为了可单测 —— 门禁版本错一个版本号就全量 403。
func parseOCUpdateAPIVersion(body []byte) string {
	var info struct {
		Artifacts []struct {
			Version      string `json:"version"`
			Distribution string `json:"distribution"`
			Active       *bool  `json:"active"`
		} `json:"artifacts"`
	}
	if json.Unmarshal(body, &info) != nil {
		return ""
	}
	for _, a := range info.Artifacts {
		if a.Version != "" && a.Distribution == "npm" {
			return a.Version
		}
	}
	for _, a := range info.Artifacts {
		if a.Version != "" && (a.Active == nil || *a.Active) {
			return a.Version
		}
	}
	return ""
}

// acceptUAVersion 决定要不要采用取到的版本号：空（解析/网络失败）或比兜底地板
// 还旧 ⇒ 丢弃。低于 floor 的版本会吃 426，从不主动发。
func acceptUAVersion(next string) bool {
	return next != "" && compareVersion(next, ocUABakedVersion) >= 0
}

// refreshOCUA 用官方更新服务的版本刷新 UA。
// 取失败、或取到的版本低于兜底地板，一律丢弃并保留当前值 ——
// 陈旧 UA 会吃 426，宁可用已知可用的地板也不主动发一个更旧的版本号。
func refreshOCUA() {
	next := fetchOCUpdateAPIVersion()
	if !acceptUAVersion(next) {
		return
	}
	if compareVersion(next, ocUABakedVersion) > 0 {
		log.Printf("[告警] UA 兜底地板已陈旧: baked=%s live=%s —— 请抬 ocUABakedVersion，"+
			"否则官方更新服务不可达时 UA 会低于 floor 而全量 426", ocUABakedVersion, next)
	}
	ua := buildOCUA(next)
	if cur, ok := ocUAValue.Load().(string); ok && cur == ua {
		return
	}
	ocUAValue.Store(ua)
	log.Printf("UA 版本已刷新: %s", ua)
}

func initOCSession() {
	ocOnce.Do(func() {
		ocClientVer = fetchOCVersion()
		ocSessionID = ocCurrentSessionID()
		ocProjectID = randomHex(40)
		// 后台刷新：冷缓存先回兜底地板，启动与请求热路径都不等网络。
		go refreshOCUA()
		log.Printf("OpenCode Version: %s", ocClientVer)
		log.Printf("UA: %s", ocClientUA())
		log.Printf("Session: %s", ocSessionID)
		log.Printf("Project: %s", ocProjectID)
		if !hasInjectedOCSession() {
			log.Printf("提示: 未配置 oc_session_id / OC_SESSION_ID，将自动生成反转时间戳会话。" +
				"上游按会话 ID 的时间戳结构校验、不要求谁签发，自动生成即可过门；" +
				"仍可另配 oc_session_id 固定会话（对会话 sticky 有独立价值）" +
				"或 oc_session_refresh_cmd 让代理自动续命。")
		}
	})
}

func refreshOCSession() {
	ocClientVer = fetchOCVersion()
	ocSessionID = ocCurrentSessionID()
	ocProjectID = randomHex(40)
	go refreshOCUA()
	log.Printf("会话已刷新: version=%s ua=%s session=%s", ocClientVer, ocClientUA(), ocSessionID)
}

// ======================== 模型 ========================

type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

var (
	modelsCache  []ModelInfo
	modelMu      sync.RWMutex
	modelsLoaded bool
)

func fetchModels() ([]ModelInfo, error) {
	// 模型目录同属免费层：2026-10-14 起随业务流迁到 /inference，并要全套指纹头
	// （/zen/v1/models 裸头虽仍 200，但那只是遗留只读面，不代表业务车道）。
	initOCSession() // 保证 ocProjectID 已生成——空 project 头会撞免费层指纹门
	// 同 fetchOCVersion：请求必须带 context，否则黑洞上游下这里永久阻塞，
	// 启动期的模型拉取会把 /v1/models 一起拖死。
	ctx, cancel := context.WithTimeout(context.Background(), upstreamModelsTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ocInferenceBase+"/v1/models", nil)
	sid := ocCurrentSessionID()
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("User-Agent", ocClientUA())
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-session", sid)
	req.Header.Set("x-opencode-session-id", sid)
	req.Header.Set("x-session-affinity", sid)
	req.Header.Set("X-Session-Id", sid)
	req.Header.Set("x-opencode-project", ocProjectID)
	client, _, err := getHTTPClient(req.Context())
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	var models []ModelInfo
	now := time.Now().Unix()
	for _, m := range result.Data {
		models = append(models, ModelInfo{ID: m.ID, Object: "model", Created: now, OwnedBy: "opencode"})
	}
	return models, nil
}

// ======================== 配置 ========================

var (
	port       string
	listenAddr string
	configPath = "config.json"
	modelAlias = map[string]ModelAlias{}

	reasoningEffortMap = map[string]string{}
	debugMode          bool
	configMu           sync.RWMutex

	// ocInjectedSession 是配置注入的真实 OpenCode 会话 id，受 configMu 保护。
	ocInjectedSession string
	// ocRefreshCmd 是会话续命命令；受 configMu 保护。
	ocRefreshCmd string
)

// ocFreeTierHint 点名免费层门禁的校验项（2026-10-14 版），供 403 诊断日志引用
func ocFreeTierHint() string {
	return "免费层门禁（2026-10-14 版，任一不合即 403）：" +
		"(1) 端点 /inference/v1；(2) UA 三段式 " + ocClientUA() + "；" +
		"(3) x-opencode-session-id=反转时间戳 ses_*（见 newOCSessionID）；" +
		"(4) x-opencode-project 为裸 40hex，勿带 x-opencode-request 头；" +
		"(5) tools 含 shell/edit/glob/grep/read 且同名工具是官方真实定义（ensureFreeTierTools 保证）；" +
		"(6) body stream:true"
}

// logFreeTierRejection 在上游回 403 FreeTierError 时无条件记一行诊断。
//
// 403 是硬失败、不是临时故障，且几道门任一失效都表现为它，故不受 debugMode 约束 ——
// 生产默认不开 -debug，此前这类 403 在日志里完全不可见。
// key_id 与 429 行对齐：判「是粘滞 key 撞上失效集合，还是这道门整体失效」全靠它。
// triedExits 是本轮真实打过的出口标签序列：只有一个元素 = 单出口（换 key 无望、换出口
// 才有机会）；多个不同元素仍全 403 = 整类出口都被拒（CMP-128 形态）。事后判「整池被拒」
// vs「单节点被拒」全看这一列。
func logFreeTierRejection(ctx context.Context, modelID, keyID string, status int, body []byte, triedExits []string) {
	if !isFreeTier403(status, body) {
		return
	}
	log.Printf("%s[free tier rejected] model=%s key_id=%s status=%d exits=%s body=%s hint=%s",
		labelPrefix(ctx), modelID, keyID, status, strings.Join(triedExits, ">"), string(body), ocFreeTierHint())
}

// ocSessionRe 匹配 OpenCode 会话 id：ses_ + 12 位小写十六进制 + 14 位 [A-Za-z0-9]。
var ocSessionRe = regexp.MustCompile(`ses_[0-9a-f]{12}[A-Za-z0-9]{14}`)

// refreshOCSessionOnce 运行续命命令，解析出新会话则写回内存与 config.json。
// 返回是否拿到了新会话。命令失败/输出里没有 ses_* 都只记日志，不影响主流程。
func refreshOCSessionOnce() bool {
	configMu.RLock()
	cmd := ocRefreshCmd
	configMu.RUnlock()
	if strings.TrimSpace(cmd) == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sh", "-c", cmd).CombinedOutput()
	if err != nil {
		log.Printf("会话续命命令失败: %v (输出: %s)", err, strings.TrimSpace(string(out)))
		return false
	}
	m := ocSessionRe.Find(out)
	if m == nil {
		log.Printf("会话续命命令未输出 ses_*: %s", strings.TrimSpace(string(out)))
		return false
	}
	sid := string(m)
	if sid == ocCurrentSessionID() {
		return false
	}
	configMu.Lock()
	ocInjectedSession = sid
	ocSessionID = sid
	cmd2 := ocRefreshCmd
	configMu.Unlock()
	log.Printf("会话已自动续命: %s", sid)
	// 落盘必须带**全量**字段：这里自建 AppConfig 字面量只填几个键时，
	// 少填的键会按零值整份覆盖掉 config.json —— socks5_proxies/active_socks5 一没，
	// 下次重启就是 exit=direct 绕开 mihomo 直连出墙，而健康检查一路报绿（2026-10-14 14:18 复发）。
	// 走 configPostBody.merge() 复用「省略即沿用现值」：本次只改会话，
	// 其余字段一律从内存现值回填。POST /api/config 同一条保护。
	cfg := AppConfig{OCSessionRefreshCmd: cmd2}
	configPostBody{OCSessionID: &sid}.merge(&cfg)
	if err := saveConfig(configPath, cfg); err != nil {
		log.Printf("警告: 续命后的会话写回 config 失败: %v", err)
	}
	return true
}

// startOCSessionRefresh 按固定间隔后台续命。间隔取 refresh_interval，
// 未配置时用默认 30 分钟——实测有效期下限 52 分钟，30 分钟留了一倍余量。
func startOCSessionRefresh(interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			refreshOCSessionOnce()
		}
	}()
}

// ocSessionRefreshInterval 解析续命间隔，非法或未设置时回落到 30 分钟。
func ocSessionRefreshInterval() time.Duration {
	const def = 30 * time.Minute
	v := strings.TrimSpace(os.Getenv("OC_SESSION_REFRESH_INTERVAL"))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Printf("警告: OC_SESSION_REFRESH_INTERVAL=%q 无法解析，回落到 %s", v, def)
		return def
	}
	return d
}

// ocCurrentSessionID 返回本次上游请求使用的 x-session-id。
// 上游 2026-10 起校验会话是否存在，编造值必然 403，因此只认注入的真实会话；
// 未配置时返回空串，由 callOpenCodeAPI 转成 503 可读错误——静默 403 无法排查。
// ocCurrentSessionID 返回本次上游请求使用的 x-session-id。
// 配了 oc_session_id 就用配置值（手动指定的真实会话优先，且对「会话 sticky」有独立价值）；
// 没配则用进程内生成的反转时间戳会话 —— 上游校验的是 ID 的时间戳结构而不是「谁签发过」，
// 所以自动生成就能过门，不必强制注入。
func ocCurrentSessionID() string {
	configMu.RLock()
	injected := ocInjectedSession
	configMu.RUnlock()
	if injected != "" {
		return injected
	}
	ocGeneratedMu.Lock()
	defer ocGeneratedMu.Unlock()
	if ocGeneratedSession == "" {
		ocGeneratedSession = newOCSessionID()
		log.Printf("未配置 oc_session_id，使用自动生成的反转时间戳会话: %s（上游按 ID 的时间戳结构校验，不要求谁签发）", ocGeneratedSession)
	}
	return ocGeneratedSession
}

var (
	ocGeneratedMu      sync.Mutex
	ocGeneratedSession string
)

func hasInjectedOCSession() bool {
	configMu.RLock()
	defer configMu.RUnlock()
	return ocInjectedSession != ""
}

// mergeOCSessionID 把面板/脚本回传的 oc_session_id 合进 cfg。
// present=false（请求体省略该键）时沿用现值，避免一次旧版面板回传就把会话静默清空——
// 这个字段是免费层能否通过上游存在性校验的唯一开关，清掉等于退回全量 403。
// configPostBody 是 POST /api/config 的请求体。会被「保存」动作静默清空的字段一律
// 用指针：解到普通值时「键缺失」和「显式置空」是同一个零值，服务端区分不了，
// 于是一次只带部分字段的回传就会把其余字段全量覆盖掉。出口配置被清空尤其致命——
// config.json 里没了 socks5_proxies，容器重启后就是 exit=direct 直连出墙。
type configPostBody struct {
	AppConfig
	ModelAlias         *map[string]ModelAlias `json:"model_alias"`
	ReasoningEffortMap *map[string]string     `json:"reasoning_effort_map"`
	Socks5Proxies      *[]Socks5Proxy         `json:"socks5_proxies"`
	ActiveSocks5       *string                `json:"active_socks5"`
	OCSessionID        *string                `json:"oc_session_id"`
	KeyPool            *KeyPoolConfig         `json:"key_pool"`
}

// merge 把请求体合进 cfg：键缺失一律沿用现值，显式给出（哪怕是空数组/空串）才是「清空」。
// normalizeConfig 会把 nil map 补成空 map，所以沿用必须发生在它之前。
func (b configPostBody) merge(cfg *AppConfig) {
	configMu.RLock()
	if b.ModelAlias == nil {
		cfg.ModelAlias = modelAlias
	} else {
		cfg.ModelAlias = *b.ModelAlias
	}
	if b.ReasoningEffortMap == nil {
		cfg.ReasoningEffortMap = reasoningEffortMap
	} else {
		cfg.ReasoningEffortMap = *b.ReasoningEffortMap
	}
	configMu.RUnlock()
	socks5Mu.RLock()
	if b.Socks5Proxies == nil {
		cfg.Socks5Proxies = socks5Proxies
	} else {
		cfg.Socks5Proxies = *b.Socks5Proxies
	}
	if b.ActiveSocks5 == nil {
		// 403 整类被拒时的回落直连只改内存、不落盘（重启即回原状）。回填若取内存
		// 现值，续命/面板保存就会把回落态整份写回 config.json，可回滚性随之失效，
		// 所以这里一律以磁盘现值为准。
		cfg.ActiveSocks5 = persistedActiveSocks5(activeSocks5)
	} else {
		cfg.ActiveSocks5 = *b.ActiveSocks5
	}
	socks5Mu.RUnlock()
	mergeOCSessionID(cfg, b.OCSessionID != nil, func() string {
		if b.OCSessionID != nil {
			return *b.OCSessionID
		}
		return ""
	}())
	// 面板没有 key 管理 UI，回传体里不会有 key_pool。不做「省略即沿用」的话，
	// 运营在面板点一次保存就会把整池凭据静默清空，免费层随即退化成单凭据。
	if b.KeyPool != nil {
		cfg.KeyPool = b.KeyPool
	} else {
		cfg.KeyPool = currentKeyPool()
	}
}

func mergeOCSessionID(cfg *AppConfig, present bool, val string) {
	if present {
		cfg.OCSessionID = val
		return
	}
	if hasInjectedOCSession() {
		configMu.RLock()
		cfg.OCSessionID = ocInjectedSession
		configMu.RUnlock()
	}
}

// ======================== 管理面板认证 ========================

var (
	adminPassword string
	sessions      = map[string]struct{}{}
	sessionsMu    sync.Mutex
)

func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if adminPassword == "" {
			next(w, r)
			return
		}
		cookie, err := r.Cookie("session")
		if err != nil || cookie.Value == "" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		sessionsMu.Lock()
		_, ok := sessions[cookie.Value]
		sessionsMu.Unlock()
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	if adminPassword == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			renderLoginPage(w, "表单解析失败")
			return
		}
		if r.FormValue("password") != adminPassword {
			renderLoginPage(w, "密码错误")
			return
		}
		token, err := generateToken()
		if err != nil {
			renderLoginPage(w, "创建会话失败")
			return
		}
		sessionsMu.Lock()
		sessions[token] = struct{}{}
		sessionsMu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "session", Value: token, Path: "/", HttpOnly: true})
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	renderLoginPage(w, "")
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	cookie, err := r.Cookie("session")
	if err == nil && cookie.Value != "" {
		sessionsMu.Lock()
		delete(sessions, cookie.Value)
		sessionsMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "session", Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusFound)
}

// ======================== Token 统计 ========================

type ModelStats struct {
	RequestCount     int64 `json:"request_count"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// DailyStats 单日统计，每天0点自动重置
type DailyStats struct {
	Date          string                 `json:"date"`
	TotalRequests int64                  `json:"total_requests"`
	Models        map[string]*ModelStats `json:"models"`
}

type TokenStatsData struct {
	TotalRequests int64                  `json:"total_requests"`
	Models        map[string]*ModelStats `json:"models"`
	Daily         *DailyStats            `json:"daily,omitempty"`
}

var (
	tokenStats     = &TokenStatsData{Models: map[string]*ModelStats{}, Daily: nil}
	tokenStatsMu   sync.Mutex
	tokenStatsPath = "stats.json"
	statsDate      string // 当前统计日期 YYYY-MM-DD
)

// ======================== 数据模型 ========================

type OpenAIRequest struct {
	Model           string    `json:"model"`
	Messages        []Message `json:"messages"`
	Stream          bool      `json:"stream"`
	Temperature     *float64  `json:"temperature,omitempty"`
	MaxTokens       int       `json:"max_tokens,omitempty"`
	TopP            *float64  `json:"top_p,omitempty"`
	Thinking        any       `json:"thinking,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
	// ThinkingLevel 捕获客户端传来的 thinking_level（DeepSeek/RUEINET 等扩展字段），
	// 否则该字段在 json.Unmarshal 时被静默丢弃，无法触发 thinking 补空逻辑。
	ThinkingLevel string         `json:"thinking_level,omitempty"`
	ExtraBody     map[string]any `json:"extra_body,omitempty"`
	Tools         []Tool         `json:"tools,omitempty"`
	ToolChoice    any            `json:"tool_choice,omitempty"`
}

type Message struct {
	Role             string     `json:"role,omitempty"`
	Content          any        `json:"content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	Name             string     `json:"name,omitempty"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ModelAlias struct {
	TargetModel     string `json:"target_model"`
	MultimodalModel string `json:"multimodal_model,omitempty"`
}

// UnmarshalJSON 兼容旧配置中的 "alias": "target-model" 写法。
func (m *ModelAlias) UnmarshalJSON(data []byte) error {
	var legacy string
	if err := json.Unmarshal(data, &legacy); err == nil {
		m.TargetModel = strings.TrimSpace(legacy)
		return nil
	}
	type modelAliasPlain ModelAlias
	var value modelAliasPlain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	value.TargetModel = strings.TrimSpace(value.TargetModel)
	*m = ModelAlias(value)
	return nil
}

type AppConfig struct {
	ModelAlias map[string]ModelAlias `json:"model_alias"`

	ReasoningEffortMap map[string]string `json:"reasoning_effort_map"`
	Socks5Proxies      []Socks5Proxy     `json:"socks5_proxies,omitempty"`
	ActiveSocks5       string            `json:"active_socks5,omitempty"`

	// ListenAddr 是监听主机（不含端口）。留空 = 沿用历史行为（":"+port，绑全网卡）。
	// 命令行 -listen 与环境变量 OC_LISTEN_ADDR 优先于本字段。
	ListenAddr string `json:"listen_addr,omitempty"`

	// OCSessionID 是 OpenCode 客户端真实创建过的会话 id。上游 2026-10 起校验
	// 会话存在性，编造的 ses_* 一律 403；留空则拒绝服务（503），不再编造。
	OCSessionID string `json:"oc_session_id,omitempty"`

	// OCSessionRefreshCmd 是可执行命令：代理定期运行它，从 stdout/stderr 里解析
	// 出新的 ses_* 并写回配置，用于会话过期后自动续命（有效期下限实测 ≥52 分钟，
	// 上限未知）。留空则只用 OCSessionID，不自动续。
	OCSessionRefreshCmd string `json:"oc_session_refresh_cmd,omitempty"`

	// KeyPool 是上游凭据池。免费层限流维度实测为（模型 × 凭据）而非 IP / 会话，
	// 单凭据的日预算就是整条链路的日预算；把凭据维度扩回多值才能放大总额度。
	// 未启用或池为空时鉴权回落到 Bearer public，行为与本字段引入前完全一致。
	KeyPool *KeyPoolConfig `json:"key_pool,omitempty"`
}

// ======================== key_pool（多凭据轮转） ========================

// KeyEntry 是池中的一条凭据。兼容老容器 config.example.json 记录的两种写法：
// 完整对象，或直接写一个字符串（UnmarshalJSON 补成 {"key": ...}）。
// id 缺省时由 normalizeKeyPool 按数组位置补 k1..kN —— 老容器日志里的 key_id=k82
// 就是位置号，而现网那 120 条里有 86 条根本没有 id 字段，不补号诊断行对不上。
type KeyEntry struct {
	ID      string `json:"id,omitempty"`
	Key     string `json:"key"`
	Weight  int    `json:"weight,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
}

func (e *KeyEntry) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		e.Key = strings.TrimSpace(s)
		return nil
	}
	type plain KeyEntry // 别名类型，避免 UnmarshalJSON 自我递归
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*e = KeyEntry(p)
	e.Key = strings.TrimSpace(e.Key)
	e.ID = strings.TrimSpace(e.ID)
	return nil
}

// KeyPoolConfig 对齐老容器 key_pool 的语义，只取本卡需要的子集。
// 老容器还有 max_retries / retry_on / cooldown_secs / blacklist_after，
// 这里保留 max_retries（换凭据的校准旋钮），其余属于冷却/黑名单状态机，不做。
type KeyPoolConfig struct {
	Enabled    bool       `json:"enabled"`
	Strategy   string     `json:"strategy,omitempty"` // sticky（默认）| round_robin
	MaxRetries int        `json:"max_retries,omitempty"`
	Keys       []KeyEntry `json:"keys,omitempty"`
}

const (
	keyStrategySticky     = "sticky"
	keyStrategyRoundRobin = "round_robin"
	// keyPoolDefaultRetries 是「凭据额度耗尽后额外换几个 key」的上限。取 4 是折中：
	// 现网 86 个凭据里 37 个有额度时，4 次内命中活凭据的概率约 89%，8 次约 97%；
	// 而每次 429 重试都要等一个上游往返，太多会把首字节延迟拖到分钟级。
	// 现网实测偏紧时调大 config 的 key_pool.max_retries 即可，不用改代码。
	keyPoolDefaultRetries = 4
)

// normalizeKeyPool 归一化凭据池：补位置 id、剔除停用/空值、按凭据值去重。
// 去重是有实际收益的 —— 同一凭据在池里出现两次不会多拿到一份配额，
// 只会把轮转位白占一半（现网 120 条里就有 34 条是重复项）。
func normalizeKeyPool(kp *KeyPoolConfig) {
	if kp == nil {
		return
	}
	kp.Strategy = strings.TrimSpace(kp.Strategy)
	if kp.Strategy == "" {
		kp.Strategy = keyStrategySticky
	}
	if kp.MaxRetries <= 0 {
		kp.MaxRetries = keyPoolDefaultRetries
	}
	seen := make(map[string]bool, len(kp.Keys))
	out := make([]KeyEntry, 0, len(kp.Keys))
	for i, k := range kp.Keys {
		k.Key = strings.TrimSpace(k.Key)
		if k.Key == "" {
			continue
		}
		if k.Enabled != nil && !*k.Enabled {
			continue
		}
		if seen[k.Key] {
			continue
		}
		seen[k.Key] = true
		k.ID = strings.TrimSpace(k.ID)
		if k.ID == "" {
			k.ID = fmt.Sprintf("k%d", i+1)
		}
		out = append(out, k)
	}
	kp.Keys = out
}

// ocKey 是一条归一化后可用的凭据。val 只进 Authorization 头，不进任何日志。
type ocKey struct {
	id  string
	val string
}

// ocKeys 是生效中的凭据池与轮转游标。零值即「未启用」，所有取值都回落到 public。
var ocKeys struct {
	mu       sync.Mutex
	keys     []ocKey
	strategy string
	retries  int
	sticky   map[string]int
	rr       int
	// cfg 是归一化后的池子副本，只给 /api/config 的 GET 回读用（面板要能确认
	// 线上到底挂着哪些凭据）。存副本而不是指针，避免与并发保存的数据竞争。
	cfg KeyPoolConfig
}

// currentKeyPool 返回当前生效池的副本，供面板回传时「省略即沿用」用。
func currentKeyPool() *KeyPoolConfig {
	ocKeys.mu.Lock()
	defer ocKeys.mu.Unlock()
	cp := ocKeys.cfg
	cp.Keys = append([]KeyEntry(nil), ocKeys.cfg.Keys...)
	return &cp
}

// installKeyPool 装载生效中的凭据池并打一行启动日志 —— 运营不必翻配置文件
// 就能确认池子到底挂上了几个、用的什么策略。
func installKeyPool(kp *KeyPoolConfig) {
	var live []ocKey
	strategy := keyStrategySticky
	retries := keyPoolDefaultRetries
	stored := KeyPoolConfig{Strategy: keyStrategySticky, MaxRetries: keyPoolDefaultRetries}
	if kp != nil {
		// 在这里归一化而不是指望调用方：loadConfig / 面板 POST / 未来的热更入口
		// 未必都先走过 normalizeConfig。漏归一化的后果很隐蔽 ——
		// max_retries=0 会让 429 换凭据的重试次数算成 0，整个轮转机制静默失效，
		// 而日志里「凭据数=N」看着一切正常。
		normalized := *kp
		normalizeKeyPool(&normalized)
		stored = normalized
		strategy = normalized.Strategy
		retries = normalized.MaxRetries
		if normalized.Enabled {
			for _, k := range normalized.Keys {
				live = append(live, ocKey{id: k.ID, val: k.Key})
			}
		}
	}
	ocKeys.mu.Lock()
	ocKeys.keys = live
	ocKeys.strategy = strategy
	ocKeys.retries = retries
	ocKeys.sticky = make(map[string]int, 8)
	ocKeys.rr = 0
	ocKeys.cfg = stored
	ocKeys.mu.Unlock()
	if len(live) > 0 {
		log.Printf("key_pool: 生效中，凭据数=%d strategy=%s max_retries=%d", len(live), strategy, retries)
		return
	}
	log.Printf("key_pool: 未启用或池为空，鉴权回落到 Bearer public")
}

// keyPoolSize 返回生效凭据数。
func keyPoolSize() int {
	ocKeys.mu.Lock()
	defer ocKeys.mu.Unlock()
	return len(ocKeys.keys)
}

// keyPoolExtraAttempts 返回「额度耗尽后允许多试几轮」。
// 池内还有 n 个凭据时最多换 min(max_retries, n-1) 次，总尝试数 = 1 + 它。
func keyPoolExtraAttempts() int {
	ocKeys.mu.Lock()
	defer ocKeys.mu.Unlock()
	n := len(ocKeys.keys)
	if n <= 1 {
		return 0
	}
	if ocKeys.retries > n-1 {
		return n - 1
	}
	return ocKeys.retries
}

// pickOCKey 选出本次上游请求该用的凭据。
// sticky：按 x-session-id 粘住一个 key，中途不换（每次换 key 上游都当新身份，
// 可能白吃掉一份配额窗口），直到它 429 被 advanceOCKey 挪走。
// round_robin：每个请求推进一次游标，把负载摊到全部凭据上。
// 会话与凭据是两条独立的线：实测同一个 ses_* 配不同 oc_sk_ 都能过上游两道门，
// 所以不需要为每个 key 单独注册会话。
func pickOCKey(session string) (ocKey, bool) {
	ocKeys.mu.Lock()
	defer ocKeys.mu.Unlock()
	n := len(ocKeys.keys)
	if n == 0 {
		return ocKey{}, false
	}
	idx := 0
	if ocKeys.strategy == keyStrategyRoundRobin {
		idx = ocKeys.rr % n
		ocKeys.rr++
	} else {
		idx = ocKeys.sticky[session] % n
	}
	return ocKeys.keys[idx], true
}

// advanceOCKey 把会话从已耗尽的凭据上挪开，返回换到的 key id（池空则空串）。
// round_robin 不用动：pick 的游标在失败那轮已经 +1，下一次 pick 必然换凭据。
func advanceOCKey(session string) string {
	ocKeys.mu.Lock()
	defer ocKeys.mu.Unlock()
	n := len(ocKeys.keys)
	if n <= 1 || ocKeys.strategy == keyStrategyRoundRobin {
		return ""
	}
	nxt := (ocKeys.sticky[session]%n + 1) % n
	ocKeys.sticky[session] = nxt
	return ocKeys.keys[nxt].id
}

// ======================== 配额类 429 熔断 ========================
//
// 上游对「免费层模型日配额耗尽」一律回 429 + body.type=FreeUsageLimitError，
// 且 Retry-After 精确指向次日 00:00 UTC（CMP-48 实测 94/94，跨 12.2h 离散 ≤4s）。
//
// 反向证据（所以轮转不能整个删掉）：命中过 429 的 31 发里 27 发最终 200 —— 轮转在
// 「池里还有活凭据」时是有救的。因此这里只对**配额类**收手，凭据级/瞬时限流照旧
// 换 key 换出口多试；配额类则单发只打一次上游，不再连试（判据 1 + 判据 3），
// 同模型连撞到阈值即整段熔断，后续请求连上游都不打（判据 2）。
//
// 代价（已知，取舍留痕）：27/31 的救回靠的正是第 2–5 次轮转，配额类单发只打一次
// 之后这部分救回会丢，换来的是首字节延迟从 8–22s 回到 ~1s（CMP-48 §3）。
// 要换回救回率，把下面 429 分支里的 `if quota` 短路段改成带重试预算的版本即可。
//
// 熔断保持时间按 Retry-After 截断到上限：Retry-After 的**声明**值不可信
// （CMP-26 实测真实恢复早于声明边界最多 8 小时，且是渐进排空），
// 照它睡 9 小时会把一个已经能用的模型挡在外面。
const (
	// 同模型连续撞到这么多次配额 429 ⇒ 熔断
	quotaBreakerThreshold = 3
	// 「连续」的计数窗口，超窗即重新计数
	quotaBreakerWindow = 10 * time.Minute
	// 熔断保持时长上限 / 下限
	quotaBreakerMaxHold = 15 * time.Minute
	quotaBreakerMinHold = 30 * time.Second
)

type quotaBreakerEntry struct {
	consecutive int
	firstAt     time.Time
	openUntil   time.Time
	retryAfter  string
	// clientStatus/clientBody 是熔断期回给客户端的东西，由打开熔断的那次命中的
	// 形态决定：配额耗尽回 429 FreeUsageLimitError，端点不可用回 503 EndpointUnavailableError。
	clientStatus int
	clientBody   []byte
}

var (
	quotaBreakerMu sync.Mutex
	quotaBreakers  = map[string]*quotaBreakerEntry{}
)

// parseRetryAfterSeconds 解析 Retry-After 的整数秒形态。上游实测都是整数秒，
// HTTP-date 形态不认（认了也只会落到别的分支，不影响配额判定）。
func parseRetryAfterSeconds(v string) (int, bool) {
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// isQuotaClass429 判定「免费层模型日配额耗尽」类 429。
// 主判据是 body 指纹 FreeUsageLimitError；Retry-After > 60s 只作兜底，
// 好让上游换措辞时仍能识别（瞬时类 `Endpoint is unavailable` 的 Retry-After 是空串，不会误命中）。
func isQuotaClass429(h http.Header, body []byte) bool {
	if bytes.Contains(body, []byte("FreeUsageLimitError")) {
		return true
	}
	if n, ok := parseRetryAfterSeconds(h.Get("Retry-After")); ok && n > 60 {
		return true
	}
	return false
}

// isEndpointUnavailableUpstream 判定「端点/模型级不可用」：只认 message
// `Endpoint is unavailable`，状态码只认上游实测用过的两种形态（429 与 403）。
//
// 同一份 body 上游两种状态码都回过（CMP-46 实测）；2026-10-17 现网复核还是同一句，
// 只是形态轮到 429（CMP-129）。两个形态共用这一个判定，不要复制第二份判断。
//
// 绝不认整个 `type:"server_error"`：同 type 下 `Model is unavailable` 是模型已下线的
// 永久错误（实测 400），归进来会把一个恢复无望的模型按整段不可用处理。
func isEndpointUnavailableUpstream(status int, body []byte) bool {
	if status != http.StatusTooManyRequests && status != http.StatusForbidden {
		return false
	}
	return bytes.Contains(body, []byte("Endpoint is unavailable"))
}

// isRetryableQuotaUpstream 是「换出口 + 换凭据后值得再试」的窄判据：
// 429，以及**以 403 形态回来的同一份瞬时上游错误**。
//
// 上游把 `Endpoint is unavailable`（server_error）有时回 429、有时回 403，
// body 逐字相同（CMP-46 实测）。429 形态本来就进重试；403 形态原先直接落透传分支，
// 既不换出口也不换凭据且零日志 —— 出口抽到持续报错的节点时会**永久卡死**。
func isRetryableQuotaUpstream(status int, body []byte) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	return isEndpointUnavailableUpstream(status, body)
}

// isFreeTier403 判定「凭据级」的 403 FreeTierError。
//
// CMP-109（2026-10-17 14:12–14:16 CST）现网五个免费层模型同时刻全灭，受控金丝雀
// 同窗口交错复现：池里 k1/k2/k10 回 200 而 k43/k86 稳定回 403 FreeTierError，
// 会话新鲜度与出口两条假设都被单变量实验证伪。⇒ 上游对**部分凭据**回这道门，
// 粘滞 key 一旦撞上失效集合就整机 403，而池中其余凭据全部闲置、且不自愈。
//
// 修法只换 key：换出口/换会话已被实验证伪，跟着换只会白烧额度。
// 别宽成整个 `type:"server_error"`：同 type 下 `Model is unavailable` 是模型已下线的
// 永久错误（实测 400），拖进重试同样白烧。
func isFreeTier403(status int, body []byte) bool {
	return status == http.StatusForbidden && bytes.Contains(body, []byte("FreeTierError"))
}

// quotaBreakerBlocked 熔断期内返回要回给客户端的状态码/body 与缓存的 Retry-After。
// 熔断期满即删除条目，让下一发真实探一次上游。
func quotaBreakerBlocked(model string) (int, []byte, string, bool) {
	quotaBreakerMu.Lock()
	defer quotaBreakerMu.Unlock()
	e := quotaBreakers[model]
	if e == nil || e.openUntil.IsZero() {
		return 0, nil, "", false
	}
	if time.Now().After(e.openUntil) {
		delete(quotaBreakers, model)
		return 0, nil, "", false
	}
	return e.clientStatus, e.clientBody, e.retryAfter, true
}

// noteQuotaExhausted 记一次配额 429；同模型在窗口内连撞到阈值即打开熔断。
func noteQuotaExhausted(model, retryAfter string) {
	noteBreakerHit(model, retryAfter, http.StatusTooManyRequests, quotaExhaustedBody())
}

// noteEndpointUnavailable 记一次「端点/模型级不可用」。计数与保持时间复用 CMP-83 的
// 按模型熔断（同一个 map、同一套阈值），只有回给客户端的东西不同。
func noteEndpointUnavailable(model string) {
	noteBreakerHit(model, "", http.StatusServiceUnavailable, endpointUnavailableBody())
}

func noteBreakerHit(model, retryAfter string, clientStatus int, clientBody []byte) {
	now := time.Now()
	quotaBreakerMu.Lock()
	defer quotaBreakerMu.Unlock()
	e := quotaBreakers[model]
	if e == nil || now.Sub(e.firstAt) > quotaBreakerWindow {
		e = &quotaBreakerEntry{firstAt: now}
		quotaBreakers[model] = e
	}
	e.consecutive++
	e.retryAfter = retryAfter
	e.clientStatus = clientStatus
	e.clientBody = clientBody
	if e.consecutive < quotaBreakerThreshold {
		return
	}
	hold := quotaBreakerMaxHold
	if n, ok := parseRetryAfterSeconds(retryAfter); ok {
		if d := time.Duration(n) * time.Second; d < hold {
			hold = d
		}
	}
	if hold < quotaBreakerMinHold {
		hold = quotaBreakerMinHold
	}
	e.openUntil = now.Add(hold)
}

// noteUpstreamOK 该模型拿到 2xx：清零计数并解除熔断。
func noteUpstreamOK(model string) {
	quotaBreakerMu.Lock()
	delete(quotaBreakers, model)
	quotaBreakerMu.Unlock()
}

// quotaExhaustedBody 熔断快速失败时回给客户端的 body。形状与上游 FreeUsageLimitError
// 一致，免得客户端按两套错误结构分支。
func quotaExhaustedBody() []byte {
	return []byte(`{"type":"error","error":{"type":"FreeUsageLimitError","message":"Rate limit exceeded. Please try again later."}}`)
}

// endpointUnavailableBody 「端点/模型级不可用」回给客户端的 body，刻意与配额耗尽
// 不同形态：配额是 429 + FreeUsageLimitError + Retry-After（客户端按配额退避），
// 这里是 503 且不带 Retry-After（上游连配额窗口都没给，重发同一请求不会更快成功）。
// 429 + 空 Retry-After 会让客户端立刻重试形成打转，这是本形态必须单独成型的理由。
func endpointUnavailableBody() []byte {
	return []byte(`{"type":"error","error":{"type":"EndpointUnavailableError","message":"Upstream endpoint is unavailable for this model. This is not a rate limit; retrying now will not succeed sooner."}}`)
}

// quotaExhaustedHeaders 熔断快速失败时的响应头，带上最后一次观测到的 Retry-After。
func quotaExhaustedHeaders(retryAfter string) http.Header {
	h := http.Header{}
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	return h
}

// ======================== Claude Messages API 类型 ========================

type ClaudeRequest struct {
	Model       string          `json:"model"`
	Messages    []ClaudeMessage `json:"messages"`
	System      any             `json:"system,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Tools       []ClaudeTool    `json:"tools,omitempty"`
	ToolChoice  any             `json:"tool_choice,omitempty"`
	Metadata    any             `json:"metadata,omitempty"`
	Thinking    any             `json:"thinking,omitempty"`
}

type ClaudeMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type ClaudeContent struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     any    `json:"input,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"`
}

type ClaudeTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema"`
}

type ClaudeResponse struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Role       string          `json:"role"`
	Content    []ClaudeContent `json:"content"`
	Model      string          `json:"model"`
	StopReason string          `json:"stop_reason"`
	Usage      *ClaudeUsage    `json:"usage,omitempty"`
}

type ClaudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ======================== Responses API 类型 ========================

type ResponsesAPIRequest struct {
	Model             string          `json:"model"`
	Input             any             `json:"input"`
	Messages          []Message       `json:"messages,omitempty"`
	Instructions      string          `json:"instructions,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	Temperature       float64         `json:"temperature,omitempty"`
	MaxTokens         int             `json:"max_output_tokens,omitempty"`
	TopP              float64         `json:"top_p,omitempty"`
	FrequencyPenalty  float64         `json:"frequency_penalty,omitempty"`
	PresencePenalty   float64         `json:"presence_penalty,omitempty"`
	Reasoning         ReasonEffort    `json:"reasoning,omitempty"`
	Include           []string        `json:"include,omitempty"`
	Store             *bool           `json:"store,omitempty"`
	Tools             []ResponsesTool `json:"tools,omitempty"`
	ToolChoice        any             `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	Stop              any             `json:"stop,omitempty"`
	User              string          `json:"user,omitempty"`
	StreamOptions     any             `json:"stream_options,omitempty"`
	Metadata          any             `json:"metadata,omitempty"`
}

type ResponsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  map[string]any  `json:"parameters,omitempty"`
	Function    *ToolFunction   `json:"function,omitempty"`
	Tools       []ResponsesTool `json:"tools,omitempty"`
}

type ResponseToolNameMapping struct {
	Namespace string
	Name      string
}

type ReasonEffort struct {
	Effort string `json:"effort,omitempty"`
}

// ======================== 配置管理 ========================

func loadConfig(path string) AppConfig {
	var cfg AppConfig
	data, err := os.ReadFile(path)
	if err != nil {
		normalizeConfig(&cfg)
		return cfg
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("警告: 配置文件解析失败: %v", err)
	}
	normalizeConfig(&cfg)
	return cfg
}

// persistedActiveSocks5 返回 config.json 里持久化的出口配置。fallback 只在磁盘
// 读不到/解析失败时兜底——那类部署下 saveConfig 同样写不进去，不会造成新的持久化。
func persistedActiveSocks5(fallback string) string {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fallback
	}
	var cfg AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("警告: 读取 config.json 的 active_socks5 失败: %v，沿用内存现值", err)
		return fallback
	}
	return cfg.ActiveSocks5
}

func normalizeConfig(cfg *AppConfig) {
	if cfg.ModelAlias == nil {
		cfg.ModelAlias = map[string]ModelAlias{}
	}
	for name, alias := range cfg.ModelAlias {
		alias.TargetModel = strings.TrimSpace(alias.TargetModel)
		if alias.TargetModel == "" {
			alias.TargetModel = strings.TrimSpace(name)
		}
		alias.MultimodalModel = strings.TrimSpace(alias.MultimodalModel)
		cfg.ModelAlias[name] = alias
	}
	if cfg.ReasoningEffortMap == nil {
		cfg.ReasoningEffortMap = map[string]string{}
	}
	cfg.OCSessionID = strings.TrimSpace(cfg.OCSessionID)
	cfg.OCSessionRefreshCmd = strings.TrimSpace(cfg.OCSessionRefreshCmd)
	normalizeKeyPool(cfg.KeyPool)
}

func saveConfig(path string, cfg AppConfig) error {
	normalizeConfig(&cfg)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func applyConfig(cfg AppConfig) {
	configMu.Lock()
	defer configMu.Unlock()
	if cfg.ModelAlias != nil {
		modelAlias = cfg.ModelAlias
	}
	if cfg.ReasoningEffortMap != nil {
		reasoningEffortMap = cfg.ReasoningEffortMap
	}
	ocInjectedSession = cfg.OCSessionID
	ocRefreshCmd = cfg.OCSessionRefreshCmd
	if ocInjectedSession != "" && ocSessionID != "" {
		// 面板改会话后立即生效，无需重启：把已初始化的会话也换掉。
		ocSessionID = ocInjectedSession
	}
	// 凭据池同理热更：面板存一次配置就生效，不必重启容器。
	installKeyPool(cfg.KeyPool)

	socks5Mu.Lock()
	proxiesUpdated := false
	if cfg.Socks5Proxies != nil {
		socks5Proxies = cfg.Socks5Proxies
		proxiesUpdated = true
	}
	if proxiesUpdated || activeSocks5 != cfg.ActiveSocks5 {
		activeSocks5 = cfg.ActiveSocks5
		closeHTTPClientIdle(socks5Client)
		socks5Client = nil
		socks5ClientKey = ""
		atomic.StoreUint32(&socks5RRIndex, 0)
		atomic.StoreUint32(&socks5RateLimitIndex, 0)
	}
	socks5Mu.Unlock()
}

func resolveModel(model string) (string, string, bool) {
	m := strings.TrimSpace(model)
	configMu.RLock()
	alias, ok := modelAlias[m]
	configMu.RUnlock()
	if !ok {
		return "", "", false
	}
	if alias.TargetModel == "" {
		alias.TargetModel = m
	}
	return alias.TargetModel, alias.MultimodalModel, true
}

func getReasoningEffortMap() map[string]string {
	configMu.RLock()
	defer configMu.RUnlock()
	copyMap := make(map[string]string, len(reasoningEffortMap))
	for key, value := range reasoningEffortMap {
		copyMap[key] = value
	}
	return copyMap
}

// ======================== Token 统计 ========================

func getToday() string {
	return time.Now().Format("2006-01-02")
}

func checkAndResetDailyStats() {
	today := getToday()
	tokenStatsMu.Lock()
	reset := false
	if statsDate == "" {
		statsDate = today
		if tokenStats.Daily == nil || tokenStats.Daily.Date != today {
			tokenStats.Daily = &DailyStats{Date: today, Models: map[string]*ModelStats{}}
		}
	} else if statsDate != today {
		log.Printf("[统计] 日期变更 %s -> %s，重置每日统计并刷新 OpenCode 会话", statsDate, today)
		statsDate = today
		tokenStats.Daily = &DailyStats{Date: today, Models: map[string]*ModelStats{}}
		reset = true
	}
	tokenStatsMu.Unlock()
	// 每日统计重置时同步刷新客户端身份，保持 opencode 上游版本/session/project 最新。
	// 在锁外执行：refreshOCSession 内部会做网络请求（拉取 npm 版本），避免阻塞统计锁。
	if reset {
		refreshOCSession()
	}
}

func loadTokenStats() {
	data, err := os.ReadFile(tokenStatsPath)
	if err != nil {
		checkAndResetDailyStats()
		return
	}
	var st TokenStatsData
	if err := json.Unmarshal(data, &st); err != nil {
		checkAndResetDailyStats()
		return
	}
	tokenStatsMu.Lock()
	if st.Models == nil {
		st.Models = map[string]*ModelStats{}
	}
	today := getToday()
	if st.Daily != nil && st.Daily.Date != today {
		log.Printf("[统计] 每日统计日期 %s 已过期，重置", st.Daily.Date)
		st.Daily = &DailyStats{Date: today, Models: map[string]*ModelStats{}}
	} else if st.Daily == nil {
		st.Daily = &DailyStats{Date: today, Models: map[string]*ModelStats{}}
	}
	statsDate = today
	tokenStats = &st
	tokenStatsMu.Unlock()
}

// statsDirty 表示内存统计比磁盘新。由 recordTokenUsage 打标，后台 statsFlusher 落盘。
var statsDirty atomic.Bool

// statsFlushInterval 统计落盘间隔。
// 原来 recordTokenUsage 是每请求一次 go saveTokenStats()：整份 MarshalIndent + 重写 stats.json，
// 高频请求时是明显的 I/O/CPU 热点；而且并发写同一个文件可能写出交错内容把 stats.json 写坏
// （写坏后下次启动解析失败 → 统计被静默清零）。
var statsFlushInterval = 5 * time.Second

func markTokenStatsDirty() { statsDirty.Store(true) }

func saveTokenStats() {
	tokenStatsMu.Lock()
	data, err := json.MarshalIndent(tokenStats, "", "  ")
	tokenStatsMu.Unlock()
	if err != nil {
		return
	}
	// 先写临时文件再 rename：避免写到一半进程退出留下被截断的 stats.json
	tmp := tokenStatsPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return
	}
	os.Rename(tmp, tokenStatsPath)
}

// statsFlusher 后台定时落盘，只有脏数据才写。
func statsFlusher() {
	ticker := time.NewTicker(statsFlushInterval)
	defer ticker.Stop()
	for range ticker.C {
		if statsDirty.Swap(false) {
			saveTokenStats()
		}
	}
}

func recordTokenUsage(model string, promptTokens, completionTokens, totalTokens int64) {
	checkAndResetDailyStats()
	tokenStatsMu.Lock()
	tokenStats.TotalRequests++
	ms, ok := tokenStats.Models[model]
	if !ok {
		ms = &ModelStats{}
		tokenStats.Models[model] = ms
	}
	ms.RequestCount++
	ms.PromptTokens += promptTokens
	ms.CompletionTokens += completionTokens
	ms.TotalTokens += totalTokens
	if tokenStats.Daily == nil {
		tokenStats.Daily = &DailyStats{Date: getToday(), Models: map[string]*ModelStats{}}
	}
	tokenStats.Daily.TotalRequests++
	dms, ok := tokenStats.Daily.Models[model]
	if !ok {
		dms = &ModelStats{}
		tokenStats.Daily.Models[model] = dms
	}
	dms.RequestCount++
	dms.PromptTokens += promptTokens
	dms.CompletionTokens += completionTokens
	dms.TotalTokens += totalTokens
	tokenStatsMu.Unlock()
	markTokenStatsDirty()
}

// ======================== Thinking/Reasoning 参数 ========================

func thinkingMode(value any) (string, bool) {
	switch typed := value.(type) {
	case map[string]any:
		mode, _ := typed["type"].(string)
		if mode == "enabled" || mode == "disabled" {
			return mode, true
		}
	case bool:
		if typed {
			return "enabled", true
		}
		return "disabled", true
	}
	return "", false
}

// ======================== 消息处理 ========================
// normalizeContent 是 dumb pipe 透传：保留 string 与 []any 两种入参形状
// （其它非常规类型走 json.Marshal 兜底），不解析或过滤任何 multimodal part。
// 能力协商由 opencode 客户端 + 上游负责；这里既不"硬降级"也不"补全"。
func normalizeContent(content any) any {
	if content == nil {
		return nil
	}
	if s, ok := content.(string); ok {
		return s
	}
	if arr, ok := content.([]any); ok {
		return arr
	}
	b, err := json.Marshal(content)
	if err != nil {
		return nil
	}
	return string(b)
}

// hasImageContent 检测消息中是否包含图像（全量扫描，用于统计/调试）。
// 注意：路由判定应使用 hasNewImageContent（只查本轮新增图片），
// 否则历史图片会永久锁定多模态路由。
func hasImageContent(messages []Message) bool {
	for _, msg := range messages {
		if contentHasImage(msg.Content) {
			return true
		}
	}
	return false
}

// contentHasImage 检查单个消息的 content 是否包含图片 part。
func contentHasImage(content any) bool {
	arr, ok := content.([]any)
	if !ok {
		return false
	}
	for _, item := range arr {
		if block, ok := item.(map[string]any); ok {
			switch block["type"] {
			case "image_url", "input_image", "image":
				return true
			}
		}
	}
	return false
}

// hasNewImageContent 只检测"本轮"是否新增了图片，用于多模态路由判定。
// codex 每轮请求都携带完整历史，历史图片不应影响本轮路由。
// 判定策略（已用 codex 真实请求结构验证）：
//
//	① 最后一个 user 消息自身 content 含图 → 本轮新增图片（最常见）
//	② 最后一个 user 消息紧邻的前 1 条是 tool 输出且含图 → 本轮 view_image 场景
//
// 其余情况（包括历史图片）都不视为本轮新增，返回 false。
func hasNewImageContent(messages []Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			if contentHasImage(messages[i].Content) {
				return true
			}
			if i-1 >= 0 && messages[i-1].Role == "tool" &&
				contentHasImage(messages[i-1].Content) {
				return true
			}
			return false
		}
	}
	return false
}

// stripImagesForTextModel 路由到纯文本模型（如 deepseek）时调用：
// 把请求中所有图片 part 替换为文本占位，避免文本模型上游报错。
// 因为 codex 每轮携带全量历史，历史图片也必须一并剥离。
func stripImagesForTextModel(messages []Message) []Message {
	for i := range messages {
		parts, ok := messages[i].Content.([]any)
		if !ok {
			continue
		}
		changed := false
		cleaned := make([]any, 0, len(parts))
		for _, p := range parts {
			block, ok := p.(map[string]any)
			if !ok {
				// 裸字符串元素：保留为 text block（上游要求 content 全为 block）
				if s, ok := p.(string); ok {
					cleaned = append(cleaned, map[string]any{"type": "text", "text": s})
					changed = true
				} else {
					cleaned = append(cleaned, p)
				}
				continue
			}
			switch block["type"] {
			case "image_url", "input_image", "image":
				// 图片 → 文本占位（文本模型不认图，但认得上下文说明）
				cleaned = append(cleaned, map[string]any{
					"type": "text",
					"text": "[图片已省略]",
				})
				changed = true
			default:
				cleaned = append(cleaned, p)
			}
		}
		if changed {
			messages[i].Content = cleaned
		}
	}
	return messages
}

func fixToolCallGaps(messages []Message) []Message {
	// 没有任何工具调用/工具结果时直接返回原切片：旧实现无条件新建 map 并把整个消息切片
	// 复制一份（长历史下每个纯文本请求都要白造几十 KB 垃圾）。
	hasToolFlow := false
	for i := range messages {
		if messages[i].Role == "tool" || len(messages[i].ToolCalls) > 0 {
			hasToolFlow = true
			break
		}
	}
	if !hasToolFlow {
		return messages
	}
	toolResponses := map[string]*Message{}
	for i := range messages {
		if messages[i].Role == "tool" && messages[i].ToolCallID != "" {
			toolResponses[messages[i].ToolCallID] = &messages[i]
		}
	}
	fixed := make([]Message, 0, len(messages)+len(messages)/4)
	emitted := map[string]bool{}
	for _, msg := range messages {
		if msg.Role == "tool" && msg.ToolCallID != "" {
			if emitted[msg.ToolCallID] {
				continue
			}
		}
		fixed = append(fixed, msg)
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				if resp, found := toolResponses[tc.ID]; found {
					fixed = append(fixed, *resp)
				} else {
					fixed = append(fixed, Message{Role: "tool", ToolCallID: tc.ID, Content: "Tool call result not available"})
				}
				emitted[tc.ID] = true
			}
		}
	}
	return fixed
}

func normalizeToolCallArguments(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "{}", nil
	}
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return "", err
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return "", fmt.Errorf("must decode to JSON object, got %T", parsed)
	}
	normalized, err := json.Marshal(object)
	if err != nil {
		return "", err
	}
	return string(normalized), nil
}

func normalizeMessagesToolCallArguments(messages []Message) ([]Message, error) {
	for i := range messages {
		if messages[i].Role != "assistant" {
			continue
		}
		for j := range messages[i].ToolCalls {
			normalized, err := normalizeToolCallArguments(messages[i].ToolCalls[j].Function.Arguments)
			if err != nil {
				return nil, fmt.Errorf("messages[%d].tool_calls[%d].function.arguments invalid JSON object: %w", i, j, err)
			}
			messages[i].ToolCalls[j].Function.Arguments = normalized
		}
	}
	return messages, nil
}

// prepareReasoningMessages 保留所有 reasoning_content 原值不删除。
// 回传与否由 convertMessagesForUpstream 自动判断：消息带 reasoning_content 就回传，
// 不带则忽略（普通模型上游不产生该字段，天然不会回传），无需手动开关。
func prepareReasoningMessages(messages []Message) []Message {
	// 不再物理删除 ReasoningContent，仅保留原值供 convertMessagesForUpstream 判断
	return messages
}

func firstToolCallID(toolCalls []ToolCall) string {
	if len(toolCalls) == 0 {
		return ""
	}
	return toolCalls[0].ID
}

func convertMessagesForUpstream(messages []Message) []map[string]any {
	converted := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		clean := map[string]any{}
		if msg.Role != "" {
			clean["role"] = msg.Role
		}
		content := normalizeContent(msg.Content)
		reasoningContent := msg.ReasoningContent
		// 上游（Console provider）serde 要求 content 为 block 数组，字符串 content
		// 会报 "invalid type: string, expected struct ChatCompletionRequestContentBlock"。
		// tool 消息按 OpenAI 规范保持 string，其余角色统一包成 text block。
		if content != nil {
			if s, ok := content.(string); ok && msg.Role != "tool" {
				content = []any{map[string]any{"type": "text", "text": s}}
			} else if arr, ok := content.([]any); ok && msg.Role != "tool" {
				// content 是数组时，若内含裸字符串元素（如 user 消息的
				// [字符串, 图片] 混合结构被 stripImages 处理后残留），
				// 也必须包成 text block，否则上游 serde 报
				// "invalid type: string, expected ...ContentBlock"。
				hasBareString := false
				for _, p := range arr {
					if _, ok := p.(string); ok {
						hasBareString = true
						break
					}
				}
				if hasBareString {
					normalized := make([]any, 0, len(arr))
					for _, p := range arr {
						if s, ok := p.(string); ok {
							normalized = append(normalized, map[string]any{"type": "text", "text": s})
						} else {
							normalized = append(normalized, p)
						}
					}
					content = normalized
				}
			}
			clean["content"] = content
		}
		// assistant 消息无条件带 reasoning_content 字段（有值回传原值，无值补空串占位）。
		// 对标 llm2api：thinking 模式上游（DeepSeek）要求所有历史 assistant 消息
		// 必须存在该字段（空串可以，键缺失不行），缺失会报
		// "The reasoning_content in the thinking mode must be passed back"。
		// 无条件补空从根上杜绝 400，无需任何模型名/字段判定。
		if msg.Role == "assistant" {
			if reasoningContent != nil && *reasoningContent != "" {
				clean["reasoning_content"] = *reasoningContent
			} else {
				clean["reasoning_content"] = ""
			}
		}
		if len(msg.ToolCalls) > 0 {
			clean["tool_calls"] = msg.ToolCalls
		}
		if msg.ToolCallID != "" {
			clean["tool_call_id"] = msg.ToolCallID
		}
		if msg.Name != "" {
			clean["name"] = msg.Name
		}
		converted = append(converted, clean)
	}
	return converted
}

// ======================== 完整请求转换 ========================

func convertRequest(req *OpenAIRequest) map[string]any {
	converted := map[string]any{
		"model":    req.Model,
		"messages": convertMessagesForUpstream(req.Messages),
		"stream":   req.Stream,
	}
	if req.Temperature != nil {
		converted["temperature"] = *req.Temperature
	}
	if req.MaxTokens != 0 {
		converted["max_tokens"] = req.MaxTokens
	}
	if req.TopP != nil {
		converted["top_p"] = *req.TopP
	}
	if len(req.Tools) > 0 {
		converted["tools"] = req.Tools
	}
	if req.ToolChoice != nil {
		converted["tool_choice"] = req.ToolChoice
	}
	if mode, ok := thinkingMode(req.Thinking); ok {
		converted["thinking"] = map[string]string{"type": mode}
	} else if req.ExtraBody != nil {
		if mode, ok := thinkingMode(req.ExtraBody["thinking"]); ok {
			converted["thinking"] = map[string]string{"type": mode}
		}
	}
	effort := req.ReasoningEffort
	if effort == "" && req.ExtraBody != nil {
		effort, _ = req.ExtraBody["reasoning_effort"].(string)
	}
	if effort != "" {
		if mapped, ok := getReasoningEffortMap()[effort]; ok {
			effort = mapped
		}
		converted["reasoning_effort"] = effort
	}
	// 透传 thinking_level（DeepSeek 扩展字段，控制思考强度）
	if req.ThinkingLevel != "" {
		converted["thinking_level"] = req.ThinkingLevel
	}
	// 合并 ExtraBody
	if req.ExtraBody != nil {
		for k, v := range req.ExtraBody {
			if k == "thinking" || k == "reasoning_effort" {
				continue
			}
			if _, exists := converted[k]; !exists {
				converted[k] = v
			}
		}
	}
	return converted
}

// buildUpstreamBody 把请求转成上游需要的 JSON 对象（只产出 map，不做序列化）。
// 序列化由 callOpenCodeAPI / callOpenCodeAPIStream 在补完 model/tools/stream 之后统一做一次：
// 旧实现是「这里 Marshal → 调用方 Unmarshal 回 map → 重试循环里每轮再 Marshal」，
// 同一份 body 要走三遍 JSON（实测 200 条历史时每请求多花约 1.3ms、0.7MB 分配）。
func buildUpstreamBody(req *OpenAIRequest) map[string]any {
	return convertRequest(req)
}

// 免费层（2026-10-14 起 /inference 车道）对「官方认识的工具名」校验真实定义：
// description 子串匹配、parameters 逐字节相等，占位桩/截断定义本身就是 403
// FreeTierError 信号。 definitions 抄自官方 OpenCode 客户端（经
// jasonxu114514/opencode2api#83 抓包固化，逐字节对齐），文件即唯一事实源。
//
//go:embed opencode_tools.json
var genuineToolsJSON []byte

var (
	genuineToolsOnce sync.Once
	genuineTools     map[string]ToolFunction
)

// genuineToolDef 返回官方客户端该名字的真实定义；名字不认识时 ok=false（调用方不得动它）。
func genuineToolDef(name string) (ToolFunction, bool) {
	genuineToolsOnce.Do(func() {
		m := map[string]ToolFunction{}
		if err := json.Unmarshal(genuineToolsJSON, &m); err != nil {
			// 嵌入文件损坏是构建期问题；运行期退化为「不认识任何名字」，
			// 等价于关闭替换与补齐，行为和加门禁前的老版本一致。
			m = map[string]ToolFunction{}
		}
		genuineTools = m
	})
	def, ok := genuineTools[name]
	return def, ok
}

// freeTierCoreToolNames 免费层要求在场的代理型核心工具（OpenCode 2.x 命名）。
var freeTierCoreToolNames = []string{"shell", "edit", "glob", "grep", "read"}

// genuineChatTool 组装一个 chat 协议形态（type:function）的真实工具定义。
func genuineChatTool(name string) map[string]any {
	def, _ := genuineToolDef(name)
	return map[string]any{"type": "function", "function": map[string]any{
		"name":        name,
		"description": def.Description,
		"parameters":  def.Parameters,
	}}
}

// toolNameOf 从工具的任意形态里取出名字：可能是 Tool 结构体（convertRequest 直接构造的形态），
// 也可能是 map[string]any（旧实现经过 JSON 往返之后的形态）。
func toolNameOf(raw any) string {
	switch t := raw.(type) {
	case Tool:
		return t.Function.Name
	case *Tool:
		return t.Function.Name
	case map[string]any:
		if fn, ok := t["function"].(map[string]any); ok {
			name, _ := fn["name"].(string)
			return name
		}
	}
	return ""
}

// ensureFreeTierTools 检查上游请求体里的 tools，把 bash/glob/grep/read 四件中缺失的补上。
// 只在 tools 层面做存在性修补，不改动客户端已有的工具定义；原本就没有 tools 字段的
// 请求会获得完整的四件（上游免费层强制要求，否则 403 FreeTierError）。
//
// 关键：tools 有两种形态 —— convertRequest 直接构造时是 []Tool，经过 JSON 往返时是 []any。
// 只认一种会**静默失败**：曾经只断言 []any，[]Tool 进来时 existing 为空、判定四件全缺，
// 于是把客户端自带的全部工具替换掉、只留四个桩 —— 表现为模型抱怨"看不到 get_weather 这类工具"。
// ensureFreeTierTools 把请求体 tools 修成官方 2.x 客户端形态（2026-10-14 新门禁）：
//   - 官方认识的名字 → 定义整体替换为真实定义（桩、截断描述、篡改 schema 都会 403）；
//   - 缺核心五件（shell/edit/glob/grep/read）→ 用真实定义补齐；
//   - 官方不认识的名字原样保留，客户端自有 function calling 不受影响。
//
// 关键：tools 有两种形态 —— convertRequest 直接构造时是 []Tool，经过 JSON 往返时是 []any。
// 只认一种会**静默失败**：曾经只断言 []any，[]Tool 进来时 existing 为空、判定全缺，
// 于是把客户端自带的全部工具替换掉、只留四个桩 —— 表现为模型抱怨"看不到 get_weather 这类工具"。
func ensureFreeTierTools(bodyMap map[string]any) {
	existing := make(map[string]bool, 8)
	switch tools := bodyMap["tools"].(type) {
	case []Tool:
		for i := range tools {
			name := tools[i].Function.Name
			if name == "" {
				continue
			}
			existing[name] = true
			if def, ok := genuineToolDef(name); ok {
				tools[i].Function.Description = def.Description
				tools[i].Function.Parameters = def.Parameters
			}
		}
	case []any:
		for _, raw := range tools {
			name := toolNameOf(raw)
			if name == "" {
				continue
			}
			existing[name] = true
			replaceWithGenuineDef(raw, name)
		}
	case []map[string]any:
		for _, t := range tools {
			name := toolNameOf(t)
			if name == "" {
				continue
			}
			existing[name] = true
			replaceWithGenuineDef(t, name)
		}
	}
	missing := make([]any, 0, len(freeTierCoreToolNames))
	for _, name := range freeTierCoreToolNames {
		if !existing[name] {
			missing = append(missing, genuineChatTool(name))
		}
	}
	if len(missing) == 0 {
		return
	}
	// 保留客户端原有 tools（两种形态都不能丢），把缺的追加在后面
	switch tools := bodyMap["tools"].(type) {
	case []Tool:
		merged := make([]any, 0, len(tools)+len(missing))
		for i := range tools {
			merged = append(merged, tools[i])
		}
		bodyMap["tools"] = append(merged, missing...)
	case []any:
		bodyMap["tools"] = append(tools, missing...)
	default:
		bodyMap["tools"] = append([]any(nil), missing...)
	}
}

// replaceWithGenuineDef 把一个 map 形态工具条目的定义替换为官方真实定义。
// 非 "function" 子键形态（不在 chat 协议里出现过的意外形状）整条替换。
func replaceWithGenuineDef(raw any, name string) {
	def, ok := genuineToolDef(name)
	if !ok {
		return
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return
	}
	fn, ok := m["function"].(map[string]any)
	if !ok {
		fresh := genuineChatTool(name)
		for k := range m {
			delete(m, k)
		}
		for k, v := range fresh {
			m[k] = v
		}
		return
	}
	fn["description"] = def.Description
	fn["parameters"] = def.Parameters
}

// ======================== Anthropic 格式兼容 ========================

func isAnthropicFormat(body []byte) bool {
	var obj map[string]any
	if json.Unmarshal(body, &obj) == nil {
		if typ, _ := obj["type"].(string); typ == "message" {
			return true
		}
	}
	lines := bytes.Split(body, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		typ, _ := event["type"].(string)
		switch typ {
		case "message_start", "content_block_start", "content_block_delta",
			"content_block_stop", "message_delta", "message_stop", "ping":
			return true
		}
		return false
	}
	return false
}

func parseAnthropicSSE(body []byte) (map[string]any, string, []map[string]any) {
	lines := bytes.Split(body, []byte("\n"))
	var anthropicMsg map[string]any
	var textBuilder, currentToolInputBuilder strings.Builder
	var currentToolUse map[string]any
	var toolUseBlocks []map[string]any
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		typ, _ := event["type"].(string)
		switch typ {
		case "message_start":
			if m, ok := event["message"].(map[string]any); ok {
				anthropicMsg = m
			}
		case "content_block_start":
			if cb, ok := event["content_block"].(map[string]any); ok {
				if cbType, _ := cb["type"].(string); cbType == "tool_use" {
					currentToolUse = cb
					currentToolInputBuilder.Reset()
				}
			}
		case "content_block_delta":
			if delta, ok := event["delta"].(map[string]any); ok {
				if t, ok := delta["text"].(string); ok {
					textBuilder.WriteString(t)
				}
				if dt, _ := delta["type"].(string); dt == "input_json_delta" {
					if partial, ok := delta["partial_json"].(string); ok {
						currentToolInputBuilder.WriteString(partial)
					}
				}
			}
		case "content_block_stop":
			if currentToolUse != nil {
				inputStr := currentToolInputBuilder.String()
				var input any = inputStr
				var parsed any
				if json.Unmarshal([]byte(inputStr), &parsed) == nil {
					input = parsed
				}
				currentToolUse["input"] = input
				toolUseBlocks = append(toolUseBlocks, currentToolUse)
				currentToolUse = nil
			}
		case "message_delta":
			if delta, ok := event["delta"].(map[string]any); ok {
				if anthropicMsg == nil {
					anthropicMsg = map[string]any{}
				}
				if stop, ok := delta["stop_reason"].(string); ok {
					anthropicMsg["stop_reason"] = stop
				}
				if usage, ok := delta["usage"].(map[string]any); ok {
					anthropicMsg["usage"] = usage
				}
			}
		case "message_stop":
		case "error":
			return nil, "", nil
		}
	}
	return anthropicMsg, textBuilder.String(), toolUseBlocks
}

func buildOpenAIResponse(anthropicMsg map[string]any, text string, toolUseBlocks []map[string]any, modelID string) []byte {
	if anthropicMsg == nil {
		return nil
	}
	now := time.Now().Unix()
	role, _ := anthropicMsg["role"].(string)
	if role == "" {
		role = "assistant"
	}
	finishReason, _ := anthropicMsg["stop_reason"].(string)
	if finishReason == "tool_use" {
		finishReason = "tool_calls"
	}
	choice := map[string]any{
		"index":         0,
		"message":       map[string]any{"role": role, "content": text},
		"finish_reason": finishReason,
	}
	if len(toolUseBlocks) > 0 {
		var toolCalls []map[string]any
		for _, tb := range toolUseBlocks {
			toolInput := tb["input"]
			argsJSON, _ := json.Marshal(toolInput)
			toolCalls = append(toolCalls, map[string]any{
				"id":   tb["id"],
				"type": "function",
				"function": map[string]any{
					"name":      tb["name"],
					"arguments": string(argsJSON),
				},
			})
		}
		choice["message"].(map[string]any)["tool_calls"] = toolCalls
		if text == "" {
			choice["message"].(map[string]any)["content"] = nil
		}
	}
	resp := map[string]any{
		"id":      anthropicMsg["id"],
		"object":  "chat.completion",
		"created": now,
		"model":   modelID,
		"choices": []map[string]any{choice},
	}
	if usage, ok := anthropicMsg["usage"]; ok {
		resp["usage"] = usage
	}
	result, _ := json.Marshal(resp)
	return result
}

func convertAnthropicMessageToOpenAI(msg map[string]any, modelID string) []byte {
	if msg["model"] == nil {
		msg["model"] = modelID
	}
	var textBuilder strings.Builder
	var toolUses []map[string]any
	if content, ok := msg["content"].([]any); ok {
		for _, c := range content {
			if block, ok := c.(map[string]any); ok {
				switch block["type"] {
				case "text":
					if t, ok := block["text"].(string); ok {
						textBuilder.WriteString(t)
					}
				case "tool_use":
					toolUses = append(toolUses, block)
				}
			}
		}
	}
	return buildOpenAIResponse(msg, textBuilder.String(), toolUses, modelID)
}

func convertAnthropicToOpenAI(body []byte, modelID string) []byte {
	var singleMsg map[string]any
	if json.Unmarshal(body, &singleMsg) == nil {
		if typ, _ := singleMsg["type"].(string); typ == "message" {
			return convertAnthropicMessageToOpenAI(singleMsg, modelID)
		}
	}
	msg, text, toolUses := parseAnthropicSSE(body)
	if msg == nil {
		return body
	}
	if msg["model"] == nil {
		msg["model"] = modelID
	}
	return buildOpenAIResponse(msg, text, toolUses, modelID)
}

// ======================== 响应清理 ========================

func cleanNulls(m map[string]any) {
	for k, v := range m {
		if v == nil {
			delete(m, k)
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			delete(m, k)
		}
	}
}

func normalizeReasoningContent(message map[string]any) {
	if message == nil {
		return
	}
	if existing, ok := message["reasoning_content"].(string); ok && existing != "" {
		delete(message, "reasoning")
		return
	}
	if reasoning, ok := message["reasoning"].(string); ok && reasoning != "" {
		message["reasoning_content"] = reasoning
		delete(message, "reasoning")
	}
}

func cleanStreamDelta(delta map[string]any) {
	normalizeReasoningContent(delta)
	if v, ok := delta["content"]; ok && v == nil {
		delete(delta, "content")
	}
	if s, ok := delta["content"].(string); ok && s == "" {
		delete(delta, "content")
	}
	if v, ok := delta["reasoning_content"]; ok && v == nil {
		delete(delta, "reasoning_content")
	}
	if s, ok := delta["reasoning_content"].(string); ok && s == "" {
		delete(delta, "reasoning_content")
	}
	if s, ok := delta["role"].(string); ok && s == "" {
		delete(delta, "role")
	}
}

// convertStreamChunkWithUsage 转换流式 chunk 并同时提取 usage，避免二次解析
// extractUpstreamErrorMessage 从上游 error 对象中提取人类可读的 message。
// 上游常见的错误结构为 {"type": "server_error", "message": "...", "param": null}，
// 直接 fmt.Sprint 整个 map 会输出 map[message:... param:<nil> type:server_error] 乱码，
// 这里优先取 message 字段，取不到再退回原样字符串化。
func extractUpstreamErrorMessage(v any) string {
	if m, ok := v.(map[string]any); ok {
		if s, ok := m["message"].(string); ok && s != "" {
			return s
		}
		if inner, ok := m["error"].(map[string]any); ok {
			if s, ok := inner["message"].(string); ok && s != "" {
				return s
			}
		}
	}
	return fmt.Sprint(v)
}

func convertStreamChunkWithUsage(line string, modelID string) (string, map[string]any, any) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "data: [DONE]" || trimmed == "[DONE]" {
		return line, nil, nil
	}
	if !strings.HasPrefix(line, "data: ") {
		return line, nil, nil
	}
	data := line[6:]
	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return line, nil, nil
	}
	// 上游 error 事件直接从这次解析里带出去，调用方不必为了查 error 再 Unmarshal 一遍
	// （旧实现每个 chunk 要解析两次 JSON，这里省掉一次）
	if upstreamError, ok := raw["error"]; ok {
		return "", nil, upstreamError
	}
	// 重写 model 为请求别名，避免暴露上游模型名
	raw["model"] = modelID

	// 提取 usage
	var usage map[string]any
	if u, ok := raw["usage"].(map[string]any); ok {
		usage = u
	}

	choices, ok := raw["choices"].([]any)
	if !ok || len(choices) == 0 {
		if usage != nil {
			// OpenAI 流式标准：请求带 include_usage 时，上游在内容结束、
			// [DONE] 之前下发一个 choices 为空的 usage 收尾 chunk，
			// 必须原样透传给客户端（此前实现把该 chunk 丢弃导致下游收不到统计）。
			delete(raw, "cost")
			converted, err := json.Marshal(raw)
			if err != nil {
				return "", usage, nil
			}
			return "data: " + string(converted), usage, nil
		}
		return "", usage, nil
	}
	for i, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if delta, ok := choice["delta"].(map[string]any); ok {
			cleanStreamDelta(delta)
			choice["delta"] = delta
		}
		if msg, ok := choice["message"].(map[string]any); ok {
			normalizeReasoningContent(msg)
			cleanNulls(msg)
			choice["message"] = msg
		}
		if v, ok := choice["logprobs"]; ok && v == nil {
			delete(choice, "logprobs")
		}
		if v, ok := choice["finish_reason"]; ok && v == nil {
			delete(choice, "finish_reason")
		}
		if s, ok := choice["finish_reason"].(string); ok && s == "" {
			delete(choice, "finish_reason")
		}
		choices[i] = choice
	}
	raw["choices"] = choices
	if v, ok := raw["usage"]; ok && v == nil {
		delete(raw, "usage")
	}
	delete(raw, "cost")
	converted, err := json.Marshal(raw)
	if err != nil {
		return line, usage, nil
	}
	return "data: " + string(converted), usage, nil
}

func convertResponse(data []byte, modelID string) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Printf("Warning: convertResponse unmarshal failed: %v", err)
		return data, nil
	}
	// 重写 model 为请求别名，避免暴露上游模型名
	raw["model"] = modelID
	if choices, ok := raw["choices"].([]any); ok {
		for i, c := range choices {
			if choice, ok := c.(map[string]any); ok {
				if msg, ok := choice["message"].(map[string]any); ok {
					normalizeReasoningContent(msg)
					cleanNulls(msg)
					choice["message"] = msg
				}
				if v, ok := choice["logprobs"]; ok && v == nil {
					delete(choice, "logprobs")
				}
				choices[i] = choice
			}
		}
		raw["choices"] = choices
	}
	if usage, ok := raw["usage"].(map[string]any); ok {
		cleanU := map[string]any{
			"prompt_tokens":     usage["prompt_tokens"],
			"completion_tokens": usage["completion_tokens"],
			"total_tokens":      usage["total_tokens"],
		}
		raw["usage"] = cleanU
	}
	delete(raw, "cost")
	delete(raw, "system_fingerprint")
	return json.Marshal(raw)
}

// buildOCRequest 用已经序列化好的请求体构造上游请求。
// 每次重试都会重新 NewRequest（bytes.Reader 游标独立），但不再重新 Marshal 整个 body。
// 第二个返回值是本次用掉的凭据编号 key_id，只进诊断日志，绝不含凭据明文。
//
// 头组逐字符镜像官方 2.x 客户端（jasonxu114514/opencode2api#83 今日实测消融）：
// 缺 x-opencode-session-id、UA 换形、project 带 prj_ 前缀、多带 x-opencode-request，
// 任何一项单独出现都 403 FreeTierError；按手指纹一致时过门与凭据有无无关。
func buildOCRequest(ctx context.Context, bodyBytes []byte) (*http.Request, string, error) {
	// key_pool 命中就用池内凭据；未启用或池空一律回落到 Bearer public，
	// 保证没配 key_pool 的部署行为与引入本字段之前逐字节一致。
	auth, keyID := "public", ""
	if k, ok := pickOCKey(ocCurrentSessionID()); ok {
		auth, keyID = k.val, k.id
	}
	// 免费层（keyless）车道 2026-10-14 整体迁到 /inference；keyed 车道仍在 /zen。
	base := ocZenBase
	if auth == "public" {
		base = ocInferenceBase
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/v1/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, "", err
	}
	sid := ocCurrentSessionID()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+auth)
	req.Header.Set("User-Agent", ocClientUA())
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-session", sid)
	req.Header.Set("x-opencode-session-id", sid)
	req.Header.Set("x-session-affinity", sid)
	req.Header.Set("X-Session-Id", sid)
	// ocProjectID 由 randomHex(40) 生成，裸 40 位小写 hex 即官方期望形状
	req.Header.Set("x-opencode-project", ocProjectID)
	return req, keyID, nil
}

// aggregateOpenAIStream 把上游强制 stream:true 返回的 OpenAI SSE 流聚合为
// 一个完整的 chat.completion JSON。上游 2026-09-18 新校验要求免费层请求必须
// stream:true（stream:false 直接 403 FreeTierError）；对客户端声明的非流式请求，
// 代理在上游侧改走流式并在本地还原为等价的非流式 JSON，客户端感知不变。
// 主体不是 OpenAI SSE（如上游直发 JSON 或流中带 error 事件）时原样返回。
func aggregateOpenAIStream(body []byte, modelID string) []byte {
	var id string
	var created int64
	var contentBuilder, reasoningBuilder strings.Builder
	finishReason := ""
	role := "assistant"
	var usage map[string]any
	type toolAcc struct {
		id, name string
		args     strings.Builder
	}
	tools := map[int]*toolAcc{}
	toolOrder := []int{}
	sawChunk := false

	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		data := bytes.TrimSpace(line[6:])
		if string(data) == "[DONE]" {
			break
		}
		var chunk map[string]any
		if err := json.Unmarshal(data, &chunk); err != nil {
			continue
		}
		sawChunk = true
		if _, ok := chunk["error"]; ok {
			// 流内错误事件：交给上层原有逻辑处理，不做聚合
			return body
		}
		if v, ok := chunk["id"].(string); ok && v != "" && id == "" {
			id = v
		}
		if v, ok := chunk["created"].(float64); ok && created == 0 {
			created = int64(v)
		}
		if u, ok := chunk["usage"].(map[string]any); ok && u != nil {
			usage = u
		}
		choices, ok := chunk["choices"].([]any)
		if !ok || len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if choice == nil {
			continue
		}
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			finishReason = fr
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if r, ok := delta["role"].(string); ok && r != "" {
			role = r
		}
		if c, ok := delta["content"].(string); ok && c != "" {
			contentBuilder.WriteString(c)
		}
		if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
			reasoningBuilder.WriteString(rc)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, rtc := range tcs {
				tc, ok := rtc.(map[string]any)
				if !ok {
					continue
				}
				idxF, _ := tc["index"].(float64)
				idx := int(idxF)
				acc := tools[idx]
				if acc == nil {
					acc = &toolAcc{}
					tools[idx] = acc
					toolOrder = append(toolOrder, idx)
				}
				if v, ok := tc["id"].(string); ok && v != "" {
					acc.id = v
				}
				if fn, ok := tc["function"].(map[string]any); ok {
					if n, ok := fn["name"].(string); ok && n != "" {
						acc.name = n
					}
					if a, ok := fn["arguments"].(string); ok && a != "" {
						acc.args.WriteString(a)
					}
				}
			}
		}
	}
	if !sawChunk {
		return body
	}
	if id == "" {
		id = "chatcmpl_" + randomString(24)
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	if finishReason == "" {
		finishReason = "stop"
	}

	message := map[string]any{"role": role, "content": contentBuilder.String()}
	if reasoningBuilder.Len() > 0 {
		message["reasoning_content"] = reasoningBuilder.String()
	}
	if len(toolOrder) > 0 {
		toolCalls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			acc := tools[idx]
			toolCalls = append(toolCalls, map[string]any{
				"id":   acc.id,
				"type": "function",
				"function": map[string]any{
					"name":      acc.name,
					"arguments": acc.args.String(),
				},
			})
		}
		message["tool_calls"] = toolCalls
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   modelID,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return body
	}
	return out
}

func callOpenCodeAPI(ctx context.Context, bodyMap map[string]any, modelID string) ([]byte, int, http.Header, error) {
	initOCSession()

	// 只使用调用方指定的模型；失败仅按代理/限流重试，不切换到其他模型
	bodyMap["model"] = modelID
	ensureFreeTierTools(bodyMap)
	// tool_choice:"none" 会被免费层门禁直接拒（只剩省略/auto 两种合法姿势）
	if tc, _ := bodyMap["tool_choice"].(string); tc == "none" {
		delete(bodyMap, "tool_choice")
	}
	// 上游 2026-09-18 新校验：免费层必须 stream:true（stream:false 直接 403 FreeTierError）。
	// 非流式请求也以上游流式发出，成功后由 aggregateOpenAIStream 本地还原为完整 JSON，
	// 客户端拿到的仍是非流式 chat.completion，感知不变。
	bodyMap["stream"] = true
	bodyMap["stream_options"] = map[string]any{"include_usage": true}

	// 只序列化一次，重试时复用同一份字节（循环体内不再改动 body）
	bodyBytes, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, http.StatusInternalServerError, nil, fmt.Errorf("marshal upstream body: %w", err)
	}

	var lastErr error
	var lastBody []byte
	var lastStatus int
	var lastHeader http.Header
	attempts := socks5RateLimitAttemptCount()
	// 池内还有别的凭据时，429 也要允许多试几轮 —— 单凭据的额度用完是常态，
	// 换凭据才是 key_pool 存在的意义。
	if extra := keyPoolExtraAttempts(); extra > 0 && attempts < extra+1 {
		attempts = extra + 1
	}
	// 该模型额度已被打光（或整段不可用）时连上游都不打：轮转与换出口对这两类都无效，
	// 每发白烧凭据与出口，首字节延迟从 ~1s 拉到 8–22s（CMP-48 §3 判据 3 实测）。
	if st, b, ra, blocked := quotaBreakerBlocked(modelID); blocked {
		log.Printf("%s[quota breaker open] model=%s status=%d 跳过上游直接回 %d", labelPrefix(ctx), modelID, st, st)
		return b, st, quotaExhaustedHeaders(ra), fmt.Errorf("upstream breaker open")
	}
	// 这个循环只服务两类重试：429 换出口/换凭据重试、Tor 电路故障 / 网络层错误重试。
	// 5xx 已不再重试 —— 直接走下面的透传分支，把上游状态码与错误体原样交给客户端。
	// triedExits 记录本轮真实打过的出口标签，供 [free tier rejected] 行判「整类出口被拒」。
	var triedExits []string
	for attempt := 0; ; {
		up, keyID, err := buildOCRequest(ctx, bodyBytes)
		if err != nil {
			lastErr = err
			break
		}
		client, selection, err := getHTTPClient(ctx)
		if err != nil {
			return nil, http.StatusBadGateway, nil, err
		}
		triedExits = append(triedExits, selection.Label)
		attempt++
		resp, err := client.Do(up)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				// 客户端断开/取消：立即退出，避免急装重试占满 CPU
				log.Printf("%s[client gone] model=%s 客户端断开，在途上游请求被取消: %v", labelPrefix(ctx), modelID, ctx.Err())
				break
			}
			// CMP-119：网络层错误原来是无条件 continue —— 不查 attempts、不退避、
			// 不记日志。上游一挂就变成满 CPU 的死循环，且日志里一个字都没有。
			if attempt < attempts {
				log.Printf("%s[upstream dial failed] model=%s attempt=%d/%d key_id=%s exit=%s: %v",
					labelPrefix(ctx), modelID, attempt, attempts, keyID, selection.Label, err)
				continue
			}
			break
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			b, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				return nil, 0, nil, readErr
			}
			if isAnthropicFormat(b) {
				b = convertAnthropicToOpenAI(b, modelID)
			} else {
				// 上游被强制 stream:true，返回的是 OpenAI SSE 流；
				// 聚合为完整 chat.completion JSON 还原客户端要的非流式响应。
				b = aggregateOpenAIStream(b, modelID)
			}
			if debugMode && keyID != "" {
				log.Printf("%s[upstream ok] model=%s key_id=%s exit=%s status=%d", labelPrefix(ctx), modelID, keyID, selection.Label, resp.StatusCode)
			}
			noteUpstreamOK(modelID)
			return b, resp.StatusCode, resp.Header, nil
		}
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		// 三类上游信号各走各的路：配额类（429 + FreeUsageLimitError）单发只打一次并进熔断；
		// 「端点/模型级不可用」（429 或 403 + Endpoint is unavailable）同样不轮转、进熔断；
		// 403 FreeTierError 是凭据级门（CMP-109），换 key 能自救，继续轮转。
		freeTier403 := isFreeTier403(resp.StatusCode, errBody)
		if freeTier403 || isRetryableQuotaUpstream(resp.StatusCode, errBody) {
			retryAfter := resp.Header.Get("Retry-After")
			quota := resp.StatusCode == http.StatusTooManyRequests && isQuotaClass429(resp.Header, errBody)
			// 「端点/模型级不可用」不是限流，轮转纯属白烧：现网同一时刻 k1→k16 逐字同一句
			// （CMP-129），同一把 key 在 10 个不同 OC 节点上也零救回（单变量对照实测）。
			// 直接记一次按模型熔断并回 503 —— 熔断期满了自动放行去探活，上游一恢复即自愈。
			if !quota && isEndpointUnavailableUpstream(resp.StatusCode, errBody) {
				noteEndpointUnavailable(modelID)
				log.Printf("%s[endpoint unavailable] model=%s key_id=%s exit=%s status=%d 不换 key/出口，记熔断并回 503 body=%s", labelPrefix(ctx), modelID, keyID, selection.Label, resp.StatusCode, string(errBody))
				return endpointUnavailableBody(), http.StatusServiceUnavailable, quotaExhaustedHeaders(""), fmt.Errorf("upstream endpoint unavailable")
			}
			nextKey := ""
			if !quota {
				nextKey = advanceOCKey(ocCurrentSessionID())
			}
			if resp.StatusCode == http.StatusTooManyRequests {
				// key_id 打进这行是刻意的：运营要能只凭日志确认轮转真的在工作
				// （老容器对应的是 msg=upstream_attempt ... key_id=k82）。
				log.Printf("%s[upstream rate limited] model=%s key_id=%s exit=%s status=%d retry_after=%q quota=%t next_key_id=%s body=%s", labelPrefix(ctx), modelID, keyID, selection.Label, resp.StatusCode, retryAfter, quota, nextKey, string(errBody))
			} else {
				log.Printf("%s[upstream retryable] model=%s key_id=%s exit=%s status=%d next_key_id=%s body=%s", labelPrefix(ctx), modelID, keyID, selection.Label, resp.StatusCode, nextKey, string(errBody))
			}
			lastBody = errBody
			lastStatus = resp.StatusCode
			lastHeader = resp.Header.Clone()
			lastErr = fmt.Errorf("upstream retryable error")
			if quota {
				noteQuotaExhausted(modelID, retryAfter)
				log.Printf("%s[quota exhausted] model=%s attempt=%d retry_after=%q 不换 key/出口，透传上游 429", labelPrefix(ctx), modelID, attempt, retryAfter)
				return lastBody, lastStatus, lastHeader, lastErr
			}
			// 403 FreeTierError：换 key 之外还要换出口 —— 整类出口被拒时换 key 结构上无效。
			if freeTier403 {
				rotateSocks5OnFreeTier403(ctx, selection.Label)
			} else {
				rotateSocks5OnRateLimit(ctx)
			}
			if attempt < attempts {
				continue
			}
		}
		logFreeTierRejection(ctx, modelID, keyID, resp.StatusCode, errBody, triedExits)
		// 没进任何重试分支的上游非 2xx 一律无条件记一行：此前只有 -debug 才可见，
		// 而生产默认不开 -debug ⇒ 这些错误（400 `Model is unavailable`、各类 5xx、
		// 上游换措辞后的新 4xx）在日志里完全不可观测。
		log.Printf("%s[upstream error] model=%s status=%d body=%s", labelPrefix(ctx), modelID, resp.StatusCode, string(errBody))
		// 5xx（500/502/503/504…）不再重试：直接走下面的透传分支，把上游状态码与错误体原样交给客户端，
		// 由客户端自己决定要不要重发。（这里原先做的是无限指数退避重试，会把请求永久挂住。）
		lastBody = errBody
		lastStatus = resp.StatusCode
		lastHeader = resp.Header.Clone()
		lastErr = fmt.Errorf("upstream error")
		return lastBody, lastStatus, lastHeader, lastErr
	}
	return lastBody, lastStatus, lastHeader, lastErr
}

func callOpenCodeAPIStream(ctx context.Context, bodyMap map[string]any, modelID string) (io.ReadCloser, int, http.Header, error) {
	initOCSession()

	// 只使用调用方指定的模型；失败仅按代理/限流重试，不切换到其他模型
	bodyMap["model"] = modelID
	ensureFreeTierTools(bodyMap)
	// tool_choice:"none" 会被免费层门禁直接拒（只剩省略/auto 两种合法姿势）
	if tc, _ := bodyMap["tool_choice"].(string); tc == "none" {
		delete(bodyMap, "tool_choice")
	}

	// 只序列化一次，重试时复用同一份字节（循环体内不再改动 body）
	bodyBytes, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, http.StatusInternalServerError, nil, fmt.Errorf("marshal upstream body: %w", err)
	}

	attempts := socks5RateLimitAttemptCount()
	if extra := keyPoolExtraAttempts(); extra > 0 && attempts < extra+1 {
		attempts = extra + 1
	}
	// 同 callOpenCodeAPI：该模型额度打光（或整段不可用）后连上游都不打。
	if st, b, ra, blocked := quotaBreakerBlocked(modelID); blocked {
		log.Printf("%s[quota breaker open] model=%s status=%d 跳过上游直接回 %d", labelPrefix(ctx), modelID, st, st)
		return io.NopCloser(bytes.NewReader(b)), st, quotaExhaustedHeaders(ra), nil
	}
	// 这个循环只服务两类重试：429 换出口/换凭据重试、Tor 电路故障 / 网络层错误重试。
	// 5xx 已不再重试 —— 直接走下面的透传分支，把上游状态码与错误体原样交给客户端。
	// triedExits 与 callOpenCodeAPI 同源，供 [free tier rejected] 行判「整类出口被拒」。
	var triedExits []string
	for attempt := 0; ; {
		up, keyID, err := buildOCRequest(ctx, bodyBytes)
		if err != nil {
			break
		}
		client, selection, err := getHTTPClient(ctx)
		if err != nil {
			return nil, http.StatusBadGateway, nil, err
		}
		triedExits = append(triedExits, selection.Label)
		attempt++
		resp, err := client.Do(up)
		if err != nil {
			if ctx.Err() != nil {
				// 客户端断开/取消：立即退出，避免急装重试占满 CPU
				log.Printf("%s[client gone] model=%s 客户端断开，在途上游请求被取消: %v", labelPrefix(ctx), modelID, ctx.Err())
				break
			}
			// 同 callOpenCodeAPI：网络层错误必须有上界，否则是满 CPU 死循环。
			if attempt < attempts {
				log.Printf("%s[upstream dial failed] model=%s attempt=%d/%d key_id=%s exit=%s: %v",
					labelPrefix(ctx), modelID, attempt, attempts, keyID, selection.Label, err)
				continue
			}
			break
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if debugMode && keyID != "" {
				log.Printf("%s[upstream ok] model=%s key_id=%s exit=%s status=%d", labelPrefix(ctx), modelID, keyID, selection.Label, resp.StatusCode)
			}
			noteUpstreamOK(modelID)
			return resp.Body, resp.StatusCode, resp.Header, nil
		}
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		// 与 callOpenCodeAPI 同源：配额类与「端点/模型级不可用」都单发只打一次并进熔断，
		// 「403 + FreeTierError」只换凭据（见 isFreeTier403）。
		freeTier403 := isFreeTier403(resp.StatusCode, errBody)
		if freeTier403 || isRetryableQuotaUpstream(resp.StatusCode, errBody) {
			retryAfter := resp.Header.Get("Retry-After")
			quota := resp.StatusCode == http.StatusTooManyRequests && isQuotaClass429(resp.Header, errBody)
			// 与 callOpenCodeAPI 同源：端点/模型级不可用不换 key/出口，记熔断后回 503。
			if !quota && isEndpointUnavailableUpstream(resp.StatusCode, errBody) {
				noteEndpointUnavailable(modelID)
				log.Printf("%s[endpoint unavailable] model=%s key_id=%s exit=%s status=%d 不换 key/出口，记熔断并回 503 body=%s", labelPrefix(ctx), modelID, keyID, selection.Label, resp.StatusCode, string(errBody))
				return io.NopCloser(bytes.NewReader(endpointUnavailableBody())), http.StatusServiceUnavailable, quotaExhaustedHeaders(""), nil
			}
			nextKey := ""
			if !quota {
				nextKey = advanceOCKey(ocCurrentSessionID())
			}
			if resp.StatusCode == http.StatusTooManyRequests {
				log.Printf("%s[upstream rate limited] model=%s key_id=%s exit=%s status=%d retry_after=%q quota=%t next_key_id=%s body=%s", labelPrefix(ctx), modelID, keyID, selection.Label, resp.StatusCode, retryAfter, quota, nextKey, string(errBody))
			} else {
				log.Printf("%s[upstream retryable] model=%s key_id=%s exit=%s status=%d next_key_id=%s body=%s", labelPrefix(ctx), modelID, keyID, selection.Label, resp.StatusCode, nextKey, string(errBody))
			}
			if quota {
				noteQuotaExhausted(modelID, retryAfter)
				log.Printf("%s[quota exhausted] model=%s attempt=%d retry_after=%q 不换 key/出口，透传上游 429", labelPrefix(ctx), modelID, attempt, retryAfter)
				return io.NopCloser(bytes.NewReader(errBody)), resp.StatusCode, resp.Header.Clone(), nil
			}
			// 与 callOpenCodeAPI 同源：403 FreeTierError 也换出口（见 rotateSocks5OnFreeTier403）。
			if freeTier403 {
				rotateSocks5OnFreeTier403(ctx, selection.Label)
			} else {
				rotateSocks5OnRateLimit(ctx)
			}
			if attempt < attempts {
				continue
			}
		}
		logFreeTierRejection(ctx, modelID, keyID, resp.StatusCode, errBody, triedExits)
		// 同 callOpenCodeAPI：未进重试分支的上游非 2xx 无条件记一行，不再受 -debug 约束。
		log.Printf("%s[upstream error] model=%s status=%d body=%s", labelPrefix(ctx), modelID, resp.StatusCode, string(errBody))
		// 5xx（500/502/503/504…）不再重试：直接走下面的透传分支，把上游状态码与错误体原样交给客户端，
		// 由客户端自己决定要不要重发。（这里原先做的是无限指数退避重试，会把请求永久挂住。）
		// 返回错误体供下游透传
		return io.NopCloser(bytes.NewReader(errBody)), resp.StatusCode, resp.Header.Clone(), nil
	}
	return nil, 500, nil, fmt.Errorf("all requests failed")
}

// ======================== 安全响应头过滤 ========================

var safeResponseHeaders = map[string]bool{
	"Content-Type":          true,
	"Retry-After":           true,
	"RateLimit-Limit":       true,
	"RateLimit-Remaining":   true,
	"RateLimit-Reset":       true,
	"X-RateLimit-Limit":     true,
	"X-RateLimit-Remaining": true,
	"X-RateLimit-Reset":     true,
}

func filterResponseHeaders(h http.Header) http.Header {
	filtered := make(http.Header)
	for k, v := range h {
		if safeResponseHeaders[k] {
			filtered[k] = v
		}
	}
	return filtered
}

func copyFilteredResponseHeaders(dst http.Header, src http.Header) {
	for k, values := range filterResponseHeaders(src) {
		dst.Del(k)
		for _, v := range values {
			dst.Add(k, v)
		}
	}
}

func normalizeUpstreamStatus(status int) int {
	if status < 100 || status > 999 {
		return http.StatusBadGateway
	}
	return status
}

func applyUpstreamErrorHeaders(w http.ResponseWriter, upstreamHeaders http.Header, status int) int {
	status = normalizeUpstreamStatus(status)
	copyFilteredResponseHeaders(w.Header(), upstreamHeaders)
	w.Header().Set("X-Upstream-Status", strconv.Itoa(status))
	if status == http.StatusTooManyRequests {
		w.Header().Set("X-Upstream-Rate-Limited", "true")
	}
	return status
}

// ======================== Chat Completions Handler ========================

func chatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	cnt := requestCount.Add(1)
	if debugMode {
		log.Printf("[request #%d] POST /v1/chat/completions\n%s", cnt, string(body))
	}

	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	reqCtx := withRequestLabel(r.Context(), reqLabel(cnt, r))
	log.Printf("[访问] %s %s %s model=%s", reqLabel(cnt, r), r.Method, r.URL.Path, req.Model)
	aliasModel := req.Model
	targetModel, mmModel, ok := resolveModel(req.Model)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"message": "模型 " + req.Model + " 未在别名列表中配置，禁止调用",
			"type":    "invalid_request_error",
		}})
		return
	}
	// 多模态路由：只按"本轮新增图片"判定，历史图片不锁定多模态路由。
	// （与 /v1/responses、/v1/messages 路径一致）
	useModel := targetModel
	if mmModel != "" && hasNewImageContent(req.Messages) {
		useModel = mmModel
	}
	req.Model = useModel

	// 配置了多模态模型但本轮未路由过去（纯文本轮次）时，剥离所有图片
	// （含历史图片），避免纯文本上游报错。
	// 未配置 multimodal_model 时视为目标模型自带能力，图片 dumb pipe 透传。
	if mmModel != "" && useModel != mmModel {
		req.Messages = stripImagesForTextModel(req.Messages)
	}

	req.Messages = fixToolCallGaps(req.Messages)
	req.Messages, err = normalizeMessagesToolCallArguments(req.Messages)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Messages = prepareReasoningMessages(req.Messages)
	if req.Stream {
		if req.ExtraBody == nil {
			req.ExtraBody = map[string]any{}
		}
		req.ExtraBody["stream_options"] = map[string]any{"include_usage": true}
	}
	upstreamBody := buildUpstreamBody(&req)

	if req.Stream {
		upResp, status, upHeader, err := callOpenCodeAPIStream(reqCtx, upstreamBody, req.Model)
		if err != nil || status < 200 || status >= 300 {
			w.Header().Set("Content-Type", "application/json")
			status = applyUpstreamErrorHeaders(w, upHeader, status)
			w.WriteHeader(status)
			if upResp != nil {
				errBody, _ := io.ReadAll(upResp)
				if len(errBody) > 0 {
					w.Write(errBody)
					return
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error", "type": "upstream_error"}})
			return
		}
		defer upResp.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		reader := bufio.NewReader(upResp)
		// 流式 usage 统计：上游按 OpenAI 标准只在收尾的纯 usage chunk 带统计字段，
		// 若上游异常在多条 chunk 重复则只取最后一次，一次请求统一记录一次，
		// 避免重复累加 token。
		var finalUsage map[string]any
		defer func() {
			if finalUsage == nil {
				return
			}
			pt, _ := finalUsage["prompt_tokens"].(float64)
			ct, _ := finalUsage["completion_tokens"].(float64)
			tt, _ := finalUsage["total_tokens"].(float64)
			if tt > 0 {
				recordTokenUsage(req.Model, int64(pt), int64(ct), int64(tt))
			}
		}()
		doneSeen := false
		// CMP-119：客户端断开后写端立即报错，收到就退出循环，别读到上游 EOF
		// 才罢手（上游流可以一直开着，那样 goroutine 与连接永不释放）。
		var clientGone bool
		for {
			if clientGone {
				log.Printf("%s client gone mid-stream, stop reading upstream", reqLabel(cnt, r))
				return
			}
			line, err := reader.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					break
				}
				log.Printf("%s Error reading stream: %v", reqLabel(cnt, r), err)
				// 发送错误事件通知客户端
				w.Write([]byte("data: {\"error\":\"stream read error\"}\n\n"))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				return
			}
			if doneSeen {
				continue
			}
			trimmed := strings.TrimSpace(line)
			if trimmed == "data: [DONE]" {
				doneSeen = true
				if _, werr := w.Write([]byte("data: [DONE]\n\n")); werr != nil {
					clientGone = true
					log.Printf("%s client gone mid-stream: %v", reqLabel(cnt, r), werr)
					continue
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				continue
			}
			if debugMode && strings.HasPrefix(line, "data: ") {
				log.Printf("[upstream raw chunk] %s", strings.TrimSpace(line[6:]))
			}

			out, usage, upstreamErr := convertStreamChunkWithUsage(line, aliasModel)
			if upstreamErr != nil {
				errBody, _ := json.Marshal(map[string]any{"error": map[string]any{
					"message": extractUpstreamErrorMessage(upstreamErr),
					"type":    "upstream_error",
				}})
				io.WriteString(w, "data: "+string(errBody)+"\n\n")
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				return
			}
			// 只收集最后一次 usage，由上面的 defer 统一记录一次
			if usage != nil {
				finalUsage = usage
			}
			if out == "" {
				// 空choices chunk（纯 usage chunk）
				continue
			}

			if _, werr := io.WriteString(w, out+"\n"); werr != nil {
				clientGone = true
				log.Printf("%s client gone mid-stream: %v", reqLabel(cnt, r), werr)
				continue
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		return
	}

	respBody, status, upHeader, err := callOpenCodeAPI(reqCtx, upstreamBody, req.Model)
	if err != nil || status < 200 || status >= 300 {
		w.Header().Set("Content-Type", "application/json")
		status = applyUpstreamErrorHeaders(w, upHeader, status)
		w.WriteHeader(status)
		if len(respBody) > 0 {
			w.Write(respBody)
		} else {
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error", "type": "upstream_error"}})
		}
		return
	}
	outBody := respBody
	convertedResp, err := convertResponse(respBody, aliasModel)
	if err == nil {
		outBody = convertedResp
	}
	// Record token usage
	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			pt, _ := u["prompt_tokens"].(float64)
			ct, _ := u["completion_tokens"].(float64)
			tt, _ := u["total_tokens"].(float64)
			if tt > 0 {
				recordTokenUsage(req.Model, int64(pt), int64(ct), int64(tt))
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(outBody)
}

// ======================== Models Handler ========================

func listModelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	modelMu.RLock()
	loaded, models := modelsLoaded, modelsCache
	modelMu.RUnlock()
	if !loaded || len(models) == 0 {
		fetched, err := fetchModels()
		if err == nil && len(fetched) > 0 {
			modelMu.Lock()
			modelsCache = fetched
			modelsLoaded = true
			models = modelsCache
			modelMu.Unlock()
		}
	}
	if len(models) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "无法获取模型列表，请检查上游服务是否可用",
		})
		return
	}
	// 只返回别名映射的模型，不暴露上游全量模型列表
	configMu.RLock()
	aliases := make([]string, 0, len(modelAlias))
	for k := range modelAlias {
		aliases = append(aliases, k)
	}
	configMu.RUnlock()
	now := time.Now().Unix()
	aliasModels := make([]ModelInfo, 0, len(aliases))
	for _, alias := range aliases {
		aliasModels = append(aliasModels, ModelInfo{ID: alias, Object: "model", Created: now, OwnedBy: "alias"})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   aliasModels,
	})
}

// ======================== Claude Messages API ========================

func extractClaudeSystemText(system any) string {
	if system == nil {
		return ""
	}
	switch v := system.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if block, ok := item.(map[string]any); ok {
				if block["type"] == "text" {
					if text, ok := block["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func cleanJsonSchema(schema any) any {
	m, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	delete(m, "$schema")
	delete(m, "title")
	delete(m, "examples")
	delete(m, "additionalProperties")
	if m["type"] == "string" {
		delete(m, "format")
	}
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			m[k] = cleanJsonSchema(sub)
		}
		if arr, ok := v.([]any); ok {
			for i, elem := range arr {
				if sub, ok := elem.(map[string]any); ok {
					arr[i] = cleanJsonSchema(sub)
				}
			}
			m[k] = arr
		}
	}
	return m
}

func claudeToOpenAIMessages(claudeMsgs []ClaudeMessage, system any) []Message {
	var messages []Message
	if sysText := extractClaudeSystemText(system); sysText != "" {
		messages = append(messages, Message{Role: "system", Content: sysText})
	}
	for _, msg := range claudeMsgs {
		switch content := msg.Content.(type) {
		case string:
			messages = append(messages, Message{Role: msg.Role, Content: content})
		case []any:
			var textParts []string
			var reasoningParts []string
			var toolCalls []ToolCall
			var toolResults []Message
			var imageParts []map[string]any
			for _, item := range content {
				block, ok := item.(map[string]any)
				if !ok {
					continue
				}
				blockType, _ := block["type"].(string)
				switch blockType {
				case "text":
					if text, ok := block["text"].(string); ok && text != "" {
						textParts = append(textParts, text)
					}
				case "image":
					source, _ := block["source"].(map[string]any)
					if source != nil {
						srcType, _ := source["type"].(string)
						mediaType, _ := source["media_type"].(string)
						data, _ := source["data"].(string)
						if srcType == "base64" && data != "" {
							if mediaType == "" {
								mediaType = "image/png"
							}
							imageParts = append(imageParts, map[string]any{
								"type": "image_url",
								"image_url": map[string]string{
									"url": "data:" + mediaType + ";base64," + data,
								},
							})
						} else if srcType == "url" {
							if url, ok := source["url"].(string); ok && url != "" {
								imageParts = append(imageParts, map[string]any{
									"type": "image_url",
									"image_url": map[string]string{
										"url": url,
									},
								})
							}
						}
					}
				case "thinking":
					if thinking, ok := block["thinking"].(string); ok && thinking != "" {
						reasoningParts = append(reasoningParts, thinking)
					}
				case "tool_use":
					id, _ := block["id"].(string)
					name, _ := block["name"].(string)
					var args string
					switch input := block["input"].(type) {
					case string:
						args = input
					default:
						if input != nil {
							b, _ := json.Marshal(input)
							args = string(b)
						}
					}
					if args == "" {
						args = "{}"
					}
					toolCalls = append(toolCalls, ToolCall{
						ID:   id,
						Type: "function",
						Function: FunctionCall{
							Name:      name,
							Arguments: args,
						},
					})
				case "tool_result":
					toolUseID, _ := block["tool_use_id"].(string)
					var resultText string
					switch c := block["content"].(type) {
					case string:
						resultText = c
					case []any:
						var parts []string
						for _, p := range c {
							if pb, ok := p.(map[string]any); ok && pb["type"] == "text" {
								if t, ok := pb["text"].(string); ok {
									parts = append(parts, t)
								}
							}
						}
						resultText = strings.Join(parts, "\n")
					default:
						if c != nil {
							b, _ := json.Marshal(c)
							resultText = string(b)
						}
					}
					toolResults = append(toolResults, Message{
						Role:       "tool",
						ToolCallID: toolUseID,
						Content:    resultText,
					})
				}
			}
			om := Message{Role: msg.Role}
			if len(imageParts) > 0 {
				var contentArr []any
				for _, img := range imageParts {
					contentArr = append(contentArr, img)
				}
				if len(textParts) > 0 {
					contentArr = append(contentArr, map[string]any{
						"type": "text",
						"text": strings.Join(textParts, "\n"),
					})
				}
				om.Content = contentArr
			} else if len(textParts) > 0 {
				om.Content = strings.Join(textParts, "\n")
			} else if len(toolCalls) == 0 {
				om.Content = ""
			}
			if len(reasoningParts) > 0 {
				rc := strings.Join(reasoningParts, "\n")
				om.ReasoningContent = &rc
			}
			if len(toolCalls) > 0 {
				om.ToolCalls = toolCalls
			}
			messages = append(messages, om)
			messages = append(messages, toolResults...)
		default:
			b, _ := json.Marshal(content)
			messages = append(messages, Message{Role: msg.Role, Content: string(b)})
		}
	}
	return messages
}

func claudeToOpenAITools(claudeTools []ClaudeTool) []Tool {
	tools := make([]Tool, 0, len(claudeTools))
	for _, ct := range claudeTools {
		params := ct.InputSchema
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		params = cleanJsonSchema(params)
		paramsMap, ok := params.(map[string]any)
		if !ok {
			paramsMap = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, Tool{
			Type: "function",
			Function: ToolFunction{
				Name:        ct.Name,
				Description: ct.Description,
				Parameters:  paramsMap,
			},
		})
	}
	return tools
}

func convertClaudeToolChoice(choice any) any {
	if choice == nil {
		return nil
	}
	switch value := choice.(type) {
	case string:
		return value
	case map[string]any:
		choiceType, _ := value["type"].(string)
		switch choiceType {
		case "auto", "none":
			return choiceType
		case "any":
			return "required"
		case "tool":
			name, _ := value["name"].(string)
			if name == "" {
				return "auto"
			}
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	return choice
}

func openAIToClaudeResponse(chatBody []byte, model string) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(chatBody, &raw); err != nil {
		return nil, fmt.Errorf("upstream returned invalid JSON: %w", err)
	}
	if upstreamError, ok := raw["error"]; ok {
		return nil, fmt.Errorf("upstream returned error: %s", extractUpstreamErrorMessage(upstreamError))
	}
	var chat struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Choices []struct {
			Message struct {
				Content          string     `json:"content"`
				ReasoningContent string     `json:"reasoning_content"`
				Reasoning        string     `json:"reasoning"`
				ToolCalls        []ToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		return nil, err
	}
	if len(chat.Choices) == 0 {
		return nil, errors.New("upstream returned chat completion without choices")
	}

	content := []ClaudeContent{}
	stopReason := "end_turn"

	msg := chat.Choices[0].Message
	fr := chat.Choices[0].FinishReason
	reasoning := msg.ReasoningContent
	if reasoning == "" {
		reasoning = msg.Reasoning
	}
	if reasoning != "" {
		content = append(content, ClaudeContent{
			Type:      "thinking",
			Thinking:  reasoning,
			Signature: fakeThinkingSignature(),
		})
	}
	if msg.Content != "" {
		content = append(content, ClaudeContent{Type: "text", Text: msg.Content})
	}
	for _, tc := range msg.ToolCalls {
		var input any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
			return nil, fmt.Errorf("tool call %q returned invalid arguments: %w", tc.ID, err)
		}
		if input == nil {
			input = map[string]any{}
		}
		content = append(content, ClaudeContent{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input})
	}
	switch fr {
	case "length":
		stopReason = "max_tokens"
	case "tool_calls", "function_call":
		stopReason = "tool_use"
	}

	if len(content) == 0 {
		content = append(content, ClaudeContent{Type: "text", Text: ""})
	}

	resp := ClaudeResponse{
		ID:         fmt.Sprintf("msg_%s", randomString(24)),
		Type:       "message",
		Role:       "assistant",
		Content:    content,
		Model:      model,
		StopReason: stopReason,
	}
	if chat.Usage != nil {
		inputTokens := chat.Usage["prompt_tokens"]
		if inputTokens == nil {
			inputTokens = chat.Usage["input_tokens"]
		}
		outputTokens := chat.Usage["completion_tokens"]
		if outputTokens == nil {
			outputTokens = chat.Usage["output_tokens"]
		}
		resp.Usage = &ClaudeUsage{
			InputTokens:  int(toFloat64(inputTokens)),
			OutputTokens: int(toFloat64(outputTokens)),
		}
	}
	result, _ := json.Marshal(resp)
	return result, nil
}

func toFloat64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}

func claudeMessagesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	cnt := requestCount.Add(1)
	if debugMode {
		log.Printf("[request #%d] POST /v1/messages\n%s", cnt, string(body))
	}

	var claudeReq ClaudeRequest
	if err := json.Unmarshal(body, &claudeReq); err != nil {
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"Invalid JSON"}}`, http.StatusBadRequest)
		return
	}
	reqCtx := withRequestLabel(r.Context(), reqLabel(cnt, r))
	log.Printf("[访问] %s %s %s model=%s", reqLabel(cnt, r), r.Method, r.URL.Path, claudeReq.Model)
	aliasModel := claudeReq.Model
	targetModel, mmModel, ok := resolveModel(claudeReq.Model)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]any{
			"type":    "invalid_request_error",
			"message": "模型 " + claudeReq.Model + " 未在别名列表中配置，禁止调用",
		}})
		return
	}

	messages := claudeToOpenAIMessages(claudeReq.Messages, claudeReq.System)
	messages = fixToolCallGaps(messages)
	messages, err = normalizeMessagesToolCallArguments(messages)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "invalid_request_error", "message": err.Error()}})
		return
	}

	// 多模态路由：只按"本轮新增图片"判定，历史图片不锁定多模态路由。
	// （与 /v1/responses 路径一致：codex 每轮携带全量历史，
	//   若全量扫描会导致带图轮次之后的纯文本轮次也永久路由到多模态模型）
	useModel := targetModel
	if mmModel != "" && hasNewImageContent(messages) {
		useModel = mmModel
	}
	claudeReq.Model = useModel

	// 配置了多模态模型但本轮未路由过去（纯文本轮次）时，剥离所有图片
	// （含历史图片），避免纯文本上游报错；命中多模态模型保留图片原样发送。
	// 未配置 multimodal_model 时视为目标模型自带能力，图片 dumb pipe 透传。
	if mmModel != "" && useModel != mmModel {
		messages = stripImagesForTextModel(messages)
	}

	chatReq := OpenAIRequest{
		Model:    claudeReq.Model,
		Messages: messages,
		Stream:   claudeReq.Stream,
		Thinking: claudeReq.Thinking,
	}
	if claudeReq.Stream {
		chatReq.ExtraBody = map[string]any{
			"stream_options": map[string]any{"include_usage": true},
		}
	}
	if claudeReq.MaxTokens > 0 {
		chatReq.MaxTokens = claudeReq.MaxTokens
	}
	if claudeReq.Temperature != nil {
		chatReq.Temperature = claudeReq.Temperature
	}
	if claudeReq.TopP != nil {
		chatReq.TopP = claudeReq.TopP
	}
	if len(claudeReq.Tools) > 0 {
		chatReq.Tools = claudeToOpenAITools(claudeReq.Tools)
		if claudeReq.ToolChoice != nil {
			chatReq.ToolChoice = convertClaudeToolChoice(claudeReq.ToolChoice)
		} else {
			chatReq.ToolChoice = "auto"
		}
	}

	chatReq.Messages = prepareReasoningMessages(chatReq.Messages)

	upstreamBody := buildUpstreamBody(&chatReq)

	if claudeReq.Stream {
		upResp, status, upHeader, err := callOpenCodeAPIStream(reqCtx, upstreamBody, chatReq.Model)
		if err != nil || status < 200 || status >= 300 {
			errResp := map[string]any{
				"type":  "error",
				"error": map[string]string{"type": "api_error", "message": "upstream error"},
			}
			w.Header().Set("Content-Type", "application/json")
			status = applyUpstreamErrorHeaders(w, upHeader, status)
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(errResp)
			return
		}
		defer upResp.Close()
		claudeStreamHandler(w, upResp, aliasModel, claudeReq.Model, reqLabel(cnt, r))
		return
	}

	respBody, status, upHeader, err := callOpenCodeAPI(reqCtx, upstreamBody, chatReq.Model)
	if err != nil || status < 200 || status >= 300 {
		w.Header().Set("Content-Type", "application/json")
		status = applyUpstreamErrorHeaders(w, upHeader, status)
		w.WriteHeader(status)
		if len(respBody) > 0 {
			w.Write(respBody)
		} else {
			json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": "upstream error"}})
		}
		return
	}

	claudeRespBody, convertErr := openAIToClaudeResponse(respBody, aliasModel)
	if convertErr != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": convertErr.Error()}})
		return
	}

	// Record token usage
	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			pt, _ := u["prompt_tokens"].(float64)
			ct, _ := u["completion_tokens"].(float64)
			tt, _ := u["total_tokens"].(float64)
			if tt > 0 {
				recordTokenUsage(claudeReq.Model, int64(pt), int64(ct), int64(tt))
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if debugMode {
		log.Printf("[client response]\n%s", string(claudeRespBody))
	}
	w.Write(claudeRespBody)
}

func claudeStreamHandler(w http.ResponseWriter, respBody io.ReadCloser, model string, actualModel string, label string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(respBody)

	msgID := fmt.Sprintf("msg_%s", randomString(24))
	blockIndex := 0
	thinkingBlockOpen := false
	textBlockOpen := false
	toolCallAccumulator := map[int]map[string]string{}
	toolCallOrder := []int{}
	messageStartSent := false
	finishSeen := false
	finalStopReason := "end_turn"
	toolBlocksClosed := false
	fullUsage := map[string]any{}
	defer func() {
		if len(fullUsage) > 0 {
			pt, _ := fullUsage["prompt_tokens"].(float64)
			ct, _ := fullUsage["completion_tokens"].(float64)
			tt, _ := fullUsage["total_tokens"].(float64)
			if tt > 0 {
				recordTokenUsage(actualModel, int64(pt), int64(ct), int64(tt))
			}
		}
	}()

	var clientGone bool
	emitClaudeEvent := func(event string, data any) {
		if clientGone {
			return
		}
		jsonData, err := json.Marshal(data)
		if err != nil {
			log.Printf("Error marshaling Claude SSE event: %v", err)
			return
		}
		if _, err := io.WriteString(w, "event: "+event+"\ndata: "+string(jsonData)+"\n\n"); err != nil {
			// CMP-119：客户端断开后写端立即报错。原来写错误被丢弃，
			// handler 一直读到上游 EOF 才返回，goroutine 和上游连接一起泄漏；
			// 面板反复刷新/切页累积起来就是 fd 被吃光、accept 停摆。
			clientGone = true
			log.Printf("%s client gone mid-stream: %v", label, err)
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	closeThinkingBlock := func() {
		if !thinkingBlockOpen {
			return
		}
		emitClaudeEvent("content_block_stop", map[string]any{
			"type":          "content_block_stop",
			"index":         blockIndex - 1,
			"content_block": map[string]any{"type": "thinking"},
		})
		thinkingBlockOpen = false
	}

	closeTextBlock := func() {
		if !textBlockOpen {
			return
		}
		emitClaudeEvent("content_block_stop", map[string]any{
			"type":          "content_block_stop",
			"index":         blockIndex - 1,
			"content_block": map[string]any{"type": "text"},
		})
		textBlockOpen = false
	}
	closeToolBlocks := func() {
		if toolBlocksClosed {
			return
		}
		for _, idx := range toolCallOrder {
			acc := toolCallAccumulator[idx]
			emitClaudeEvent("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": blockIndex - len(toolCallOrder) + indexOfInt(toolCallOrder, idx),
				"content_block": map[string]any{
					"type": "tool_use", "id": acc["id"], "name": acc["name"], "input": map[string]any{},
				},
			})
		}
		toolBlocksClosed = true
	}

	for {
		if clientGone {
			return
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Printf("%s Error reading stream: %v", label, err)
			break
		}
		if debugMode && strings.HasPrefix(line, "data: ") {
			log.Printf("[upstream raw chunk] %s", strings.TrimSpace(line[6:]))
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "data: [DONE]" || trimmed == "[DONE]" {
			break
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var chunk map[string]any
		if err := json.Unmarshal([]byte(line[6:]), &chunk); err != nil {
			continue
		}
		if upstreamError, ok := chunk["error"]; ok {
			emitClaudeEvent("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": extractUpstreamErrorMessage(upstreamError)}})
			return
		}

		choices, ok := chunk["choices"].([]any)
		if !ok || len(choices) == 0 {
			if usage, ok := chunk["usage"].(map[string]any); ok {
				fullUsage = usage
			}
			continue
		}
		if usageMap, ok := chunk["usage"].(map[string]any); ok && len(usageMap) > 0 {
			fullUsage = usageMap
		}

		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		normalizeReasoningContent(delta)
		finishReason, _ := choice["finish_reason"].(string)

		if !messageStartSent {
			messageStartSent = true
			emitClaudeEvent("message_start", map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id":          msgID,
					"type":        "message",
					"role":        "assistant",
					"content":     []any{},
					"model":       model,
					"stop_reason": nil,
					"usage":       map[string]any{"input_tokens": 0, "output_tokens": 0},
				},
			})
			emitClaudeEvent("ping", map[string]any{"type": "ping"})
		}

		if rc, ok := delta["reasoning_content"]; ok {
			rcStr, _ := rc.(string)
			if rcStr != "" {
				closeTextBlock()
				if !thinkingBlockOpen {
					emitClaudeEvent("content_block_start", map[string]any{
						"type":  "content_block_start",
						"index": blockIndex,
						"content_block": map[string]any{
							"type":      "thinking",
							"thinking":  "",
							"signature": fakeThinkingSignature(),
						},
					})
					thinkingBlockOpen = true
					blockIndex++
				}
				emitClaudeEvent("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": blockIndex - 1,
					"delta": map[string]any{
						"type":     "thinking_delta",
						"thinking": rcStr,
					},
				})
			}
		}

		if c, ok := delta["content"]; ok && c != nil {
			contentStr, _ := c.(string)
			if contentStr != "" {
				closeThinkingBlock()
				if !textBlockOpen {
					emitClaudeEvent("content_block_start", map[string]any{
						"type":  "content_block_start",
						"index": blockIndex,
						"content_block": map[string]any{
							"type": "text",
							"text": "",
						},
					})
					textBlockOpen = true
					blockIndex++
				}
				emitClaudeEvent("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": blockIndex - 1,
					"delta": map[string]any{
						"type": "text_delta",
						"text": contentStr,
					},
				})
			}
		}

		if rawToolCalls, ok := delta["tool_calls"].([]any); ok {
			for _, rawTC := range rawToolCalls {
				tc, ok := rawTC.(map[string]any)
				if !ok {
					continue
				}
				idxFloat, _ := tc["index"].(float64)
				upstreamIndex := int(idxFloat)

				closeThinkingBlock()
				closeTextBlock()

				if _, exists := toolCallAccumulator[upstreamIndex]; !exists {
					callID, _ := tc["id"].(string)
					if callID == "" {
						callID = "toolu_" + randomString(12)
					}
					fn, _ := tc["function"].(map[string]any)
					name, _ := fn["name"].(string)
					// 只存 id/name，不存参数（参数以 input_json_delta 增量转发，不需要累积）
					toolCallAccumulator[upstreamIndex] = map[string]string{
						"id":   callID,
						"name": name,
					}
					toolCallOrder = append(toolCallOrder, upstreamIndex)
					emitClaudeEvent("content_block_start", map[string]any{
						"type":  "content_block_start",
						"index": blockIndex,
						"content_block": map[string]any{
							"type":  "tool_use",
							"id":    callID,
							"name":  name,
							"input": map[string]any{},
						},
					})
					blockIndex++
				}

				fn, _ := tc["function"].(map[string]any)
				if argDelta, ok := fn["arguments"].(string); ok && argDelta != "" {
					// 不要再把参数往累积串里拼：Claude 侧只把参数以 input_json_delta 增量转发出去，
					// 收尾时用的是空 input（见 closeToolBlocks），这个字符串从头到尾没人读。
					// 而它每来一个分片就要复制一次整串，长工具调用是 O(n²) 的纯浪费。
					emitClaudeEvent("content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": blockIndex - 1,
						"delta": map[string]any{
							"type":         "input_json_delta",
							"partial_json": argDelta,
						},
					})
				}
			}
		}

		if usage, ok := chunk["usage"].(map[string]any); ok {
			fullUsage = usage
		}

		if finishReason == "stop" || finishReason == "length" || finishReason == "tool_calls" || finishReason == "function_call" || finishReason == "content_filter" {
			closeThinkingBlock()
			closeTextBlock()
			closeToolBlocks()
			finalStopReason = "end_turn"
			switch finishReason {
			case "length":
				finalStopReason = "max_tokens"
			case "tool_calls", "function_call":
				finalStopReason = "tool_use"
			}
			finishSeen = true
		}
	}

	closeThinkingBlock()
	closeTextBlock()
	closeToolBlocks()
	if !messageStartSent {
		emitClaudeEvent("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "upstream stream ended without a message"}})
		return
	}
	if !finishSeen {
		// 流未正常结束（上游中断/截断）：如实报告错误，绝不假装 end_turn 成功。
		emitClaudeEvent("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "upstream stream interrupted before completion"}})
		return
	}
	inputTokens := int(toFloat64(fullUsage["prompt_tokens"]))
	if inputTokens == 0 {
		inputTokens = int(toFloat64(fullUsage["input_tokens"]))
	}
	outputTokens := int(toFloat64(fullUsage["completion_tokens"]))
	if outputTokens == 0 {
		outputTokens = int(toFloat64(fullUsage["output_tokens"]))
	}
	emitClaudeEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": finalStopReason, "stop_sequence": nil},
		"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens},
	})
	emitClaudeEvent("message_stop", map[string]any{"type": "message_stop"})
}

func indexOfInt(slice []int, val int) int {
	for i, v := range slice {
		if v == val {
			return i
		}
	}
	return 0
}

// ======================== Responses API ========================

func responsesInputToMessages(input any, instructions string) []Message {
	var messages []Message
	if instructions != "" {
		messages = append(messages, Message{Role: "system", Content: instructions})
	}
	switch v := input.(type) {
	case string:
		messages = append(messages, Message{Role: "user", Content: v})
	case []any:
		var pendingAssistant *Message
		// pendingOutputs 缓存当前 assistant turn 的工具输出（方案A：不立即 flush，
		// 等同一 turn 的所有 function_call 聚合完成后统一输出，避免拆分出无
		// reasoning_content 的 assistant 消息导致 DeepSeek thinking 模式拒绝）
		var pendingOutputs []struct {
			CallID string
			Output any
		}
		// pendingImages 暂存从 tool 输出中提取的图片 part（mimo 等上游不接受
		// tool 消息携带 image_url）。等待下一个 user 消息时附加进去。
		var pendingImages []map[string]any
		ensurePendingAssistant := func() *Message {
			if pendingAssistant == nil {
				pendingAssistant = &Message{Role: "assistant", Content: ""}
			}
			return pendingAssistant
		}
		flushPendingAssistant := func() {
			if pendingAssistant == nil {
				return
			}
			if pendingAssistant.Content == nil {
				pendingAssistant.Content = ""
			}
			messages = append(messages, *pendingAssistant)
			pendingAssistant = nil
			// assistant turn 聚合完成后，按序输出缓存的工具结果
			for _, po := range pendingOutputs {
				toolMsg := Message{Role: "tool", ToolCallID: po.CallID, Content: po.Output}
				// 若 tool 输出含图片 part，提取到 pendingImages（tool 消息不能带图），
				// 文本部分保留在原位，图片稍后附加到下一个 user 消息。
				if parts, ok := po.Output.([]any); ok {
					var kept []any
					for _, p := range parts {
						if block, ok := p.(map[string]any); ok {
							if block["type"] == "image_url" {
								pendingImages = append(pendingImages, block)
								continue
							}
						}
						kept = append(kept, p)
					}
					if len(kept) > 0 {
						toolMsg.Content = kept
					} else {
						toolMsg.Content = "[tool output image]"
					}
				}
				messages = append(messages, toolMsg)
			}
			pendingOutputs = nil
		}
		appendPendingReasoning := func(text string) {
			if text == "" {
				return
			}
			msg := ensurePendingAssistant()
			if msg.ReasoningContent == nil || *msg.ReasoningContent == "" {
				rc := text
				msg.ReasoningContent = &rc
				return
			}
			rc := *msg.ReasoningContent + "\n" + text
			msg.ReasoningContent = &rc
		}
		appendPendingText := func(text string) {
			if text == "" {
				return
			}
			msg := ensurePendingAssistant()
			if existing, ok := msg.Content.(string); ok && existing != "" {
				msg.Content = existing + "\n" + text
			} else {
				msg.Content = text
			}
		}
		for _, item := range v {
			switch elem := item.(type) {
			case string:
				flushPendingAssistant()
				// 若之前有 tool 输出的图片，附加到本 user 消息（总是以 user 消息承载图片）
				if len(pendingImages) > 0 {
					content := []any{map[string]any{"type": "text", "text": elem}}
					for _, img := range pendingImages {
						content = append(content, img)
					}
					messages = append(messages, Message{Role: "user", Content: content})
					pendingImages = nil
				} else {
					messages = append(messages, Message{Role: "user", Content: elem})
				}
			case map[string]any:
				itemType, _ := elem["type"].(string)
				switch itemType {
				case "function_call", "tool_call":
					if tc, ok := responsesToolCallFromItem(elem); ok {
						msg := ensurePendingAssistant()
						msg.ToolCalls = append(msg.ToolCalls, tc)
					}
				case "function_call_output", "tool_result":
					// 不立即 flush：同一 assistant turn 内可能有多个 function_call，
					// 立即 flush 会把它们拆成多条 assistant 消息，导致后续消息
					// 丢失 reasoning_content（DeepSeek thinking 模式会拒绝）。
					// 先缓存输出，等当前 turn 聚合完成后再统一输出。
					callID, output := responsesToolOutputFromItem(elem)
					if callID != "" {
						pendingOutputs = append(pendingOutputs, struct {
							CallID string
							Output any
						}{callID, output})
					}
					continue
				case "reasoning":
					// 前一个 assistant turn 若还有未输出的 tool_calls（同一 turn 聚合中
					// 被新的 reasoning 打断），先 flush 它，避免新 reasoning 错误合并进旧 turn。
					// 连续 reasoning（无 tool_calls）时不 flush，保持合并语义。
					if pendingAssistant != nil && len(pendingAssistant.ToolCalls) > 0 {
						flushPendingAssistant()
					}
					text := extractTextFromContentParts(elem["summary"])
					if text == "" {
						text = extractTextFromContentParts(elem["content"])
					}
					if text == "" {
						text, _ = elem["text"].(string)
					}
					appendPendingReasoning(text)
					continue
				case "message", "":
					role := "user"
					if r, ok := elem["role"].(string); ok && r != "" {
						role = r
					}
					if role == "developer" {
						role = "system"
					}
					if role == "assistant" {
						text := extractTextFromContentParts(elem["content"])
						appendPendingText(text)
					} else {
						flushPendingAssistant()
						content := responsesContentToChatContent(elem["content"])
						if role == "system" {
							content = extractTextFromContentParts(elem["content"])
						}
						// tool 输出的图片附加到 user 消息（mimo 等上游不接受 tool 消息带图）
						if role == "user" && len(pendingImages) > 0 {
							imgParts := []any{content}
							for _, img := range pendingImages {
								imgParts = append(imgParts, img)
							}
							content = imgParts
							pendingImages = nil
						}
						messages = append(messages, Message{Role: role, Content: content})
					}
				default:
					flushPendingAssistant()
					role := "user"
					if r, ok := elem["role"].(string); ok && r != "" {
						role = r
					}
					content := responsesContentToChatContent(elem["content"])
					if content == "" || content == nil {
						b, _ := json.Marshal(elem)
						content = string(b)
					}
					messages = append(messages, Message{Role: role, Content: content})
				}
			default:
				flushPendingAssistant()
				b, _ := json.Marshal(elem)
				messages = append(messages, Message{Role: "user", Content: string(b)})
			}
		}
		flushPendingAssistant()
	default:
		b, _ := json.Marshal(v)
		messages = append(messages, Message{Role: "user", Content: string(b)})
	}
	return messages
}

func responsesToolCallFromItem(elem map[string]any) (ToolCall, bool) {
	callID, _ := elem["call_id"].(string)
	if callID == "" {
		callID, _ = elem["id"].(string)
	}
	name, _ := elem["name"].(string)
	args, _ := elem["arguments"].(string)
	if args == "" {
		if rawArgs, ok := elem["arguments"]; ok && rawArgs != nil {
			b, _ := json.Marshal(rawArgs)
			args = string(b)
		}
	}
	if name == "" {
		if tu, ok := elem["tool_use"].(map[string]any); ok {
			name, _ = tu["name"].(string)
			if callID == "" {
				callID, _ = tu["id"].(string)
			}
			if a, ok := tu["arguments"].(string); ok {
				args = a
			} else if inp, ok := tu["input"]; ok {
				b, _ := json.Marshal(inp)
				args = string(b)
			}
		}
	}
	if callID == "" || name == "" {
		return ToolCall{}, false
	}
	if args == "" {
		args = "{}"
	}
	return ToolCall{
		ID:   callID,
		Type: "function",
		Function: FunctionCall{
			Name:      name,
			Arguments: args,
		},
	}, true
}

func responsesToolOutputFromItem(elem map[string]any) (string, any) {
	callID, _ := elem["call_id"].(string)
	if callID == "" {
		callID, _ = elem["tool_use_id"].(string)
	}
	if callID == "" {
		return "", ""
	}
	var output any
	switch o := elem["output"].(type) {
	case string:
		output = o
	case []any:
		if converted := responsesContentToChatContent(o); converted != "" && converted != nil {
			output = converted
		} else {
			b, _ := json.Marshal(o)
			output = string(b)
		}
	default:
		if o != nil {
			b, _ := json.Marshal(o)
			output = string(b)
		}
	}
	switch value := output.(type) {
	case nil:
		output = "[tool output missing]"
	case string:
		if value == "" {
			output = "[tool output missing]"
		}
	case []any:
		if len(value) == 0 {
			output = "[tool output missing]"
		}
	}
	return callID, output
}

func chatToolsToResponses(tools []Tool) []map[string]any {
	converted := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if tool.Type != "function" || strings.TrimSpace(tool.Function.Name) == "" {
			continue
		}
		parameters := tool.Function.Parameters
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		converted = append(converted, map[string]any{
			"type":        "function",
			"name":        tool.Function.Name,
			"description": tool.Function.Description,
			"parameters":  parameters,
		})
	}
	return converted
}

func chatToolChoiceToResponses(choice any) any {
	choiceMap, ok := choice.(map[string]any)
	if !ok {
		return choice
	}
	if choiceMap["type"] == "function" {
		if function, ok := choiceMap["function"].(map[string]any); ok {
			if name, ok := function["name"].(string); ok && name != "" {
				return map[string]any{"type": "function", "name": name}
			}
		}
	}
	return choice
}

func convertResponsesTools(tools []ResponsesTool) []Tool {
	converted, _ := convertResponsesToolsWithMappings(tools)
	return converted
}

func convertResponsesToolsWithMappings(tools []ResponsesTool) ([]Tool, map[string]ResponseToolNameMapping) {
	converted := make([]Tool, 0, len(tools))
	mappings := map[string]ResponseToolNameMapping{}
	for _, tool := range tools {
		switch tool.Type {
		case "function":
			if fn, ok := responsesToolFunction(tool, ""); ok {
				converted = append(converted, Tool{Type: "function", Function: fn})
			}
		case "namespace":
			namespace := strings.TrimSpace(tool.Name)
			for _, nested := range tool.Tools {
				if nested.Type != "function" {
					continue
				}
				if fn, ok := responsesToolFunction(nested, namespace); ok {
					converted = append(converted, Tool{Type: "function", Function: fn})
					mappings[fn.Name] = ResponseToolNameMapping{
						Namespace: namespace,
						Name:      responseToolName(nested),
					}
				}
			}
		}
	}
	return converted, mappings
}

func responsesToolFunction(tool ResponsesTool, namespace string) (ToolFunction, bool) {
	fn := ToolFunction{
		Name:        tool.Name,
		Description: tool.Description,
		Parameters:  tool.Parameters,
	}
	if tool.Function != nil {
		fn = *tool.Function
	}
	fn.Name = strings.TrimSpace(fn.Name)
	if fn.Name == "" {
		return ToolFunction{}, false
	}
	if namespace != "" {
		fn.Name = flattenNamespaceToolName(namespace, fn.Name)
	}
	if fn.Parameters == nil {
		fn.Parameters = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return fn, true
}

func responseToolName(tool ResponsesTool) string {
	if tool.Function != nil {
		return strings.TrimSpace(tool.Function.Name)
	}
	return strings.TrimSpace(tool.Name)
}

func flattenNamespaceToolName(namespace, toolName string) string {
	ns := strings.TrimSuffix(strings.TrimSpace(namespace), "__")
	name := strings.TrimSpace(toolName)
	if ns == "" {
		return name
	}
	return ns + "__" + name
}

func convertResponsesToolChoice(choice any) any {
	if choice == nil {
		return nil
	}
	choiceMap, ok := choice.(map[string]any)
	if !ok {
		return choice
	}
	if choiceMap["type"] == "function" {
		if name, ok := choiceMap["name"].(string); ok && name != "" {
			return map[string]any{
				"type":     "function",
				"function": map[string]any{"name": name},
			}
		}
	}
	if choiceMap["type"] == "namespace" {
		namespace, _ := choiceMap["name"].(string)
		toolName, _ := choiceMap["tool"].(string)
		if toolName == "" {
			toolName, _ = choiceMap["tool_name"].(string)
		}
		if namespace != "" && toolName != "" {
			return map[string]any{
				"type":     "function",
				"function": map[string]any{"name": flattenNamespaceToolName(namespace, toolName)},
			}
		}
	}
	return choice
}

func collectFunctionOutputs(items []any) map[string]string {
	outputs := map[string]string{}
	for _, item := range items {
		elem, ok := item.(map[string]any)
		if !ok || elem["type"] != "function_call_output" {
			continue
		}
		callID, _ := elem["call_id"].(string)
		if callID == "" {
			continue
		}
		switch v := elem["output"].(type) {
		case string:
			outputs[callID] = v
		default:
			b, _ := json.Marshal(v)
			outputs[callID] = string(b)
		}
	}
	return outputs
}

func responseFunctionCallItem(itemID, status, arguments, callID, name string, mappings map[string]ResponseToolNameMapping) map[string]any {
	item := map[string]any{
		"id":        itemID,
		"type":      "function_call",
		"status":    status,
		"arguments": arguments,
		"call_id":   callID,
		"name":      name,
	}
	if mapping, ok := responseToolNameMapping(name, mappings); ok {
		item["name"] = mapping.Name
		item["namespace"] = mapping.Namespace
	}
	return item
}

func responseToolNameMapping(name string, mappings map[string]ResponseToolNameMapping) (ResponseToolNameMapping, bool) {
	if len(mappings) == 0 {
		return ResponseToolNameMapping{}, false
	}
	if mapping, ok := mappings[name]; ok {
		return mapping, true
	}
	normalized := normalizeResponseToolCallKey(name)
	if mapping, ok := mappings[normalized]; ok {
		return mapping, true
	}
	return ResponseToolNameMapping{}, false
}

func normalizeResponseToolCallKey(name string) string {
	normalized := strings.NewReplacer(":", "__", ".", "__", "/", "__", "-", "_").Replace(strings.TrimSpace(name))
	for strings.Contains(normalized, "___") {
		normalized = strings.ReplaceAll(normalized, "___", "__")
	}
	return normalized
}

func responsesContentToChatContent(content any) any {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	parts, ok := content.([]any)
	if !ok {
		text := extractTextFromContentParts(content)
		if text != "" {
			return text
		}
		return ""
	}

	var converted []any
	var textParts []string
	hasImage := false
	for _, p := range parts {
		part, ok := p.(map[string]any)
		if !ok {
			continue
		}
		partType, _ := part["type"].(string)
		switch partType {
		case "input_text", "output_text", "summary_text", "text":
			if text, ok := part["text"].(string); ok && text != "" {
				textParts = append(textParts, text)
				converted = append(converted, map[string]any{"type": "text", "text": text})
			}
		case "input_image", "image_url":
			imageURL := responsesImageURLFromPart(part)
			if imageURL != nil {
				hasImage = true
				converted = append(converted, map[string]any{"type": "image_url", "image_url": imageURL})
			}
		}
	}
	if len(converted) == 0 {
		return ""
	}
	if hasImage {
		return converted
	}
	return strings.Join(textParts, "\n")
}

func responsesImageURLFromPart(part map[string]any) map[string]any {
	url := ""
	detail := ""
	if v, ok := part["image_url"].(string); ok {
		url = v
	}
	if imageURL, ok := part["image_url"].(map[string]any); ok {
		if u, ok := imageURL["url"].(string); ok {
			url = u
		}
		if d, ok := imageURL["detail"].(string); ok {
			detail = d
		}
	}
	if url == "" {
		if v, ok := part["url"].(string); ok {
			url = v
		}
	}
	if detail == "" {
		detail, _ = part["detail"].(string)
	}
	if url == "" {
		return nil
	}
	imageURL := map[string]any{"url": url}
	if detail != "" {
		imageURL["detail"] = detail
	}
	return imageURL
}

func extractTextFromContentParts(content any) string {
	parts, ok := content.([]any)
	if !ok {
		if s, ok := content.(string); ok {
			return s
		}
		return ""
	}
	var texts []string
	for _, p := range parts {
		if part, ok := p.(map[string]any); ok {
			if part["type"] == "input_text" || part["type"] == "output_text" || part["type"] == "summary_text" || part["type"] == "text" {
				if t, ok := part["text"].(string); ok {
					texts = append(texts, t)
				}
			}
		}
	}
	return strings.Join(texts, "\n")
}

func responsesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	cnt := requestCount.Add(1)
	if debugMode {
		log.Printf("[request #%d] POST /v1/responses\n%s", cnt, string(body))
	}

	var respReq ResponsesAPIRequest
	if err := json.Unmarshal(body, &respReq); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	reqCtx := withRequestLabel(r.Context(), reqLabel(cnt, r))
	log.Printf("[访问] %s %s %s model=%s", reqLabel(cnt, r), r.Method, r.URL.Path, respReq.Model)
	aliasModel := respReq.Model
	targetModel, mmModel, ok := resolveModel(respReq.Model)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"message": "模型 " + respReq.Model + " 未在别名列表中配置，禁止调用",
			"type":    "invalid_request_error",
		}})
		return
	}

	messages := respReq.Messages
	if len(messages) == 0 {
		messages = responsesInputToMessages(respReq.Input, respReq.Instructions)
	} else if respReq.Instructions != "" {
		messages = append([]Message{{Role: "system", Content: respReq.Instructions}}, messages...)
	}

	// 多模态路由：只按"本轮新增图片"判定，历史图片不锁定多模态路由。
	// codex 每轮携带全量历史，若全量扫描会导致带图轮次之后的纯文本
	// 轮次也永久路由到多模态模型。
	useModel := targetModel
	if mmModel != "" && hasNewImageContent(messages) {
		useModel = mmModel
	}
	respReq.Model = useModel

	// 配置了多模态模型但本轮未路由过去（纯文本轮次）时，剥离所有图片
	// （含历史图片），避免纯文本上游报错；命中多模态模型保留图片原样发送。
	// 未配置 multimodal_model 时视为目标模型自带能力，图片 dumb pipe 透传。
	if mmModel != "" && useModel != mmModel {
		messages = stripImagesForTextModel(messages)
	}

	chatReq := OpenAIRequest{
		Model:    respReq.Model,
		Messages: messages,
		Stream:   respReq.Stream,
	}
	if respReq.Stream {
		chatReq.ExtraBody = map[string]any{
			"stream_options": map[string]any{"include_usage": true},
		}
	}
	toolNameMappings := map[string]ResponseToolNameMapping{}
	if respReq.Temperature != 0 {
		chatReq.Temperature = &respReq.Temperature
	}
	if respReq.MaxTokens != 0 {
		chatReq.MaxTokens = respReq.MaxTokens
	}
	if respReq.TopP != 0 {
		chatReq.TopP = &respReq.TopP
	}
	if len(respReq.Tools) > 0 {
		chatReq.Tools, toolNameMappings = convertResponsesToolsWithMappings(respReq.Tools)
	}
	if respReq.ToolChoice != nil {
		chatReq.ToolChoice = convertResponsesToolChoice(respReq.ToolChoice)
	}
	if respReq.ParallelToolCalls != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["parallel_tool_calls"] = *respReq.ParallelToolCalls
	}
	if respReq.Reasoning.Effort != "" && respReq.Reasoning.Effort != "none" {
		chatReq.ReasoningEffort = respReq.Reasoning.Effort
	}
	chatReq.Messages = fixToolCallGaps(chatReq.Messages)
	chatReq.Messages, err = normalizeMessagesToolCallArguments(chatReq.Messages)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	chatReq.Messages = prepareReasoningMessages(chatReq.Messages)

	upstreamBody := buildUpstreamBody(&chatReq)

	if respReq.Stream {
		upResp, status, upHeader, err := callOpenCodeAPIStream(reqCtx, upstreamBody, chatReq.Model)
		if err != nil || status < 200 || status >= 300 {
			w.Header().Set("Content-Type", "application/json")
			status = applyUpstreamErrorHeaders(w, upHeader, status)
			w.WriteHeader(status)
			if upResp != nil {
				errBody, _ := io.ReadAll(upResp)
				if len(errBody) > 0 {
					w.Write(errBody)
					return
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error"}})
			return
		}
		defer upResp.Close()

		resp := &http.Response{
			StatusCode: status,
			Body:       upResp,
			Header:     make(http.Header),
		}
		responsesStreamHandler(w, resp, aliasModel, respReq.Model, chatReq.Tools, chatReq.ToolChoice, toolNameMappings, reqLabel(cnt, r))
		return
	}

	respBody, status, upHeader, err := callOpenCodeAPI(reqCtx, upstreamBody, chatReq.Model)
	if err != nil || status < 200 || status >= 300 {
		w.Header().Set("Content-Type", "application/json")
		status = applyUpstreamErrorHeaders(w, upHeader, status)
		w.WriteHeader(status)
		if len(respBody) > 0 {
			w.Write(respBody)
		} else {
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error"}})
		}
		return
	}

	responsesBody := convertChatToResponses(respBody, aliasModel, chatReq.Tools, chatReq.ToolChoice, toolNameMappings)

	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			pt, _ := u["prompt_tokens"].(float64)
			ct, _ := u["completion_tokens"].(float64)
			tt, _ := u["total_tokens"].(float64)
			if tt > 0 {
				recordTokenUsage(chatReq.Model, int64(pt), int64(ct), int64(tt))
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if debugMode {
		log.Printf("[responses response]\n%s", string(responsesBody))
	}
	w.Write(responsesBody)
}

// ======================== Responses Stream Handler ========================

func responsesStreamHandler(w http.ResponseWriter, resp *http.Response, model string, actualModel string, tools []Tool, toolChoice any, toolNameMappings map[string]ResponseToolNameMapping, label string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(resp.Body)

	responseID := "resp_" + time.Now().Format("20060102150405") + "_" + randomString(8)
	reasoningID := "rs_" + responseID
	msgID := "msg_" + responseID + "_0"
	createdAt := time.Now().Unix()
	seq := 0

	reasoningStarted := false
	reasoningDone := false
	messageStarted := false
	messageDone := false
	// 用 strings.Builder 累积：旧实现 fullText/fullReasoning 用 `+=` 拼接，
	// 长输出（尤其推理内容）是按分片数平方级增长的内存拷贝。
	var fullReasoning strings.Builder
	var fullText strings.Builder
	totalUsage := map[string]any{}
	createdSent := false
	toolCalls := map[int]map[string]any{}
	toolOrder := []int{}
	terminalFinishSeen := false
	isIncomplete := false
	incompleteReason := ""

	messageOutputIndex := func() int {
		if reasoningStarted {
			return 1
		}
		return 0
	}

	reasoningItem := func(status string) map[string]any {
		item := map[string]any{
			"id":      reasoningID,
			"type":    "reasoning",
			"summary": []any{},
		}
		if status != "" {
			item["status"] = status
		}
		if status == "completed" {
			item["encrypted_content"] = ""
		}
		if fullReasoning.Len() > 0 {
			item["summary"] = []any{map[string]any{"type": "summary_text", "text": fullReasoning.String()}}
		}
		return item
	}

	messageItem := func(status string) map[string]any {
		content := []any{map[string]any{
			"type":        "output_text",
			"annotations": []any{},
			"logprobs":    []any{},
			"text":        fullText.String(),
		}}
		return map[string]any{
			"id":      msgID,
			"type":    "message",
			"status":  status,
			"content": content,
			"role":    "assistant",
		}
	}

	emitReasoningDone := func() {
		if !reasoningStarted || reasoningDone {
			return
		}
		seq++
		emitSSEEvent(w, flusher, "response.reasoning_summary_text.done", map[string]any{
			"type":            "response.reasoning_summary_text.done",
			"sequence_number": seq,
			"item_id":         reasoningID,
			"output_index":    0,
			"summary_index":   0,
			"text":            fullReasoning.String(),
		})
		seq++
		emitSSEEvent(w, flusher, "response.reasoning_summary_part.done", map[string]any{
			"type":            "response.reasoning_summary_part.done",
			"sequence_number": seq,
			"item_id":         reasoningID,
			"output_index":    0,
			"summary_index":   0,
			"part":            map[string]any{"type": "summary_text", "text": fullReasoning.String()},
		})
		seq++
		emitSSEEvent(w, flusher, "response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
			"sequence_number": seq,
			"output_index":    0,
			"item":            reasoningItem("completed"),
		})
		reasoningDone = true
	}

	emitMessageDone := func() {
		if !messageStarted || messageDone {
			return
		}
		idx := messageOutputIndex()
		seq++
		emitSSEEvent(w, flusher, "response.output_text.done", map[string]any{
			"type":            "response.output_text.done",
			"sequence_number": seq,
			"item_id":         msgID,
			"output_index":    idx,
			"content_index":   0,
			"text":            fullText.String(),
			"logprobs":        []any{},
		})
		seq++
		emitSSEEvent(w, flusher, "response.content_part.done", map[string]any{
			"type":            "response.content_part.done",
			"sequence_number": seq,
			"item_id":         msgID,
			"output_index":    idx,
			"content_index":   0,
			"part":            map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": fullText.String()},
		})
		seq++
		emitSSEEvent(w, flusher, "response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
			"sequence_number": seq,
			"output_index":    idx,
			"item":            messageItem("completed"),
		})
		messageDone = true
	}

	emitToolCallDone := func(idx int, call map[string]any) {
		if done, _ := call["done"].(bool); done {
			return
		}
		call["done"] = true
		itemID, _ := call["item_id"].(string)
		callID, _ := call["call_id"].(string)
		name, _ := call["name"].(string)
		args := toolCallArgs(call)
		normalizedArgs, err := normalizeToolCallArguments(args)
		if err != nil {
			isIncomplete = true
			incompleteReason = "tool_call_arguments_incomplete"
			return
		}
		call["arguments"] = normalizedArgs
		seq++
		emitSSEEvent(w, flusher, "response.function_call_arguments.done", map[string]any{
			"type":            "response.function_call_arguments.done",
			"sequence_number": seq,
			"item_id":         itemID,
			"output_index":    idx,
			"arguments":       normalizedArgs,
		})
		seq++
		emitSSEEvent(w, flusher, "response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
			"sequence_number": seq,
			"output_index":    idx,
			"item":            responseFunctionCallItem(itemID, "completed", normalizedArgs, callID, name, toolNameMappings),
		})
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Printf("%s Error reading stream: %v", label, err)
			isIncomplete = true
			incompleteReason = "stream_read_error"
			break
		}
		if debugMode && strings.HasPrefix(line, "data: ") {
			log.Printf("[upstream raw chunk] %s", strings.TrimSpace(line[6:]))
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "data: [DONE]" || trimmed == "[DONE]" {
			break
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var chunk map[string]any
		if err := json.Unmarshal([]byte(line[6:]), &chunk); err != nil {
			continue
		}
		if !createdSent {
			if id, ok := chunk["id"].(string); ok && id != "" {
				responseID = id
				reasoningID = "rs_" + responseID + "_0"
				msgID = "msg_" + responseID + "_0"
			}
			if created, ok := chunk["created"].(float64); ok {
				createdAt = int64(created)
			}
			seq++
			emitSSEEvent(w, flusher, "response.created", map[string]any{
				"type":            "response.created",
				"sequence_number": seq,
				"response":        map[string]any{"id": responseID, "object": "response", "created_at": createdAt, "status": "in_progress", "background": false, "error": nil, "output": []any{}},
			})
			seq++
			emitSSEEvent(w, flusher, "response.in_progress", map[string]any{
				"type":            "response.in_progress",
				"sequence_number": seq,
				"response":        map[string]any{"id": responseID, "object": "response", "created_at": createdAt, "status": "in_progress"},
			})
			createdSent = true
		}
		if upstreamError, ok := chunk["error"]; ok {
			isIncomplete = true
			incompleteReason = extractUpstreamErrorMessage(upstreamError)
			break
		}
		choices, ok := chunk["choices"].([]any)
		if !ok || len(choices) == 0 {
			if usage, ok := chunk["usage"].(map[string]any); ok {
				totalUsage = usage
			}
			continue
		}

		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		normalizeReasoningContent(delta)
		finishReason, _ := choice["finish_reason"].(string)

		if rc, ok := delta["reasoning_content"]; ok {
			rcStr, _ := rc.(string)
			if rcStr != "" {
				if !reasoningStarted {
					seq++
					emitSSEEvent(w, flusher, "response.output_item.added", map[string]any{
						"type":            "response.output_item.added",
						"sequence_number": seq,
						"output_index":    0,
						"item":            reasoningItem("in_progress"),
					})
					seq++
					emitSSEEvent(w, flusher, "response.reasoning_summary_part.added", map[string]any{
						"type":            "response.reasoning_summary_part.added",
						"sequence_number": seq,
						"item_id":         reasoningID,
						"output_index":    0,
						"summary_index":   0,
						"part":            map[string]any{"type": "summary_text", "text": ""},
					})
					reasoningStarted = true
				}
				fullReasoning.WriteString(rcStr)
				seq++
				emitSSEEvent(w, flusher, "response.reasoning_summary_text.delta", map[string]any{
					"type":            "response.reasoning_summary_text.delta",
					"sequence_number": seq,
					"item_id":         reasoningID,
					"output_index":    0,
					"summary_index":   0,
					"delta":           rcStr,
				})
			}
		}

		contentStr := ""
		if c, ok := delta["content"]; ok && c != nil {
			contentStr, _ = c.(string)
		}
		if contentStr != "" {
			emitReasoningDone()
			if !messageStarted {
				idx := messageOutputIndex()
				seq++
				emitSSEEvent(w, flusher, "response.output_item.added", map[string]any{
					"type":            "response.output_item.added",
					"sequence_number": seq,
					"output_index":    idx,
					"item":            map[string]any{"id": msgID, "type": "message", "status": "in_progress", "content": []any{}, "role": "assistant"},
				})
				seq++
				emitSSEEvent(w, flusher, "response.content_part.added", map[string]any{
					"type":            "response.content_part.added",
					"sequence_number": seq,
					"item_id":         msgID,
					"output_index":    idx,
					"content_index":   0,
					"part":            map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": ""},
				})
				messageStarted = true
			}
			fullText.WriteString(contentStr)
			seq++
			emitSSEEvent(w, flusher, "response.output_text.delta", map[string]any{
				"type":            "response.output_text.delta",
				"sequence_number": seq,
				"item_id":         msgID,
				"output_index":    messageOutputIndex(),
				"content_index":   0,
				"delta":           contentStr,
				"logprobs":        []any{},
			})
		}

		rawToolCalls, _ := delta["tool_calls"].([]any)
		for _, rawToolCall := range rawToolCalls {
			tc, ok := rawToolCall.(map[string]any)
			if !ok {
				continue
			}
			idxFloat, _ := tc["index"].(float64)
			upstreamIndex := int(idxFloat)
			call, exists := toolCalls[upstreamIndex]
			if !exists {
				outputIndex := messageOutputIndex()
				if messageStarted {
					outputIndex++
				}
				outputIndex += len(toolOrder)
				callID, _ := tc["id"].(string)
				if callID == "" {
					callID = "call_" + randomString(12)
				}
				fn, _ := tc["function"].(map[string]any)
				name, _ := fn["name"].(string)
				call = map[string]any{
					"output_index": outputIndex,
					"item_id":      "fc_" + callID,
					"call_id":      callID,
					"name":         name,
					"arguments":    &strings.Builder{},
					"done":         false,
				}
				toolCalls[upstreamIndex] = call
				toolOrder = append(toolOrder, upstreamIndex)
				seq++
				emitSSEEvent(w, flusher, "response.output_item.added", map[string]any{
					"type":            "response.output_item.added",
					"sequence_number": seq,
					"output_index":    outputIndex,
					"item":            responseFunctionCallItem(call["item_id"].(string), "in_progress", "", callID, name, toolNameMappings),
				})
			}
			fn, _ := tc["function"].(map[string]any)
			if name, _ := fn["name"].(string); name != "" {
				call["name"] = name
			}
			if argDelta, _ := fn["arguments"].(string); argDelta != "" {
				if argDeltaBuf, ok := call["arguments"].(*strings.Builder); ok {
					argDeltaBuf.WriteString(argDelta)
				}
				seq++
				emitSSEEvent(w, flusher, "response.function_call_arguments.delta", map[string]any{
					"type":            "response.function_call_arguments.delta",
					"sequence_number": seq,
					"item_id":         call["item_id"],
					"output_index":    call["output_index"],
					"delta":           argDelta,
				})
			}
		}

		if usage, ok := chunk["usage"].(map[string]any); ok {
			totalUsage = usage
		}
		if finishReason == "stop" || finishReason == "length" || finishReason == "tool_calls" || finishReason == "function_call" || finishReason == "content_filter" {
			terminalFinishSeen = true
			if finishReason == "length" {
				isIncomplete = true
				incompleteReason = "max_output_tokens"
			}
			emitReasoningDone()
			if !messageStarted && len(toolCalls) == 0 {
				idx := messageOutputIndex()
				seq++
				emitSSEEvent(w, flusher, "response.output_item.added", map[string]any{
					"type":            "response.output_item.added",
					"sequence_number": seq,
					"output_index":    idx,
					"item":            map[string]any{"id": msgID, "type": "message", "status": "in_progress", "content": []any{}, "role": "assistant"},
				})
				seq++
				emitSSEEvent(w, flusher, "response.content_part.added", map[string]any{
					"type":            "response.content_part.added",
					"sequence_number": seq,
					"item_id":         msgID,
					"output_index":    idx,
					"content_index":   0,
					"part":            map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": ""},
				})
				messageStarted = true
			}
			emitMessageDone()
			for _, idx := range toolOrder {
				emitToolCallDone(toolCalls[idx]["output_index"].(int), toolCalls[idx])
			}
		}
	}
	if !terminalFinishSeen {
		isIncomplete = true
		if incompleteReason == "" {
			incompleteReason = "stream_ended_early"
		}
	}

	emitReasoningDone()
	emitMessageDone()
	if terminalFinishSeen {
		for _, idx := range toolOrder {
			emitToolCallDone(toolCalls[idx]["output_index"].(int), toolCalls[idx])
		}
	}

	output := []any{}
	if reasoningStarted {
		output = append(output, reasoningItem("completed"))
	}
	if messageStarted {
		output = append(output, messageItem("completed"))
	}
	for _, idx := range toolOrder {
		call := toolCalls[idx]
		arguments, err := normalizeToolCallArguments(toolCallArgs(call))
		if err != nil {
			isIncomplete = true
			incompleteReason = "tool_call_arguments_incomplete"
			continue
		}
		itemStatus := "completed"
		if !terminalFinishSeen {
			itemStatus = "in_progress"
		}
		output = append(output, responseFunctionCallItem(
			call["item_id"].(string),
			itemStatus,
			arguments,
			call["call_id"].(string),
			call["name"].(string),
			toolNameMappings,
		))
	}

	responseStatus := "completed"
	var incompleteDetails any
	if isIncomplete {
		responseStatus = "incomplete"
		incompleteDetails = map[string]any{"reason": incompleteReason}
	}
	completedResponse := map[string]any{
		"id":                 responseID,
		"object":             "response",
		"created_at":         createdAt,
		"status":             responseStatus,
		"background":         false,
		"error":              nil,
		"incomplete_details": incompleteDetails,
		"model":              model,
		"output":             output,
	}
	if len(tools) > 0 {
		completedResponse["tools"] = chatToolsToResponses(tools)
	}
	if toolChoice != nil {
		completedResponse["tool_choice"] = chatToolChoiceToResponses(toolChoice)
	}

	if len(totalUsage) > 0 {
		usage := map[string]any{}
		if v, ok := totalUsage["prompt_tokens"]; ok {
			usage["input_tokens"] = v
		}
		if v, ok := totalUsage["prompt_tokens_details"]; ok {
			usage["input_tokens_details"] = v
		} else {
			usage["input_tokens_details"] = map[string]any{"cached_tokens": 0}
		}
		if v, ok := totalUsage["completion_tokens"]; ok {
			usage["output_tokens"] = v
		}
		if v, ok := totalUsage["completion_tokens_details"]; ok {
			usage["output_tokens_details"] = v
		}
		if v, ok := totalUsage["total_tokens"]; ok {
			usage["total_tokens"] = v
		}
		if v, ok := totalUsage["input_tokens"]; ok && usage["input_tokens"] == nil {
			usage["input_tokens"] = v
		}
		if v, ok := totalUsage["output_tokens"]; ok && usage["output_tokens"] == nil {
			usage["output_tokens"] = v
		}
		if _, ok := usage["input_tokens"]; !ok {
			usage["input_tokens"] = 0
		}
		if _, ok := usage["output_tokens"]; !ok {
			usage["output_tokens"] = 0
		}
		if _, ok := usage["total_tokens"]; !ok {
			usage["total_tokens"] = toFloat64(usage["input_tokens"]) + toFloat64(usage["output_tokens"])
		}
		normalizeUsageDetails(usage)
		completedResponse["usage"] = usage
	} else {
		defaultUsage := map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
		normalizeUsageDetails(defaultUsage)
		completedResponse["usage"] = defaultUsage
	}

	if totalUsage != nil {
		pt, _ := totalUsage["prompt_tokens"].(float64)
		ct, _ := totalUsage["completion_tokens"].(float64)
		tt, _ := totalUsage["total_tokens"].(float64)
		if tt > 0 {
			recordTokenUsage(actualModel, int64(pt), int64(ct), int64(tt))
		}
	}

	seq++
	emitSSEEvent(w, flusher, "response."+responseStatus, map[string]any{
		"type":            "response." + responseStatus,
		"sequence_number": seq,
		"response":        completedResponse,
	})

	if flusher != nil {
		flusher.Flush()
	}
}

// normalizeUsageDetails 保证 Responses API usage 的必填嵌套字段存在。
// Codex 等客户端反序列化 response.completed 时，input_tokens_details.cached_tokens
// 与 output_tokens_details.reasoning_tokens 均为必填字段，上游 details 结构不完整时
// 会报 "failed to parse ResponseCompleted: missing field cached_tokens/reasoning_tokens"，
// 这里在透传后统一兜底补全。
func normalizeUsageDetails(usage map[string]any) {
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		if _, ok := details["cached_tokens"]; !ok {
			details["cached_tokens"] = 0
		}
	} else {
		usage["input_tokens_details"] = map[string]any{"cached_tokens": 0}
	}
	if details, ok := usage["output_tokens_details"].(map[string]any); ok {
		if _, ok := details["reasoning_tokens"]; !ok {
			details["reasoning_tokens"] = 0
		}
	} else {
		usage["output_tokens_details"] = map[string]any{"reasoning_tokens": 0}
	}
}

func convertChatToResponses(chatBody []byte, model string, tools []Tool, toolChoice any, toolNameMappings map[string]ResponseToolNameMapping) []byte {
	var chat struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string     `json:"content"`
				ReasoningContent string     `json:"reasoning_content"`
				Reasoning        string     `json:"reasoning"`
				ToolCalls        []ToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		log.Printf("Warning: convertChatToResponses unmarshal failed: %v", err)
	}

	text := ""
	reasoning := ""
	finishReason := ""
	var toolCalls []ToolCall
	if len(chat.Choices) > 0 {
		text = chat.Choices[0].Message.Content
		reasoning = chat.Choices[0].Message.ReasoningContent
		if reasoning == "" {
			reasoning = chat.Choices[0].Message.Reasoning
		}
		toolCalls = chat.Choices[0].Message.ToolCalls
		finishReason = chat.Choices[0].FinishReason
	}

	status := "completed"
	if finishReason == "length" {
		status = "incomplete"
	}

	responses := map[string]any{
		"id":                 chat.ID,
		"object":             "response",
		"status":             status,
		"background":         false,
		"error":              nil,
		"incomplete_details": nil,
		"model":              model,
		"created_at":         chat.Created,
	}
	if len(tools) > 0 {
		responses["tools"] = chatToolsToResponses(tools)
	}
	if toolChoice != nil {
		responses["tool_choice"] = chatToolChoiceToResponses(toolChoice)
	}
	outputID := "msg_" + chat.ID + "_0"
	output := []any{}
	if reasoning != "" {
		output = append(output, map[string]any{
			"id":                "rs_" + chat.ID,
			"type":              "reasoning",
			"encrypted_content": "",
			"summary":           []any{map[string]any{"type": "summary_text", "text": reasoning}},
		})
	}
	if text != "" || len(toolCalls) == 0 {
		output = append(output, map[string]any{
			"id":     outputID,
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []any{map[string]any{
				"type":        "output_text",
				"text":        text,
				"annotations": []any{},
				"logprobs":    []any{},
			}},
		})
	}
	for _, tc := range toolCalls {
		arguments, err := normalizeToolCallArguments(tc.Function.Arguments)
		if err != nil {
			responses["status"] = "incomplete"
			responses["incomplete_details"] = map[string]any{"reason": "tool_call_arguments_incomplete"}
			continue
		}
		output = append(output, responseFunctionCallItem("fc_"+tc.ID, "completed", arguments, tc.ID, tc.Function.Name, toolNameMappings))
	}
	responses["output"] = output
	if chat.Usage != nil {
		usage := map[string]any{}
		if v, ok := chat.Usage["prompt_tokens"]; ok {
			usage["input_tokens"] = v
		}
		if v, ok := chat.Usage["prompt_tokens_details"]; ok {
			usage["input_tokens_details"] = v
		} else {
			usage["input_tokens_details"] = map[string]any{"cached_tokens": 0}
		}
		if v, ok := chat.Usage["completion_tokens"]; ok {
			usage["output_tokens"] = v
		}
		if v, ok := chat.Usage["completion_tokens_details"]; ok {
			usage["output_tokens_details"] = v
		}
		if v, ok := chat.Usage["total_tokens"]; ok {
			usage["total_tokens"] = v
		}
		if v, ok := chat.Usage["input_tokens"]; ok && usage["input_tokens"] == nil {
			usage["input_tokens"] = v
		}
		if v, ok := chat.Usage["output_tokens"]; ok && usage["output_tokens"] == nil {
			usage["output_tokens"] = v
		}
		normalizeUsageDetails(usage)
		responses["usage"] = usage
	}

	result, _ := json.Marshal(responses)
	return result
}

// toolCallArgs 取出已累积的工具调用参数文本。
// 累积用 strings.Builder（旧实现是 call["arguments"].(string) + argDelta，
// 每来一个分片就复制整串，长工具调用是 O(n²)）；归一化后的结果仍以 string 存回同一个键。
func toolCallArgs(call map[string]any) string {
	switch v := call["arguments"].(type) {
	case *strings.Builder:
		return v.String()
	case string:
		return v
	default:
		return ""
	}
}

func emitSSEEvent(w http.ResponseWriter, flusher http.Flusher, event string, data map[string]any) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		log.Printf("Error marshaling SSE event: %v", err)
		return
	}
	io.WriteString(w, "event: "+event+"\ndata: "+string(jsonData)+"\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// ======================== Admin 管理页面 ========================

func reloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	refreshOCSession()
	fetched, err := fetchModels()
	if err == nil && len(fetched) > 0 {
		modelMu.Lock()
		modelsCache = fetched
		modelsLoaded = true
		modelMu.Unlock()
		log.Printf("模型列表已刷新: %d 个模型", len(fetched))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"session": ocSessionID,
		"models":  len(modelsCache),
	})
}

// adminModelsHandler 返回上游全量真实模型列表，供管理面板"实际模型"下拉框使用
func adminModelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	modelMu.RLock()
	loaded, models := modelsLoaded, modelsCache
	modelMu.RUnlock()
	if !loaded || len(models) == 0 {
		fetched, err := fetchModels()
		if err == nil && len(fetched) > 0 {
			modelMu.Lock()
			modelsCache = fetched
			modelsLoaded = true
			models = modelsCache
			modelMu.Unlock()
		}
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"models": ids,
	})
}

func adminConfigHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		configMu.RLock()
		cfg := AppConfig{ModelAlias: modelAlias, ReasoningEffortMap: reasoningEffortMap, OCSessionID: ocInjectedSession, OCSessionRefreshCmd: ocRefreshCmd}
		configMu.RUnlock()
		socks5Mu.RLock()
		cfg.Socks5Proxies = socks5Proxies
		// 面板是「GET 回来原样 POST」的形态：回读必须是磁盘上的持久化值，
		// 否则一次面板保存就会把内存里的 403 回落态钉进 config.json。
		cfg.ActiveSocks5 = persistedActiveSocks5(activeSocks5)
		socks5Mu.RUnlock()
		// 凭据池原样回读：面板没有 key 管理 UI，但运营要能从 /api/config
		// 确认线上池子的规模与策略（这也是本卡的验收判据之一）。
		if kp := currentKeyPool(); kp != nil {
			cfg.KeyPool = kp
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cfg)
	case http.MethodPost:
		// 指针区分「字段省略」与「显式置空」：解到普通值时两者都是零值，
		// 会让面板的清空操作失效；省略时沿用现值（一次旧版面板或脚本回传就会把
		// 出口配置静默清空，服务随即绕过 mihomo 直连出墙）。见 configPostBody。
		var body configPostBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"Invalid JSON"}`, http.StatusBadRequest)
			return
		}
		cfg := body.AppConfig
		body.merge(&cfg)
		normalizeConfig(&cfg)
		if err := saveConfig(configPath, cfg); err != nil {
			http.Error(w, `{"error":"Failed to save config"}`, http.StatusInternalServerError)
			return
		}
		applyConfig(cfg)
		if debugMode {
			log.Printf("Config updated: aliases=%d, effort_map=%d", len(cfg.ModelAlias), len(cfg.ReasoningEffortMap))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func adminStatsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		tokenStatsMu.Lock()
		data, err := json.Marshal(tokenStats)
		tokenStatsMu.Unlock()
		if err != nil {
			http.Error(w, `{"error":"marshal error"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	case http.MethodDelete:
		tokenStatsMu.Lock()
		tokenStats = &TokenStatsData{Models: map[string]*ModelStats{}, Daily: &DailyStats{Date: getToday(), Models: map[string]*ModelStats{}}}
		statsDate = getToday()
		tokenStatsMu.Unlock()
		saveTokenStats()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func adminPageHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(adminHTML))
}

func renderLoginPage(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(adminLoginHTML))
	if msg != "" {
		w.Write([]byte("<script>document.addEventListener('DOMContentLoaded',function(){var m=document.getElementById('login-msg');if(m){m.textContent='" + msg + "';m.style.display='block'}})</script>"))
	}
}

const adminLoginHTML = `<!DOCTYPE html>
<html lang="zh" data-theme="light">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>登录 — OPENCODE TO API</title>
<style>
:root{--bg:#f4f6fa;--surface:#fff;--border:#e2e6ed;--text:#1a1d26;--text-sec:#6a7180;--accent:#6c8aff;--accent-hover:#5a78f0;--radius:12px;--radius-sm:8px;--font:'Noto Sans SC',system-ui,-apple-system,sans-serif;--mono:'JetBrains Mono',Consolas,monospace}
[data-theme="dark"]{--bg:#0c0e14;--surface:#14161e;--border:#252835;--text:#e8eaf0;--text-sec:#8b90a5;--accent:#6c8aff;--accent-hover:#5a78f0}
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:var(--font);background:var(--bg);color:var(--text);font-size:14px;line-height:1.6;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:20px}
body::before{content:'';position:fixed;top:-50%;left:-50%;width:200%;height:200%;background:radial-gradient(ellipse at 30% 20%,rgba(108,138,255,.04) 0%,transparent 50%),radial-gradient(ellipse at 70% 80%,rgba(61,214,140,.03) 0%,transparent 50%);pointer-events:none;z-index:0}
.container{max-width:400px;width:100%;position:relative;z-index:1}
.card{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);padding:36px 32px 32px}
.logo{display:flex;align-items:center;gap:10px;margin-bottom:6px}
.logo-mark{width:36px;height:36px;background:linear-gradient(135deg,var(--accent),#8b6cff);border-radius:10px;display:flex;align-items:center;justify-content:center;font-size:20px;color:#fff;flex-shrink:0}
.logo-text{font-size:20px;font-weight:700;letter-spacing:-.5px;background:linear-gradient(135deg,var(--text),var(--text-sec));-webkit-background-clip:text;-webkit-text-fill-color:transparent}
.logo-sub{font-size:12px;color:var(--text-sec);margin-top:2px}
.subtitle{font-size:13px;color:var(--text-sec);margin-bottom:28px;margin-top:4px}
.field{margin-bottom:16px}
.field label{display:block;font-size:12px;font-weight:500;color:var(--text-sec);margin-bottom:6px;letter-spacing:.3px}
.field input{width:100%;padding:10px 14px;border:1px solid var(--border);border-radius:var(--radius-sm);font-size:14px;font-family:var(--mono);background:var(--surface);color:var(--text);transition:border-color .15s,box-shadow .15s}
.field input:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 3px rgba(108,138,255,.1)}
.msg{display:none;background:rgba(240,96,96,.1);color:#d64545;padding:10px 14px;border-radius:var(--radius-sm);margin-bottom:16px;font-size:13px;text-align:center;border:1px solid rgba(240,96,96,.2)}
[data-theme="dark"] .msg{color:#f06060}
.btn{width:100%;padding:10px;border:none;border-radius:var(--radius-sm);font-size:14px;font-weight:600;cursor:pointer;font-family:var(--font);background:var(--accent);color:#fff;transition:background .15s}
.btn:hover{background:var(--accent-hover)}
.theme-bar{display:flex;justify-content:space-between;align-items:center;margin-bottom:24px}
.theme-toggle{background:transparent;border:1px solid var(--border);border-radius:var(--radius-sm);padding:6px 12px;cursor:pointer;font-size:13px;color:var(--text-sec);font-family:var(--font);transition:all .15s}
.theme-toggle:hover{border-color:var(--accent);color:var(--accent)}
@media(max-width:500px){.card{padding:24px 20px}}
</style>
</head>
<body>
<div class="container">
<div class="card">
<div class="theme-bar">
<div class="logo">
<div class="logo-mark">⌨</div>
<div>
<div class="logo-text">OPENCODE TO API</div>
<div class="logo-sub">管理面板</div>
</div>
</div>
<button class="theme-toggle" onclick="toggleTheme()">☀</button>
</div>
<div class="subtitle">请输入管理密码以继续</div>
<div class="msg" id="login-msg"></div>
<form method="post" action="/login">
<div class="field">
<label for="pwd">密码</label>
<input id="pwd" name="password" type="password" placeholder="输入管理密码" autocomplete="current-password" required>
</div>
<button class="btn" type="submit">登录</button>
</form>
</div>
</div>
<script>
(function(){var t=localStorage.getItem('theme');if(t==='dark'){document.documentElement.setAttribute('data-theme','dark')}})();
function toggleTheme(){var d=document.documentElement;var n=d.getAttribute('data-theme')==='dark'?'light':'dark';if(n==='dark')d.setAttribute('data-theme','dark');else d.removeAttribute('data-theme');localStorage.setItem('theme',n);document.querySelector('.theme-toggle').textContent=n==='dark'?'🌙':'☀'}
</script>
</body>
</html>`

const adminHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>OPENCODE TO API 管理面板</title>
<style>
:root {
  --bg: #f4f6fa;
  --surface: #ffffff;
  --surface-2: #f0f2f7;
  --border: #e2e6ed;
  --border-light: #d0d4df;
  --text: #1a1d26;
  --text-sec: #6a7180;
  --text-ter: #9ca3b0;
  --accent: #6c8aff;
  --accent-dim: rgba(108,138,255,.08);
  --accent-hover: #5a78f0;
  --green: #22a85a;
  --green-dim: rgba(34,168,90,.08);
  --green-hover: #1d9850;
  --orange: #d9600a;
  --orange-dim: rgba(217,96,10,.08);
  --orange-hover: #c45507;
  --red: #dc2626;
  --red-dim: rgba(220,38,38,.08);
  --radius: 12px;
  --radius-sm: 8px;
  --font: 'Noto Sans SC', system-ui, -apple-system, sans-serif;
  --mono: 'JetBrains Mono', Consolas, monospace;
  --glow-a: rgba(108,138,255,.03);
  --glow-b: rgba(61,214,140,.02);
  --stats-total-bg: #f0f2f7;
}
[data-theme="dark"] {
  --bg: #0c0e14;
  --surface: #14161e;
  --surface-2: #1a1d27;
  --border: #252835;
  --border-light: #2e3142;
  --text: #e8eaf0;
  --text-sec: #8b90a5;
  --text-ter: #5c6080;
  --accent: #6c8aff;
  --accent-dim: rgba(108,138,255,.12);
  --accent-hover: #5a78f0;
  --green: #3dd68c;
  --green-dim: rgba(61,214,140,.12);
  --green-hover: #30c47a;
  --orange: #f0a050;
  --orange-dim: rgba(240,160,80,.12);
  --orange-hover: #e09040;
  --red: #f06060;
  --red-dim: rgba(240,96,96,.12);
  --glow-a: rgba(108,138,255,.04);
  --glow-b: rgba(61,214,140,.03);
  --stats-total-bg: var(--surface-2);
}
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:var(--font);background:var(--bg);color:var(--text);font-size:14px;line-height:1.6;min-height:100vh}
body::before{content:'';position:fixed;top:-50%;left:-50%;width:200%;height:200%;background:radial-gradient(ellipse at 30% 20%,var(--glow-a) 0%,transparent 50%),radial-gradient(ellipse at 70% 80%,var(--glow-b) 0%,transparent 50%);pointer-events:none;z-index:0}
.container{max-width:1020px;margin:0 auto;padding:32px 24px;position:relative;z-index:1}
header{display:flex;align-items:flex-end;gap:16px;margin-bottom:28px;padding-bottom:20px;border-bottom:1px solid var(--border);justify-content:space-between}
.head-actions{display:flex;align-items:center;gap:8px;flex-wrap:wrap}
.head-btn{font-size:13px;padding:7px 11px;white-space:nowrap}
.logo{display:flex;align-items:center;gap:10px}
.logo-mark{width:36px;height:36px;background:linear-gradient(135deg,var(--accent),#8b6cff);border-radius:10px;display:flex;align-items:center;justify-content:center;font-size:20px;color:#fff;flex-shrink:0}
.logo-text{font-size:22px;font-weight:700;letter-spacing:-.5px;background:linear-gradient(135deg,var(--text),var(--text-sec));-webkit-background-clip:text;-webkit-text-fill-color:transparent}
.logo-sub{font-size:12.5px;color:var(--text-ter);margin-bottom:2px}
.card{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);padding:22px 24px;transition:border-color .2s}
.card:hover{border-color:var(--border-light)}
.card h2{font-size:13px;font-weight:600;margin-bottom:16px;letter-spacing:.2px;display:flex;align-items:center;gap:8px;color:var(--text-sec);text-transform:uppercase}
.card h2 .dot{width:6px;height:6px;border-radius:50%;flex-shrink:0}
.config-grid{display:grid;grid-template-columns:2fr 3fr;gap:16px;margin-top:16px}
.config-grid>*{min-width:0}
.config-grid .card{margin-bottom:0}
.full-row{grid-column:1/-1}
.form-group{margin-bottom:14px}
.form-group:last-child{margin-bottom:0}
.form-group label{display:block;font-size:11.5px;font-weight:500;color:var(--text-ter);margin-bottom:5px;letter-spacing:.4px;text-transform:uppercase}
.form-group input[type="text"],.form-group input[type="url"],.form-group input[type="password"],.form-group textarea,.form-group select,.m-select,.m-input{width:100%;padding:8px 12px;border:1px solid var(--border);border-radius:var(--radius-sm);font-size:13px;font-family:var(--mono);background:var(--surface-2);color:var(--text);transition:border-color .15s,box-shadow .15s}
.form-group input:focus,.form-group textarea:focus,.form-group select:focus,.m-select:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 3px var(--accent-dim)}
.form-group .hint{font-size:11px;color:var(--text-ter);margin-top:4px;line-height:1.4}
.actions{display:flex;gap:8px;margin-top:14px;flex-wrap:wrap}
.btn{padding:8px 16px;border-radius:var(--radius-sm);font-size:12.5px;font-weight:500;cursor:pointer;border:none;transition:all .15s;font-family:var(--font);white-space:nowrap}
.btn-primary{background:var(--accent-dim);color:var(--accent)}
.btn-primary:hover{background:var(--accent);color:#fff}
.btn-default{background:var(--surface-2);color:var(--text-sec);border:1px solid var(--border)}
.btn-default:hover{border-color:var(--border-light);color:var(--text)}
.btn-success{background:var(--green-dim);color:var(--green)}
.btn-success:hover{background:var(--green);color:#fff}
.btn-warning{background:var(--orange-dim);color:var(--orange)}
.btn-warning:hover{background:var(--orange);color:#fff}
.btn-danger{background:var(--red-dim);color:var(--red)}
.btn-danger:hover{background:var(--red);color:#fff}
.tbl-wrap{margin-bottom:12px;overflow-x:auto;-webkit-overflow-scrolling:touch}
#statsContent{overflow-x:auto;-webkit-overflow-scrolling:touch}
.tbl{width:100%;border-collapse:collapse;font-size:12.5px}
.tbl th{text-align:left;font-weight:500;color:var(--text-ter);padding:8px 10px;border-bottom:1px solid var(--border);font-size:11px;letter-spacing:.4px;text-transform:uppercase;white-space:nowrap}
.tbl td{padding:7px 10px;border-bottom:1px solid var(--border)}
.tbl tr:last-child td{border-bottom:none}
.tbl input{width:100%;padding:6px 10px;border:1px solid var(--border);border-radius:6px;font-size:12.5px;font-family:var(--mono);background:var(--surface-2);color:var(--text);transition:border-color .15s,box-shadow .15s}
.tbl input:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 2px var(--accent-dim)}
.tbl .m-select{padding:6px 10px;font-size:12.5px}
.tbl th:last-child{width:52px}
.tbl td:last-child{white-space:nowrap;text-align:center}
#statsTable th:last-child{width:auto}
#statsTable td:last-child{text-align:left;white-space:nowrap}
.tbl .btn{padding:4px 10px;font-size:11px;white-space:nowrap}
#statsTable td:first-child{font-weight:500;color:var(--text)}
#statsTable td:not(:first-child){font-family:var(--mono);color:var(--text-sec);text-align:left}
#statsTable tbody tr:hover{background:var(--surface-2)}
#statsTable thead+tbody tr:last-child td{font-weight:600;color:var(--text);background:var(--stats-total-bg);border-top:1px solid var(--border-light)}
#dailyTable th:last-child{width:auto}
#dailyTable td:last-child{text-align:left;white-space:nowrap}
#dailyTable td:first-child{font-weight:500;color:var(--text)}
#dailyTable td:not(:first-child){font-family:var(--mono);color:var(--text-sec);text-align:left}
#dailyTable tbody tr:hover{background:var(--surface-2)}
#dailyTable thead+tbody tr:last-child td{font-weight:600;color:var(--text);background:var(--stats-total-bg);border-top:1px solid var(--border-light)}
.stats-header{display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:8px;margin-bottom:12px}
.stats-header .btns{display:flex;gap:6px;align-items:center}
#toast{position:fixed;top:20px;right:20px;padding:12px 20px;border-radius:var(--radius-sm);font-size:13px;font-weight:500;color:#fff;opacity:0;transition:opacity .25s,transform .25s;z-index:999;transform:translateY(-8px);pointer-events:none;backdrop-filter:blur(8px)}
#toast.success{background:rgba(61,214,140,.85)}
#toast.error{background:rgba(240,96,96,.85)}
#toast.show{opacity:1;transform:translateY(0)}
.empty-hint{color:var(--text-ter);font-size:13px;padding:28px;text-align:center}
.theme-toggle{background:var(--surface-2);border:1px solid var(--border);border-radius:var(--radius-sm);padding:6px 12px;cursor:pointer;font-size:18px;display:flex;align-items:center;justify-content:center;transition:all .15s;color:var(--text-sec);flex-shrink:0;line-height:1}
.theme-toggle:hover{border-color:var(--border-light);color:var(--text)}
.panel-header{display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:10px;margin-bottom:16px}
.panel-header h2{margin-bottom:0}
.panel-header .btns{display:flex;align-items:center;gap:8px;flex-wrap:wrap}
.advanced-settings{margin-top:18px;border:1px solid var(--border);border-radius:var(--radius-sm);background:var(--surface-2);overflow:hidden;transition:border-color .15s}
.advanced-settings:hover{border-color:var(--border-light)}
.advanced-settings summary{display:flex;align-items:center;gap:9px;padding:11px 14px;cursor:pointer;list-style:none;color:var(--text-sec);font-size:12.5px;font-weight:600;user-select:none}
.advanced-settings summary::-webkit-details-marker{display:none}
.advanced-settings summary::before{content:'›';font-size:18px;line-height:1;color:var(--text-ter);transition:transform .15s}
.advanced-settings[open] summary::before{transform:rotate(90deg)}
.advanced-settings[open] summary{border-bottom:1px solid var(--border)}
.advanced-summary-count{margin-left:auto;color:var(--text-ter);font-size:11px;font-weight:400}
.advanced-content{padding:14px}
.advanced-hint{font-size:11px;color:var(--text-ter);margin-bottom:10px}
.effort-label-row,.effort-row{display:grid;grid-template-columns:minmax(120px,1fr) 28px minmax(120px,1fr) auto;gap:8px;align-items:center}
.effort-label-row{padding:0 1px 5px;color:var(--text-ter);font-size:10.5px;letter-spacing:.3px;text-transform:uppercase}
.effort-row{margin-bottom:8px}
.effort-row input{width:100%;padding:7px 10px;border:1px solid var(--border);border-radius:6px;font-size:12.5px;font-family:var(--mono);background:var(--surface);color:var(--text);transition:border-color .15s,box-shadow .15s}
.effort-row input:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 2px var(--accent-dim)}
.effort-arrow{text-align:center;color:var(--text-ter);font-family:var(--mono)}
.effort-row .btn{padding:6px 10px;font-size:11px}
.effort-empty{padding:16px 8px;color:var(--text-ter);font-size:12px;text-align:center;border:1px dashed var(--border);border-radius:6px}
.effort-actions{margin-top:10px}
.ver-chip{display:flex;align-items:center;gap:8px;padding:6px 12px;background:var(--surface-2);border:1px solid var(--border);border-radius:var(--radius-sm);font-size:12px;color:var(--text-sec);white-space:nowrap;flex-shrink:0}
.ver-chip b{font-family:var(--mono);font-weight:600;color:var(--text)}
.ver-chip a{color:var(--orange);font-weight:500;text-decoration:none}
.ver-chip a:hover{text-decoration:underline}
.ver-chip .up{color:var(--orange);font-weight:600}
.ver-chip .ok{color:var(--green);font-weight:500}
.ver-chip .fail{color:var(--text-ter)}
/* —— 手机视口适配（放最后：同特异度下靠后的规则才压得住上面那些基础规则）——
   竖屏走 ≤700px（实测 OPPO PKG110 = 361px CSS 宽）；横屏宽 794px 过了宽度断点、
   但可视高度只剩 ~397px，故并入 max-height:520px 一档，否则横屏退回桌面字号与触控尺寸 */
@media(max-width:700px),(max-height:520px) and (orientation:landscape){.config-grid{grid-template-columns:1fr}.container{padding:16px 12px}header{flex-direction:column;align-items:flex-start;gap:8px}.head-actions{width:100%}.ver-chip{flex:0 0 100%;white-space:normal;flex-wrap:wrap}.effort-label-row,.effort-row{grid-template-columns:minmax(90px,1fr) 22px minmax(90px,1fr) auto}.stats-header .btns,.panel-header .btns{flex-wrap:wrap}#toast{left:12px;right:12px;top:12px}}
/* 字号下限 14px；输入框 16px（低于 16 时 iOS/部分安卓浏览器聚焦会自动放大页面）；可点控件 ≥44px */
@media(max-width:700px),(max-height:520px) and (orientation:landscape){.tbl th,.tbl td,.form-group label,.form-group .hint,.effort-label-row,.advanced-settings summary,.advanced-hint,.advanced-summary-count,.empty-hint,.ver-chip{font-size:14px}.form-group input,.form-group textarea,.form-group select,.m-select,.m-input,.tbl input,.tbl .m-select,.effort-row input{font-size:16px;min-height:44px}.btn,.tbl .btn,.effort-row .btn,.theme-toggle,.head-btn,.advanced-settings summary{min-height:44px}.btn,.tbl .btn,.effort-row .btn,.head-btn{font-size:14px;padding:10px 14px}.tbl input,.tbl select,.tbl .m-select{min-width:120px}#statsTable td:first-child,#dailyTable td:first-child{min-width:110px}}
/* 横屏专属：压扁页头。竖屏那条两行页头留在 ~397px 高的屏上会吃掉小半屏 */
@media(max-height:520px) and (orientation:landscape){header{flex-direction:row;align-items:center;gap:8px;margin-bottom:14px;padding-bottom:10px}.head-actions{width:auto;flex-wrap:nowrap;gap:6px;justify-content:flex-end}.ver-chip{flex:0 1 auto;min-width:0;max-width:230px}.logo-text{font-size:15px;white-space:nowrap}.logo-sub{display:none}.container{padding:12px 16px}#toast{top:8px;right:8px}}
</style>
</head>
<body>
<div class="container">
<header>
<div class="logo">
<div class="logo-mark">⌨</div>
<div>
<div class="logo-text">OPENCODE TO API</div>
<div class="logo-sub">OpenCode 免费 API → 兼容格式代理</div>
</div>
</div>
<div class="head-actions">
<span class="ver-chip"><b id="verCur" title="当前版本">…</b><span id="verStatus"></span></span>
<button class="theme-toggle head-btn" id="verCheckBtn" onclick="checkUpdate()" title="检查更新">检查更新</button>
<button class="theme-toggle" onclick="toggleTheme()" title="切换主题">☀</button>
<form method="post" action="/logout" style="margin:0"><button class="theme-toggle head-btn" type="submit" title="退出登录">退出</button></form>
</div>
</header>

<div class="card">
<div class="stats-header">
<h2><span class="dot" style="background:var(--green)"></span>Token 统计</h2>
<div class="btns">
<button class="btn btn-success" onclick="reloadConfig()">刷新</button>
<button class="btn btn-danger" onclick="resetStats()">清空统计</button>
<span id="resetStatus" style="font-size:11px;color:var(--text-ter)"></span>
</div>
</div>
<div id="statsContent" style="font-size:12.5px">
<div class="empty-hint">加载中...</div>
</div>
</div>

<div class="config-grid">
<div class="card full-row">
<div class="panel-header">
<h2><span class="dot" style="background:var(--accent)"></span>模型映射</h2>
<div class="btns">
<button class="btn btn-primary" onclick="addAliasRow()">添加别名</button>
<button class="btn btn-success" onclick="saveConfig()">保存全部</button>
</div>
</div>
<div class="tbl-wrap">
<table class="tbl" id="aliasTable">
<thead><tr><th style="width:22%">别名（请求名）</th><th style="width:28%">实际模型（无图）</th><th style="width:28%">多模态模型（带图）</th><th style="width:22%"></th></tr></thead>
<tbody></tbody>
</table>
</div>
<details class="advanced-settings" id="reasoningEffortDetails">
<summary><span>高级设置 · 推理力度映射</span><span class="advanced-summary-count" id="effortSummary">未配置 · 原值透传</span></summary>
<div class="advanced-content">
<div class="advanced-hint">将客户端传入的 reasoning_effort 映射为上游支持的值；未配置的值保持原样。</div>
<div class="effort-label-row"><span>请求值</span><span></span><span>上游值</span><span></span></div>
<div id="effortList"></div>
<div class="effort-actions"><button class="btn btn-primary" onclick="addEffortRow()">添加映射</button></div>
</div>
</details>
</div>

<div class="card full-row">
<h2><span class="dot" style="background:var(--accent)"></span>出口代理</h2>
<div class="tbl-wrap">
<table class="tbl" id="socks5Table">
<thead><tr><th style="width:13%">名称</th><th style="width:11%">类型</th><th style="width:20%">地址 / 节点</th><th style="width:10%">用户名</th><th style="width:10%">密码</th><th style="width:12%">Auth</th><th style="width:13%">Connect Host</th><th style="width:11%"></th></tr></thead>
<tbody></tbody>
</table>
</div>
<div class="form-group">
<label>启用代理</label>
<select id="activeSocks5" class="m-select">
<option value="">直连（不使用代理）</option>
</select>
</div>
<div class="form-group">
<label>OpenCode 会话 ID</label>
<input id="ocSessionId" class="m-input" placeholder="ses_...（OpenCode 客户端真实创建的会话；留空则服务返回 503）">
</div>
<div class="form-group">
<label>会话自动续命命令</label>
<input id="ocSessionRefreshCmd" class="m-input" placeholder="留空则不自动续；填命令后代理定期执行并解析输出里的 ses_*">
</div>
<div class="actions">
<button class="btn btn-primary" onclick="addSocks5Row()">添加代理</button>
<button class="btn btn-primary" onclick="addBaiduRow()">添加百度隧道</button>
<button class="btn btn-success" onclick="saveConfig()">保存全部</button>
</div>
</div>
</div>
</div>
<div id="toast"></div>
<script>
let aliasData={},effortData={},modelList=[],socks5Data=[];
function toggleTheme(){const d=document.documentElement;const cur=d.getAttribute('data-theme');const next=cur==='dark'?null:'dark';if(next)d.setAttribute('data-theme',next);else d.removeAttribute('data-theme');localStorage.setItem('theme',next||'light');document.querySelector('.theme-toggle').textContent=next==='dark'?'🌙':'☀'}
(function(){const t=localStorage.getItem('theme');if(t==='dark'){document.documentElement.setAttribute('data-theme','dark');document.addEventListener('DOMContentLoaded',()=>{const b=document.querySelector('.theme-toggle');if(b)b.textContent='🌙'})}})();
function reloadConfig(){const sy=window.scrollY;fetch('/api/reload',{method:'POST'}).then(r=>r.json()).then(d=>{showToast('会话已刷新，模型 '+d.models+' 个','success')}).catch(()=>{}).finally(()=>{loadConfig();loadStats();setTimeout(()=>window.scrollTo(0,sy),100)})}
function normalizeAliasData(){const next={};Object.keys(aliasData||{}).forEach(k=>{const raw=aliasData[k];if(typeof raw==='object'&&raw){next[k]={target_model:raw.target_model||k,multimodal_model:raw.multimodal_model||''}}else{next[k]={target_model:typeof raw==='string'&&raw?raw:k,multimodal_model:''}}});aliasData=next}
async function loadConfig(){const sy=window.scrollY;try{const r=await fetch('/api/config');if(!r.ok)throw new Error(await r.text());const cfg=await r.json();aliasData=cfg.model_alias||{};normalizeAliasData();effortData=cfg.reasoning_effort_map||{};socks5Data=cfg.socks5_proxies||[];renderAliasTable();renderEffortTable();renderSocks5Table();document.getElementById('activeSocks5').value=cfg.active_socks5||'';document.getElementById('ocSessionId').value=cfg.oc_session_id||'';document.getElementById('ocSessionRefreshCmd').value=cfg.oc_session_refresh_cmd||'';setTimeout(()=>window.scrollTo(0,sy),0);try{const mr=await fetch('/api/models');const md=await mr.json();modelList=(md.models||[]).sort();renderAliasTable()}catch(_){}}catch(e){showToast('失败: '+e.message,'error')}}
function renderAliasTable(){const tb=document.querySelector('#aliasTable tbody');const ks=Object.keys(aliasData);if(!ks.length){tb.innerHTML='<tr><td colspan="4" class="empty-hint">暂无别名配置</td></tr>';return}tb.innerHTML=ks.map(k=>{const entry=aliasData[k]||{target_model:k};return '<tr><td><input value="'+esc(k)+'" data-field="key"></td><td>'+modelSelectHtml(entry.target_model||k,'val')+'</td><td>'+modelSelectHtml(entry.multimodal_model||'','mm')+'</td><td><button class="btn btn-danger" onclick="delAlias(this)">删除</button></td></tr>'}).join('')}
function modelSelectHtml(selected,field){let h='<select data-field="'+(field||'val')+'" class="m-select">';h+='<option value="">-- 选择模型 --</option>';let found=!selected;for(const m of modelList){if(selected===m)found=true;h+='<option value="'+esc(m)+'"'+(selected===m?' selected':'')+'>'+esc(m)+'</option>'}if(selected&&!found)h+='<option value="'+esc(selected)+'" selected>'+esc(selected)+' (自定义)</option>';h+='</select>';return h}
function addAliasRow(){const tb=document.querySelector('#aliasTable tbody');if(tb.querySelector('.empty-hint'))tb.innerHTML='';tb.insertAdjacentHTML('beforeend','<tr><td><input value="" placeholder="例如: deepseek-reasoner" data-field="key"></td><td>'+modelSelectHtml('','val')+'</td><td>'+modelSelectHtml('','mm')+'</td><td><button class="btn btn-danger" onclick="delAlias(this)">删除</button></td></tr>')}
function delAlias(btn){const row=btn.closest('tr');const ki=row.querySelector('[data-field="key"]');if(ki&&ki.value&&aliasData[ki.value])delete aliasData[ki.value];row.remove();if(!Object.keys(aliasData).length)document.querySelector('#aliasTable tbody').innerHTML='<tr><td colspan="4" class="empty-hint">暂无别名配置</td></tr>'}
function collectAliases(){const r={};document.querySelectorAll('#aliasTable tbody tr').forEach(tr=>{const k=tr.querySelector('[data-field="key"]'),v=tr.querySelector('[data-field="val"]'),mm=tr.querySelector('[data-field="mm"]');if(k&&k.value.trim()){const key=k.value.trim();const target=v&&v.value.trim()?v.value.trim():key;r[key]={target_model:target,multimodal_model:mm&&mm.value.trim()?mm.value.trim():''}}});aliasData=r;return r}

function effortRowHtml(key,val){return '<div class="effort-row"><input value="'+esc(key||'')+'" data-field="key" placeholder="例如: low"><span class="effort-arrow">→</span><input value="'+esc(val||'')+'" data-field="val" placeholder="例如: high"><button class="btn btn-danger" onclick="delEffort(this)">删除</button></div>'}
function updateEffortSummary(){const el=document.getElementById('effortSummary');if(!el)return;const count=Object.keys(effortData||{}).length;el.textContent=count?count+' 条映射':'未配置 · 原值透传'}
function renderEffortTable(){const list=document.getElementById('effortList');const ks=Object.keys(effortData);list.innerHTML=ks.length?ks.map(k=>effortRowHtml(k,effortData[k])).join(''):'<div class="effort-empty">未配置，reasoning_effort 将按原值透传</div>';updateEffortSummary()}
function addEffortRow(){collectEfforts();const details=document.getElementById('reasoningEffortDetails');details.open=true;const list=document.getElementById('effortList');const empty=list.querySelector('.effort-empty');if(empty)empty.remove();list.insertAdjacentHTML('beforeend',effortRowHtml('',''));const rows=list.querySelectorAll('.effort-row');const input=rows.length?rows[rows.length-1].querySelector('[data-field="key"]'):null;if(input)input.focus()}
function delEffort(btn){const row=btn.closest('.effort-row');if(row)row.remove();collectEfforts();renderEffortTable()}
function collectEfforts(){const r={};document.querySelectorAll('#effortList .effort-row').forEach(row=>{const k=row.querySelector('[data-field="key"]'),v=row.querySelector('[data-field="val"]');if(k&&k.value.trim())r[k.value.trim()]=v?v.value.trim():''});effortData=r;updateEffortSummary();return r}
function renderSocks5Table(){const tb=document.querySelector('#socks5Table tbody');if(!socks5Data.length){tb.innerHTML='<tr><td colspan="8" class="empty-hint">暂无代理配置</td></tr>';renderSocks5Select();return}tb.innerHTML=socks5Data.map((p,i)=>'<tr><td><input value="'+esc(p.name||'')+'" data-field="name"></td><td><select data-field="kind"><option value="">SOCKS5</option><option value="baidu_connect"'+(p.kind==='baidu_connect'?' selected':'')+'>百度隧道</option></select></td><td><input value="'+esc(p.addr||'')+'" data-field="addr" placeholder="127.0.0.1:1080 或 baidu://cloudnproxy.baidu.com:443"></td><td><input value="'+esc(p.username||'')+'" data-field="username"></td><td><input value="'+esc(p.password||'')+'" data-field="password" type="password"></td><td><input value="'+esc(p.auth||'')+'" data-field="auth" type="password" placeholder="X-T5-Auth"></td><td><input value="'+esc(p.connect_host||'')+'" data-field="connect_host" placeholder="sptest.baidu.com"></td><td><button class="btn btn-danger" onclick="delSocks5('+i+')">删除</button></td></tr>').join('');renderSocks5Select()}
function addSocks5Row(){const tb=document.querySelector('#socks5Table tbody');if(tb.querySelector('.empty-hint'))tb.innerHTML='';socks5Data.push({addr:'',name:''});renderSocks5Table()}
function addBaiduRow(){const tb=document.querySelector('#socks5Table tbody');if(tb.querySelector('.empty-hint'))tb.innerHTML='';socks5Data.push({addr:'baidu://cloudnproxy.baidu.com:443',name:'baidu',kind:'baidu_connect',connect_host:'sptest.baidu.com'});renderSocks5Table()}
function delSocks5(i){socks5Data.splice(i,1);renderSocks5Table()}
function collectSocks5(){const r=[];document.querySelectorAll('#socks5Table tbody tr').forEach(tr=>{const a=tr.querySelector('[data-field="addr"]');if(a&&a.value.trim()){const v=f=>(tr.querySelector('[data-field="'+f+'"]')||{}).value?.trim()||'';r.push({addr:a.value.trim(),name:v('name'),username:v('username'),password:v('password'),kind:v('kind'),auth:v('auth'),connect_host:v('connect_host')})}});socks5Data=r;return r}
function renderSocks5Select(){const sel=document.getElementById('activeSocks5');const cur=sel.value;sel.innerHTML='<option value="">直连（不使用代理）</option>';socks5Data.forEach(p=>{if(p.addr){const label=p.name?p.name+' ('+p.addr+')':p.addr;const opt=document.createElement('option');opt.value=p.addr;opt.textContent=label;sel.appendChild(opt)}});if(socks5Data.length>=1){const opt=document.createElement('option');opt.value='__rate_limit_switch__';opt.textContent='限流切换（429 后切换，含直连）';sel.appendChild(opt);const opt2=document.createElement('option');opt2.value='__rate_limit_switch_no_direct__';opt2.textContent='限流切换（429 后切换，不含直连）';sel.appendChild(opt2)}if(socks5Data.length>=2){const opt=document.createElement('option');opt.value='__round_robin__';opt.textContent='轮询（每次请求切换）';sel.appendChild(opt)}sel.value=cur;if(!sel.value)sel.value='';}
async function saveConfig(){collectAliases();collectEfforts();collectSocks5();const cfg={model_alias:aliasData,reasoning_effort_map:effortData,socks5_proxies:socks5Data,active_socks5:document.getElementById('activeSocks5').value,oc_session_id:document.getElementById('ocSessionId').value.trim(),oc_session_refresh_cmd:document.getElementById('ocSessionRefreshCmd').value.trim()};try{const r=await fetch('/api/config',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(cfg)});if(!r.ok)throw new Error(await r.text());showToast('配置已保存','success');loadConfig()}catch(e){showToast('保存失败: '+e.message,'error')}}
function esc(s){const d=document.createElement('div');d.textContent=s;return d.innerHTML}
function showToast(msg,t){const e=document.getElementById('toast');e.textContent=msg;e.className=t+' show';clearTimeout(e._tid);e._tid=setTimeout(()=>e.classList.remove('show'),2500)}
async function resetStats(){if(!confirm('确认清空所有 Token 统计？\n此操作不可撤销。'))return;const s=document.getElementById('resetStatus');s.textContent='清空中...';try{const r=await fetch('/api/stats',{method:'DELETE'});if(!r.ok)throw new Error(await r.text());document.getElementById('statsContent').innerHTML='<div class="empty-hint">暂无数据</div>';s.textContent='已清空';setTimeout(()=>s.textContent='',2000)}catch(e){s.textContent='失败: '+e.message}}
async function loadStats(){try{const r=await fetch('/api/stats');const d=await r.json();const ms=d.models||{};const ks=Object.keys(ms);const dm=d.daily?d.daily.models||{}:{};const dk=Object.keys(dm);let h='';if(d.daily&&d.daily.date){h+='<div style="margin-bottom:16px;padding:10px 14px;background:var(--accent);color:#fff;border-radius:8px;font-size:14px">📊 今日统计 ('+esc(d.daily.date)+')：请求 '+fmt(d.daily.total_requests)+' 次</div>'}if(dk.length>0){h+='<h3 style="font-size:14px;font-weight:600;margin:0 0 8px">今日模型用量</h3><table class="tbl" id="dailyTable"><thead><tr><th>模型</th><th>请求数</th><th>输入 Token</th><th>输出 Token</th><th>总计 Token</th></tr></thead><tbody>';let dr=0,dp=0,dc=0,dt=0;for(const k of dk){const m=dm[k];if(!m)continue;h+='<tr><td>'+esc(k)+'</td><td>'+fmt(m.request_count)+'</td><td>'+fmt(m.prompt_tokens)+'</td><td>'+fmt(m.completion_tokens)+'</td><td>'+fmt(m.total_tokens)+'</td></tr>';dr+=m.request_count;dp+=m.prompt_tokens;dc+=m.completion_tokens;dt+=m.total_tokens}h+='<tr style="font-weight:600"><td>今日合计</td><td>'+fmt(dr)+'</td><td>'+fmt(dp)+'</td><td>'+fmt(dc)+'</td><td>'+fmt(dt)+'</td></tr>';h+='</tbody></table><hr style="border:none;border-top:1px solid var(--border);margin:20px 0">'}h+='<h3 style="font-size:14px;font-weight:600;margin:0 0 8px">累计统计</h3><table class="tbl" id="statsTable"><thead><tr><th>模型</th><th>请求数</th><th>输入 Token</th><th>输出 Token</th><th>总计 Token</th></tr></thead><tbody>';if(!ks.length){h+='<tr><td colspan="5" class="empty-hint">暂无数据</td></tr>'}else{let tr=0,pt=0,ct=0,tt=0;for(const k of ks){const m=ms[k];h+='<tr><td>'+esc(k)+'</td><td>'+fmt(m.request_count)+'</td><td>'+fmt(m.prompt_tokens)+'</td><td>'+fmt(m.completion_tokens)+'</td><td>'+fmt(m.total_tokens)+'</td></tr>';tr+=m.request_count;pt+=m.prompt_tokens;ct+=m.completion_tokens;tt+=m.total_tokens}h+='<tr style="font-weight:600"><td>累计总计</td><td>'+fmt(tr)+'</td><td>'+fmt(pt)+'</td><td>'+fmt(ct)+'</td><td>'+fmt(tt)+'</td></tr>'}h+='</tbody></table>';document.getElementById('statsContent').innerHTML=h}catch(e){document.getElementById('statsContent').innerHTML='<div class="empty-hint">加载失败</div>'}}
let verBusy=false;
function renderVersion(d){const cur=document.getElementById('verCur'),st=document.getElementById('verStatus');const v=d.version||'dev';cur.textContent=v;cur.title=v==='dev'?'本地构建，未注入版本号':'当前版本 '+v;st.textContent='';st.className='';if(d.error){st.textContent='检查失败';st.className='fail';return}if(d.has_update){st.className='up';st.textContent='有新版本 '+d.latest+' ';const a=document.createElement('a');a.href=d.release_url||'https://github.com/callacat/opencode2api-lite/releases/latest';a.target='_blank';a.rel='noopener';a.textContent='查看';st.appendChild(a);return}if(v==='dev'){st.textContent='未注入版本';st.className='fail';return}st.textContent='已是最新';st.className='ok'}
async function checkUpdate(silent){if(verBusy)return;verBusy=true;const b=document.getElementById('verCheckBtn'),label=b.textContent;b.disabled=true;b.textContent='检查中…';try{const r=await fetch('/api/version');const d=await r.json();renderVersion(d);if(silent)return;if(d.error)showToast('检查更新失败','error');else if(d.has_update)showToast('发现新版本 '+d.latest,'error');else showToast('已是最新','success')}catch(e){const st=document.getElementById('verStatus');st.textContent='检查失败';st.className='fail';if(!silent)showToast('检查更新失败','error')}finally{b.disabled=false;b.textContent=label;verBusy=false}}
function fmt(n){return n.toString().replace(/\B(?=(\d{3})+(?!\d))/g,',')}window.onload=function(){loadConfig();loadStats();checkUpdate(true)};setInterval(loadStats,5000);document.addEventListener('visibilitychange',function(){if(!document.hidden){loadStats()}});
</script>
</body>
</html>`

// ======================== Main ========================

func initializeOpenCode() {
	initOCSession()
	models, err := fetchModels()
	if err != nil {
		log.Printf("警告: 无法获取模型列表: %v", err)
		return
	}
	modelMu.Lock()
	modelsCache = models
	modelsLoaded = true
	modelMu.Unlock()
	log.Printf("已异步加载 %d 个模型", len(models))
}

// version 由构建时注入（-ldflags "-X main.version=..."，取 release tag），
// 未注入时（本地 go build）为 dev。版本单一来源 = git tag。
var version = "dev"

// ======================== 版本自检 ========================

const releaseRepo = "callacat/opencode2api-lite"

// releaseCheckTTL 是 GitHub 查询结果（成功与失败同用）的缓存时长。
// api.github.com 未认证限 60 次/小时/IP，面板每开一次都打一次会很快打满，
// 故成功和失败都缓存：最坏 2 次/小时。
var releaseCheckTTL = 30 * time.Minute

type latestRelease struct {
	Tag string `json:"tag_name"`
	URL string `json:"html_url"`
}

// fetchLatestRelease 是真正出站的那一步，测试里替换掉即可离线验证缓存行为。
var fetchLatestRelease = func(ctx context.Context) (latestRelease, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/"+releaseRepo+"/releases/latest", nil)
	if err != nil {
		return latestRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	client, _, err := getHTTPClient(ctx)
	if err != nil {
		return latestRelease{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return latestRelease{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return latestRelease{}, fmt.Errorf("github 返回 %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return latestRelease{}, err
	}
	var rel latestRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return latestRelease{}, err
	}
	if rel.Tag == "" {
		return latestRelease{}, errors.New("github 响应缺少 tag_name")
	}
	return rel, nil
}

var (
	releaseMu     sync.Mutex
	releaseCache  latestRelease
	releaseCached bool
	releaseAt     time.Time
	releaseErr    error
)

func cachedLatestRelease(ctx context.Context) (latestRelease, time.Time, error) {
	releaseMu.Lock()
	defer releaseMu.Unlock()
	if releaseCached && time.Since(releaseAt) < releaseCheckTTL {
		return releaseCache, releaseAt, releaseErr
	}
	rel, err := fetchLatestRelease(ctx)
	releaseAt, releaseErr = time.Now(), err
	if err == nil {
		releaseCache = rel
		log.Printf("检查更新: GitHub releases/latest = %s", rel.Tag)
	} else {
		log.Printf("检查更新: 查询 GitHub 失败（%v），%s 内不再重试", err, releaseCheckTTL)
	}
	releaseCached = true
	return releaseCache, releaseAt, err
}

// hasNewerVersion 判断 latest 是否比 current 新。
// 两侧都先去掉 tag 前缀 "v"：versionNumbers 逐段只吃数字字符，
// "v1.0.4" 首段非数字即 break 会被解析成 [0,0,4]，与 "1.0.4" = [1,0,4] 不等，
// 不归一化会把同版本误报成「有更新」。
func hasNewerVersion(current, latest string) bool {
	current = strings.TrimPrefix(strings.TrimSpace(current), "v")
	latest = strings.TrimPrefix(strings.TrimSpace(latest), "v")
	if current == "" || latest == "" || current == "dev" {
		return false
	}
	return compareVersion(latest, current) > 0
}

type versionStatus struct {
	Version    string `json:"version"`
	Latest     string `json:"latest,omitempty"`
	HasUpdate  bool   `json:"has_update"`
	ReleaseURL string `json:"release_url,omitempty"`
	CheckedAt  string `json:"checked_at,omitempty"`
	Error      string `json:"error,omitempty"`
}

func checkLatestVersion(ctx context.Context) versionStatus {
	st := versionStatus{Version: strings.TrimSpace(version)}
	rel, at, err := cachedLatestRelease(ctx)
	if err != nil {
		st.Error = "检查失败"
		return st
	}
	st.Latest = rel.Tag
	st.ReleaseURL = rel.URL
	st.CheckedAt = at.UTC().Format(time.RFC3339)
	st.HasUpdate = hasNewerVersion(st.Version, rel.Tag)
	return st
}

// adminVersionHandler 供面板显示当前版本与是否有新版本。
// 出站失败一律降级为 error 字段，不阻塞、不影响其它端点。
func adminVersionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(checkLatestVersion(r.Context()))
}

// healthHandler 是存活探针：只写 200，不碰上游、不取任何全局锁。
// 提取成具名函数是为了让回归测试跑到真实代码路径，而不是复制一份 handler
// ——CMP-119 要求「上游黑洞时 /health 仍须 200」这条判据可自动跑。
func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func main() {
	configureTimeLocal(os.Getenv("TZ"))
	flag.StringVar(&port, "port", "8000", "服务端口")
	flag.StringVar(&listenAddr, "listen", "", "监听地址（留空 = 绑全网卡，与历史行为一致；写 127.0.0.1 只绑回环。写非回环地址必须同时设 -password）")
	flag.StringVar(&configPath, "config", "config.json", "配置文件路径")
	flag.StringVar(&adminPassword, "password", "", "管理面板密码（留空则不启用登录验证）")
	flag.BoolVar(&debugMode, "debug", false, "启用调试日志")
	flag.Parse()

	cfg := loadConfig(configPath)
	// 环境变量优先于配置文件：容器部署时改会话不必重建 data/config.json。
	if env := strings.TrimSpace(os.Getenv("OC_SESSION_ID")); env != "" {
		cfg.OCSessionID = env
	}
	applyConfig(cfg)
	if err := saveConfig(configPath, cfg); err != nil {
		log.Printf("警告: 无法保存配置: %v", err)
	}

	// 监听地址三级回落：-listen > OC_LISTEN_ADDR > config.listen_addr。都空 = 沿用历史行为。
	listenAddr = resolveListenAddr(listenAddr, os.Getenv("OC_LISTEN_ADDR"), cfg.ListenAddr)
	if listenAddr != "" {
		if err := validateListenAddr(listenAddr, adminPassword); err != nil {
			log.Fatalf("启动失败: %v", err)
		}
	}

	if strings.TrimSpace(cfg.OCSessionRefreshCmd) != "" {
		startOCSessionRefresh(ocSessionRefreshInterval())
		log.Printf("会话自动续命已启用，每 %s 执行一次", ocSessionRefreshInterval())
	}
	loadTokenStats()
	log.Printf("配置已从 %s 加载", configPath)
	log.Printf("OPENCODE TO API 代理服务器")
	log.Printf("===================")
	log.Printf("版本:     %s", version)
	log.Printf("端口:     %s", port)
	log.Printf("上游:     %s/v1/chat/completions (API，免费层)", ocInferenceBase)
	log.Printf("模型：  异步加载中")
	log.Printf("别名：  %d", len(modelAlias))

	if adminPassword != "" {
		log.Printf("管理面板: http://localhost:%s/ （密码认证已启用）", port)
	} else {
		log.Printf("管理面板: http://localhost:%s/ （无密码）", port)
	}
	log.Printf("===================")
	http.HandleFunc("/v1/chat/completions", chatCompletionsHandler)
	http.HandleFunc("/v1/responses", responsesHandler)
	http.HandleFunc("/v1/messages", claudeMessagesHandler)
	http.HandleFunc("/v1/models", listModelsHandler)
	http.HandleFunc("/login", loginHandler)
	http.HandleFunc("/logout", logoutHandler)
	http.HandleFunc("/api/config", requireAuth(adminConfigHandler))
	http.HandleFunc("/api/stats", requireAuth(adminStatsHandler))
	http.HandleFunc("/api/reload", requireAuth(reloadHandler))
	http.HandleFunc("/api/models", requireAuth(adminModelsHandler))
	http.HandleFunc("/api/version", requireAuth(adminVersionHandler))
	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			requireAuth(adminPageHandler)(w, r)
			return
		}
		http.NotFound(w, r)
	})
	addr := listenAddrString(listenAddr, port)
	log.Printf("服务器启动在 %s", addr)
	go initializeOpenCode()
	go statsFlusher()
	// 显式配置 http.Server：Handler 留空仍走 http.DefaultServeMux，路由注册方式不变。
	// ReadHeaderTimeout / IdleTimeout 用来防住"连接只建不读、或长期空挂"导致的 fd 堆积。
	server := &http.Server{
		Addr:              addr,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	// 旧代码是 log.Fatal(err)：任何监听层错误（端口被抢、fd 耗尽导致 accept 失败）
	// 都会让进程当场退出 —— 表现就是"偶尔闪退"而且几乎没有线索可查。
	// 改成先把原因打清楚、短暂退避后重试，连续失败才退出。
	for attempt := 1; ; attempt++ {
		err := server.ListenAndServe()
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return
		}
		log.Printf("监听异常（第 %d 次）: %v", attempt, err)
		if attempt >= 5 {
			log.Fatalf("连续 %d 次监听失败，进程退出；请检查端口 %s 是否被占用、文件描述符是否耗尽。最后错误: %v", attempt, port, err)
		}
		time.Sleep(2 * time.Second)
	}
}

// resolveListenAddr 决定监听主机：命令行 > 环境变量 > 配置文件。
// 三者都空时返回空串，表示沿用历史行为（":"+port，绑全网卡）。这里刻意不给默认值：
// 改默认值会波及所有消费者（现网容器就不传 -listen），要只绑回环由调用方显式传。
func resolveListenAddr(flagVal, envVal, cfgVal string) string {
	for _, v := range []string{flagVal, envVal, cfgVal} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// listenAddrString 把主机+端口拼成 http.Server 用的地址。
// host 为空 = 沿用历史行为，绑全网卡（即 ":"+port）——这是老部署不传 -listen 时的路径。
func listenAddrString(host, port string) string {
	if strings.TrimSpace(host) == "" {
		return ":" + port
	}
	return net.JoinHostPort(host, port)
}

// isLoopbackHost 判断绑定主机是否只对本机可见。
func isLoopbackHost(host string) bool {
	h := strings.TrimSpace(host)
	if h == "" || h == "localhost" {
		return true
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// validateListenAddr 拦住「显式绑非回环地址却没有面板密码」的组合：面板能改上游会话与
// 凭据池，裸暴露等于把凭据送人。只在调用方显式给地址时才校验，不传 -listen 的历史行为不受影响。
func validateListenAddr(host, password string) error {
	if isLoopbackHost(host) || strings.TrimSpace(password) != "" {
		return nil
	}
	return fmt.Errorf("监听地址 %q 不是回环地址：对外共享必须同时设置面板密码（-password）", host)
}
