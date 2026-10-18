package com.callacat.oc2api;

import java.io.File;
import java.io.FileInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;

/**
 * sha256 校验。Release 里的 sha256sums-android.txt 是唯一可信清单：校验不过就不许提示安装。
 */
final class Sha256 {

    private Sha256() {
    }

    static String of(File f) throws IOException {
        MessageDigest md = newDigest();
        byte[] buf = new byte[8192];
        try (InputStream in = new FileInputStream(f)) {
            int n;
            while ((n = in.read(buf)) > 0) {
                md.update(buf, 0, n);
            }
        }
        return hex(md.digest());
    }

    /**
     * 从 sha256sums 内容里取 fileName 对应的哈希。格式同 sha256sum 工具：`<hex>  <name>`，
     * 前缀可能带目录（CI 在 dist/ 下算，所以按 basename 匹配）。
     */
    static String expectedFor(String sumsContent, String fileName) {
        if (sumsContent == null || fileName == null) {
            return null;
        }
        for (String line : sumsContent.split("\n")) {
            String t = line.trim();
            if (t.isEmpty()) {
                continue;
            }
            String[] parts = t.split("\\s+");
            if (parts.length < 2) {
                continue;
            }
            String name = parts[parts.length - 1];
            if (name.startsWith("*")) {
                name = name.substring(1);
            }
            int slash = name.lastIndexOf('/');
            if (slash >= 0) {
                name = name.substring(slash + 1);
            }
            if (name.equals(fileName)) {
                return parts[0].toLowerCase();
            }
        }
        return null;
    }

    /** 清单里没有该文件、或哈希对不上，都返回 false。 */
    static boolean matches(String sumsContent, String fileName, File apk) throws IOException {
        String expected = expectedFor(sumsContent, fileName);
        return expected != null && expected.equals(of(apk));
    }

    private static MessageDigest newDigest() {
        try {
            return MessageDigest.getInstance("SHA-256");
        } catch (NoSuchAlgorithmException e) {
            // Android 与任何标准 JRE 都带 SHA-256，走到这里说明运行环境坏了。
            throw new IllegalStateException("SHA-256 不可用", e);
        }
    }

    private static String hex(byte[] b) {
        StringBuilder sb = new StringBuilder(b.length * 2);
        for (byte x : b) {
            sb.append(Character.forDigit((x >> 4) & 0xf, 16));
            sb.append(Character.forDigit(x & 0xf, 16));
        }
        return sb.toString();
    }
}
