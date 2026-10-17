package com.callacat.oc2api;

import android.Manifest;
import android.app.Activity;
import android.content.Context;
import android.content.Intent;
import android.content.SharedPreferences;
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

/**
 * 唯一的界面：几个开关 + 一个 WebView 直接复用代理自带的管理面板。
 * 不写业务 UI —— 面板 HTML 由 Go 进程内联吐出来，这里只是把它显示出来。
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
    private WebView webView;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(R.layout.activity_main);

        portInput = findViewById(R.id.port);
        passwordInput = findViewById(R.id.password);
        lanSwitch = findViewById(R.id.lan);
        debugBox = findViewById(R.id.debug);
        status = findViewById(R.id.status);
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

        findViewById(R.id.start).setOnClickListener(v -> onStart());
        findViewById(R.id.stop).setOnClickListener(v -> onStop());
        Button battery = findViewById(R.id.battery);
        battery.setOnClickListener(v -> requestIgnoreBatteryOptimizations());

        if (Build.VERSION.SDK_INT >= 33
                && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS)
                != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, REQ_NOTIFICATIONS);
        }
    }

    private void onStart() {
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

    private void onStop() {
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
