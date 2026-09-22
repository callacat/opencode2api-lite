package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 直连客户端：不设任何请求超时。Client.Timeout 会覆盖整个请求（含流式读 body），
// ResponseHeaderTimeout 也会掐断长时间不返回的请求。
// 请求生命周期完全由调用方 context 控制：客户端断开 / 上游发 [DONE] / 网络层断连。
var httpClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	},
}

// ======================== SOCKS5 代理 ========================

type Socks5Proxy struct {
	Addr     string `json:"addr"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Name     string `json:"name,omitempty"`
}

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

// newSocks5HTTPClient 构建走 SOCKS5 代理的 http.Client。
// 不设任何请求超时，请求生命周期由调用方 context 控制。
func newSocks5HTTPClient(proxy Socks5Proxy) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:         socks5Dial(proxy),
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     90 * time.Second,
		},
	}
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
	return proxy.Addr + "\x00" + proxy.Username + "\x00" + proxy.Password
}

func socks5ProxyLabel(proxy Socks5Proxy) string {
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
	if proxy.Name != "" {
		return proxy.Name + " (" + proxy.Addr + ")"
	}
	return proxy.Addr
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
// 上游 /zen/v1/chat/completions 对免费层模型做指纹校验（2026-09-18 实测），必要条件：
//
//  1. 请求头必须携带 x-session-id，值格式 ses_ + 12 位小写十六进制 + 14 位 [A-Za-z0-9]。
//     缺失、缺 ses_ 前缀或长度不符都会 403 FreeTierError。旧协议头
//     x-opencode-session / x-opencode-client / x-opencode-project / x-opencode-request
//     已被上游无视（可带可不带，官方客户端 1.18.31 已不发这些头）。
//     x-session-affinity 官方客户端会带（与 x-session-id 同值），实测非必需，这里一并带上更贴近真实客户端。
//  2. 请求 body 的 tools 必须包含 bash / glob / grep / read 四件工具
//     （顺序、描述、额外工具均无所谓，缺任一即 403）。缺失时由 ensureFreeTierTools 自动补齐。
//  3. 请求 body 必须 stream:true（stream:false 直接 403）。非流式请求由代理
//     以上游流式发出并用 aggregateOpenAIStream 本地聚合还原，客户端感知不变。
//
// UA 后缀（ai-sdk/... runtime/...）实测非必需（纯 opencode/<ver> 也能过），但保留以贴近真实客户端。
//
// User-Agent 必须能解析出 opencode/<version>，且 version >= minOCVersion。
// 低于该版本上游返回 426 Upgrade Required。
const (
	minOCVersion     = "1.17.0"
	defaultOCVersion = "1.18.31"
)

// newOCSessionID 生成符合上游格式校验的 session id（ses_ + 26 位）。
func newOCSessionID() string {
	const hexChars = "0123456789abcdef"
	const alnumChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 26)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败时退化为时间戳+计数器，仍保持格式合规（长度与字符集正确）
		for i := range b {
			b[i] = byte(time.Now().UnixNano()>>uint(i%8)) ^ byte(i*37)
		}
	}
	for i := 0; i < 12; i++ {
		b[i] = hexChars[b[i]%16]
	}
	for i := 12; i < 26; i++ {
		b[i] = alnumChars[b[i]%byte(len(alnumChars))]
	}
	return "ses_" + string(b)
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

func fetchOCVersion() string {
	req, _ := http.NewRequest("GET", "https://registry.npmjs.org/opencode-ai/latest", nil)
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

func initOCSession() {
	ocOnce.Do(func() {
		ocClientVer = fetchOCVersion()
		ocSessionID = newOCSessionID()
		ocProjectID = randomHex(40)
		log.Printf("OpenCode Version: %s", ocClientVer)
		log.Printf("Session: %s", ocSessionID)
		log.Printf("Project: %s", ocProjectID)
	})
}

func refreshOCSession() {
	ocClientVer = fetchOCVersion()
	ocSessionID = newOCSessionID()
	ocProjectID = randomHex(40)
	log.Printf("会话已刷新: version=%s session=%s", ocClientVer, ocSessionID)
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
	req, _ := http.NewRequest("GET", "https://opencode.ai/zen/v1/models", nil)
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("x-opencode-session", ocSessionID)
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
	configPath = "config.json"
	modelAlias = map[string]ModelAlias{}

	reasoningEffortMap = map[string]string{}
	debugMode          bool
	configMu           sync.RWMutex
)

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

// freeTierRequiredTools 上游 2026-09-18 新增校验：免费层请求 body 的 tools 必须包含
// bash / glob / grep / read 四件工具（顺序、描述、额外工具均无所谓）。
// 客户端（cherry studio / codex / 普通聊天）通常不带全这些工具，这里准备最小定义，
// 由 ensureFreeTierTools 把缺失的补进请求。描述与参数结构可任意，仅需名字命中。
var freeTierRequiredTools = []map[string]any{
	{"type": "function", "function": map[string]any{
		"name":        "bash",
		"description": "Run a shell command and return its output",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string", "description": "The shell command to execute"}},
			"required":   []string{"command"},
		},
	}},
	{"type": "function", "function": map[string]any{
		"name":        "glob",
		"description": "Find files matching a glob pattern",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"pattern": map[string]any{"type": "string", "description": "The glob pattern to match files against"}},
			"required":   []string{"pattern"},
		},
	}},
	{"type": "function", "function": map[string]any{
		"name":        "grep",
		"description": "Search file contents with a regular expression",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"pattern": map[string]any{"type": "string", "description": "The regular expression pattern to search for"}},
			"required":   []string{"pattern"},
		},
	}},
	{"type": "function", "function": map[string]any{
		"name":        "read",
		"description": "Read the contents of a file",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"file_path": map[string]any{"type": "string", "description": "The path of the file to read"}},
			"required":   []string{"file_path"},
		},
	}},
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
func ensureFreeTierTools(bodyMap map[string]any) {
	existing := make(map[string]bool, 8)
	switch tools := bodyMap["tools"].(type) {
	case []Tool:
		for i := range tools {
			if name := tools[i].Function.Name; name != "" {
				existing[name] = true
			}
		}
	case []any:
		for _, raw := range tools {
			if name := toolNameOf(raw); name != "" {
				existing[name] = true
			}
		}
	case []map[string]any:
		for _, t := range tools {
			if name := toolNameOf(t); name != "" {
				existing[name] = true
			}
		}
	}
	missing := make([]any, 0, len(freeTierRequiredTools))
	for _, tool := range freeTierRequiredTools {
		fn, _ := tool["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name != "" && !existing[name] {
			missing = append(missing, tool)
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
func buildOCRequest(ctx context.Context, bodyBytes []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", "https://opencode.ai/zen/v1/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("User-Agent", fmt.Sprintf("opencode/%s ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14", ocClientVer))
	// 2026-09-18 新校验层：上游改查 x-session-id（旧 x-opencode-session 系列头已失效）。
	// 每请求生成新的合规 session（随机编造即可，无需服务端注册，随机化只为避免
	// 所有流量长期绑定同一 id 而在上游侧被集中限流）。
	sid := newOCSessionID()
	req.Header.Set("x-session-id", sid)
	req.Header.Set("x-session-affinity", sid)
	req.Header.Set("Accept", "application/json")
	return req, nil
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
	// 这个循环只服务两类重试：429 换出口重试、Tor 电路故障 / 网络层错误重试。
	// 5xx 已不再重试 —— 直接走下面的透传分支，把上游状态码与错误体原样交给客户端。
	for attempt := 0; ; {
		up, err := buildOCRequest(ctx, bodyBytes)
		if err != nil {
			lastErr = err
			break
		}
		client, selection, err := getHTTPClient(ctx)
		if err != nil {
			return nil, http.StatusBadGateway, nil, err
		}
		attempt++
		resp, err := client.Do(up)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				// 客户端断开/取消：立即退出，避免急装重试占满 CPU
				log.Printf("%s[client gone] model=%s 客户端断开，在途上游请求被取消: %v", labelPrefix(ctx), modelID, ctx.Err())
				break
			}
			continue
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
			return b, resp.StatusCode, resp.Header, nil
		}
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			log.Printf("%s[upstream rate limited] model=%s exit=%s status=%d retry_after=%q body=%s", labelPrefix(ctx), modelID, selection.Label, resp.StatusCode, resp.Header.Get("Retry-After"), string(errBody))
			lastBody = errBody
			lastStatus = resp.StatusCode
			lastHeader = resp.Header.Clone()
			lastErr = fmt.Errorf("upstream rate limited")
			rotateSocks5OnRateLimit(ctx)
			if attempt < attempts {
				continue
			}
		}
		if debugMode {
			log.Printf("%s[upstream error] model=%s status=%d body=%s", labelPrefix(ctx), modelID, resp.StatusCode, string(errBody))
		}
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

	// 只序列化一次，重试时复用同一份字节（循环体内不再改动 body）
	bodyBytes, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, http.StatusInternalServerError, nil, fmt.Errorf("marshal upstream body: %w", err)
	}

	attempts := socks5RateLimitAttemptCount()
	// 这个循环只服务两类重试：429 换出口重试、Tor 电路故障 / 网络层错误重试。
	// 5xx 已不再重试 —— 直接走下面的透传分支，把上游状态码与错误体原样交给客户端。
	for attempt := 0; ; {
		up, err := buildOCRequest(ctx, bodyBytes)
		if err != nil {
			break
		}
		client, selection, err := getHTTPClient(ctx)
		if err != nil {
			return nil, http.StatusBadGateway, nil, err
		}
		attempt++
		resp, err := client.Do(up)
		if err != nil {
			if ctx.Err() != nil {
				// 客户端断开/取消：立即退出，避免急装重试占满 CPU
				log.Printf("%s[client gone] model=%s 客户端断开，在途上游请求被取消: %v", labelPrefix(ctx), modelID, ctx.Err())
				break
			}
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp.Body, resp.StatusCode, resp.Header, nil
		}
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			log.Printf("%s[upstream rate limited] model=%s exit=%s status=%d retry_after=%q body=%s", labelPrefix(ctx), modelID, selection.Label, resp.StatusCode, resp.Header.Get("Retry-After"), string(errBody))
			rotateSocks5OnRateLimit(ctx)
			if attempt < attempts {
				continue
			}
		}
		if debugMode {
			log.Printf("%s[upstream error] model=%s status=%d body=%s", labelPrefix(ctx), modelID, resp.StatusCode, string(errBody))
		}
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
		for {
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
				w.Write([]byte("data: [DONE]\n\n"))
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

			io.WriteString(w, out+"\n")
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

	emitClaudeEvent := func(event string, data any) {
		jsonData, err := json.Marshal(data)
		if err != nil {
			log.Printf("Error marshaling Claude SSE event: %v", err)
			return
		}
		io.WriteString(w, "event: "+event+"\ndata: "+string(jsonData)+"\n\n")
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
		cfg := AppConfig{ModelAlias: modelAlias, ReasoningEffortMap: reasoningEffortMap}
		configMu.RUnlock()
		socks5Mu.RLock()
		cfg.Socks5Proxies = socks5Proxies
		cfg.ActiveSocks5 = activeSocks5
		socks5Mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cfg)
	case http.MethodPost:
		var cfg AppConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, `{"error":"Invalid JSON"}`, http.StatusBadRequest)
			return
		}
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
.logo{display:flex;align-items:center;gap:10px}
.logo-mark{width:36px;height:36px;background:linear-gradient(135deg,var(--accent),#8b6cff);border-radius:10px;display:flex;align-items:center;justify-content:center;font-size:20px;color:#fff;flex-shrink:0}
.logo-text{font-size:22px;font-weight:700;letter-spacing:-.5px;background:linear-gradient(135deg,var(--text),var(--text-sec));-webkit-background-clip:text;-webkit-text-fill-color:transparent}
.logo-sub{font-size:12.5px;color:var(--text-ter);margin-bottom:2px}
.card{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);padding:22px 24px;transition:border-color .2s}
.card:hover{border-color:var(--border-light)}
.card h2{font-size:13px;font-weight:600;margin-bottom:16px;letter-spacing:.2px;display:flex;align-items:center;gap:8px;color:var(--text-sec);text-transform:uppercase}
.card h2 .dot{width:6px;height:6px;border-radius:50%;flex-shrink:0}
.config-grid{display:grid;grid-template-columns:2fr 3fr;gap:16px;margin-top:16px}
.config-grid .card{margin-bottom:0}
.full-row{grid-column:1/-1}
.form-group{margin-bottom:14px}
.form-group:last-child{margin-bottom:0}
.form-group label{display:block;font-size:11.5px;font-weight:500;color:var(--text-ter);margin-bottom:5px;letter-spacing:.4px;text-transform:uppercase}
.form-group input[type="text"],.form-group input[type="url"],.form-group input[type="password"],.form-group textarea,.form-group select,.m-select{width:100%;padding:8px 12px;border:1px solid var(--border);border-radius:var(--radius-sm);font-size:13px;font-family:var(--mono);background:var(--surface-2);color:var(--text);transition:border-color .15s,box-shadow .15s}
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
@media(max-width:700px){.config-grid{grid-template-columns:1fr}.container{padding:16px 12px}header{flex-direction:column;align-items:flex-start;gap:8px}.effort-label-row,.effort-row{grid-template-columns:minmax(90px,1fr) 22px minmax(90px,1fr) auto}}
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
<div style="display:flex;align-items:center;gap:8px">
<button class="theme-toggle" onclick="toggleTheme()" title="切换主题">☀</button>
<form method="post" action="/logout" style="margin:0"><button class="theme-toggle" type="submit" title="退出登录" style="font-size:14px">退出</button></form>
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
<div style="margin-bottom:12px">
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
<h2><span class="dot" style="background:var(--accent)"></span>SOCKS5 代理</h2>
<div style="margin-bottom:12px">
<table class="tbl" id="socks5Table">
<thead><tr><th style="width:25%">名称</th><th style="width:28%">地址</th><th style="width:17%">用户名</th><th style="width:17%">密码</th><th style="width:13%"></th></tr></thead>
<tbody></tbody>
</table>
</div>
<div class="form-group">
<label>启用代理</label>
<select id="activeSocks5" class="m-select">
<option value="">直连（不使用代理）</option>
</select>
</div>
<div class="actions">
<button class="btn btn-primary" onclick="addSocks5Row()">添加代理</button>
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
async function loadConfig(){const sy=window.scrollY;try{const r=await fetch('/api/config');if(!r.ok)throw new Error(await r.text());const cfg=await r.json();aliasData=cfg.model_alias||{};normalizeAliasData();effortData=cfg.reasoning_effort_map||{};socks5Data=cfg.socks5_proxies||[];renderAliasTable();renderEffortTable();renderSocks5Table();document.getElementById('activeSocks5').value=cfg.active_socks5||'';setTimeout(()=>window.scrollTo(0,sy),0);try{const mr=await fetch('/api/models');const md=await mr.json();modelList=(md.models||[]).sort();renderAliasTable()}catch(_){}}catch(e){showToast('失败: '+e.message,'error')}}
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
function renderSocks5Table(){const tb=document.querySelector('#socks5Table tbody');if(!socks5Data.length){tb.innerHTML='<tr><td colspan="5" class="empty-hint">暂无代理配置</td></tr>';renderSocks5Select();return}tb.innerHTML=socks5Data.map((p,i)=>'<tr><td><input value="'+esc(p.name||'')+'" data-field="name"></td><td><input value="'+esc(p.addr)+'" data-field="addr" placeholder="例如: 127.0.0.1:1080"></td><td><input value="'+esc(p.username||'')+'" data-field="username"></td><td><input value="'+esc(p.password||'')+'" data-field="password" type="password"></td><td><button class="btn btn-danger" onclick="delSocks5('+i+')">删除</button></td></tr>').join('');renderSocks5Select()}
function addSocks5Row(){const tb=document.querySelector('#socks5Table tbody');if(tb.querySelector('.empty-hint'))tb.innerHTML='';socks5Data.push({addr:'',name:''});renderSocks5Table()}
function delSocks5(i){socks5Data.splice(i,1);renderSocks5Table()}
function collectSocks5(){const r=[];document.querySelectorAll('#socks5Table tbody tr').forEach(tr=>{const a=tr.querySelector('[data-field="addr"]');if(a&&a.value.trim())r.push({addr:a.value.trim(),name:(tr.querySelector('[data-field="name"]')||{}).value?.trim()||'',username:(tr.querySelector('[data-field="username"]')||{}).value?.trim()||'',password:(tr.querySelector('[data-field="password"]')||{}).value?.trim()||''})});socks5Data=r;return r}
function renderSocks5Select(){const sel=document.getElementById('activeSocks5');const cur=sel.value;sel.innerHTML='<option value="">直连（不使用代理）</option>';socks5Data.forEach(p=>{if(p.addr){const label=p.name?p.name+' ('+p.addr+')':p.addr;const opt=document.createElement('option');opt.value=p.addr;opt.textContent=label;sel.appendChild(opt)}});if(socks5Data.length>=1){const opt=document.createElement('option');opt.value='__rate_limit_switch__';opt.textContent='限流切换（429 后切换，含直连）';sel.appendChild(opt);const opt2=document.createElement('option');opt2.value='__rate_limit_switch_no_direct__';opt2.textContent='限流切换（429 后切换，不含直连）';sel.appendChild(opt2)}if(socks5Data.length>=2){const opt=document.createElement('option');opt.value='__round_robin__';opt.textContent='轮询（每次请求切换）';sel.appendChild(opt)}sel.value=cur;if(!sel.value)sel.value='';}
async function saveConfig(){collectAliases();collectEfforts();collectSocks5();const cfg={model_alias:aliasData,reasoning_effort_map:effortData,socks5_proxies:socks5Data,active_socks5:document.getElementById('activeSocks5').value};try{const r=await fetch('/api/config',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(cfg)});if(!r.ok)throw new Error(await r.text());showToast('配置已保存','success');loadConfig()}catch(e){showToast('保存失败: '+e.message,'error')}}
function esc(s){const d=document.createElement('div');d.textContent=s;return d.innerHTML}
function showToast(msg,t){const e=document.getElementById('toast');e.textContent=msg;e.className=t+' show';clearTimeout(e._tid);e._tid=setTimeout(()=>e.classList.remove('show'),2500)}
async function resetStats(){if(!confirm('确认清空所有 Token 统计？\n此操作不可撤销。'))return;const s=document.getElementById('resetStatus');s.textContent='清空中...';try{const r=await fetch('/api/stats',{method:'DELETE'});if(!r.ok)throw new Error(await r.text());document.getElementById('statsContent').innerHTML='<div class="empty-hint">暂无数据</div>';s.textContent='已清空';setTimeout(()=>s.textContent='',2000)}catch(e){s.textContent='失败: '+e.message}}
async function loadStats(){try{const r=await fetch('/api/stats');const d=await r.json();const ms=d.models||{};const ks=Object.keys(ms);const dm=d.daily?d.daily.models||{}:{};const dk=Object.keys(dm);let h='';if(d.daily&&d.daily.date){h+='<div style="margin-bottom:16px;padding:10px 14px;background:var(--accent);color:#fff;border-radius:8px;font-size:13px">📊 今日统计 ('+esc(d.daily.date)+')：请求 '+fmt(d.daily.total_requests)+' 次</div>'}if(dk.length>0){h+='<h3 style="font-size:14px;font-weight:600;margin:0 0 8px">今日模型用量</h3><table class="tbl" id="dailyTable"><thead><tr><th>模型</th><th>请求数</th><th>输入 Token</th><th>输出 Token</th><th>总计 Token</th></tr></thead><tbody>';let dr=0,dp=0,dc=0,dt=0;for(const k of dk){const m=dm[k];if(!m)continue;h+='<tr><td>'+esc(k)+'</td><td>'+fmt(m.request_count)+'</td><td>'+fmt(m.prompt_tokens)+'</td><td>'+fmt(m.completion_tokens)+'</td><td>'+fmt(m.total_tokens)+'</td></tr>';dr+=m.request_count;dp+=m.prompt_tokens;dc+=m.completion_tokens;dt+=m.total_tokens}h+='<tr style="font-weight:600"><td>今日合计</td><td>'+fmt(dr)+'</td><td>'+fmt(dp)+'</td><td>'+fmt(dc)+'</td><td>'+fmt(dt)+'</td></tr>';h+='</tbody></table><hr style="border:none;border-top:1px solid var(--border);margin:20px 0">'}h+='<h3 style="font-size:14px;font-weight:600;margin:0 0 8px">累计统计</h3><table class="tbl" id="statsTable"><thead><tr><th>模型</th><th>请求数</th><th>输入 Token</th><th>输出 Token</th><th>总计 Token</th></tr></thead><tbody>';if(!ks.length){h+='<tr><td colspan="5" class="empty-hint">暂无数据</td></tr>'}else{let tr=0,pt=0,ct=0,tt=0;for(const k of ks){const m=ms[k];h+='<tr><td>'+esc(k)+'</td><td>'+fmt(m.request_count)+'</td><td>'+fmt(m.prompt_tokens)+'</td><td>'+fmt(m.completion_tokens)+'</td><td>'+fmt(m.total_tokens)+'</td></tr>';tr+=m.request_count;pt+=m.prompt_tokens;ct+=m.completion_tokens;tt+=m.total_tokens}h+='<tr style="font-weight:600"><td>累计总计</td><td>'+fmt(tr)+'</td><td>'+fmt(pt)+'</td><td>'+fmt(ct)+'</td><td>'+fmt(tt)+'</td></tr>'}h+='</tbody></table>';document.getElementById('statsContent').innerHTML=h}catch(e){document.getElementById('statsContent').innerHTML='<div class="empty-hint">加载失败</div>'}}
function fmt(n){return n.toString().replace(/\B(?=(\d{3})+(?!\d))/g,',')}window.onload=function(){loadConfig();loadStats()};setInterval(loadStats,5000);document.addEventListener('visibilitychange',function(){if(!document.hidden){loadStats()}});
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

func main() {
	flag.StringVar(&port, "port", "8000", "服务端口")
	flag.StringVar(&configPath, "config", "config.json", "配置文件路径")
	flag.StringVar(&adminPassword, "password", "", "管理面板密码（留空则不启用登录验证）")
	flag.BoolVar(&debugMode, "debug", false, "启用调试日志")
	flag.Parse()

	cfg := loadConfig(configPath)
	applyConfig(cfg)
	if err := saveConfig(configPath, cfg); err != nil {
		log.Printf("警告: 无法保存配置: %v", err)
	}

	loadTokenStats()
	log.Printf("配置已从 %s 加载", configPath)
	log.Printf("OPENCODE TO API 代理服务器")
	log.Printf("===================")
	log.Printf("版本:     %s", version)
	log.Printf("端口:     %s", port)
	log.Printf("上游:     https://opencode.ai/zen/v1/chat/completions (API)")
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
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			requireAuth(adminPageHandler)(w, r)
			return
		}
		http.NotFound(w, r)
	})
	addr := ":" + port
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
