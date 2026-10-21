package com.callacat.oc2api;

import android.Manifest;
import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.content.SharedPreferences;
import android.content.pm.PackageInstaller;
import android.content.pm.PackageManager;
import android.graphics.Typeface;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.PowerManager;
import android.provider.Settings;
import android.text.Editable;
import android.text.TextUtils;
import android.text.TextWatcher;
import android.view.HapticFeedbackConstants;
import android.view.View;
import android.widget.Button;
import android.widget.CompoundButton;
import android.widget.EditText;
import android.widget.RadioGroup;
import android.widget.ScrollView;
import android.widget.TextView;
import android.widget.Toast;

import androidx.appcompat.app.AlertDialog;
import androidx.appcompat.app.AppCompatActivity;
import androidx.appcompat.app.AppCompatDelegate;

import com.google.android.material.button.MaterialButton;
import com.google.android.material.color.MaterialColors;

import java.io.File;
import java.io.IOException;
import java.util.List;

/**
 * 唯一的界面：状态 + 配置 + 运行日志（CMP-118）。
 *
 * - 面板不再内嵌，改「打开面板」调系统浏览器（manifest 里配了 queries，否则 Android 11+ 起不来）；
 * - 原 WebView 的位置改成日志页，读 ProxyService 侧 LogBuffer 的环形缓冲；
 * - 三档主题（跟随系统 / 白天 / 夜间）走 AppCompatDelegate，所以本类必须是 AppCompatActivity；
 * - 启动/停止是**一个**按钮，按真实服务状态在三态间切；状态由 ProxyService 写进 prefs，
 *   这里监听 prefs 变化刷新，不靠"我刚点了按钮"这种乐观假设。
 *
 * 自更新（CMP-116）：启动查一次 + 「检查更新」按钮手动查一次，发现新版本后下载、
 * 核 sha256、交给系统安装器。不轮询，任何失败都只改一行文字，不弹窗不阻塞。
 */
public class MainActivity extends AppCompatActivity {

    private static final int REQ_NOTIFICATIONS = 1001;

    private static final String THEME_SYSTEM = "system";
    private static final String THEME_LIGHT = "light";
    private static final String THEME_DARK = "dark";

    private EditText portInput;
    private EditText passwordInput;
    private EditText updateProxyInput;
    private TextView passwordWarn;
    private CompoundButton lanSwitch;
    private CompoundButton debugBox;
    private TextView status;
    private TextView statusDetail;
    private TextView versionText;
    private TextView logText;
    private ScrollView logScroll;
    private Button checkButton;
    private MaterialButton toggleButton;
    private Button openPanelButton;
    private View progress;

    private BroadcastReceiver installReceiver;
    /** 下载/安装期间为真：同时挡住重复点「检查更新」与重复下载。 */
    private volatile boolean busy;

    /** 上一次渲染时的运行态，用来判断"刚进入运行中"该播强调动画。 */
    private boolean lastRunning;
    private String lastLog;

    private final SharedPreferences.OnSharedPreferenceChangeListener stateListener =
            (prefs, key) -> {
                if (ProxyService.KEY_STATE.equals(key) || ProxyService.KEY_ERROR.equals(key)) {
                    renderState();
                }
            };

    private final Runnable refreshLog = this::renderLog;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        // 必须早于 super.onCreate()：AppCompatDelegate 要在界面创建前定下夜昼模式。
        applyTheme();
        super.onCreate(savedInstanceState);
        setContentView(R.layout.activity_main);

        portInput = findViewById(R.id.port);
        passwordInput = findViewById(R.id.password);
        updateProxyInput = findViewById(R.id.update_proxy);
        passwordWarn = findViewById(R.id.warn_password);
        lanSwitch = findViewById(R.id.lan);
        debugBox = findViewById(R.id.debug);
        status = findViewById(R.id.status);
        statusDetail = findViewById(R.id.status_detail);
        versionText = findViewById(R.id.version);
        logText = findViewById(R.id.log_text);
        logScroll = findViewById(R.id.log_scroll);
        checkButton = findViewById(R.id.check_update);
        toggleButton = findViewById(R.id.toggle_button);
        openPanelButton = findViewById(R.id.open_panel);
        progress = findViewById(R.id.progress);

        SharedPreferences p = ProxyService.prefs(this);
        portInput.setText(String.valueOf(p.getInt(ProxyService.KEY_PORT, ProxyService.DEFAULT_PORT)));

        String savedPw = p.getString(ProxyService.KEY_PASSWORD, "");
        if (savedPw == null || savedPw.isEmpty()) {
            // 首次安装预填默认密码：省得用户为了开局域网先去面板里设一遍。
            savedPw = getString(R.string.default_password);
            passwordInput.setText(savedPw);
        } else {
            passwordInput.setText(savedPw);
        }

        lanSwitch.setChecked(ProxyService.isLanMode(p));
        updatePasswordWarn();
        debugBox.setChecked(p.getBoolean(ProxyService.KEY_DEBUG, false));

        // CMP-159：更新代理跟端口/密码不同，它不依赖「启动代理」，所以随手改随手存，
        // 免得用户填完直接点「检查更新」却还是走旧值。
        updateProxyInput.setText(p.getString(ProxyService.KEY_UPDATE_PROXY, ""));
        updateProxyInput.addTextChangedListener(new TextWatcher() {
            @Override
            public void beforeTextChanged(CharSequence s, int a, int b, int c) {
            }

            @Override
            public void onTextChanged(CharSequence s, int a, int b, int c) {
            }

            @Override
            public void afterTextChanged(Editable s) {
                ProxyService.prefs(MainActivity.this).edit()
                        .putString(ProxyService.KEY_UPDATE_PROXY, s == null ? "" : s.toString().trim())
                        .apply();
            }
        });

        // LAN 开关的联动：勾上时密码还空着就当场补默认值。
        // 原先只在"点启动"那一刻才拦，用户看到的是"开关拨得上去"的表象。
        lanSwitch.setOnCheckedChangeListener((btn, checked) -> {
            if (checked && currentPassword().isEmpty()) {
                passwordInput.setText(getString(R.string.default_password));
                toast(getString(R.string.password_autofilled));
            }
            updatePasswordWarn();
        });
        passwordInput.addTextChangedListener(new TextWatcher() {
            @Override
            public void beforeTextChanged(CharSequence s, int a, int b, int c) {
            }

            @Override
            public void onTextChanged(CharSequence s, int a, int b, int c) {
            }

            @Override
            public void afterTextChanged(Editable s) {
                updatePasswordWarn();
            }
        });

        findViewById(R.id.battery).setOnClickListener(v -> requestIgnoreBatteryOptimizations());
        RadioGroup themeGroup = findViewById(R.id.theme_group);
        themeGroup.check(themeIdFor(p.getString(ProxyService.KEY_THEME, THEME_SYSTEM)));
        themeGroup.setOnCheckedChangeListener((group, checkedId) -> {
            String key = checkedId == R.id.theme_light ? THEME_LIGHT
                    : checkedId == R.id.theme_dark ? THEME_DARK : THEME_SYSTEM;
            if (isTheme(key)) {
                return;
            }
            ProxyService.prefs(this).edit().putString(ProxyService.KEY_THEME, key).apply();
            applyTheme();
        });

        findViewById(R.id.battery).setOnClickListener(v -> requestIgnoreBatteryOptimizations());
        openPanelButton.setOnClickListener(v -> openPanel());

        // 方法名不能叫 onStart/onStop：会与 Activity 的生命周期回调撞名，编译期直接报
        // "cannot override onStart() in Activity"（首跑 39012779007 就是这么挂的）。
        toggleButton.setOnClickListener(v -> {
            v.performHapticFeedback(HapticFeedbackConstants.CONFIRM);
            if (isRunning()) {
                stopProxy();
            } else {
                startProxy();
            }
        });

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

        restoreIfNeeded();
        renderState();

        // 启动查一次（走 TTL 缓存，不阻塞界面、失败只留一行字）。
        checkUpdate(false);

        if (Build.VERSION.SDK_INT >= 33
                && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS)
                != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, REQ_NOTIFICATIONS);
        }
    }

    @Override
    protected void onResume() {
        super.onResume();
        ProxyService.prefs(this).registerOnSharedPreferenceChangeListener(stateListener);
        LogBuffer.addListener(refreshLog);
        // onCreate 里 renderState() 时 lastRunning 还是 false，这里按真实状态补齐，
        // 免得"服务早就在跑、界面刚打开"被当成刚启动而多播一次强调动画。
        lastRunning = isRunning();
        renderState();
        renderLog();
    }

    @Override
    protected void onPause() {
        ProxyService.prefs(this).unregisterOnSharedPreferenceChangeListener(stateListener);
        LogBuffer.removeListener(refreshLog);
        super.onPause();
    }

    @Override
    protected void onDestroy() {
        if (installReceiver != null) {
            unregisterReceiver(installReceiver);
            installReceiver = null;
        }
        super.onDestroy();
    }

    // ======================== 主题 ========================

    private boolean isTheme(String key) {
        SharedPreferences p = ProxyService.prefs(this);
        return key.equals(p.getString(ProxyService.KEY_THEME, THEME_SYSTEM));
    }

    private static int themeIdFor(String key) {
        if (THEME_LIGHT.equals(key)) {
            return R.id.theme_light;
        }
        return THEME_DARK.equals(key) ? R.id.theme_dark : R.id.theme_system;
    }

    /** 把持久化的选择套到 AppCompatDelegate 上；已经是目标模式就别碰，避免多余重建。 */
    private void applyTheme() {
        String key = ProxyService.prefs(this).getString(ProxyService.KEY_THEME, THEME_SYSTEM);
        int mode;
        if (THEME_LIGHT.equals(key)) {
            mode = AppCompatDelegate.MODE_NIGHT_NO;
        } else if (THEME_DARK.equals(key)) {
            mode = AppCompatDelegate.MODE_NIGHT_YES;
        } else {
            mode = AppCompatDelegate.MODE_NIGHT_FOLLOW_SYSTEM;
        }
        if (AppCompatDelegate.getDefaultNightMode() != mode) {
            AppCompatDelegate.setDefaultNightMode(mode);
        }
    }

    // ======================== 状态渲染 ========================

    private String currentState() {
        return ProxyService.prefs(this).getString(ProxyService.KEY_STATE, ProxyService.STATE_IDLE);
    }

    private String currentError() {
        String e = ProxyService.prefs(this).getString(ProxyService.KEY_ERROR, "");
        return e == null ? "" : e;
    }

    private boolean isRunning() {
        return ProxyService.STATE_RUNNING.equals(currentState());
    }

    /**
     * 覆盖安装 / force-stop / 系统回收都会杀掉进程，但 prefs 里的 state=running 会留下来。
     * 服务与本 Activity 同进程，isAlive() 能可靠区分「真在跑」和「假 running」：
     * 后者按上次意图重新拉起服务，绝不让按钮停在假的「运行中」（CMP-206）。
     */
    private void restoreIfNeeded() {
        String st = currentState();
        boolean wanted = ProxyService.STATE_RUNNING.equals(st)
                || ProxyService.STATE_STARTING.equals(st);
        if (!wanted || ProxyService.isAlive()) {
            return;
        }
        ProxyService.prefs(this).edit()
                .putString(ProxyService.KEY_STATE, ProxyService.STATE_STARTING)
                .putString(ProxyService.KEY_ERROR, "")
                .apply();
        startProxyService();
    }

    private void startProxyService() {
        Intent svc = new Intent(this, ProxyService.class);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            startForegroundService(svc);
        } else {
            startService(svc);
        }
    }

    private void renderState() {
        String st = currentState();
        boolean starting = ProxyService.STATE_STARTING.equals(st);
        boolean running = ProxyService.STATE_RUNNING.equals(st);
        boolean error = ProxyService.STATE_ERROR.equals(st);

        toggleButton.setText(running ? R.string.stop : R.string.start);
        toggleButton.setContentDescription(
                getString(running ? R.string.btn_stop_desc : R.string.btn_start_desc));
        toggleButton.setEnabled(!starting);
        progress.setVisibility(starting ? View.VISIBLE : View.GONE);

        status.setText(statusLine(st, running));
        // 颜色只取主题 attr（colorPrimary / colorError / onSurface），
        // 深浅两态的对比度由 Material 3 主题保证，不写死色值。
        int colorAttr = running ? com.google.android.material.R.attr.colorPrimary
                : error ? com.google.android.material.R.attr.colorError
                : com.google.android.material.R.attr.colorOnSurface;
        status.setTextColor(MaterialColors.getColor(status, colorAttr));

        String err = currentError();
        if (error && !err.isEmpty()) {
            statusDetail.setText(err);
            statusDetail.setTextColor(MaterialColors.getColor(statusDetail,
                    com.google.android.material.R.attr.colorError));
            statusDetail.setVisibility(View.VISIBLE);
        } else {
            statusDetail.setVisibility(View.GONE);
        }

        if (running && !lastRunning) {
            pulse(toggleButton);
            toggleButton.announceForAccessibility(getString(R.string.state_running));
        } else if (!running && lastRunning) {
            toggleButton.announceForAccessibility(getString(R.string.state_stopped));
        }
        lastRunning = running;
    }

    private String statusLine(String st, boolean running) {
        if (running) {
            return lanSwitch.isChecked()
                    ? "局域网共享中（0.0.0.0:" + currentPort() + "）"
                    : "运行中 http://127.0.0.1:" + currentPort() + "/";
        }
        if (ProxyService.STATE_STARTING.equals(st)) {
            return getString(R.string.state_starting);
        }
        return getString(R.string.idle);
    }

    /** 一次性强调动画，播完即停 —— 运行中不挂常驻动画（前台服务 + 动画 = 烧电）。 */
    private void pulse(View v) {
        if (!animationsEnabled()) {
            return;
        }
        v.animate().cancel();
        v.setScaleX(1f);
        v.setScaleY(1f);
        v.animate().scaleX(1.08f).scaleY(1.08f).setDuration(125)
                .withEndAction(() -> v.animate().scaleX(1f).scaleY(1f).setDuration(125).start())
                .start();
    }

    /** 尊重系统「移除动画」：动画时长被拉到 0 时一律不播。 */
    private boolean animationsEnabled() {
        // 带默认值的三参重载本身不抛 SettingNotFoundException（只有两参版才抛），
        // 所以这里不需要 try/catch —— 上一版多包一层反而编译不过（CI run 39030826223）。
        return Settings.Global.getFloat(
                getContentResolver(), Settings.Global.ANIMATOR_DURATION_SCALE, 1f) > 0f;
    }

    // ======================== 日志 ========================

    private void renderLog() {
        List<String> lines = LogBuffer.snapshot(getFilesDir());
        String text = TextUtils.join("\n", lines);
        if (text.equals(lastLog)) {
            return;
        }
        lastLog = text;
        logText.setText(text.isEmpty() ? getString(R.string.log_empty) : text);
        logScroll.post(() -> logScroll.fullScroll(ScrollView.FOCUS_DOWN));
    }

    // ======================== 代理开关 ========================

    private int currentPort() {
        try {
            int p = Integer.parseInt(portInput.getText().toString().trim());
            if (p >= 1024 && p <= 65535) {
                return p;
            }
        } catch (NumberFormatException e) {
            // 落到下面的兜底
        }
        return ProxyService.prefs(this).getInt(ProxyService.KEY_PORT, ProxyService.DEFAULT_PORT);
    }

    private String currentPassword() {
        Editable e = passwordInput.getText();
        return e == null ? "" : e.toString().trim();
    }

    private void updatePasswordWarn() {
        // 默认密码随 APK 公开，局域网模式下等同没有保护 —— 当着用户的面说清楚。
        // 局域网开启时升级成红色加粗的强提醒（CMP-210）：不拦启动，但风险要摆在明面上。
        boolean isDefault = getString(R.string.default_password).equals(currentPassword());
        boolean lan = lanSwitch.isChecked();
        passwordWarn.setVisibility(isDefault ? View.VISIBLE : View.GONE);
        passwordWarn.setText(lan ? R.string.password_warn_lan : R.string.password_warn);
        passwordWarn.setTypeface(null, lan ? Typeface.BOLD : Typeface.NORMAL);
    }

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
        String pw = currentPassword();
        boolean lan = lanSwitch.isChecked();
        // 安全默认：只有显式打开局域网开关才绑全网卡，且必须带面板密码。
        // 代理侧（main.go validateListenAddr）会再拦一次，这里先给用户看得懂的提示。
        if (lan && pw.isEmpty()) {
            toast(getString(R.string.need_password));
            return;
        }
        if (lan && getString(R.string.default_password).equals(pw)) {
            // CMP-210：默认密码不拦启动（东哥 2026-10-21 拍板：做强提醒不做拒绝），
            // 但每次点启动都要再敲一遍。
            toast(getString(R.string.password_warn_lan_toast));
        }

        ProxyService.prefs(this).edit()
                .putInt(ProxyService.KEY_PORT, port)
                .putBoolean(ProxyService.KEY_LAN, lan)
                .putString(ProxyService.KEY_PASSWORD, pw)
                .putBoolean(ProxyService.KEY_DEBUG, debugBox.isChecked())
                .putString(ProxyService.KEY_STATE, ProxyService.STATE_STARTING)
                .apply();
        renderState();

        startProxyService();
    }

    private void stopProxy() {
        stopService(new Intent(this, ProxyService.class));
        ProxyService.prefs(this).edit()
                .putString(ProxyService.KEY_STATE, ProxyService.STATE_IDLE)
                .putString(ProxyService.KEY_ERROR, "")
                .apply();
        renderState();
    }

    /** 面板改由系统浏览器打开：用 127.0.0.1 而不是 localhost，避免 IPv6 解析歧义。 */
    private void openPanel() {
        // 服务没在跑时打开面板只会得到浏览器的"无法连接"，先在这里说清楚（CMP-206）。
        if (!ProxyService.STATE_RUNNING.equals(currentState()) || !ProxyService.isAlive()) {
            toast("代理未运行，请先点「启动」");
            return;
        }
        String url = "http://127.0.0.1:" + currentPort() + "/";
        try {
            startActivity(new Intent(Intent.ACTION_VIEW, Uri.parse(url)));
        } catch (android.content.ActivityNotFoundException e) {
            toast(getString(R.string.panel_no_browser));
        }
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

    // ======================== 自更新（CMP-116）=======================

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
                // CMP-159：补一句可操作提示，否则用户只知道失败、不知道该动哪里。
                setUpdateText(getString(R.string.check_failed_hint));
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
}