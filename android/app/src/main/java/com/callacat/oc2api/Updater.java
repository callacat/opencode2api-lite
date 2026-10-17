package com.callacat.oc2api;

import android.app.PendingIntent;
import android.content.Context;
import android.content.Intent;
import android.content.pm.PackageInstaller;
import android.content.pm.PackageManager;
import android.os.Build;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.charset.StandardCharsets;

/**
 * 「跟着主仓库 Release 自更新」的全部出站动作：查最新 Release → 下载 APK → 核 sha256
 * → 交给系统安装器（PackageInstaller 会弹系统确认，普通 app 没有静默安装这条路）。
 *
 * 红线（见 CMP-116 卡面）：不做后台轮询（启动 + 手动触发，带 TTL 缓存）、
 * 校验不过不许提示安装、任何失败都静默降级成「检查失败」不阻塞。
 */
final class Updater {

    /** 版本单一来源 = 同一个仓库的 Release（CI 由 tag 出包并附到同名 Release）。 */
    static final String REPO = "callacat/opencode2api-lite";
    private static final String LATEST_API = "https://api.github.com/repos/" + REPO + "/releases/latest";
    private static final String SUMS_ASSET = "sha256sums-android.txt";
    private static final String UA = "oc2api-android";

    /** 出站超时一律 5s：卡面要求「断网/限流 5s 内降级」。 */
    private static final int TIMEOUT_MS = 5000;
    /** 下载 APK 允许更长的读超时（几十 MB，5s 会误判成失败）。 */
    private static final int DOWNLOAD_READ_TIMEOUT_MS = 30000;
    /** 清单文本很小，超过这个大小说明拿到的不是清单。 */
    private static final int MAX_TEXT_BYTES = 64 * 1024;

    static final String ACTION_INSTALL_RESULT = "com.callacat.oc2api.INSTALL_RESULT";
    static final String EXTRA_INSTALL_STATUS = "status";

    private Updater() {
    }

    enum State {
        /** 远端有更新的版本，且带 APK + 哈希清单。 */
        UPDATE,
        UP_TO_DATE,
        /** 最新 Release 里没有可装的 APK（例如只发了 Go 二进制的那次发布）。 */
        NO_APK,
        /** 网络失败 / 限流 / 响应不可解析。 */
        FAILED
    }

    static final class Result {
        State state = State.FAILED;
        String tag = "";
        String local = "";
        String error = "";
        String apkUrl = "";
        String apkName = "";
        String sumsUrl = "";
        String releaseUrl = "";
        boolean cached;

        boolean hasUpdate() {
            return state == State.UPDATE;
        }
    }

    // ======================== 检查 ========================

    /**
     * 查最新 Release 并与本机版本比对。阻塞调用，必须在工作线程里跑。
     *
     * @param force true = 手动触发，跳过 TTL 缓存
     */
    static Result check(Context ctx, String localVersion, boolean force) {
        Result r = new Result();
        r.local = localVersion;

        UpdateCache cache = UpdateCache.load(ctx);
        if (!force && cache.fresh(System.currentTimeMillis())) {
            r.cached = true;
            return fromCache(r, cache);
        }

        try {
            JSONObject rel = fetchLatestRelease();
            populate(r, rel);
        } catch (Exception e) {
            r.state = State.FAILED;
            r.error = e.getMessage() == null ? e.toString() : e.getMessage();
            return r;
        }

        cache.tag = r.tag;
        cache.apkUrl = r.apkUrl;
        cache.apkName = r.apkName;
        cache.sumsUrl = r.sumsUrl;
        cache.releaseUrl = r.releaseUrl;
        cache.checkedAt = System.currentTimeMillis();
        cache.save(ctx);
        return r;
    }

    private static Result fromCache(Result r, UpdateCache c) {
        r.tag = c.tag;
        r.apkUrl = c.apkUrl;
        r.apkName = c.apkName;
        r.sumsUrl = c.sumsUrl;
        r.releaseUrl = c.releaseUrl;
        decide(r);
        return r;
    }

    /**
     * 拿到 Release 后判「有没有可装的更新」：缺 APK 或缺哈希清单都不算可更新 ——
     * 卡面红线是「不校验不许提示安装」，那就不如不提示。
     */
    private static void decide(Result r) {
        if (r.apkUrl.isEmpty() || r.sumsUrl.isEmpty()) {
            r.state = State.NO_APK;
            r.error = "最新 Release 缺少 APK 或 sha256 清单";
            return;
        }
        r.state = VersionCompare.isNewer(r.tag, r.local) ? State.UPDATE : State.UP_TO_DATE;
    }

    private static void populate(Result r, JSONObject rel) throws IOException {
        r.tag = rel.optString("tag_name", "");
        if (r.tag.isEmpty()) {
            throw new IOException("Release 响应缺少 tag_name");
        }
        r.releaseUrl = rel.optString("html_url", "");

        JSONArray assets = rel.optJSONArray("assets");
        if (assets != null) {
            for (int i = 0; i < assets.length(); i++) {
                JSONObject a = assets.optJSONObject(i);
                if (a == null) {
                    continue;
                }
                String name = a.optString("name", "");
                if (r.apkUrl.isEmpty() && name.toLowerCase().endsWith(".apk")) {
                    r.apkName = name;
                    r.apkUrl = a.optString("browser_download_url", "");
                } else if (r.sumsUrl.isEmpty() && SUMS_ASSET.equals(name)) {
                    r.sumsUrl = a.optString("browser_download_url", "");
                }
            }
        }
        decide(r);
    }

    private static JSONObject fetchLatestRelease() throws Exception {
        HttpURLConnection c = open(LATEST_API, TIMEOUT_MS);
        try {
            int code = c.getResponseCode();
            if (code != 200) {
                throw new IOException("GitHub 返回 " + code);
            }
            return new JSONObject(readText(c.getInputStream(), MAX_TEXT_BYTES));
        } finally {
            c.disconnect();
        }
    }

    // ======================== 下载 + 校验 ========================

    interface Progress {
        void onProgress(int percent);
    }

    /**
     * 下载 APK 到 app 私有缓存目录，**并且核过 sha256 才返回**。任何一个环节不合格都抛异常，
     * 调用方据此提示「检查失败/下载失败」，绝不进入安装。
     */
    static File download(Context ctx, Result r, Progress cb) throws IOException {
        if (r.apkUrl.isEmpty() || r.sumsUrl.isEmpty()) {
            throw new IOException("缺少 APK 或 sha256 清单地址");
        }
        File dir = new File(ctx.getCacheDir(), "update");
        if (!dir.isDirectory() && !dir.mkdirs()) {
            throw new IOException("无法创建下载目录 " + dir);
        }
        File apk = new File(dir, r.apkName);

        downloadTo(r.apkUrl, apk, cb);
        try {
            String sums = fetchText(r.sumsUrl);
            if (!Sha256.matches(sums, r.apkName, apk)) {
                throw new IOException("sha256 校验不通过（清单里没有该文件或哈希不符）");
            }
        } catch (IOException e) {
            // 校验不通过的文件留在缓存里只会被误装，直接删掉。
            //noinspection ResultOfMethodCallIgnored
            apk.delete();
            throw e;
        }
        return apk;
    }

    private static void downloadTo(String url, File dest, Progress cb) throws IOException {
        HttpURLConnection c = open(url, TIMEOUT_MS);
        c.setReadTimeout(DOWNLOAD_READ_TIMEOUT_MS);
        try {
            int code = c.getResponseCode();
            if (code != 200) {
                throw new IOException("下载 APK 返回 " + code);
            }
            long total = c.getContentLengthLong();
            try (InputStream in = c.getInputStream();
                 OutputStream out = new FileOutputStream(dest)) {
                byte[] buf = new byte[64 * 1024];
                long done = 0;
                int n;
                int lastPct = -1;
                while ((n = in.read(buf)) > 0) {
                    out.write(buf, 0, n);
                    done += n;
                    if (cb != null && total > 0) {
                        int pct = (int) (done * 100 / total);
                        if (pct != lastPct) {
                            lastPct = pct;
                            cb.onProgress(pct);
                        }
                    }
                }
            }
        } finally {
            c.disconnect();
        }
    }

    private static String fetchText(String url) throws IOException {
        HttpURLConnection c = open(url, TIMEOUT_MS);
        try {
            if (c.getResponseCode() != 200) {
                throw new IOException("拉取 sha256 清单返回 " + c.getResponseCode());
            }
            return readText(c.getInputStream(), MAX_TEXT_BYTES);
        } finally {
            c.disconnect();
        }
    }

    private static HttpURLConnection open(String url, int timeoutMs) throws IOException {
        HttpURLConnection c = (HttpURLConnection) new URL(url).openConnection();
        c.setConnectTimeout(timeoutMs);
        c.setReadTimeout(timeoutMs);
        c.setInstanceFollowRedirects(true);
        c.setRequestProperty("User-Agent", UA);
        c.setRequestProperty("Accept", "application/vnd.github+json");
        return c;
    }

    private static String readText(InputStream in, int max) throws IOException {
        StringBuilder sb = new StringBuilder();
        try (BufferedReader r = new BufferedReader(new InputStreamReader(in, StandardCharsets.UTF_8))) {
            char[] buf = new char[4096];
            int n;
            while ((n = r.read(buf)) > 0) {
                sb.append(buf, 0, n);
                if (sb.length() > max) {
                    throw new IOException("响应超过 " + max + " 字节");
                }
            }
        }
        return sb.toString();
    }

    // ======================== 安装 ========================

    /** API 26+ 需要用户显式允许「安装未知应用」；更低版本是全局开关，直接尝试即可。 */
    static boolean canRequestInstall(Context ctx) {
        return Build.VERSION.SDK_INT < Build.VERSION_CODES.O
                || ctx.getPackageManager().canRequestPackageInstalls();
    }

    static Intent unknownSourcesSettingsIntent(Context ctx) {
        return new Intent(
                Build.VERSION.SDK_INT >= Build.VERSION_CODES.O
                        ? android.provider.Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES
                        : android.provider.Settings.ACTION_SECURITY_SETTINGS,
                android.net.Uri.parse("package:" + ctx.getPackageName()));
    }

    /**
     * 走系统安装器：把 APK 写进一个 PackageInstaller 会话后 commit，系统会弹安装确认界面。
     * 结果经 {@link #ACTION_INSTALL_RESULT} 广播回 Activity。
     */
    static void install(Context ctx, File apk) throws IOException {
        PackageManager pm = ctx.getPackageManager();
        PackageInstaller installer = pm.getPackageInstaller();
        PackageInstaller.SessionParams params =
                new PackageInstaller.SessionParams(PackageInstaller.SessionParams.MODE_FULL_INSTALL);
        params.setSize(apk.length());

        int id = installer.createSession(params);
        try (PackageInstaller.Session session = installer.openSession(id)) {
            try (OutputStream out = session.openWrite("oc2api.apk", 0, apk.length());
                 InputStream in = new FileInputStream(apk)) {
                byte[] buf = new byte[64 * 1024];
                int n;
                while ((n = in.read(buf)) > 0) {
                    out.write(buf, 0, n);
                }
                session.fsync(out);
            }
            PendingIntent cb = PendingIntent.getBroadcast(
                    ctx, id,
                    new Intent(ACTION_INSTALL_RESULT).setPackage(ctx.getPackageName()),
                    PendingIntent.FLAG_UPDATE_CURRENT
                            | (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S
                            ? PendingIntent.FLAG_MUTABLE : 0));
            session.commit(cb.getIntentSender());
        } catch (IOException e) {
            installer.abandonSession(id);
            throw e;
        }
    }
}
