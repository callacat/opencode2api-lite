# Android 侧 R8 keep 规则
#
# 面板是 Go 进程自己吐的 HTML（内联在 main.go 里），页面内不含任何
# addJavascriptInterface 注册的桥——本 app 没有 WebView JS 接口，
# 因此没有「JS 接口被裁」的暴露面。若日后新增 @JavascriptInterface 方法，
# 必须在这里补 -keepclassmembers 并同步本注释。

# 组件由 manifest 引用，AGP 会自动 keep；显式写出以防 shrinkResources/R8 版本行为变化。
-keep class com.callacat.oc2api.MainActivity { *; }
-keep class com.callacat.oc2api.ProxyService { *; }

# 进程/日志相关的反射面很小，保留行号便于 logcat 定位。
-keepattributes SourceFile,LineNumberTable
-renamesourcefileattribute SourceFile
