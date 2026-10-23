package com.callacat.oc2api;

import android.Manifest;
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
import android.os.Handler;
import android.os.Looper;
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

    /** 面板 intent:// 唤起时带的 extra：值为 "1" 表示「重启代理」（CMP-215）。 */
    static final String EXTRA_RESTART = "oc2api_restart";

    /** 面板 intent:// 唤起时带的 extra:值为 "1" 表示「去 ColorOS 加白」(CMP-236)。 */
    static final String EXTRA_WHITELIST = "oc2api_whitelist";

    /**
     * 验收演练用(CMP-236):值为 "1" 时注入一次「疑似被冻结」。
     * 真机加白后冻结不再复现,靠它验「提示 + 去加白」这条链路还在。
     */
    static final String EXTRA_FREEZE_TEST = "oc2api_freeze_test";

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

    /** 刚从 ColorOS 加白设置页回来:这一次回前台不弹冻结提示,免得来回弹(CMP-236)。 */
    private boolean suppressFreezePrompt;

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
        updatePasswordWarn();

        lanSwitch.setChecked(ProxyService.isLanMode(p));
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
                updatePasswordWarn();
                toast(getString(R.string.password_autofilled));
            }
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
        findViewById(R.id.freeze_check).setOnClickListener(v -> showFreezeSelfCheck());

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
        clearStaleInstallSession();
        handleRestartIntent(getIntent());

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
        // 后台被系统冻结的判定(CMP-236):回前台这一刻断档最清晰(冻结已解、心跳还没恢复)。
        // 探测链在 ProxyService 侧,这里只负责判定结果落地成「提示 + 去加白」。
        if (ProxyService.onAppForeground(this) && !suppressFreezePrompt) {
            promptFreeze(ProxyService.prefs(this).getLong(ProxyService.KEY_FREEZE_GAP, 0));
        }
        suppressFreezePrompt = false;
    }

    @Override
    protected void onPause() {
        // 开始一段新的后台期(CMP-236):冻结只发生在后台。
        ProxyService.onAppBackground();
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
        boolean isDefault = getString(R.string.default_password).equals(currentPassword());
        passwordWarn.setVisibility(isDefault ? View.VISIBLE : View.GONE);
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

    @Override
    protected void onNewIntent(Intent intent) {
        // 面板的「重启代理」用 intent:// 唤起本 app（CMP-215）；singleTask 下走这里。
        super.onNewIntent(intent);
        setIntent(intent);
        handleRestartIntent(intent);
    }

    /**
     * 处理面板发来的「重启代理」请求（CMP-215）。
     * 面板跑在系统浏览器里、代理进程可能正卡死（连不上），没法直接调本 app 的接口，
     * 只能用 intent:// 把 app 唤起来由它来重启 —— 用户不用回 app 手点。
     */
    private void handleRestartIntent(Intent intent) {
        if (intent == null) {
            return;
        }
        // CMP-236:「去加白」点进来的 intent，打开应用详情页。
        handleWhitelistIntent(intent);
        if ("1".equals(intent.getStringExtra(EXTRA_FREEZE_TEST))) {
            // 验收演练：注入一次「被冻结」并直接弹提示。
            // 置 suppress：onCreate→onResume 那段会再判一次，免得连弹两遍。
            intent.removeExtra(EXTRA_FREEZE_TEST);
            ProxyService.injectFreezeForTest(45_000L);
            suppressFreezePrompt = true;
            promptFreeze(45_000L);
        }
        if (!"1".equals(intent.getStringExtra(EXTRA_RESTART))) {
            return;
        }
        // 消费掉：否则旋转屏幕重建 Activity 时会再重启一次。
        intent.removeExtra(EXTRA_RESTART);
        toast("正在重启代理…");
        stopProxy();
        // stopService 是异步的，等它把服务拆干净再拉起来，否则新的 onStartCommand
        // 会撞上「旧实例还 running」而被忽略。
        new Handler(Looper.getMainLooper()).postDelayed(this::startProxy, 500);
    }

    // ======================== 后台冻结提示与加白入口(CMP-236)========================

    /** 处理面板的「去加白」intent(CMP-236)。 */
    private void handleWhitelistIntent(Intent intent) {
        if (!"1".equals(intent.getStringExtra(EXTRA_WHITELIST))) {
            return;
        }
        intent.removeExtra(EXTRA_WHITELIST);
        openAppDetails();
    }

    /**
     * 疑似被系统冻结时的原生提示(CMP-236)。
     *
     * 只有 App 回到前台才提示:冻结期整个 uid 不调度,提示弹不出来也传不到面板。
     */
    private void promptFreeze(long gapMs) {
        String msg = (gapMs > 0 ? "刚才退到后台 " + (gapMs / 1000) + " 秒内代理没有响应。\n\n" : "")
                + "手机系统(ColorOS)可能把本应用的后台行为限制住了,建议加白:\n\n"
                + getString(R.string.whitelist_path)
                + "\n\n" + getString(R.string.whitelist_note)
                + "\n\n点「去加白」打开应用详情页,依次进入耗电管理。";
        new AlertDialog.Builder(this)
                .setTitle("疑似后台被冻结")
                .setMessage(msg)
                .setPositiveButton("去加白", (d, w) -> openAppDetails())
                .setNegativeButton("稍后", null)
                .show();
    }

    /**
     * 跳到本应用的系统详情页(CMP-236)。
     *
     * 不硬编 ColorOS 私有 intent:各版本 action/开关名都会变。落到应用详情页 +
     * 文案指路(耗电管理 → 耗电行为控制 → 完全允许后台行为)是实测能走通的路径。
     */
    private void openAppDetails() {
        suppressFreezePrompt = true;
        try {
            startActivity(new Intent(
                    Settings.ACTION_APPLICATION_DETAILS_SETTINGS,
                    Uri.parse("package:" + getPackageName())));
        } catch (Exception e) {
            // 个别 ROM 没这个页:退回全部应用列表页,至少让用户能自己找。
            try {
                startActivity(new Intent(Settings.ACTION_APPLICATION_SETTINGS));
            } catch (Exception e2) {
                toast("打不开系统设置页,请手动进入:设置 → 应用管理 → oc2api → 耗电管理");
                suppressFreezePrompt = false;
            }
        }
    }

    /** 自检入口(CMP-236):点一下就能看到当前是否疑似被冻结 / 加白是否生效。 */
    private void showFreezeSelfCheck() {
        int st = ProxyService.prefs(this).getInt(
                ProxyService.KEY_FREEZE_STATE, ProxyService.FREEZE_STATE_UNKNOWN);
        long gap = ProxyService.prefs(this).getLong(ProxyService.KEY_FREEZE_GAP, 0);
        String msg;
        if (st == ProxyService.FREEZE_STATE_FROZEN) {
            msg = "上次退到后台时疑似被系统冻结(心跳断档 " + (gap / 1000) + " 秒)。\n\n"
                    + "请按这个路径加白:\n" + getString(R.string.whitelist_path);
        } else if (st == ProxyService.FREEZE_STATE_OK) {
            msg = "上次退到后台没有再被冻结 —— 加白已生效。";
        } else {
            msg = "还没有可以判断的记录:先切到别的应用待一会儿,再回来点这里。";
        }
        new AlertDialog.Builder(this)
                .setTitle("后台冻结自检")
                .setMessage(msg)
                .setPositiveButton("去加白", (d, w) -> openAppDetails())
                .setNegativeButton("知道了", null)
                .show();
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
            int sessionId = Updater.install(this, apk);
            // 记下会话号：系统回调按它对号入座，旧会话迟到的结果不写当前界面（CMP-215）。
            ProxyService.prefs(this).edit()
                    .putInt(ProxyService.KEY_INSTALL_SESSION, sessionId)
                    .apply();
            setUpdateText("已交给系统安装器，请在系统弹窗里确认");
        } catch (IOException e) {
            busy = false;
            checkButton.setEnabled(true);
            setUpdateText("安装失败：" + e.getMessage());
        }
    }

    /**
     * 启动时清掉已经没有对应会话的「待安装」标记（CMP-215）。
     * PendingIntent 不是 one-shot 且从不取消，旧会话的结果可能迟到；
     * 连不上会话就当没有安装在进行，那些结果一律不写界面。
     */
    private void clearStaleInstallSession() {
        int sid = ProxyService.prefs(this).getInt(ProxyService.KEY_INSTALL_SESSION, 0);
        if (sid <= 0) {
            return;
        }
        PackageInstaller.SessionInfo si =
                getPackageManager().getPackageInstaller().getSessionInfo(sid);
        if (si == null) {
            ProxyService.prefs(this).edit().remove(ProxyService.KEY_INSTALL_SESSION).apply();
        }
    }

    private void onInstallResult(Intent intent) {
        int sid = intent.getIntExtra(Updater.EXTRA_SESSION_ID, 0);
        int pending = ProxyService.prefs(this).getInt(ProxyService.KEY_INSTALL_SESSION, 0);
        if (pending == 0 || sid != pending) {
            // 不是当前这次安装的结果：旧会话迟到、或压根没发起过安装。
            // 真机踩过：覆盖安装后第一次打开 App，版本行挂着上一次的
            // "INSTALL FAILED ABORTED: User rejected permissions"（CMP-215）。
            return;
        }
        busy = false;
        checkButton.setEnabled(true);
        int code = intent.getIntExtra(PackageInstaller.EXTRA_STATUS, PackageInstaller.STATUS_FAILURE);
        if (code == PackageInstaller.STATUS_PENDING_USER_ACTION) {
            // 系统把「确认安装」这一步交回调用方：必须把它给的 Intent 起起来，
            // 否则用户看不到任何界面、会话也不会往下走。
            // （真机首测就卡在这里：EXTRA_STATUS = -1，被我当初当成失败处理了。）
            // 这还是中间态：会话没结束，别清会话号，否则后面那条终态结果会被丢掉。
            Intent confirm = intent.getParcelableExtra(Intent.EXTRA_INTENT);
            setUpdateText("等待系统安装确认…");
            if (confirm != null) {
                confirm.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
                startActivity(confirm);
            } else {
                setUpdateText("等待系统安装确认…（系统未返回确认界面）");
            }
            return;
        }
        // 终态：这次会话结束了，之后再有同会话的结果也不认。
        ProxyService.prefs(this).edit().remove(ProxyService.KEY_INSTALL_SESSION).apply();
        if (code == PackageInstaller.STATUS_SUCCESS) {
            setUpdateText("安装完成，重启后生效");
            toast("更新完成");
            return;
        }
        String msg = intent.getStringExtra(PackageInstaller.EXTRA_STATUS_MESSAGE);
        setUpdateText(InstallMessage.failureText(code, msg));
    }
}