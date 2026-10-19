package com.callacat.oc2api;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertNull;

import java.net.InetSocketAddress;
import java.net.Proxy;

import org.junit.Test;

/**
 * 更新代理配置串的解析（CMP-159）。
 *
 * 两条红线锁在这里：
 * ① 空/垃圾输入必须回 null（= 跟随系统），不能因为一个笔误让「检查更新」彻底不可用；
 * ② socks5:// 必须落到 SOCKETS，否则把 SOCKS 端口当 HTTP 代理连，报错信息会非常难查。
 */
public class UpdateProxyTest {

    @Test
    public void emptyOrJunkFallsBackToSystemDefault() {
        assertNull(UpdateProxy.parse(null));
        assertNull(UpdateProxy.parse(""));
        assertNull(UpdateProxy.parse("   "));
        assertNull(UpdateProxy.parse("127.0.0.1"));       // 缺端口
        assertNull(UpdateProxy.parse(":7890"));            // 缺主机
        assertNull(UpdateProxy.parse("127.0.0.1:0"));      // 端口越界
        assertNull(UpdateProxy.parse("127.0.0.1:70000"));
        assertNull(UpdateProxy.parse("127.0.0.1:abc"));
        assertNull(UpdateProxy.parse("ftp://127.0.0.1:7890")); // 不认的 scheme
    }

    @Test
    public void bareHostPortIsHttp() {
        Proxy p = UpdateProxy.parse("192.168.1.150:30001");
        assertNotNull(p);
        assertEquals(Proxy.Type.HTTP, p.type());
        assertEquals(new InetSocketAddress("192.168.1.150", 30001), p.address());
    }

    @Test
    public void schemeAndTrailingPathAreTolerated() {
        for (String spec : new String[]{
                "http://127.0.0.1:7890",
                "https://127.0.0.1:7890",
                "HTTP://127.0.0.1:7890",
                "http://127.0.0.1:7890/",
                "  127.0.0.1:7890  ",
        }) {
            Proxy p = UpdateProxy.parse(spec);
            assertNotNull(spec, p);
            assertEquals(spec, Proxy.Type.HTTP, p.type());
            assertEquals(spec, new InetSocketAddress("127.0.0.1", 7890), p.address());
        }
    }

    @Test
    public void socksSchemesUseSocksType() {
        for (String spec : new String[]{"socks5://10.0.0.1:1080", "socks://10.0.0.1:1080",
                "socks5h://10.0.0.1:1080", "SOCKS5://10.0.0.1:1080"}) {
            Proxy p = UpdateProxy.parse(spec);
            assertNotNull(spec, p);
            assertEquals(spec, Proxy.Type.SOCKS, p.type());
            assertEquals(spec, new InetSocketAddress("10.0.0.1", 1080), p.address());
        }
    }

    /** 从浏览器地址栏粘来的带凭据写法：去掉凭据段按匿名连，总比整条更新不可用强。 */
    @Test
    public void credentialsAreStrippedNotRejected() {
        Proxy p = UpdateProxy.parse("http://user:pass@127.0.0.1:7890");
        assertNotNull(p);
        assertEquals(new InetSocketAddress("127.0.0.1", 7890), p.address());
    }

    @Test
    public void ipv6LiteralWithBrackets() {
        Proxy p = UpdateProxy.parse("[::1]:7890");
        assertNotNull(p);
        assertEquals(new InetSocketAddress("::1", 7890), p.address());
    }
}
