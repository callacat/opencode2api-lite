package com.callacat.oc2api;

import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import org.junit.Test;

/**
 * CMP-215 ②：安装失败文案的映射表。
 * 判据是「用户看到的是中文 + 下一步」，所以这里同时钉住「该说什么」和「不许出现什么」。
 */
public class InstallMessageTest {

    private static final int FAILURE = 2;

    @Test
    public void userRejectedInstallGetsChineseWithNextStep() {
        // 真机原串：INSTALL FAILED ABORTED: User rejected permissions
        String s = InstallMessage.failureText(FAILURE, "INSTALL FAILED ABORTED: User rejected permissions");
        assertTrue(s, s.contains("安装未知应用"));
        assertTrue(s, s.contains("设置"));
        assertFalse("不能再把系统英文串甩给用户: " + s, s.toUpperCase().contains("INSTALL FAILED"));
        assertFalse(s, s.toUpperCase().contains("USER REJECTED"));
    }

    @Test
    public void insufficientStorageSaysSo() {
        String s = InstallMessage.failureText(FAILURE, "INSTALL_FAILED_INSUFFICIENT_STORAGE");
        assertTrue(s, s.contains("存储空间不足"));
        assertFalse(s, s.contains("INSTALL_FAILED"));
    }

    @Test
    public void downgradeIsExplained() {
        String s = InstallMessage.failureText(FAILURE, "INSTALL_FAILED_VERSION_DOWNGRADE");
        assertTrue(s, s.contains("降级"));
        assertTrue(s, s.contains("升级"));
    }

    @Test
    public void unknownFailureKeepsRawTextAndSystemCode() {
        String s = InstallMessage.failureText(FAILURE, "INSTALL_FAILED_SOMETHING_NEW");
        assertTrue("认不出的码要保留原文便于排查: " + s, s.contains("INSTALL_FAILED_SOMETHING_NEW"));
        assertTrue("要带系统码: " + s, s.contains("status=2"));
    }

    @Test
    public void emptyMessageStillCarriesSystemCode() {
        String nullMsg = InstallMessage.failureText(FAILURE, null);
        assertTrue(nullMsg, nullMsg.contains("status=2"));
        assertFalse("不能拼出「：null」: " + nullMsg, nullMsg.contains("null"));

        String blankMsg = InstallMessage.failureText(7, "   ");
        assertTrue(blankMsg, blankMsg.contains("status=7"));
        assertFalse(blankMsg, blankMsg.contains("："));
    }
}
