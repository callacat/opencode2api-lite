package com.callacat.oc2api;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;

import org.junit.Test;

import java.io.File;
import java.io.FileOutputStream;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;

/**
 * sha256 校验单测。
 *
 * 这是安装前的唯一门禁（卡面红线：不校验不许提示安装），所以清单解析的边角也要锁住：
 * 路径前缀、二进制模式的 "*" 前缀、多个条目、大小写、文件名不在清单里。
 */
public class Sha256Test {

    /** sha256("hello") —— 定值，用来锁住 of() 的实现没被改错。 */
    private static final String HELLO = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824";

    private static File tempFile(String content) throws Exception {
        File f = File.createTempFile("oc2api-sha", ".apk");
        f.deleteOnExit();
        try (OutputStream out = new FileOutputStream(f)) {
            out.write(content.getBytes(StandardCharsets.UTF_8));
        }
        return f;
    }

    @Test
    public void hashesFile() throws Exception {
        assertEquals(HELLO, Sha256.of(tempFile("hello")));
    }

    @Test
    public void parsesSumsLine() {
        String sums = HELLO + "  oc2api-android-1.0.5.apk\n";
        assertEquals(HELLO, Sha256.expectedFor(sums, "oc2api-android-1.0.5.apk"));
    }

    @Test
    public void parsesMultiLineSumsWithPathsAndBinaryMarkers() {
        String sums = "1111  other.apk\n"
                + HELLO + "  dist/oc2api-android-1.0.5.apk\n"
                + "2222 *bin.apk\n";
        assertEquals(HELLO, Sha256.expectedFor(sums, "oc2api-android-1.0.5.apk"));
        assertEquals("2222", Sha256.expectedFor(sums, "bin.apk"));
        assertEquals("1111", Sha256.expectedFor(sums, "other.apk"));
    }

    @Test
    public void missingEntryReturnsNull() {
        assertNull(Sha256.expectedFor(HELLO + "  a.apk\n", "b.apk"));
        assertNull(Sha256.expectedFor("", "a.apk"));
        assertNull(Sha256.expectedFor(null, "a.apk"));
        assertNull(Sha256.expectedFor("整行没有哈希 的乱码", "a.apk"));
    }

    @Test
    public void matchesOnlyWhenHashAndNameAgree() throws Exception {
        File apk = tempFile("hello");
        assertTrue(Sha256.matches(HELLO + "  app.apk\n", "app.apk", apk));
        // 哈希对不上
        assertFalse(Sha256.matches("00" + HELLO.substring(2) + "  app.apk\n", "app.apk", apk));
        // 清单里没有这个文件
        assertFalse(Sha256.matches(HELLO + "  app.apk\n", "other.apk", apk));
    }
}
