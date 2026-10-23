package com.callacat.oc2api;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import org.junit.Test;

/**
 * CMP-236：冻结判定的纯 JVM 单测。
 *
 * 时间全部注入（不碰真实时钟），所以三条关键分支都能钉死：
 * - 后台期 + 心跳断档 + 设备醒着 ⇒ 判「疑似被冻结」；
 * - 心跳断档但设备睡着（uptime 不涨）⇒ **不**判（息屏/Doze，不是 ColorOS 的锅）；
 * - 没进过后台的断档 ⇒ **不**判（前台 GC 卡顿不算冻结）。
 */
public class FreezeDetectorTest {

    /** 起点：随便一个非零时刻。 */
    private static final long T0 = 1_000_000L;

    @Test
    public void normalHeartbeatIsNotFrozen() {
        FreezeDetector d = new FreezeDetector();
        d.onBackground(T0);
        // 正常后台：探活心跳每 5s 一拍。
        assertFalse(d.onTick(T0, T0));
        assertFalse(d.onTick(T0 + 5_000, T0 + 5_000));
        assertFalse(d.onTick(T0 + 10_000, T0 + 10_000));
        assertFalse(d.onForeground(T0 + 10_000, T0 + 10_000));
        assertFalse(d.frozen());
    }

    @Test
    public void longGapInBackgroundWithDeviceAwakeIsFrozen() {
        FreezeDetector d = new FreezeDetector();
        d.onBackground(T0);
        d.onTick(T0, T0);
        // 冻结 30s：elapsed 与 uptime 一起涨（设备一直醒着，只是本进程没被调度）。
        assertTrue(d.onTick(T0 + 30_000, T0 + 30_000));
        assertTrue(d.frozen());
        assertEquals(30_000L, d.frozenGapMs());
    }

    @Test
    public void longGapWithDeviceAsleepIsNotFrozen() {
        FreezeDetector d = new FreezeDetector();
        d.onBackground(T0);
        d.onTick(T0, T0);
        // 整机睡了 30s：elapsed 涨 30s，uptime 几乎不涨（深睡不计入 uptime）。
        assertFalse(d.onTick(T0 + 30_000, T0 + 100));
        assertFalse(d.frozen());
    }

    @Test
    public void gapBelowThresholdIsNotFrozen() {
        FreezeDetector d = new FreezeDetector();
        d.onBackground(T0);
        d.onTick(T0, T0);
        // 断档 8s：不到阈值（正常一轮 = 5s 睡眠 + 最多 3s 探活超时）。
        assertFalse(d.onTick(T0 + 8_000, T0 + 8_000));
        assertFalse(d.frozen());
    }

    @Test
    public void foregroundDetectsFreezeThatTickMissed() {
        FreezeDetector d = new FreezeDetector();
        d.onBackground(T0);
        d.onTick(T0, T0);
        // 冻结期 App 整个不调度 ⇒ 探活线程根本没跑到下一拍。
        // 回到前台这一刻用「当前时刻 vs 上一拍」补判。
        assertTrue(d.onForeground(T0 + 30_000, T0 + 30_000));
        assertTrue(d.frozen());
        assertEquals(30_000L, d.frozenGapMs());
    }

    @Test
    public void foregroundWithoutBackgroundIsNotFrozen() {
        FreezeDetector d = new FreezeDetector();
        d.onTick(T0, T0);
        // 没进过后台 ⇒ 不判（前台卡顿/GC 不是冻结）。
        assertFalse(d.onForeground(T0 + 30_000, T0 + 30_000));
        assertFalse(d.frozen());
    }

    @Test
    public void newBackgroundPeriodClearsPreviousVerdict() {
        FreezeDetector d = new FreezeDetector();
        d.onBackground(T0);
        d.onTick(T0, T0);
        assertTrue(d.onTick(T0 + 30_000, T0 + 30_000));
        assertTrue(d.frozen());
        // 新的一段后台期：上一段的结论作废。
        d.onBackground(T0 + 30_000);
        assertFalse(d.frozen());
    }

    @Test
    public void resetClearsStaleBaseline() {
        FreezeDetector d = new FreezeDetector();
        d.onBackground(T0);
        d.onTick(T0, T0);
        // 服务停了：没有心跳可言，陈旧基准必须清掉，否则回来就误报。
        d.reset();
        assertFalse(d.onForeground(T0 + 30_000, T0 + 30_000));
        assertFalse(d.frozen());
    }

    @Test
    public void resetTickAvoidsFalsePositiveOnProbeRestart() {
        FreezeDetector d = new FreezeDetector();
        d.onBackground(T0);
        d.onTick(T0, T0);
        // 子进程重启 ⇒ 探活线程换了一轮，基准重置，不该把这段算成断档。
        d.resetTick(T0 + 20_000, T0 + 20_000);
        assertFalse(d.onTick(T0 + 25_000, T0 + 25_000));
        assertFalse(d.frozen());
    }

    @Test
    public void injectedFreezeIsReportedForRehearsal() {
        FreezeDetector d = new FreezeDetector();
        d.onBackground(T0);
        // 演练入口：真机加白后冻结不再复现，用它验「提示链路」还在。
        d.injectFreeze(42_000);
        assertTrue(d.onTick(T0 + 1_000, T0 + 1_000));
        assertTrue(d.frozen());
    }
}
