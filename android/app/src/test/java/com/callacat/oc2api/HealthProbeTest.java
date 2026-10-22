package com.callacat.oc2api;

import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import org.junit.After;
import org.junit.Before;
import org.junit.Test;

import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.ServerSocket;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;

/**
 * CMP-215 ③：探活必须能识别「活着但不服务」。
 *
 * 真机 PKG110 的现象是「端口 LISTEN、进程在、/health 超时」——
 * 因此单测不只测「正常 200」和「连接被拒」，还必须测最关键的第三条：
 * **accept 了但永不响应**（用只 accept 不读写的假服务模拟）。
 * 摘掉 HealthProbe 的读超时，第三条会挂住直到 JVM 超时失败。
 */
public class HealthProbeTest {

    private ServerSocket good;
    private ServerSocket hung;
    private final List<Socket> accepted = new ArrayList<>();
    private final List<Thread> threads = new ArrayList<>();

    @Before
    public void setUp() throws IOException {
        // 正常一点的服务：读到请求就回 200。
        good = new ServerSocket(0);
        threads.add(serve(good, true));
        // 真机那种卡死：accept 之后既不读也不写，TCP 连得上但永远没响应。
        hung = new ServerSocket(0);
        threads.add(serve(hung, false));
    }

    private Thread serve(final ServerSocket ss, final boolean answer) {
        Thread t = new Thread(() -> {
            while (!ss.isClosed()) {
                try {
                    Socket s = ss.accept();
                    synchronized (accepted) {
                        accepted.add(s);
                    }
                    if (!answer) {
                        continue; // 故意不说话
                    }
                    InputStream in = s.getInputStream();
                    byte[] buf = new byte[1024];
                    in.read(buf);
                    OutputStream out = s.getOutputStream();
                    out.write("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nOK"
                            .getBytes(StandardCharsets.US_ASCII));
                    out.flush();
                    s.close();
                } catch (IOException e) {
                    return;
                }
            }
        }, "fake-http");
        t.setDaemon(true);
        t.start();
        return t;
    }

    @After
    public void tearDown() throws IOException {
        good.close();
        hung.close();
        synchronized (accepted) {
            for (Socket s : accepted) {
                try {
                    s.close();
                } catch (IOException ignored) {
                }
            }
        }
    }

    @Test
    public void healthyServerIsHealthy() {
        assertTrue(HealthProbe.healthy(good.getLocalPort(), 2000));
    }

    @Test
    public void hungServerIsNotHealthy() {
        long t0 = System.currentTimeMillis();
        boolean ok = HealthProbe.healthy(hung.getLocalPort(), 800);
        long ms = System.currentTimeMillis() - t0;
        assertFalse("accept 了但不响应必须判不健康", ok);
        assertTrue("要在超时附近返回而不是挂住: " + ms + "ms", ms < 5000);
    }

    @Test
    public void closedPortIsNotHealthy() throws IOException {
        ServerSocket tmp = new ServerSocket(0);
        int port = tmp.getLocalPort();
        tmp.close();
        assertFalse(HealthProbe.healthy(port, 800));
    }

    @Test
    public void non200IsNotHealthy() throws IOException {
        final ServerSocket s500 = new ServerSocket(0);
        Thread t = new Thread(() -> {
            try {
                Socket s = s500.accept();
                byte[] buf = new byte[1024];
                s.getInputStream().read(buf);
                s.getOutputStream().write(
                        "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n"
                                .getBytes(StandardCharsets.US_ASCII));
                s.getOutputStream().flush();
                s.close();
            } catch (IOException ignored) {
            }
        });
        t.setDaemon(true);
        t.start();
        try {
            assertFalse(HealthProbe.healthy(s500.getLocalPort(), 2000));
        } finally {
            s500.close();
        }
    }
}
