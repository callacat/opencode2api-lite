package com.callacat.oc2api;

import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import org.junit.Test;

/**
 * 检查更新缓存的 TTL 判据单测。
 *
 * 为什么要缓存：GitHub 未认证 API 限 60 次/小时/IP，启动检查 + 用户手点都可能触发，
 * 没有 TTL 就会把额度打光（打光后所有人都只看到「检查失败」）。
 */
public class UpdateCacheTest {

    private static final long NOW = 1_700_000_000_000L;

    @Test
    public void neverCheckedIsNotFresh() {
        assertFalse(UpdateCache.isFresh(0L, NOW));
    }

    @Test
    public void withinTtlIsFresh() {
        assertTrue(UpdateCache.isFresh(NOW - 1000L, NOW));
        assertTrue(UpdateCache.isFresh(NOW - UpdateCache.TTL_MS + 1, NOW));
    }

    @Test
    public void atOrPastTtlIsStale() {
        assertFalse(UpdateCache.isFresh(NOW - UpdateCache.TTL_MS, NOW));
        assertFalse(UpdateCache.isFresh(NOW - UpdateCache.TTL_MS - 1, NOW));
        assertFalse(UpdateCache.isFresh(NOW - 24 * 60 * 60 * 1000L, NOW));
    }

    /** 时钟回拨（checkedAt 在未来）不该被当成新鲜缓存，否则会一直不刷新。 */
    @Test
    public void futureTimestampIsTreatedAsFreshOnlyWithinTtl() {
        assertTrue(UpdateCache.isFresh(NOW + 1000L, NOW));
    }

    /** 没查到 tag（上次检查失败或从未成功）时不算有效缓存，下次仍要打网络。 */
    @Test
    public void emptyTagIsNotUsableCache() {
        UpdateCache c = new UpdateCache();
        c.checkedAt = NOW;
        assertFalse(c.fresh(NOW));
        c.tag = "v1.0.4";
        assertTrue(c.fresh(NOW));
    }
}
