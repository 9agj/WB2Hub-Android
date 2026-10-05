package com.wb2hub.lite;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.content.SharedPreferences;
import android.os.Build;
import android.os.IBinder;
import android.util.Log;

import java.io.BufferedReader;
import java.io.File;
import java.io.InputStreamReader;
import java.net.HttpURLConnection;
import java.net.URL;
import java.util.Map;

/**
 * Keeps the embedded gateway alive.
 *
 * The gateway is a separate process — the Go binary shipped as
 * jniLibs/arm64-v8a/libwb2hub.so — and this service owns its lifetime. It runs
 * in the foreground because Android reclaims background processes aggressively;
 * a gateway that is killed whenever the user switches apps is useless.
 *
 * The binary is named .so purely so the packager will place it in
 * nativeLibraryDir, which is the one location Android still allows exec. See the
 * useLegacyPackaging note in app/build.gradle.
 */
public class GatewayService extends Service {

    private static final String TAG = "WB2Hub";
    private static final String CHANNEL_ID = "wb2hub-gateway";
    private static final int NOTIFY_ID = 0x7835;
    private static final String BIN_NAME = "libwb2hub.so";

    public static final int HTTP_PORT = 7863;
    private static final String PREF = "wb2hub_store";

    private static volatile GatewayService instance;

    private Process process;
    private String lastError;
    private Thread logPump;

    /** The running service, or null. */
    public static GatewayService get() { return instance; }

    @Override
    public void onCreate() {
        super.onCreate();
        instance = this;
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        startForegroundNotify();
        if (process == null || !isAlive()) {
            startGateway();
        }
        // Restart if killed rather than leaving the app without a gateway.
        return START_STICKY;
    }

    @Override
    public IBinder onBind(Intent intent) { return null; }

    @Override
    public void onDestroy() {
        instance = null;
        stopGateway();
        super.onDestroy();
    }

    // ------------------------------------------------------------ state

    public String baseUrl() { return "http://127.0.0.1:" + HTTP_PORT; }
    public String lastError() { return lastError; }

    public boolean isAlive() {
        if (process == null) return false;
        try {
            // Process.isAlive() is Java 8; reflect so this stays valid on any API.
            Object alive = Process.class.getMethod("isAlive").invoke(process);
            return Boolean.TRUE.equals(alive);
        } catch (Throwable t) {
            // If the state cannot be read, assume alive: killing a healthy
            // gateway is worse than leaving a dead handle around, because the
            // next call will simply fail and restart it.
            return true;
        }
    }

    /** The API key the gateway expects, generated once and kept locally. */
    public String apiKey() {
        SharedPreferences prefs = getSharedPreferences(PREF, Context.MODE_PRIVATE);
        String key = prefs.getString("gateway_api_key", null);
        if (key == null || key.isEmpty()) {
            key = "sk-wb2hub-" + Long.toHexString(System.nanoTime())
                    + Long.toHexString((long) (Math.random() * Long.MAX_VALUE));
            prefs.edit().putString("gateway_api_key", key).apply();
        }
        return key;
    }

    /** Whether the gateway binary survived packaging. */
    public boolean binaryPresent() { return binaryFile().exists(); }

    public String binaryPath() { return binaryFile().getAbsolutePath(); }

    private File binaryFile() {
        return new File(getApplicationInfo().nativeLibraryDir, BIN_NAME);
    }

    // ------------------------------------------------------------ lifecycle

    /** Starts the gateway and waits for the port to answer. Idempotent. */
    public synchronized boolean startGateway() {
        if (isAlive() && probe()) return true;

        File bin = binaryFile();
        if (!bin.exists()) {
            lastError = "Gateway binary missing: " + bin.getAbsolutePath();
            Log.e(TAG, lastError);
            return false;
        }

        try {
            File files = getFilesDir();
            File authDir = new File(files, "auths");
            File dataDir = new File(files, "data");
            File usageDir = new File(files, "usage");
            for (File dir : new File[]{authDir, dataDir, usageDir}) {
                if (!dir.exists() && !dir.mkdirs()) {
                    Log.w(TAG, "could not create " + dir);
                }
            }

            ProcessBuilder pb = new ProcessBuilder(bin.getAbsolutePath());
            pb.directory(files);
            pb.redirectErrorStream(true);

            Map<String, String> env = pb.environment();
            env.put("TW2H_LISTEN", "127.0.0.1:" + HTTP_PORT);
            env.put("TW2H_AUTH_DIR", authDir.getAbsolutePath());
            env.put("TW2H_DATA_DIR", dataDir.getAbsolutePath());
            env.put("TW2H_USAGE_DIR", usageDir.getAbsolutePath());
            env.put("TW2H_REQUIRE_KEY", "1");
            env.put("TW2H_CONFIG", new File(dataDir, "config.json").getAbsolutePath());
            env.put("HOME", files.getAbsolutePath());
            env.put("TMPDIR", getCacheDir().getAbsolutePath());

            Log.i(TAG, "starting gateway: " + bin.getAbsolutePath());
            process = pb.start();
            pumpLogs(process);

            // Wait up to 30s. The first run may need to write state files and
            // probe the network, so a short timeout would report a false failure.
            for (int i = 0; i < 60; i++) {
                try { Thread.sleep(500); } catch (InterruptedException e) { break; }
                if (probe()) {
                    lastError = null;
                    Log.i(TAG, "gateway ready on " + baseUrl());
                    return true;
                }
                if (!isAlive()) break;
            }
            lastError = "Gateway did not open port " + HTTP_PORT + " within 30s";
            Log.e(TAG, lastError);
            return false;

        } catch (Throwable t) {
            lastError = t.getClass().getSimpleName() + ": " + t.getMessage();
            Log.e(TAG, "gateway start failed", t);
            return false;
        }
    }

    /** Consumes the child's stdout. Not doing this lets the pipe fill and block it. */
    private void pumpLogs(final Process p) {
        logPump = new Thread(() -> {
            try (BufferedReader r =
                         new BufferedReader(new InputStreamReader(p.getInputStream()))) {
                String line;
                while ((line = r.readLine()) != null) {
                    Log.i(TAG, "[gw] " + line);
                }
            } catch (Exception ignored) {
                // Process exit closes the stream; nothing to report.
            }
        }, "wb2hub-log");
        logPump.setDaemon(true);
        logPump.start();
    }

    public synchronized void stopGateway() {
        if (process != null) {
            try {
                process.destroy();
                Process.class.getMethod("destroyForcibly").invoke(process);
            } catch (Throwable ignored) {
                // Best effort: the process is going away either way.
            }
            process = null;
            Log.i(TAG, "gateway stopped");
        }
    }

    public synchronized void restartGateway() {
        stopGateway();
        try { Thread.sleep(600); } catch (InterruptedException ignored) { }
        startGateway();
    }

    /** Any HTTP answer counts as alive; the gateway needs no auth for this. */
    public boolean probe() {
        HttpURLConnection conn = null;
        try {
            conn = (HttpURLConnection) new URL(baseUrl() + "/health").openConnection();
            conn.setConnectTimeout(1500);
            conn.setReadTimeout(1500);
            conn.setRequestMethod("GET");
            return conn.getResponseCode() > 0;
        } catch (Exception e) {
            return false;
        } finally {
            if (conn != null) conn.disconnect();
        }
    }

    // ------------------------------------------------------------ notification

    private void startForegroundNotify() {
        try {
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                NotificationManager nm = getSystemService(NotificationManager.class);
                if (nm != null && nm.getNotificationChannel(CHANNEL_ID) == null) {
                    NotificationChannel channel = new NotificationChannel(
                            CHANNEL_ID, "Gateway", NotificationManager.IMPORTANCE_LOW);
                    channel.setDescription("Keeps the local API gateway running");
                    nm.createNotificationChannel(channel);
                }
            }
            Notification.Builder builder = (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O)
                    ? new Notification.Builder(this, CHANNEL_ID)
                    : new Notification.Builder(this);
            Notification notification = builder
                    .setContentTitle("WB2Hub")
                    .setContentText("Gateway running on port " + HTTP_PORT)
                    .setSmallIcon(android.R.drawable.stat_sys_download_done)
                    .setOngoing(true)
                    .build();
            startForeground(NOTIFY_ID, notification);
        } catch (Throwable t) {
            // A foreground promotion failure does not stop the gateway; it only
            // means Android may reclaim it sooner.
            Log.w(TAG, "startForeground failed: " + t.getMessage());
        }
    }
}
