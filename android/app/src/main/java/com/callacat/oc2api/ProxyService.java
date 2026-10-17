package com.callacat.oc2api;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Intent;
import android.content.SharedPreferences;
import android.content.pm.ServiceInfo;
import android.os.Build;
import android.os.IBinder;
import android.util.Log;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;

/**
 * 常驻前台服务：把 oc2api 代理（liboc2api.so）当成子进程拉起来并看守。
 *
 * 形态说明（东哥 2026-10-17 拍板，勿改）：
 * - 静态 Go 二进制放进 jniLibs，安装时由系统解压到 nativeLibraryDir 才能 exec
 *   （Android 10+ 的 W^X：别的目录一律不可执行）；
 * - 前台服务类型 specialUse，不用 dataSync（Android 15+ 有 6h/24h 总时长上限）；
 * - 监听地址默认 127.0.0.1，只有局域网开关打开才 `-listen 0.0.0.0`，且此时代理侧
 *   （main.go 的 validateListenAddr）会强制要求面板密码非空，这里再做一次校验。
 */
public class ProxyService extends Service {

    static final String TAG = "oc2api";
    static final String CHANNEL_ID = "oc2api-proxy";
    static final int NOTIFICATION_ID = 8801;

    static final String PREFS = "oc2api";
    static final String KEY_PORT = "port";
    static final String KEY_LAN = "lan";
    static final String KEY_PASSWORD = "password";
    static final String KEY_DEBUG = "debug";
    static final int DEFAULT_PORT = 8000;

    /** 必须 lib 前缀 + .so 后缀：否则系统不解压到 nativeLibraryDir，也就不可执行。 */
    private static final String BIN_NAME = "liboc2api.so";
    /** 子进程退避重启档位，最后一档封顶。 */
    private static final long[] BACKOFF_MS = {1000, 2000, 4000, 8000, 15000, 30000};
    /** 子进程活过这么久就认为这次启动是成功的，退避清零，避免长期把上限拉高。 */
    private static final long HEALTHY_UPTIME_MS = 30_000;

    private volatile boolean running;
    private volatile Process process;
    private Thread supervisor;

    static SharedPreferences prefs(android.content.Context ctx) {
        return ctx.getSharedPreferences(PREFS, MODE_PRIVATE);
    }

    /** 局域网模式必须带密码：返回空串表示配置不合法（UI 层也拦一次，这里是兜底）。 */
    static String resolvePassword(SharedPreferences prefs) {
        String pw = prefs.getString(KEY_PASSWORD, "");
        if (pw == null) {
            return "";
        }
        return pw.trim();
    }

    static boolean isLanMode(SharedPreferences prefs) {
        return prefs.getBoolean(KEY_LAN, false);
    }

    @Override
    public void onCreate() {
        super.onCreate();
        createChannel();
        startForegroundCompat(buildNotification("正在启动…", 0));
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        if (!running) {
            running = true;
            supervisor = new Thread(this::supervise, "oc2api-supervisor");
            supervisor.start();
        }
        // START_STICKY：系统回收后重建服务，supervise() 会重新拉起子进程。
        return START_STICKY;
    }

    @Override
    public void onDestroy() {
        running = false;
        Process p = process;
        if (p != null) {
            // destroy() 对 native 进程是 SIGTERM；Go 侧没有信号处理，会直接退出。
            p.destroy();
        }
        super.onDestroy();
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }

    // ======================== 子进程看守 ========================

    private void supervise() {
        int attempt = 0;
        while (running) {
            long startedAt = System.currentTimeMillis();
            try {
                launch();
            } catch (IOException e) {
                Log.e(TAG, "启动代理进程失败: " + e);
                updateNotification("启动失败：" + e.getMessage());
                if (!sleepBackoff(attempt++)) {
                    return;
                }
                continue;
            }

            int code;
            try {
                code = process.waitFor();
            } catch (InterruptedException e) {
                return;
            }
            if (!running) {
                return;
            }

            long uptime = System.currentTimeMillis() - startedAt;
            if (uptime >= HEALTHY_UPTIME_MS) {
                attempt = 0;
            }
            Log.w(TAG, "代理进程退出 code=" + code + "（存活 " + uptime + "ms），准备重启");
            updateNotification("代理已退出（code=" + code + "），重启中…");
            if (!sleepBackoff(attempt++)) {
                return;
            }
        }
    }

    /** 返回 false 表示服务已被停掉，看守线程该退出了。 */
    private boolean sleepBackoff(int attempt) {
        long delay = BACKOFF_MS[Math.min(attempt, BACKOFF_MS.length - 1)];
        try {
            Thread.sleep(delay);
        } catch (InterruptedException e) {
            return false;
        }
        return running;
    }

    private void launch() throws IOException {
        SharedPreferences p = prefs(this);
        int port = p.getInt(KEY_PORT, DEFAULT_PORT);
        boolean lan = isLanMode(p);
        String host = lan ? "0.0.0.0" : "127.0.0.1";
        String pw = resolvePassword(p);

        if (lan && pw.isEmpty()) {
            // 理论到不了：UI 会拦。真到了就宁可退出也不能裸绑全网卡。
            throw new IOException("局域网模式未设面板密码，拒绝启动");
        }

        File bin = new File(getApplicationInfo().nativeLibraryDir, BIN_NAME);
        if (!bin.isFile()) {
            throw new IOException("代理二进制缺失：" + bin);
        }
        if (!bin.canExecute()) {
            // 打包时被压着没解压（extractNativeLibs=false）会落到这里。
            throw new IOException("代理二进制不可执行（检查 extractNativeLibs）：" + bin);
        }

        List<String> cmd = new ArrayList<>();
        cmd.add(bin.getAbsolutePath());
        cmd.add("-config");
        cmd.add(ensureConfigFile().getAbsolutePath());
        cmd.add("-port");
        cmd.add(String.valueOf(port));
        cmd.add("-listen");
        cmd.add(host);
        if (!pw.isEmpty()) {
            cmd.add("-password");
            cmd.add(pw);
        }
        if (p.getBoolean(KEY_DEBUG, false)) {
            cmd.add("-debug");
        }

        ProcessBuilder pb = new ProcessBuilder(cmd);
        // cwd = filesDir：config.json / stats.json 都落在 app 私有目录。
        pb.directory(getFilesDir());
        pb.redirectErrorStream(true);
        Map<String, String> env = pb.environment();
        // Android 上 net/conf.go 的 preferCgo 让 cgo resolver 成为默认；这里显式写死并开
        // 调试输出，好让 logcat 里有一条可 grep 的 resolver 判据。
        // 注意别写成 netdns=go：那会把 confVal.netGo 置真，mustUseGoResolver() 直接
        // 走纯 Go resolver，而 Android 没有 /etc/resolv.conf，DNS 全挂（net/conf.go:87,214）。
        env.put("GODEBUG", "netdns=cgo+1");
        env.put("HOME", getFilesDir().getAbsolutePath());
        env.put("TMPDIR", getCacheDir().getAbsolutePath());
        env.remove("LD_PRELOAD");

        process = pb.start();
        Log.i(TAG, "代理进程已启动 listen=" + host + ":" + port + " cmd=" + cmd);
        pump(process);
        updateNotification(lan
                ? "局域网共享中（0.0.0.0:" + port + "）"
                : "运行中 http://127.0.0.1:" + port + "/");
    }

    /** 把子进程 stdout/stderr 逐行转进 logcat —— 真机验收就靠这个抓 cgo resolver 与上游日志。 */
    private void pump(Process p) {
        Thread t = new Thread(() -> {
            try (BufferedReader r = new BufferedReader(
                    new InputStreamReader(p.getInputStream(), StandardCharsets.UTF_8))) {
                String line;
                while ((line = r.readLine()) != null) {
                    Log.i(TAG, line);
                }
            } catch (IOException e) {
                Log.w(TAG, "读取子进程输出结束: " + e.getMessage());
            }
        }, "oc2api-log-pump");
        t.setDaemon(true);
        t.start();
    }

    /** 首次启动把 assets 里的默认配置落到 filesDir（之后由面板读写，不再覆盖）。 */
    private File ensureConfigFile() throws IOException {
        File cfg = new File(getFilesDir(), "config.json");
        if (cfg.isFile() && cfg.length() > 0) {
            return cfg;
        }
        try (InputStream in = getAssets().open("config.json");
             OutputStream out = new FileOutputStream(cfg)) {
            byte[] buf = new byte[8192];
            int n;
            while ((n = in.read(buf)) > 0) {
                out.write(buf, 0, n);
            }
            Log.i(TAG, "已从 assets 落默认配置到 " + cfg);
        } catch (IOException e) {
            // 种子配置缺失不是致命错：Go 侧 loadConfig 对读不到的文件返回归一化默认，
            // 启动后 saveConfig 会立刻补写。宁可少一份种子，也不能因此起不来进程。
            Log.w(TAG, "assets 无默认配置，交给代理自建: " + e.getMessage());
        }
        return cfg;
    }

    // ======================== 通知 ========================

    private void createChannel() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) {
            return;
        }
        NotificationManager nm = getSystemService(NotificationManager.class);
        if (nm == null) {
            return;
        }
        NotificationChannel ch = new NotificationChannel(
                CHANNEL_ID, "oc2api 代理", NotificationManager.IMPORTANCE_LOW);
        ch.setDescription("本地代理运行状态");
        nm.createNotificationChannel(ch);
    }

    @SuppressWarnings("deprecation")
    private Notification buildNotification(String text, int port) {
        Notification.Builder b = Build.VERSION.SDK_INT >= Build.VERSION_CODES.O
                ? new Notification.Builder(this, CHANNEL_ID)
                : new Notification.Builder(this);
        Intent open = new Intent(this, MainActivity.class);
        open.setFlags(Intent.FLAG_ACTIVITY_NEW_TASK | Intent.FLAG_ACTIVITY_CLEAR_TOP);
        PendingIntent pi = PendingIntent.getActivity(this, 0, open,
                PendingIntent.FLAG_UPDATE_CURRENT | PendingIntent.FLAG_IMMUTABLE);
        b.setContentTitle("oc2api 代理")
                .setContentText(text)
                .setSmallIcon(android.R.drawable.stat_sys_upload_done)
                .setOngoing(true)
                .setContentIntent(pi);
        return b.build();
    }

    private void startForegroundCompat(Notification n) {
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(NOTIFICATION_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE);
        } else {
            startForeground(NOTIFICATION_ID, n);
        }
    }

    private void updateNotification(String text) {
        NotificationManager nm = getSystemService(NotificationManager.class);
        if (nm != null) {
            nm.notify(NOTIFICATION_ID, buildNotification(text, 0));
        }
    }
}
