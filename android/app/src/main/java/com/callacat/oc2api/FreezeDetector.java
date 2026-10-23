package com.callacat.oc2api;

/**
 * 「后台被系统冻结」判定（纯 Java，不碰 Android API ⇒ JVM 单测直接跑）。
 *
 * 真机 OnePlus PKG110 / ColorOS / Android 15 实测：ColorOS Hans 把整个 uid 的
 * cgroup 冻结（/sys/fs/cgroup/uid_10338/cgroup.freeze=1）⇒ **App 自己的线程
 * 也不调度** ⇒ 心跳断档；同 uid 的代理子进程 liboc2api.so 一起停摆 ⇒ 内核仍
 * LISTEN 但用户态不调度 ⇒ 面板 /health 超时、按钮全「保存失败」。
 *
 * 红线：不读 /sys/fs/cgroup 下 uid 对应的 cgroup.freeze —— 非 root 应用读不到自己 uid
 * 那个目录。只用 App 内可观测的替代信号：
 * 1) 心跳断档（本进程长时间没被调度 ⇒ 冻结）；
 * 2) 设备是醒着的（uptime 在涨）—— 把「息屏/Doze 整机睡着」排除掉；
 * 3) 断档发生在一段后台期内（冻结只发生在后台）。
 *
 * 只判「疑似」不做修复：解冻只能靠用户在 ColorOS 里加白，App 侧无权干预。
 *
 * 时间全部由调用方注入（elapsedRealtime / uptimeMillis），本类不碰真实时钟 ⇒
 * JVM 单测能把「断档」「整机睡着」「没进后台」三条分支都钉住。
 */
final class FreezeDetector {

    /**
     * 断档超过这个毫秒数才判「长时间没被调度」。
     * 与卡面「连续 N 次探活失败」同一量级（探活间隔 5s × 3 次 = 15s）。
     */
    static final long GAP_SUSPECT_MS = 15_000L;

    /** 上一次心跳的时刻（elapsedRealtime / uptimeMillis），没心跳过是 -1。 */
    private long lastTickElapsed = -1;
    private long lastTickUptime = -1;

    /** 本段后台期的起点（elapsedRealtime），不在后台时是 -1。 */
    private long bgStart = -1;

    /** 本段后台期是否判出冻结。 */
    private boolean frozen;
    private long frozenGapMs;

    /**
     * App 切到后台（MainActivity.onPause 调）：开始一段新的后台期。
     * 上一段的结论作废 —— 判定回答的是「这一段后台期里有没有被冻」。
     */
    void onBackground(long elapsed) {
        bgStart = elapsed;
        frozen = false;
        frozenGapMs = 0;
    }

    /**
     * App 切回前台（MainActivity.onResume 调）：结束后台期并补判一次。
     *
     * 冻结期整个进程不调度 ⇒ 心跳线程根本跑不到下一拍 ⇒ 解冻后第一拍才看见
     * 断档；而「回前台」这一刻是断档最清晰的观察点（冻结已解、心跳还没恢复）。
     *
     * @param elapsed SystemClock.elapsedRealtime()（含设备睡眠时间）
     * @param uptime  SystemClock.uptimeMillis()（不含设备睡眠时间）
     * @return 本段后台期是否判出冻结
     */
    boolean onForeground(long elapsed, long uptime) {
        evaluate(elapsed, uptime);
        bgStart = -1;
        return frozen;
    }

    /**
     * 心跳：本进程被调度了一次（ProxyService 的心跳线程调）。
     *
     * @return 本段后台期是否判出冻结
     */
    boolean onTick(long elapsed, long uptime) {
        evaluate(elapsed, uptime);
        lastTickElapsed = elapsed;
        lastTickUptime = uptime;
        return frozen;
    }

    /**
     * 判据（三个条件都要满足）：
     * 1) 心跳断档 ≥ 阈值 —— 本进程长时间没被调度；
     * 2) 这段断档落在后台期内 —— 冻结只发生在后台（前台卡顿不算）；
     * 3) uptime 也涨了同样多 —— 设备是醒着的。整机睡过去（息屏/Doze）时
     *    只有 elapsed 涨、uptime 不涨，那不是 ColorOS 的锅。
     */
    private void evaluate(long elapsed, long uptime) {
        if (frozen || bgStart < 0 || lastTickElapsed < 0) {
            return;
        }
        long gap = elapsed - lastTickElapsed;
        long uptimeGap = uptime - lastTickUptime;
        long inBackground = elapsed - bgStart;
        if (gap < GAP_SUSPECT_MS || inBackground < GAP_SUSPECT_MS) {
            return;
        }
        if (uptimeGap < GAP_SUSPECT_MS) {
            return;
        }
        frozen = true;
        frozenGapMs = gap;
    }

    /** 本段后台期是否判出冻结。 */
    boolean frozen() {
        return frozen;
    }

    /** 触发判定的那次断档有多长（毫秒），没判过返回 0。 */
    long frozenGapMs() {
        return frozenGapMs;
    }

    /**
     * 探活线程换了一轮（子进程重启）：换掉基准，别把「重启探活」误判成断档。
     */
    void resetTick(long elapsed, long uptime) {
        lastTickElapsed = elapsed;
        lastTickUptime = uptime;
    }

    /** 服务停了（没心跳可言）：清空基准与结论，别让陈旧断档误报冻结。 */
    void reset() {
        lastTickElapsed = -1;
        lastTickUptime = -1;
        bgStart = -1;
        frozen = false;
        frozenGapMs = 0;
    }

    /** 演练入口：直接标记一次冻结（真机加白后冻结不再复现，用它验提示链路）。 */
    void injectFreeze(long gapMs) {
        frozen = true;
        frozenGapMs = gapMs;
    }
}
