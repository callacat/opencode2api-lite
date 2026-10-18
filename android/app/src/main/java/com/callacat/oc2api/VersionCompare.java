package com.callacat.oc2api;

/**
 * 版本号比对（纯 Java，无 Android 依赖，单测见 app/src/test）。
 *
 * 语义与 Go 侧 compareVersion/versionNumbers（main.go:610）保持一致：点分、逐段只吃前导数字、
 * 非数字后缀忽略、最多比三段。两边必须同一套判据 —— 面板（CMP-115）与 app 各自会跟 GitHub 比一次，
 * 分叉了就会出现「面板说有更新、app 说没有」。
 */
final class VersionCompare {

    private VersionCompare() {
    }

    /**
     * 去掉 tag 前缀与空白："v1.0.4" → "1.0.4"。
     *
     * 两侧来源都带 "v"（CI 注入的 tag、GitHub 的 tag_name），不归一化会把 "v1.0.4" 读成
     * [0,0,4]（首段 "v1" 取不到前导数字直接记 0），与 "1.0.4" = [1,0,4] 不等 ⇒ 恒误报有更新。
     */
    static String normalize(String v) {
        if (v == null) {
            return "";
        }
        String s = v.trim();
        if (!s.isEmpty() && (s.charAt(0) == 'v' || s.charAt(0) == 'V')) {
            s = s.substring(1);
        }
        return s.trim();
    }

    /** normalize 后首字符是数字才算「已注入版本号」。CI 的非 tag 构建是 "dev"/分支名，不是版本号。 */
    static boolean isVersioned(String v) {
        String s = normalize(v);
        return !s.isEmpty() && s.charAt(0) >= '0' && s.charAt(0) <= '9';
    }

    /** 逐段取前导数字，最多三段；缺段与非数字段都是 0（"1.0.5-androidtest" → [1,0,5]）。 */
    static int[] parse(String v) {
        int[] out = new int[3];
        String[] parts = normalize(v).split("\\.", -1);
        for (int i = 0; i < out.length && i < parts.length; i++) {
            int n = 0;
            for (int j = 0; j < parts[i].length(); j++) {
                char c = parts[i].charAt(j);
                if (c < '0' || c > '9') {
                    break;
                }
                n = n * 10 + (c - '0');
            }
            out[i] = n;
        }
        return out;
    }

    /**
     * 未注入版本的构建（"dev"）按 0.0.0 参与比较：它不代表任何已发布版本，任何正式版本都比它新。
     *
     * 这里刻意与 CMP-115 面板侧「dev 不比较」不同：app 若也跳过比较，CI 产物（PR 构建就是 dev）
     * 就永远无法自更新，真机验收用的正是 CI 产物，这条路径会变成不可验证。正式分发的包
     * versionName 一律来自 tag（CI 注入），生产路径不受影响。
     */
    static int[] effective(String v) {
        return isVersioned(v) ? parse(v) : new int[]{0, 0, 0};
    }

    /** remote 比 local 新返回 >0，旧返回 <0，相同（或无法比较）返回 0。 */
    static int compare(String remote, String local) {
        int[] a = effective(remote);
        int[] b = effective(local);
        for (int i = 0; i < a.length; i++) {
            if (a[i] != b[i]) {
                return a[i] < b[i] ? -1 : 1;
            }
        }
        return 0;
    }

    static boolean isNewer(String remote, String local) {
        return compare(remote, local) > 0;
    }
}
