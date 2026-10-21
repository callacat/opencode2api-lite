package com.callacat.oc2api;

import java.util.Locale;

/**
 * 把 PackageInstaller 的安装失败信息翻成用户看得懂的中文 + 下一步（CMP-215）。
 *
 * 系统原文是英文（如 `INSTALL FAILED ABORTED: User rejected permissions`），
 * 直接甩给用户既看不懂也不知道该做什么。纯 Java、不碰 Android API ——
 * 与 VersionCompare / Sha256 同一路数，JVM 单测就能钉住映射表。
 */
final class InstallMessage {

    private InstallMessage() {
    }

    /**
     * @param code PackageInstaller.EXTRA_STATUS（调用方自己传，便于单测）
     * @param msg  系统原文，可能为 null / 空
     */
    static String failureText(int code, String msg) {
        String raw = msg == null ? "" : msg.trim();
        String u = raw.toUpperCase(Locale.ROOT);
        if (u.contains("INSUFFICIENT_STORAGE")) {
            return "存储空间不足，安装未完成。请清理手机存储后重试";
        }
        if (u.contains("VERSION_DOWNGRADE")) {
            return "这是降级安装，系统已拒绝；本应用只支持升级安装";
        }
        if (u.contains("ABORTED") || u.contains("USER REJECTED")) {
            return "系统未确认安装：可能是系统安装弹窗被拒绝，或还没授予「安装未知应用」权限"
                    + "（设置 → 应用 → 特殊权限 → 安装未知应用），授权后再点一次「检查更新」";
        }
        return "安装未完成" + (raw.isEmpty() ? "" : "：" + raw) + "（系统码 status=" + code + "）";
    }
}
