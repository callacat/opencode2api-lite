package com.callacat.oc2api;

import static org.junit.Assert.assertArrayEquals;
import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import org.junit.Test;

/**
 * 版本比对单测。
 *
 * 重点是 "v" 前缀：「v1.0.4」若不做归一化，逐段取前导数字会得到 [0,0,4]，
 * 与自身 "1.0.4" = [1,0,4] 不等 ⇒ 每次都误报「有更新」。CI 注入的 tag 与 GitHub 的
 * tag_name 都带 v，两侧形式必须统一后再比。
 */
public class VersionCompareTest {

    @Test
    public void normalizeStripsTagPrefix() {
        assertEquals("1.0.4", VersionCompare.normalize("v1.0.4"));
        assertEquals("1.0.4", VersionCompare.normalize("V1.0.4"));
        assertEquals("1.0.4", VersionCompare.normalize("  v1.0.4 "));
        assertEquals("dev", VersionCompare.normalize("dev"));
        assertEquals("", VersionCompare.normalize(null));
        // 只有开头一个 v 被吃掉，中间的不动。
        assertEquals("1.0.4", VersionCompare.normalize("v1.0.4"));
    }

    @Test
    public void parseTakesLeadingDigitsOnly() {
        assertArrayEquals(new int[]{1, 0, 4}, VersionCompare.parse("v1.0.4"));
        assertArrayEquals(new int[]{1, 2, 3}, VersionCompare.parse("1.2.3"));
        assertArrayEquals(new int[]{0, 0, 0}, VersionCompare.parse("dev"));
        // 非数字后缀忽略；只比三段。
        assertArrayEquals(new int[]{1, 0, 5}, VersionCompare.parse("v1.0.5-androidtest"));
        assertArrayEquals(new int[]{1, 0, 4}, VersionCompare.parse("1.0.4.9"));
        assertArrayEquals(new int[]{1, 1, 0}, VersionCompare.parse("1.1"));
    }

    @Test
    public void sameVersionInDifferentFormsIsNotAnUpdate() {
        assertFalse(VersionCompare.isNewer("v1.0.4", "1.0.4"));
        assertFalse(VersionCompare.isNewer("1.0.4", "v1.0.4"));
        assertFalse(VersionCompare.isNewer("v1.0.4", "v1.0.4"));
        assertEquals(0, VersionCompare.compare("v1.0.4", "1.0.4"));
    }

    @Test
    public void newerAndOlder() {
        assertTrue(VersionCompare.isNewer("v1.0.5", "1.0.4"));
        assertFalse(VersionCompare.isNewer("v1.0.4", "1.0.5"));
        assertTrue(VersionCompare.isNewer("1.1", "1.0.9"));
        assertTrue(VersionCompare.isNewer("2.0.0", "1.99.99"));
        assertEquals(-1, VersionCompare.compare("1.0.4", "1.0.5"));
        assertEquals(1, VersionCompare.compare("1.0.5", "1.0.4"));
    }

    /** 带后缀的 tag（v1.0.5-rc1 / -androidtest）按前三段数字比较，与 Go 侧 versionNumbers 一致。 */
    @Test
    public void suffixedTagsCompareByLeadingNumbers() {
        assertTrue(VersionCompare.isNewer("v1.0.5-androidtest", "1.0.4"));
        assertFalse(VersionCompare.isNewer("v1.0.5-androidtest", "1.0.5"));
        assertTrue(VersionCompare.isVersioned("v1.0.5-androidtest"));
    }

    /** 未注入版本的构建按 0.0.0 处理：任何正式版本都比它新（app 自更新要靠这条才能跑起来）。 */
    @Test
    public void unversionedBuildIsTreatedAsOldest() {
        assertFalse(VersionCompare.isVersioned("dev"));
        assertFalse(VersionCompare.isVersioned(""));
        assertFalse(VersionCompare.isVersioned(null));
        assertTrue(VersionCompare.isNewer("v1.0.4", "dev"));
        assertFalse(VersionCompare.isNewer("dev", "1.0.4"));
        assertFalse(VersionCompare.isNewer("dev", "dev"));
    }

    /** 远端拿不到版本号（Release 没 tag_name 之类）时不提示更新 —— 没有可比的对象。 */
    @Test
    public void missingRemoteVersionNeverReportsAnUpdate() {
        assertFalse(VersionCompare.isNewer("", "1.0.4"));
        assertFalse(VersionCompare.isNewer(null, "1.0.4"));
        assertFalse(VersionCompare.isNewer(null, null));
    }

    /**
     * 本机版本读不到时按最旧处理（与 dev 同一条规则：不是版本号就当 0.0.0）。
     * 真机上 versionName 一定有值，这条只是锁住规则本身，别把方向写反。
     */
    @Test
    public void missingLocalVersionIsTreatedAsOldest() {
        assertTrue(VersionCompare.isNewer("v1.0.5", ""));
        assertTrue(VersionCompare.isNewer("v1.0.5", null));
    }
}
