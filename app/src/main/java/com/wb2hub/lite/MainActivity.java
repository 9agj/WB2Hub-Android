package com.wb2hub.lite;

import android.app.Activity;
import android.content.Intent;
import android.graphics.Typeface;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.text.InputType;
import android.util.TypedValue;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.widget.Button;
import android.widget.EditText;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.TextView;
import android.widget.Toast;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.net.HttpURLConnection;
import java.net.URL;

/**
 * The app's only screen.
 *
 * The gateway is a headless HTTP service, so the UI is a status board plus the
 * two things that actually need a finger: which accounts are loaded and what
 * the gateway's base URL and key are, so they can be pasted into a client.
 *
 * Deliberately built from platform views rather than a WebView: the embedded
 * gateway serves JSON only, and loading a remote page to render local state
 * would add a network dependency to a local-only tool.
 */
public class MainActivity extends Activity {

    private TextView statusLine;
    private TextView detailView;
    private TextView endpointView;
    private final Handler ui = new Handler(Looper.getMainLooper());

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(buildUi());

        // The service is started here rather than in Application: this is the
        // first moment the user has expressed intent to run the gateway.
        Intent intent = new Intent(this, GatewayService.class);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            startForegroundService(intent);
        } else {
            startService(intent);
        }

        if (Build.VERSION.SDK_INT >= 33) {
            requestPermissions(new String[]{"android.permission.POST_NOTIFICATIONS"}, 1);
        }
    }

    @Override
    protected void onResume() {
        super.onResume();
        refresh();
    }

    // ------------------------------------------------------------ UI

    private View buildUi() {
        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setBackgroundColor(color(com.wb2hub.lite.R.color.bg));
        int pad = dp(20);
        root.setPadding(pad, pad, pad, pad);

        TextView title = new TextView(this);
        title.setText("WB2Hub");
        title.setTextColor(color(com.wb2hub.lite.R.color.text));
        title.setTextSize(TypedValue.COMPLEX_UNIT_SP, 26);
        title.setTypeface(Typeface.DEFAULT_BOLD);
        root.addView(title);

        TextView subtitle = new TextView(this);
        subtitle.setText("OpenAI-compatible gateway for CodeBuddy / WorkBuddy");
        subtitle.setTextColor(color(com.wb2hub.lite.R.color.muted));
        subtitle.setTextSize(TypedValue.COMPLEX_UNIT_SP, 12);
        root.addView(subtitle);

        statusLine = new TextView(this);
        statusLine.setTextSize(TypedValue.COMPLEX_UNIT_SP, 15);
        statusLine.setPadding(0, dp(18), 0, dp(6));
        root.addView(statusLine);

        endpointView = new TextView(this);
        endpointView.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        endpointView.setTextColor(color(com.wb2hub.lite.R.color.muted));
        endpointView.setPadding(0, 0, 0, dp(14));
        endpointView.setTextIsSelectable(true);
        root.addView(endpointView);

        root.addView(button("Refresh", v -> refresh()));
        root.addView(button("Restart gateway", v -> restartGateway()));
        root.addView(button("Add account (paste credential)", v -> showImportDialog()));

        detailView = new TextView(this);
        detailView.setTextSize(TypedValue.COMPLEX_UNIT_SP, 12);
        detailView.setTextColor(color(com.wb2hub.lite.R.color.muted));
        detailView.setTypeface(Typeface.MONOSPACE);
        detailView.setPadding(0, dp(16), 0, 0);
        detailView.setTextIsSelectable(true);

        ScrollView scroll = new ScrollView(this);
        scroll.addView(detailView);
        root.addView(scroll, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, 0, 1f));

        return root;
    }

    private Button button(String label, View.OnClickListener onClick) {
        Button b = new Button(this);
        b.setText(label);
        b.setAllCaps(false);
        b.setOnClickListener(onClick);
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.topMargin = dp(6);
        b.setLayoutParams(lp);
        return b;
    }

    private int color(int res) {
        return Build.VERSION.SDK_INT >= 23
                ? getColor(res)
                : getResources().getColor(res, null);
    }

    private int dp(int value) {
        return Math.round(value * getResources().getDisplayMetrics().density);
    }

    // ------------------------------------------------------------ actions

    private void restartGateway() {
        GatewayService svc = GatewayService.get();
        if (svc == null) {
            startForegroundServiceCompat();
            ui.postDelayed(this::refresh, 1500);
            return;
        }
        new Thread(() -> {
            svc.restartGateway();
            ui.postDelayed(this::refresh, 400);
        }, "wb2hub-restart").start();
        toast("Restarting…");
    }

    private void startForegroundServiceCompat() {
        Intent intent = new Intent(this, GatewayService.class);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            startForegroundService(intent);
        } else {
            startService(intent);
        }
    }

    /** Live status: process state, port, and account counts from /health. */
    private void refresh() {
        GatewayService svc = GatewayService.get();
        final String key = svc != null ? svc.apiKey() : "";

        new Thread(() -> {
            GatewayService s = GatewayService.get();
            boolean alive = s != null && s.probe();
            String detail = null;
            String error = s != null ? s.lastError() : "service not started";
            if (alive) {
                detail = httpGet(GatewayService.HTTP_PORT, "/admin/overview", key);
            }
            final boolean fAlive = alive;
            final String fDetail = detail;
            final String fError = error;
            ui.post(() -> render(fAlive, fDetail, fError));
        }, "wb2hub-refresh").start();
    }

    private void render(boolean alive, String detail, String error) {
        statusLine.setText(alive ? "● Running" : "● Stopped");
        statusLine.setTextColor(color(alive
                ? com.wb2hub.lite.R.color.ok : com.wb2hub.lite.R.color.err));

        String key = GatewayService.get() != null ? GatewayService.get().apiKey() : "(starting)";
        endpointView.setText("Base URL   http://127.0.0.1:" + GatewayService.HTTP_PORT + "/v1\n"
                + "API key    " + key + "\n"
                + "Health     http://127.0.0.1:" + GatewayService.HTTP_PORT + "/health");

        if (detail == null) {
            detailView.setText(alive
                    ? "Gateway is up; the admin API returned nothing."
                    : "Gateway is not running.\n"
                      + (error != null ? error : "")
                      + "\n\nTap \"Restart gateway\" to try again.");
            return;
        }
        detailView.setText(prettyOverview(detail));
    }

    /** Renders the /admin/overview payload as a readable board. */
    private String prettyOverview(String json) {
        try {
            JSONObject root = new JSONObject(json);
            JSONObject counts = root.optJSONObject("counts");
            StringBuilder sb = new StringBuilder();

            if (counts != null) {
                sb.append("Accounts   ")
                  .append(counts.optInt("ready")).append(" ready / ")
                  .append(counts.optInt("enabled")).append(" enabled / ")
                  .append(counts.optInt("total")).append(" total\n");
            }
            sb.append("Web tools  ")
              .append(root.optBoolean("web_tools") ? "on" : "off").append('\n');
            sb.append("Uptime     ").append(root.optInt("uptime_s")).append("s\n");

            JSONObject limits = root.optJSONObject("limits");
            if (limits != null && limits.length() > 0) {
                sb.append("\nDaily limits\n");
                sb.append("  credits    ").append(limits.opt("daily_credit_limit")).append('\n');
                sb.append("  tokens     ").append(limits.opt("daily_token_limit")).append('\n');
                sb.append("  per-model  ").append(limits.opt("model_daily_token_limit")).append('\n');
                sb.append("  reserve    ").append(limits.opt("reserve_credits")).append('\n');
            }

            JSONArray accounts = root.optJSONArray("accounts");
            sb.append("\nAccounts\n");
            if (accounts == null || accounts.length() == 0) {
                sb.append("  (none — add one below)\n");
            } else {
                for (int i = 0; i < accounts.length(); i++) {
                    JSONObject a = accounts.getJSONObject(i);
                    sb.append("  ").append(a.optString("uid"))
                      .append("  ").append(a.optString("realm_name"))
                      .append(a.optBoolean("enabled") ? "  enabled" : "  disabled");
                    String cool = a.optString("cool_kind");
                    if (cool != null && !cool.isEmpty()) sb.append("  cooling:").append(cool);
                    String slot = a.optString("proxy_slot");
                    if (slot != null && !slot.isEmpty()) sb.append("  via:").append(slot);
                    sb.append('\n');
                }
            }
            return sb.toString();
        } catch (Exception e) {
            return json;
        }
    }

    /** GETs a path on the local gateway, returning the body or null. */
    private String httpGet(int port, String path, String apiKey) {
        HttpURLConnection conn = null;
        try {
            conn = (HttpURLConnection) new URL(
                    "http://127.0.0.1:" + port + path).openConnection();
            conn.setConnectTimeout(3000);
            conn.setReadTimeout(8000);
            conn.setRequestMethod("GET");
            if (apiKey != null && !apiKey.isEmpty()) {
                conn.setRequestProperty("Authorization", "Bearer " + apiKey);
            }
            int code = conn.getResponseCode();
            java.io.InputStream in = code >= 400 ? conn.getErrorStream() : conn.getInputStream();
            if (in == null) return null;

            StringBuilder body = new StringBuilder();
            try (BufferedReader r = new BufferedReader(new InputStreamReader(in))) {
                String line;
                while ((line = r.readLine()) != null) body.append(line).append('\n');
            }
            return body.toString();
        } catch (Exception e) {
            return null;
        } finally {
            if (conn != null) conn.disconnect();
        }
    }

    // ------------------------------------------------------------ import

    /** Asks for a credential blob and POSTs it to the gateway. */
    private void showImportDialog() {
        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(16), dp(8), dp(16), dp(8));

        TextView hint = new TextView(this);
        hint.setText("Paste the account's access_token (or the whole credential JSON).");
        hint.setTextSize(TypedValue.COMPLEX_UNIT_SP, 12);
        box.addView(hint);

        EditText access = new EditText(this);
        access.setHint("access_token");
        access.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_FLAG_MULTI_LINE);
        access.setMinLines(3);
        box.addView(access);

        EditText refresh = new EditText(this);
        refresh.setHint("refresh_token (optional)");
        refresh.setInputType(InputType.TYPE_CLASS_TEXT);
        box.addView(refresh);

        EditText uid = new EditText(this);
        uid.setHint("uid (optional, taken from the token if blank)");
        uid.setInputType(InputType.TYPE_CLASS_TEXT);
        box.addView(uid);

        new android.app.AlertDialog.Builder(this)
                .setTitle("Add account")
                .setView(box)
                .setNegativeButton("Cancel", null)
                .setPositiveButton("Import", (d, w) -> {
                    String token = access.getText().toString().trim();
                    if (token.isEmpty()) {
                        toast("An access token is required");
                        return;
                    }
                    new Thread(() -> {
                        GatewayService svc = GatewayService.get();
                        String key = svc != null ? svc.apiKey() : "";
                        String result = httpPost("/admin/accounts", key,
                                "{"
                                + "\"access_token\":" + JSONObject.quote(token) + ","
                                + "\"refresh_token\":" + JSONObject.quote(
                                        refresh.getText().toString().trim()) + ","
                                + "\"uid\":" + JSONObject.quote(uid.getText().toString().trim())
                                + "}");
                        ui.post(() -> {
                            toast(result != null ? "Account imported" : "Import failed");
                            refresh();
                        });
                    }, "wb2hub-import").start();
                })
                .show();
    }

    private String httpPost(String path, String apiKey, String jsonBody) {
        HttpURLConnection conn = null;
        try {
            conn = (HttpURLConnection) new URL(
                    "http://127.0.0.1:" + GatewayService.HTTP_PORT + path).openConnection();
            conn.setConnectTimeout(3000);
            conn.setReadTimeout(10000);
            conn.setRequestMethod("POST");
            conn.setDoOutput(true);
            conn.setRequestProperty("Content-Type", "application/json");
            if (apiKey != null && !apiKey.isEmpty()) {
                conn.setRequestProperty("Authorization", "Bearer " + apiKey);
            }
            try (java.io.OutputStream os = conn.getOutputStream()) {
                os.write(jsonBody.getBytes("UTF-8"));
            }
            int code = conn.getResponseCode();
            if (code >= 400) return null;

            StringBuilder body = new StringBuilder();
            try (BufferedReader r = new BufferedReader(
                    new InputStreamReader(conn.getInputStream()))) {
                String line;
                while ((line = r.readLine()) != null) body.append(line).append('\n');
            }
            return body.toString();
        } catch (Exception e) {
            return null;
        } finally {
            if (conn != null) conn.disconnect();
        }
    }

    private void toast(String message) {
        ui.post(() -> Toast.makeText(this, message, Toast.LENGTH_SHORT).show());
    }

    /** Unused, kept to document that the theme expects a centred title. */
    @SuppressWarnings("unused")
    private int centreGravity() { return Gravity.CENTER; }
}
