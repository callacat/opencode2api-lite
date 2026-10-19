package com.callacat.oc2api;

import java.net.InetSocketAddress;
import java.net.Proxy;

/**
 * 「更新走代理」的配置串解析（CMP-159）。
 *
 * 出站到 GitHub 的三跳（api.github.com / github.com / release-assets.githubusercontent.com）
 * 在国内需要代理。此前 app 里没有入口，只能靠系统代理（CMP-153 实测：adb 设了
 * global http_proxy 才通），而直连时下载会在响应阶段卡满读超时才报失败。
 *
 * 认三种写法：`host:port`、`http://host:port`（`https://` 同义，都是 HTTP CONNECT）、
 * `socks5://host:port`（`socks5h` 同义）。java.net.Proxy 传不了 SOCKS 凭据，
 * 需要认证的 SOCKS 代理用不了。
 *
 * 解析不出来一律返回 {@code null} = 跟随系统默认，不因为一个笔误把更新彻底堵死。
 */
final class UpdateProxy {

    private UpdateProxy() {
    }

    static Proxy parse(String spec) {
        if (spec == null) {
            return null;
        }
        String s = spec.trim();
        if (s.isEmpty()) {
            return null;
        }

        Proxy.Type type = Proxy.Type.HTTP;
        String rest = s;
        int sep = s.indexOf("://");
        if (sep >= 0) {
            String scheme = s.substring(0, sep).toLowerCase();
            rest = s.substring(sep + 3);
            if (scheme.startsWith("socks")) {
                type = Proxy.Type.SOCKS;
            } else if (!scheme.equals("http") && !scheme.equals("https")) {
                return null;
            }
        }

        // 有 path / query 的都砍掉（从浏览器地址栏粘来的 "http://127.0.0.1:7890/" 很常见）。
        int slash = rest.indexOf('/');
        if (slash >= 0) {
            rest = rest.substring(0, slash);
        }
        // user:pass@host:port —— 本类不支持凭据，去掉凭据段按匿名连（总比整条更新不可用强）。
        int at = rest.lastIndexOf('@');
        if (at >= 0) {
            rest = rest.substring(at + 1);
        }

        int colon = rest.lastIndexOf(':');
        if (colon <= 0) {
            return null;
        }
        String host = rest.substring(0, colon);
        // IPv6 字面量要带方括号：[::1]:7890
        if (host.startsWith("[") && host.endsWith("]")) {
            host = host.substring(1, host.length() - 1);
        }
        int port;
        try {
            port = Integer.parseInt(rest.substring(colon + 1));
        } catch (NumberFormatException e) {
            return null;
        }
        if (host.isEmpty() || port < 1 || port > 65535) {
            return null;
        }
        return new Proxy(type, new InetSocketAddress(host, port));
    }
}
