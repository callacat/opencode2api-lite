package com.callacat.oc2api;

import android.Manifest;
import android.app.Activity;
import android.app.AlertDialog;
import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.content.SharedPreferences;
import android.content.pm.PackageInstaller;
import android.content.pm.PackageManager;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.PowerManager;
import android.provider.Settings;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.Button;
import android.widget.CheckBox;
import android.widget.EditText;
import android.widget.Switch;
import android.widget.TextView;
import android.widget.Toast;

import java.io.File;
import java.io.IOException;

/**
 * 唯一的界面：几个开关 + 一个 WebView 直接复用代理自带的管理面板。
 * 不写业务 UI —— 面板 HTML 由 Go 进程内联吐出来，这里只是把它显示出来。
 *
 * 自更新（CMP-116）：启动查一次 + 「检查更新」按钮手动查一次，发现新版本后下载、
 * 核 sha256、交给系统安装器。不轮询，任何失败都只改一行文字，不弹窗不阻塞。
 */
public class MainActivity extends Activity {

    private static final int REQ_NOTIFICATIONS = 1001;
    /** 面板要等 Go 起监听，给个固定的等待窗口；失败时再点一次"启动"即可。 */
    private static final long PANEL_WAIT_MS = 1200;

    private EditText portInput;
    private EditText passwordInput;
    private Switch lanSwitch;
    private CheckBox debugBox;
    private TextView status;
    private TextView versionText;
    private Button checkButton;
    private WebView webView;

    private BroadcastReceiver installReceiver;
    /** 下载/安装期间为真：同时挡住重复点「检查更新」与重复下载。 */
    private volatile boolean busy;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(R.layout.activity_main);

        portInput = findViewById(R.id.port);
        passwordInput = findViewById(R.id.password);
        lanSwitch = findViewById(R.id.lan);
        debugBox = findViewById(R.id.debug);
        status = findViewById(R.id.status);
        versionText = findViewById(R.id.version);
        checkButton = findViewById(R.id.check_update);
        webView = findViewById(R.id.webview);

        WebSettings ws = webView.getSettings();
        ws.setJavaScriptEnabled(true);
        // 面板用 localStorage 记主题与登录态。
        ws.setDomStorageEnabled(true);
        webView.setWebViewClient(new WebViewClient());

        SharedPreferences p = ProxyService.prefs(this);
        portInput.setText(String.valueOf(p.getInt(ProxyService.KEY_PORT, ProxyService.DEFAULT_PORT)));
        passwordInput.setText(p.getString(ProxyService.KEY_PASSWORD, ""));
        lanSwitch.setChecked(ProxyService.isLanMode(p));
        debugBox.setChecked(p.getBoolean(ProxyService.KEY_DEBUG, false));

        // 方法名不能叫 onStart/onStop：会与 Activity 的生命周期回调撞名，编译期直接报
        // "cannot override onStart() in Activity"（首跑 39012779007 就是这么挂的）。
        findViewById(R.id.start).setOnClickListener(v -> startProxy());
        findViewById(R.id.stop).setOnClickListener(v -> stopProxy());
        Button battery = findViewById(R.id.battery);
        battery.setOnClickListener(v -> requestIgnoreBatteryOptimizations());

        checkButton.setOnClickListener(v -> checkUpdate(true));
        versionText.setText("当前版本 " + currentVersionLabel());
        installReceiver = new BroadcastReceiver() {
            @Override
            public void onReceive(Context context, Intent intent) {
                onInstallResult(intent);
            }
        };
        IntentFilter filter = new IntentFilter(Updater.ACTION_INSTALL_RESULT);
        if (Build.VERSION.SDK_INT >= 33) {
            // 广播由系统代本 app 的 PendingIntent 发出，发送方身份就是本 app，不必对全网开放。
            registerReceiver(installReceiver, filter, Context.RECEIVER_NOT_EXPORTED);
        } else {
            registerReceiver(installReceiver, filter);
        }

        // 启动查一次（走 TTL 缓存，不阻塞界面、失败只留一行字）。
        checkUpdate(false);

        if (Build.VERSION.SDK_INT >= 33
                && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS)
                != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, REQ_NOTIFICATIONS);
        }
    }

    @Override
    protected void onDestroy() {
        if (installReceiver != null) {
            unregisterReceiver(installReceiver);
            installReceiver = null;
        }
        super.onDestroy();
    }

    // ======================== 自更新 ========================

    private String currentVersion() {
        try {
            String v = getPackageManager().getPackageInfo(getPackageName(), 0).versionName;
            return v == null ? "" : v;
        } catch (PackageManager.NameNotFoundException e) {
            return "";
        }
    }

    private String currentVersionLabel() {
        String v = currentVersion();
        return VersionCompare.isVersioned(v) ? v : v + "（未注入版本）";
    }

    private void setUpdateText(String s) {
        versionText.setText("当前版本 " + currentVersionLabel() + " ｜ " + s);
    }

    private void checkUpdate(boolean manual) {
        if (busy) {
            return;
        }
        final String local = currentVersion();
        checkButton.setEnabled(false);
        if (manual) {
            setUpdateText("检查中…");
        }
        new Thread(() -> {
            final Updater.Result r = Updater.check(getApplicationContext(), local, manual);
            runOnUiThread(() -> {
                checkButton.setEnabled(true);
                renderCheck(r);
            });
        }, "oc2api-update-check").start();
    }

    private void renderCheck(Updater.Result r) {
        switch (r.state) {
            case UPDATE:
                setUpdateText("发现新版本 " + r.tag);
                promptUpdate(r);
                break;
            case UP_TO_DATE:
                setUpdateText("已是最新（" + r.tag + "）");
                break;
            case NO_APK:
                setUpdateText("最新 " + r.tag + " 没有可安装的 APK");
                break;
            default:
                // 卡面红线：断网/限流/解析失败只显示「检查失败」，不弹窗、不影响其它功能。
                setUpdateText("检查失败");
        }
    }

    private void promptUpdate(Updater.Result r) {
        new AlertDialog.Builder(this)
                .setTitle("发现新版本 " + r.tag)
                .setMessage("当前 " + currentVersionLabel()
                        + "\n\n下载后会核对 Release 里的 sha256，再由系统安装器确认安装。")
                .setPositiveButton("下载并安装", (d, w) -> downloadAndInstall(r))
                .setNegativeButton("稍后", null)
                .show();
    }

    private void downloadAndInstall(Updater.Result r) {
        if (busy) {
            return;
        }
        busy = true;
        checkButton.setEnabled(false);
        setUpdateText("下载中…");
        new Thread(() -> {
            try {
                File apk = Updater.download(getApplicationContext(), r,
                        pct -> runOnUiThread(() -> setUpdateText("下载中 " + pct + "%")));
                runOnUiThread(() -> install(apk));
            } catch (IOException e) {
                runOnUiThread(() -> {
                    busy = false;
                    checkButton.setEnabled(true);
                    setUpdateText("下载失败：" + e.getMessage());
                });
            }
        }, "oc2api-update-download").start();
    }

    private void install(File apk) {
        if (!Updater.canRequestInstall(this)) {
            busy = false;
            checkButton.setEnabled(true);
            setUpdateText("需要允许「安装未知应用」");
            new AlertDialog.Builder(this)
                    .setTitle("需要一次授权")
                    .setMessage("系统要求先允许本应用安装未知来源的应用，之后回到这里再点一次「检查更新」。")
                    .setPositiveButton("去开启", (d, w) -> {
                        try {
                            startActivity(Updater.unknownSourcesSettingsIntent(this));
                        } catch (Exception e) {
                            toast("打不开系统设置页");
                        }
                    })
                    .setNegativeButton("取消", null)
                    .show();
            return;
        }
        try {
            Updater.install(this, apk);
            setUpdateText("已交给系统安装器，请在系统弹窗里确认");
        } catch (IOException e) {
            busy = false;
            checkButton.setEnabled(true);
            setUpdateText("安装失败：" + e.getMessage());
        }
    }

    private void onInstallResult(Intent intent) {
        busy = false;
        checkButton.setEnabled(true);
        int code = intent.getIntExtra(PackageInstaller.EXTRA_STATUS, PackageInstaller.STATUS_FAILURE);
        if (code == PackageInstaller.STATUS_PENDING_USER_ACTION) {
            // 系统把「确认安装」这一步交回调用方：必须把它给的 Intent 起起来，
            // 否则用户看不到任何界面、会话也不会往下走。
            // （真机首测就卡在这里：EXTRA_STATUS = -1，被我当初当成失败处理了。）
            Intent confirm = intent.getParcelableExtra(Intent.EXTRA_INTENT);
            setUpdateText("等待系统安装确认…");
            if (confirm != null) {
                confirm.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
                startActivity(confirm);
            } else {
                setUpdateText("安装未完成：系统未返回确认界面");
            }
            return;
        }
        if (code == PackageInstaller.STATUS_SUCCESS) {
            setUpdateText("安装完成，重启后生效");
            toast("更新完成");
            return;
        }
        String msg = intent.getStringExtra(PackageInstaller.EXTRA_STATUS_MESSAGE);
        setUpdateText("安装未完成：" + (msg == null || msg.isEmpty() ? "status=" + code : msg));
    }

    // ======================== 代理开关 ========================

    private void startProxy() {
        int port;
        try {
            port = Integer.parseInt(portInput.getText().toString().trim());
        } catch (NumberFormatException e) {
            port = -1;
        }
        if (port < 1024 || port > 65535) {
            toast(getString(R.string.bad_port));
            return;
        }
        String pw = passwordInput.getText().toString().trim();
        boolean lan = lanSwitch.isChecked();
        // 安全默认：只有显式打开局域网开关才绑全网卡，且必须带面板密码。
        // 代理侧（main.go validateListenAddr）会再拦一次，这里先给用户看得懂的提示。
        if (lan && pw.isEmpty()) {
            toast(getString(R.string.need_password));
            return;
        }

        ProxyService.prefs(this).edit()
                .putInt(ProxyService.KEY_PORT, port)
                .putBoolean(ProxyService.KEY_LAN, lan)
                .putString(ProxyService.KEY_PASSWORD, pw)
                .putBoolean(ProxyService.KEY_DEBUG, debugBox.isChecked())
                .apply();

        Intent svc = new Intent(this, ProxyService.class);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            startForegroundService(svc);
        } else {
            startService(svc);
        }

        final String url = "http://127.0.0.1:" + port + "/";
        status.setText(lan ? "局域网共享中（0.0.0.0:" + port + "）" : "运行中 " + url);
        webView.postDelayed(() -> webView.loadUrl(url), PANEL_WAIT_MS);
    }

    private void stopProxy() {
        stopService(new Intent(this, ProxyService.class));
        status.setText(R.string.idle);
        webView.loadUrl("about:blank");
    }

    /**
     * 请求忽略电池优化：Doze 下系统会掐掉应用的网络访问，代理会连不上上游。
     * 不做这一步，"关屏 30 分钟仍 200"只在插着电的机器上成立。
     */
    private void requestIgnoreBatteryOptimizations() {
        PowerManager pm = (PowerManager) getSystemService(Context.POWER_SERVICE);
        if (pm != null && pm.isIgnoringBatteryOptimizations(getPackageName())) {
            toast("已忽略电池优化");
            return;
        }
        try {
            startActivity(new Intent(
                    Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS,
                    Uri.parse("package:" + getPackageName())));
        } catch (Exception e) {
            startActivity(new Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS));
        }
    }

    private void toast(String msg) {
        Toast.makeText(this, msg, Toast.LENGTH_SHORT).show();
    }
}
