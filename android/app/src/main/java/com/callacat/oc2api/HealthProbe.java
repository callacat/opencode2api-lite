package com.callacat.oc2api;

import java.io.BufferedReader;
import java.io.IOException;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.net.Socket;
import java.nio.charset.StandardCharsets;

/**
 * 探活：连一次 127.0.0.1:&lt;port&gt;/health，看有没有「200」。
 *
 * 纯 Java（只依赖 java.net），不碰 Android API —— JVM 单测能直接拿一个
 * 「accept 了但永不响应」的假服务钉住「卡死要被判不健康」。
 *
 * 为什么不用 HTTP 客户端库：只需要判断「端口后面还有没有人处理请求」，
 * 一次连接 + 一行状态行足够，且能精确控制超时。
 */
final class HealthProbe {

    private HealthProbe() {
    }

    /**
     * @return true = 3s 内拿到 HTTP 200；false = 连不上 / 超时 / 非 200
     */
    static boolean healthy(int port, int timeoutMs) {
        Socket s = new Socket();
        try {
            s.connect(new InetSocketAddress("127.0.0.1", port), timeoutMs);
            s.setSoTimeout(timeoutMs);
            OutputStream out = s.getOutputStream();
            out.write(("GET /health HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n")
                    .getBytes(StandardCharsets.US_ASCII));
            out.flush();
            BufferedReader r = new BufferedReader(
                    new InputStreamReader(s.getInputStream(), StandardCharsets.US_ASCII));
            String statusLine = r.readLine();
            return statusLine != null && statusLine.contains(" 200");
        } catch (IOException e) {
            return false;
        } finally {
            try {
                s.close();
            } catch (IOException ignored) {
            }
        }
    }
}
