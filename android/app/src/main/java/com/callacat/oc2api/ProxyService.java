package com.callacat.oc2api;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.content.SharedPreferences;
import android.content.pm.ServiceInfo;
import android.os.Build;
import android.os.IBinder;
import android.os.SystemClock;
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
    /** 真实服务状态（CMP-118）。UI 靠它渲染按钮三态，不靠"点了按钮"这种乐观假设。 */
    static final String KEY_STATE = "state";
    /** 状态为 error 时的可读原因，直接显示给用户。 */
    static final String KEY_ERROR = "error";
    /** 三档主题：system / light / dark（CMP-118，AppCompatDelegate 用）。 */
    static final String KEY_THEME = "theme";
    /**
     * 自更新下载走的代理（CMP-159），例如 `192.168.1.150:30001` 或 `socks5://127.0.0.1:7890`。
     * 空 = 跟随系统默认。只作用于 Updater 的出站，不影响代理服务本身的上游。
     */
    static final String KEY_UPDATE_PROXY = "update_proxy";
    /**
     * 当前进行中的 PackageInstaller 会话 id，0 = 没有安装在进行（CMP-215）。
     * 系统回调的安装结果按它对号入座：旧会话迟到的结果不再写当前界面。
     */
    static final String KEY_INSTALL_SESSION = "install_session";

    static final String STATE_IDLE = "idle";
    static final String STATE_STARTING = "starting";
    static final String STATE_RUNNING = "running";
    static final String STATE_ERROR = "error";

    /** 上一次退到后台的冻结判定（CMP-236）：0=没判过，1=正常，2=疑似被冻结。 */
    static final int FREEZE_STATE_UNKNOWN = 0;
    static final int FREEZE_STATE_OK = 1;
    static final int FREEZE_STATE_FROZEN = 2;
    static final String KEY_FREEZE_STATE = "freeze_state";
    static final String KEY_FREEZE_GAP = "freeze_gap";

    /**
     * 后台冻结判定器(CMP-236)。static:Activity 与服务同进程,共用一份。
     * 判据与红线(不读自己 uid 的 cgroup.freeze)见 FreezeDetector 的类注释。
     */
    private static final FreezeDetector FREEZE = new FreezeDetector();

    /** 本段后台期的冻结是否已经记过状态/日志(避免每次回前台重复记)。 */
    private static volatile boolean freezeRecorded;

    static final int DEFAULT_PORT = 8000;

    /** 必须 lib 前缀 + .so 后缀：否则系统不解压到 nativeLibraryDir，也就不可执行。 */
    private static final String BIN_NAME = "liboc2api.so";
    /** 子进程退避重启档位，最后一档封顶。 */
    private static final long[] BACKOFF_MS = {1000, 2000, 4000, 8000, 15000, 30000};
    /** 子进程活过这么久就认为这次启动是成功的，退避清零，避免长期把上限拉高。 */
    private static final long HEALTHY_UPTIME_MS = 30_000;

    /**
     * 探活自愈参数（CMP-215）。真机 PKG110 实测过一种「活着但不服务」的卡死：
     * 进程在、端口 LISTEN、/health 却超时 —— 看门狗只看进程退出，覆盖不到，
     * 于是面板所有请求挂住、用户看到「点任何按钮都崩 / 保存失败 / 很久打不开」。
     * 这里定期探 /health，连续失败就判定卡死并杀掉子进程，让看守线程按退避重启。
     * 判据（真机）：把子进程卡住后不手动点「停止→启动」，1 分钟内自愈。
     */
    private static final long PROBE_INTERVAL_MS = 5_000;
    private static final long PROBE_TIMEOUT_MS = 3_000;
    private static final int PROBE_FAIL_LIMIT = 3;
    /** 启动宽限：子进程刚起来时端口还没 bind 上，这段时间探失败不算。 */
    private static final long PROBE_GRACE_MS = 15_000;

    private volatile boolean running;
    private volatile Process process;
    private Thread supervisor;

    /**
     * 服务实例是否活着。Activity 与服务同进程，进程被系统杀掉（覆盖安装 / force-stop / 回收）
     * 后静态标志自然归 false —— 用来识别 prefs 里还写着 running、实际服务早没了的假状态。
     */
    private static volatile boolean alive;

    static boolean isAlive() {
        return alive;
    }

    static SharedPreferences prefs(android.content.Context ctx) {
        return ctx.getSharedPreferences(PREFS, MODE_PRIVATE);
    }

    // ======================== 后台冻结判定(CMP-236)========================

    /** App 切到后台(MainActivity.onPause 调):开始一段新的后台期。 */
    static void onAppBackground() {
        FREEZE.onBackground(SystemClock.elapsedRealtime());
        freezeRecorded = false;
    }

    /**
     * App 切回前台(MainActivity.onResume 调):判本段后台期有没有被系统冻结,
     * 并记状态 + 日志。
     *
     * @return true = 疑似被冻结(调用方给原生提示 +「去加白」跳转)
     */
    static boolean onAppForeground(Context ctx) {
        boolean frozen = FREEZE.onForeground(SystemClock.elapsedRealtime(), SystemClock.uptimeMillis());
        if (frozen) {
            recordFreezeIfNeeded(ctx, FREEZE.frozenGapMs());
        } else if (prefs(ctx).getInt(KEY_FREEZE_STATE, FREEZE_STATE_UNKNOWN) == FREEZE_STATE_FROZEN) {
            // 上次疑似冻结、这次没有 ⇒ 多半是加白生效了。翻成 OK 并留一条日志当自检证据。
            prefs(ctx).edit().putInt(KEY_FREEZE_STATE, FREEZE_STATE_OK).apply();
            LogBuffer.append(ctx.getFilesDir(), "本次退后台未检测到冻结(加白生效或未触发)");
        }
        return frozen;
    }

    /** 记一次冻结判定:prefs 存状态 + 日志页留痕。同一段后台期只记一次。 */
    private static void recordFreezeIfNeeded(Context ctx, long gapMs) {
        if (freezeRecorded) {
            return;
        }
        freezeRecorded = true;
        prefs(ctx).edit()
                .putInt(KEY_FREEZE_STATE, FREEZE_STATE_FROZEN)
                .putLong(KEY_FREEZE_GAP, gapMs)
                .apply();
        String msg = "疑似被系统后台冻结:探活心跳断档 " + (gapMs / 1000) + " 秒(设备仍醒着)。"
                + "需在 ColorOS 加白:" + ctx.getString(R.string.whitelist_path);
        Log.w(TAG, msg);
        LogBuffer.append(ctx.getFilesDir(), msg);
    }

    /** 验收演练用(CMP-236):注入一次「疑似被冻结」。 */
    static void injectFreezeForTest(long gapMs) {
        FREEZE.injectFreeze(gapMs);
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

    /**
     * 把状态写进 prefs，Activity 用 OnSharedPreferenceChangeListener 监听。
     * 走 prefs 而不是 binder/broadcast：这个 app 只有两个类，一个键值通道够了，
     * 也顺带跨 Activity 重建存活（重开后立刻能读到服务当前在干什么）。
     */
    private void setState(String state, String error) {
        prefs(this).edit()
                .putString(KEY_STATE, state)
                .putString(KEY_ERROR, error == null ? "" : error)
                .apply();
    }

    @Override
    public void onCreate() {
        super.onCreate();
        createChannel();
        startForegroundCompat(buildNotification("正在启动…", 0));
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        alive = true;
        if (!running) {
            running = true;
            setState(STATE_STARTING, null);
            supervisor = new Thread(this::supervise, "oc2api-supervisor");
            supervisor.start();
        }
        // START_STICKY：系统回收后重建服务，supervise() 会重新拉起子进程。
        return START_STICKY;
    }

    @Override
    public void onDestroy() {
        alive = false;
        running = false;
        // 服务停了就没有心跳可言:清掉基准与结论,别让陈旧断档在下次回前台时误报冻结(CMP-236)。
        FREEZE.reset();
        setState(STATE_IDLE, null);
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
            } catch (Exception e) {
                // 不只 IOException：pb.start() 还会抛 SecurityException 等运行时异常。
                // 漏掉它们会静默杀死看守线程，界面就永久停在假的"运行中"（CMP-206）。
                note("启动失败：" + e);
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
            // 置空：探活线程靠「process == 我那一个」判断自己该不该退出。
            process = null;
            // 心跳源(子进程)没了:清掉基准,别把「子进程重启期间的空档」误判成系统冻结(CMP-236)。
            FREEZE.reset();
            if (!running) {
                return;
            }

            long uptime = System.currentTimeMillis() - startedAt;
            if (uptime >= HEALTHY_UPTIME_MS) {
                attempt = 0;
            }
            note("代理进程退出（code=" + code + "，存活 " + uptime + "ms），正在自动重启");
            if (!sleepBackoff(attempt++)) {
                return;
            }
        }
    }

    /** 失败/退出同时写 logcat、日志页、通知和 UI 状态，四处都不留空白（CMP-206）。 */
    private void note(String msg) {
        Log.w(TAG, msg);
        LogBuffer.append(getFilesDir(), msg);
        updateNotification(msg);
        setState(STATE_ERROR, msg);
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
        // 日志页也留一条（cmd 带面板密码，不写进落盘日志）。
        LogBuffer.append(getFilesDir(), "代理进程已启动 listen=" + host + ":" + port);
        pump(process);
        startProbe(port);
        updateNotification(lan
                ? "局域网共享中（0.0.0.0:" + port + "）"
                : "运行中 http://127.0.0.1:" + port + "/");
        // 子进程拉起来就算 running；真绑不上端口这类失败会在毫秒内退出，
        // 由 supervise() 的 exit 分支翻成 error，两边不重叠。
        setState(STATE_RUNNING, null);
    }

    /**
     * 探活自愈（CMP-215）：子进程可能「活着、端口在听、但不服务」——
     * 真机 PKG110 实测：/proc/net/tcp 显示 127.0.0.1:8000 LISTEN，ps 里进程在，
     * /health 却超时（连接被 accept 后无人处理）。看门狗只管进程退出，覆盖不到这种卡死。
     * 这里定期探 /health，连续失败就判定卡死、杀掉子进程，让 supervise() 按退避重启。
     */
    private void startProbe(int port) {
        final Process p = process;
        Thread t = new Thread(() -> {
            long start = System.currentTimeMillis();
            int fail = 0;
            FREEZE.resetTick(SystemClock.elapsedRealtime(), SystemClock.uptimeMillis());
            while (running && process == p) {
                try {
                    Thread.sleep(PROBE_INTERVAL_MS);
                } catch (InterruptedException e) {
                    return;
                }
                FREEZE.onTick(SystemClock.elapsedRealtime(), SystemClock.uptimeMillis());
                if (!running || process != p) {
                    return;
                }
                if (System.currentTimeMillis() - start < PROBE_GRACE_MS) {
                    continue;
                }
                if (probeHealthy(port)) {
                    fail = 0;
                    continue;
                }
                if (++fail < PROBE_FAIL_LIMIT) {
                    continue;
                }
                fail = 0;
                String msg = "探活连续失败：代理进程活着但不服务（/health 无响应），杀掉重启";
                Log.w(TAG, msg);
                LogBuffer.append(getFilesDir(), msg);
                // 先 destroy()（SIGTERM）。真机那种卡死的进程可能连信号处理都不走了，
                // 信号只是排队不生效 —— 等 2s 还不退就强杀，否则看门狗等不来 exit，
                // 面板会一直挂着（用 SIGSTOP 冻住进程实测：SIGTERM 不生效，SIGKILL 才行）。
                p.destroy();
                try {
                    if (!p.waitFor(2, java.util.concurrent.TimeUnit.SECONDS)) {
                        Log.w(TAG, "探活自愈：SIGTERM 未生效，强制结束进程");
                        p.destroyForcibly();
                    }
                } catch (InterruptedException e) {
                    p.destroyForcibly();
                    return;
                }
                return;
            }
        }, "oc2api-probe");
        t.setDaemon(true);
        t.start();
    }

    /** 探一次 /health：连不上、或超时内拿不到 200，都算不健康。 */
    private boolean probeHealthy(int port) {
        return HealthProbe.healthy(port, (int) PROBE_TIMEOUT_MS);
    }

    /** 把子进程 stdout/stderr 逐行转进 logcat 与日志环形缓冲（GUI 日志页读它）。 */
    private void pump(Process p) {
        Thread t = new Thread(() -> {
            try (BufferedReader r = new BufferedReader(
                    new InputStreamReader(p.getInputStream(), StandardCharsets.UTF_8))) {
                String line;
                while ((line = r.readLine()) != null) {
                    Log.i(TAG, line);
                    LogBuffer.append(getFilesDir(), line);
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
