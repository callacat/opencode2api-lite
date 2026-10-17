package com.callacat.oc2api;

import android.os.Handler;
import android.os.Looper;

import java.io.BufferedReader;
import java.io.BufferedWriter;
import java.io.File;
import java.io.FileInputStream;
import java.io.FileWriter;
import java.io.IOException;
import java.io.InputStreamReader;
import java.nio.charset.StandardCharsets;
import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.Deque;
import java.util.List;

/**
 * 代理输出的日志缓冲（CMP-118 日志页的数据源）。
 *
 * 两份存储，各有各的用处：
 * - 内存：Deque 环形，固定上限 1000 行。**不能换成无界 List** —— 代理长跑几小时，
 *   按每秒几行算也能堆到几万行，直接把 app 的堆吃穿。
 * - 落盘：filesDir/oc2api.log，超过 256KB 就整个重来。兜的是"进程被杀后重开"
 *   —— 内存环形随进程一起没了，光有它的话关屏久了再进来日志页会是空的。
 *
 * 通知用「去抖 + 主线程派发」：pump 线程可能一秒钟灌进几十行，逐行 post 会让
 * Activity 每行都重排一次 1000 行的 TextView。
 */
final class LogBuffer {

    private static final int MAX_LINES = 1000;
    private static final long MAX_FILE_BYTES = 256L * 1024;
    private static final String LOG_NAME = "oc2api.log";

    private static final Deque<String> LINES = new ArrayDeque<>();
    private static final List<Runnable> LISTENERS = new ArrayList<>();
    private static final Handler MAIN = new Handler(Looper.getMainLooper());

    /** 每轮追加只派发一次；true 表示已经有一个 drain 排在主线程队列里。 */
    private static boolean drainPending;

    private static File file;
    private static BufferedWriter writer;

    private LogBuffer() {
    }

    static synchronized void append(File filesDir, String line) {
        LINES.addLast(line);
        while (LINES.size() > MAX_LINES) {
            LINES.removeFirst();
        }
        writeFile(filesDir, line);
        if (!drainPending) {
            drainPending = true;
            MAIN.post(DRAIN);
        }
    }

    /** 当前全部日志。内存空（进程重启过）时回落到落盘文件。 */
    static synchronized List<String> snapshot(File filesDir) {
        if (LINES.isEmpty()) {
            readFile(filesDir);
        }
        return new ArrayList<>(LINES);
    }

    static synchronized void addListener(Runnable r) {
        if (!LISTENERS.contains(r)) {
            LISTENERS.add(r);
        }
    }

    static synchronized void removeListener(Runnable r) {
        LISTENERS.remove(r);
    }

    private static final Runnable DRAIN = () -> {
        List<Runnable> targets;
        synchronized (LogBuffer.class) {
            drainPending = false;
            targets = new ArrayList<>(LISTENERS);
        }
        for (Runnable r : targets) {
            r.run();
        }
    };

    // ==================== 落盘 ====================

    private static void writeFile(File filesDir, String line) {
        try {
            if (writer == null) {
                File f = new File(filesDir, LOG_NAME);
                // 超上限就整个重来：按行算大小要全读一遍，而这里本来也只是给人看个近况。
                if (f.isFile() && f.length() > MAX_FILE_BYTES && !f.delete()) {
                    return;
                }
                writer = new BufferedWriter(new FileWriter(f, true));
                file = f;
            }
            writer.write(line);
            writer.write('\n');
            writer.flush();
        } catch (IOException e) {
            // 落盘失败不影响代理本身，只是重开后看不到历史；下次追加再试。
            writer = null;
        }
    }

    private static void readFile(File filesDir) {
        File f = file != null && file.isFile() ? file : new File(filesDir, LOG_NAME);
        if (!f.isFile()) {
            return;
        }
        Deque<String> tail = new ArrayDeque<>();
        try (BufferedReader r = new BufferedReader(
                new InputStreamReader(new FileInputStream(f), StandardCharsets.UTF_8))) {
            String line;
            while ((line = r.readLine()) != null) {
                tail.addLast(line);
                if (tail.size() > MAX_LINES) {
                    tail.removeFirst();
                }
            }
        } catch (IOException e) {
            return;
        }
        LINES.addAll(tail);
    }
}