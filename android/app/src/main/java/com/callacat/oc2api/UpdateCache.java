package com.callacat.oc2api;

import android.content.Context;
import android.content.SharedPreferences;

/**
 * 「检查更新」结果缓存。
 *
 * GitHub 未认证 API 限 60 次/小时/IP，而启动检查 + 用户手动点都会打网络；没有缓存，
 * 反复打开 app 就能把额度打光（打光后所有人都是「检查失败」）。TTL 内直接用上次结果，不发出站请求。
 */
final class UpdateCache {

    static final long TTL_MS = 30 * 60 * 1000L;

    private static final String FILE = "oc2api-update";
    private static final String K_TAG = "tag";
    private static final String K_APK_URL = "apk_url";
    private static final String K_APK_NAME = "apk_name";
    private static final String K_SUMS_URL = "sums_url";
    private static final String K_RELEASE_URL = "release_url";
    private static final String K_CHECKED_AT = "checked_at";

    String tag = "";
    String apkUrl = "";
    String apkName = "";
    String sumsUrl = "";
    String releaseUrl = "";
    long checkedAt = 0L;

    /** 纯函数，单测覆盖边界：未检查过（0）永远算过期。 */
    static boolean isFresh(long checkedAt, long now) {
        return checkedAt > 0 && now - checkedAt < TTL_MS;
    }

    boolean fresh(long now) {
        return isFresh(checkedAt, now) && !tag.isEmpty();
    }

    static UpdateCache load(Context ctx) {
        SharedPreferences p = ctx.getSharedPreferences(FILE, Context.MODE_PRIVATE);
        UpdateCache c = new UpdateCache();
        c.tag = p.getString(K_TAG, "");
        c.apkUrl = p.getString(K_APK_URL, "");
        c.apkName = p.getString(K_APK_NAME, "");
        c.sumsUrl = p.getString(K_SUMS_URL, "");
        c.releaseUrl = p.getString(K_RELEASE_URL, "");
        c.checkedAt = p.getLong(K_CHECKED_AT, 0L);
        return c;
    }

    void save(Context ctx) {
        ctx.getSharedPreferences(FILE, Context.MODE_PRIVATE).edit()
                .putString(K_TAG, tag)
                .putString(K_APK_URL, apkUrl)
                .putString(K_APK_NAME, apkName)
                .putString(K_SUMS_URL, sumsUrl)
                .putString(K_RELEASE_URL, releaseUrl)
                .putLong(K_CHECKED_AT, checkedAt)
                .apply();
    }
}
