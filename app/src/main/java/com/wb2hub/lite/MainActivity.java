package com.wb2hub.lite;

import android.app.Activity;
import android.app.AlertDialog;
import android.content.Intent;
import android.graphics.Typeface;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.text.InputType;
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
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;

/**
 * 应用主界面。
 *
 * 网关本身是无界面的 HTTP 服务，所以这里做的是一块状态看板加几个真正需要手指
 * 的操作：看账号、看出口代理、看密钥、以及把接口地址和密钥呈出来供客户端使用。
 *
 * 刻意用平台原生控件而非 WebView：内嵌网关只吐 JSON，为了渲染本地状态而去加载
 * 一个页面，等于给一个纯本地工具平添网络依赖。
 */
public class MainActivity extends Activity {

    private TextView statusLine;
    private TextView endpointView;
    private TextView detailView;
    private final Handler ui = new Handler(Looper.getMainLooper());

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(buildUi());

        // 在这里而不是 Application 里启动服务：用户点开应用才是"要跑网关"的
        // 第一个明确信号。
        startGatewayService();

        if (Build.VERSION.SDK_INT >= 33) {
            requestPermissions(new String[]{"android.permission.POST_NOTIFICATIONS"}, 1);
        }
    }

    @Override
    protected void onResume() {
        super.onResume();
        refresh();
    }

    // ------------------------------------------------------------ 界面构建

    private View buildUi() {
        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setBackgroundColor(color(R.color.bg));
        int pad = dp(20);
        root.setPadding(pad, pad, pad, pad);

        TextView title = new TextView(this);
        title.setText(R.string.app_name);
        title.setTextColor(color(R.color.text));
        title.setTextSize(26);
        title.setTypeface(Typeface.DEFAULT_BOLD);
        root.addView(title);

        TextView subtitle = new TextView(this);
        subtitle.setText(R.string.subtitle);
        subtitle.setTextColor(color(R.color.muted));
        subtitle.setTextSize(12);
        root.addView(subtitle);

        statusLine = new TextView(this);
        statusLine.setTextSize(15);
        statusLine.setPadding(0, dp(18), 0, dp(6));
        root.addView(statusLine);

        endpointView = new TextView(this);
        endpointView.setTextSize(13);
        endpointView.setTextColor(color(R.color.muted));
        endpointView.setPadding(0, 0, 0, dp(10));
        endpointView.setTextIsSelectable(true);
        root.addView(endpointView);

        // 复制按钮单独放，因为密钥和地址是这两样最常被粘到别处的东西。
        root.addView(button(R.string.btn_copy_endpoint, v -> copyEndpoints()));

        root.addView(button(R.string.btn_add_account, v -> showAccountDialog()));
        root.addView(button(R.string.btn_add_codearts, v -> showCodeartsDialog()));
        root.addView(button(R.string.btn_growth, v -> showGrowthDialog()));
        root.addView(button(R.string.btn_trial, v -> showTrialDialog()));
        root.addView(button(R.string.btn_scheduler, v -> showSchedulerDialog()));
        root.addView(button(R.string.btn_manage_proxy, v -> showProxyDialog()));
        root.addView(button(R.string.btn_manage_keys, v -> showKeysDialog()));
        root.addView(button(R.string.btn_restart, v -> restartGateway()));
        root.addView(button(R.string.btn_refresh, v -> refresh()));

        detailView = new TextView(this);
        detailView.setTextSize(12);
        detailView.setTextColor(color(R.color.muted));
        detailView.setTypeface(Typeface.MONOSPACE);
        detailView.setPadding(0, dp(16), 0, dp(8));
        detailView.setTextIsSelectable(true);

        ScrollView scroll = new ScrollView(this);
        scroll.addView(detailView);
        root.addView(scroll, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, 0, 1f));

        return root;
    }

    private Button button(int labelRes, View.OnClickListener onClick) {
        Button b = new Button(this);
        b.setText(labelRes);
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

    // ------------------------------------------------------------ 操作

    private void startGatewayService() {
        Intent intent = new Intent(this, GatewayService.class);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            startForegroundService(intent);
        } else {
            startService(intent);
        }
    }

    private void restartGateway() {
        GatewayService svc = GatewayService.get();
        if (svc == null) {
            startGatewayService();
            ui.postDelayed(this::refresh, 1500);
            return;
        }
        new Thread(() -> {
            svc.restartGateway();
            ui.postDelayed(this::refresh, 400);
        }, "wb2hub-restart").start();
        toast(getString(R.string.toast_restarting));
    }

    private void copyEndpoints() {
        GatewayService svc = GatewayService.get();
        if (svc == null) {
            toast(getString(R.string.toast_gateway_not_running));
            return;
        }
        String text = svc.baseUrl() + "/v1\n" + svc.apiKey();
        setClipboard(text);
        toast(getString(R.string.toast_copied));
    }

    @SuppressWarnings("deprecation")
    private void setClipboard(String text) {
        android.content.ClipboardManager cm =
                (android.content.ClipboardManager) getSystemService(CLIPBOARD_SERVICE);
        if (cm != null) {
            cm.setPrimaryClip(android.content.ClipData.newPlainText("wb2hub", text));
        }
    }

    /** 拉取状态：进程是否活着，以及看板数据。 */
    private void refresh() {
        new Thread(() -> {
            GatewayService s = GatewayService.get();
            boolean alive = s != null && s.probe();
            String overview = null;
            String lastError = s != null ? s.lastError() : getString(R.string.status_stopped);
            if (alive) {
                overview = httpGet("/admin/overview");
            }
            final boolean fAlive = alive;
            final String fOverview = overview;
            final String fError = lastError;
            ui.post(() -> render(fAlive, fOverview, fError));
        }, "wb2hub-refresh").start();
    }

    private void render(boolean alive, String overview, String error) {
        statusLine.setText(alive ? "● " + getString(R.string.status_running)
                : "● " + getString(R.string.status_stopped));
        statusLine.setTextColor(color(alive ? R.color.ok : R.color.err));

        GatewayService svc = GatewayService.get();
        String key = svc != null ? svc.apiKey() : "（启动中）";
        int port = GatewayService.HTTP_PORT;
        endpointView.setText(
                getString(R.string.label_endpoint) + "   http://127.0.0.1:" + port + "/v1\n"
                        + getString(R.string.label_apikey) + "   " + key + "\n"
                        + getString(R.string.label_health) + "   http://127.0.0.1:" + port + "/health");

        if (overview == null) {
            detailView.setText(alive
                    ? "网关已运行，但管理接口没有返回数据。"
                    : "网关未运行。\n" + (error == null ? "" : error)
                      + "\n\n点「重启网关」重试。");
            return;
        }
        detailView.setText(renderOverview(overview));
    }

    /** 把 /admin/overview 的返回渲染成中文看板。 */
    private String renderOverview(String json) {
        try {
            JSONObject root = new JSONObject(json);
            StringBuilder sb = new StringBuilder();

            JSONObject counts = root.optJSONObject("counts");
            if (counts != null) {
                sb.append("账号   ")
                  .append(counts.optInt("ready")).append(" 可用 / ")
                  .append(counts.optInt("enabled")).append(" 启用 / ")
                  .append(counts.optInt("total")).append(" 总数\n");
            }
            sb.append("本地工具  ")
              .append(root.optBoolean("web_tools") ? "已开启" : "已关闭").append('\n');
            sb.append("已运行  ").append(root.optInt("uptime_s")).append(" 秒\n");

            if (root.has("key_count")) {
                sb.append("独立密钥  ").append(root.optInt("key_count")).append(" 个\n");
            }

            JSONObject limits = root.optJSONObject("limits");
            if (limits != null && limits.length() > 0) {
                sb.append('\n').append(getString(R.string.section_limits)).append('\n');
                sb.append("  积分上限      ").append(dashIfZero(limits.optLong("daily_credit_limit"))).append('\n');
                sb.append("  账号 Token    ").append(dashIfZero(limits.optLong("daily_token_limit"))).append('\n');
                sb.append("  单模型 Token  ").append(dashIfZero(limits.optLong("model_daily_token_limit"))).append('\n');
                sb.append("  预留积分      ").append(dashIfZero(limits.optLong("reserve_credits"))).append('\n');
                if (limits.has("seconds_until_midnight")) {
                    long s = limits.optLong("seconds_until_midnight");
                    sb.append("  距重置        ").append(s / 3600).append(" 小时 ")
                      .append((s % 3600) / 60).append(" 分\n");
                }
            }

            JSONArray accounts = root.optJSONArray("accounts");
            sb.append('\n').append(getString(R.string.section_accounts)).append('\n');
            if (accounts == null || accounts.length() == 0) {
                sb.append("  ").append(getString(R.string.empty_accounts)).append('\n');
            } else {
                for (int i = 0; i < accounts.length(); i++) {
                    JSONObject a = accounts.getJSONObject(i);
                    String uid = a.optString("uid");
                    if (uid.length() > 14) uid = uid.substring(0, 12) + "…";

                    sb.append("  ").append(uid);
                    String nick = a.optString("nickname");
                    if (nick != null && !nick.isEmpty()) sb.append("（").append(nick).append("）");
                    sb.append('\n');

                    sb.append("     ").append(a.optString("realm_name"));
                    sb.append(a.optBoolean("enabled") ? "  · 已启用" : "  · 已停用");

                    String cool = a.optString("cool_label");
                    if (cool != null && !cool.isEmpty() && !cool.equals("正常")) {
                        sb.append("  · ").append(cool);
                    }
                    String slot = a.optString("proxy_slot");
                    if (slot != null && !slot.isEmpty()) sb.append("  · 出口 ").append(slot);
                    if (a.optBoolean("has_codearts")) sb.append("  · 含 CodeArts");
                    sb.append('\n');

                    long ok = a.optLong("successes");
                    long bad = a.optLong("failures");
                    if (ok > 0 || bad > 0) {
                        sb.append("     成功 ").append(ok).append("  失败 ").append(bad).append('\n');
                    }
                    String err = a.optString("last_error");
                    if (err != null && !err.isEmpty()) {
                        if (err.length() > 60) err = err.substring(0, 58) + "…";
                        sb.append("     最近错误：").append(err).append('\n');
                    }
                }
            }
            return sb.toString();
        } catch (Exception e) {
            return json;
        }
    }

    private String dashIfZero(long value) {
        return value > 0 ? String.valueOf(value) : "不限";
    }

    // ------------------------------------------------------------ 添加账号

    private void showAccountDialog() {
        LinearLayout box = dialobBox();
        TextView hint = dialogHint(getString(R.string.hint_import_token));
        box.addView(hint);

        EditText access = field(getString(R.string.hint_import_token), true, 3);
        EditText refresh = field(getString(R.string.hint_import_refresh), false, 1);
        EditText uid = field(getString(R.string.hint_import_uid), false, 1);
        box.addView(access);
        box.addView(refresh);
        box.addView(uid);

        new AlertDialog.Builder(this)
                .setTitle(R.string.btn_add_account)
                .setView(box)
                .setNegativeButton(R.string.dialog_cancel, null)
                .setPositiveButton(R.string.dialog_import, (d, w) -> {
                    String token = access.getText().toString().trim();
                    if (token.isEmpty()) {
                        toast(getString(R.string.toast_need_token));
                        return;
                    }
                    String body = "{"
                            + "\"access_token\":" + JSONObject.quote(token) + ","
                            + "\"refresh_token\":" + JSONObject.quote(refresh.getText().toString().trim()) + ","
                            + "\"uid\":" + JSONObject.quote(uid.getText().toString().trim())
                            + "}";
                    postAccount(body);
                })
                .show();
    }

    private void postAccount(String body) {
        new Thread(() -> {
            boolean ok = httpPost("/admin/accounts", body) != null;
            ui.post(() -> {
                toast(getString(ok ? R.string.toast_imported : R.string.toast_import_failed));
                refresh();
            });
        }, "wb2hub-import").start();
    }

    // ------------------------------------------------------------ CodeArts

    private void showCodeartsDialog() {
        LinearLayout box = dialobBox();
        box.addView(dialogHint("华为云 CodeArts 的 AK/SK。security_token 留空会在首次使用时自动换取。"));
        box.addView(dialogHint("也可以只填 ticket_id，走浏览器授权后由网关轮询取回凭证。"));

        EditText ak = field(getString(R.string.hint_codearts_ak), false, 1);
        EditText sk = field(getString(R.string.hint_codearts_sk), false, 1);
        EditText st = field(getString(R.string.hint_codearts_st), false, 1);
        EditText ticket = field(getString(R.string.hint_codearts_ticket), false, 1);
        box.addView(ak);
        box.addView(sk);
        box.addView(st);
        box.addView(ticket);

        new AlertDialog.Builder(this)
                .setTitle(R.string.btn_add_codearts)
                .setView(box)
                .setNegativeButton(R.string.dialog_cancel, null)
                .setPositiveButton(R.string.dialog_save, (d, w) -> {
                    String body = "{"
                            + "\"access_key_id\":" + JSONObject.quote(ak.getText().toString().trim()) + ","
                            + "\"secret_access_key\":" + JSONObject.quote(sk.getText().toString().trim()) + ","
                            + "\"security_token\":" + JSONObject.quote(st.getText().toString().trim()) + ","
                            + "\"ticket_id\":" + JSONObject.quote(ticket.getText().toString().trim())
                            + "}";
                    new Thread(() -> {
                        String result = httpPost("/admin/codearts/import", body);
                        ui.post(() -> {
                            toast(getString(result != null
                                    ? R.string.toast_codearts_saved
                                    : R.string.toast_import_failed));
                            refresh();
                        });
                    }, "wb2hub-codearts").start();
                })
                .show();
    }

    // ------------------------------------------------------------ 成长中心

    private void showGrowthDialog() {
        new Thread(() -> {
            String json = httpGet("/admin/growth");
            ui.post(() -> {
                if (json == null) {
                    toast(getString(R.string.toast_gateway_not_running));
                    return;
                }
                new AlertDialog.Builder(this)
                        .setTitle(R.string.section_growth)
                        .setMessage(renderGrowthJson(json))
                        .setNegativeButton("关闭", null)
                        .setPositiveButton("一键领取", (d, w) -> claimGrowthTasks())
                        .show();
            });
        }, "wb2hub-growth").start();
    }

    /** 把 /admin/growth 的返回渲染成中文任务清单。 */
    private String renderGrowthJson(String json) {
        StringBuilder sb = new StringBuilder();
        try {
            JSONObject root = new JSONObject(json);
            if (root.has("error")) {
                return root.getJSONObject("error").optString("message", json);
            }
            // The gateway answers 200 with ok=false when no account is set up,
            // so the panel explains the next step instead of showing zeros.
            if (!root.optBoolean("ok", true)) {
                return root.optString("message", "还没有可用账号。");
            }

            sb.append("夜猫窗口  ")
              .append(root.optBoolean("night_window") ? "进行中（23:00–08:00）" : "未开始")
              .append('\n');

            if (root.has("energy")) {
                sb.append("体力值    ").append(root.optInt("energy")).append('\n');
            }

            JSONObject streak = root.optJSONObject("streak");
            if (streak != null) {
                sb.append("连续天数  ").append(streak.optInt("current"))
                  .append(" 天（最长 ").append(streak.optInt("longest")).append(" 天）\n");
            }

            JSONObject travel = root.optJSONObject("travel");
            if (travel != null) {
                sb.append("猫咪旅行  ");
                if (travel.optBoolean("travelling")) {
                    sb.append("在外，可领取");
                } else if (travel.optBoolean("can_depart")) {
                    sb.append("可出发");
                } else {
                    sb.append("等待中");
                }
                sb.append('\n');
            }

            JSONObject cards = root.optJSONObject("makeup_cards");
            if (cards != null) {
                sb.append("补签卡    ").append(cards.optInt("available")).append(" 张\n");
            }

            JSONArray tasks = root.optJSONArray("tasks");
            sb.append("\n任务\n");
            if (tasks == null || tasks.length() == 0) {
                String err = root.optString("tasks_error");
                sb.append("  ").append(err == null || err.isEmpty() ? "暂无任务" : err).append('\n');
            } else {
                for (int i = 0; i < tasks.length(); i++) {
                    JSONObject t = tasks.getJSONObject(i);
                    sb.append("  ").append(t.optString("name")).append('\n');
                    sb.append("     ").append(t.optInt("current")).append("/")
                      .append(t.optInt("target"));
                    int reward = t.optInt("reward");
                    if (reward > 0) sb.append("  +").append(reward).append(" 积分");
                    if (t.optBoolean("done")) sb.append("  已完成");
                    // 只有真实客户端能完成的桌面任务：如实说明，不假装可领。
                    if (t.optBoolean("unforgeable")) {
                        sb.append("\n     ⚠ 需桌面客户端操作，网关无法代做");
                    } else if (t.optBoolean("skip")) {
                        String reason = t.optString("skip_reason");
                        if (reason != null && !reason.isEmpty()) {
                            sb.append("\n     ").append(reason);
                        }
                    }
                    sb.append('\n');
                }
            }
        } catch (Exception e) {
            sb.append(json);
        }
        return sb.toString();
    }

    private void claimGrowthTasks() {
        new Thread(() -> {
            String result = httpPost("/admin/growth/tasks/claim", "{}");
            ui.post(() -> {
                toast(result == null ? "领取失败" : "已提交领取");
                refresh();
            });
        }, "wb2hub-growth-claim").start();
    }

    // ------------------------------------------------------------ 试用额度

    private void showTrialDialog() {
        new Thread(() -> {
            String json = httpGet("/admin/trial");
            ui.post(() -> {
                if (json == null) {
                    toast(getString(R.string.toast_gateway_not_running));
                    return;
                }
                new AlertDialog.Builder(this)
                        .setTitle(R.string.section_trial)
                        .setMessage(renderTrial(json))
                        .setNegativeButton("关闭", null)
                        .setPositiveButton("领取签到", (d, w) -> claimTrial("checkin"))
                        .setNeutralButton("领取补偿", (d, w) -> claimTrial("compensation"))
                        .show();
            });
        }, "wb2hub-trial").start();
    }

    private String renderTrial(String json) {
        StringBuilder sb = new StringBuilder();
        try {
            JSONObject root = new JSONObject(json);
            if (root.has("error")) {
                return root.getJSONObject("error").optString("message", json);
            }
            if (!root.optBoolean("ok", true)) {
                return root.optString("message", "还没有可用账号。");
            }
            JSONObject res = root.optJSONObject("resource");
            if (res == null) return json;

            sb.append("剩余额度  ").append(res.optInt("remain")).append('\n');
            if (res.has("total")) {
                sb.append("总额度    ").append(res.optInt("total")).append('\n');
            }
            sb.append("已用      ").append(res.optInt("used")).append('\n');

            JSONArray packs = res.optJSONArray("packages");
            if (packs != null && packs.length() > 0) {
                sb.append("\n套餐明细\n");
                for (int i = 0; i < packs.length(); i++) {
                    JSONObject p = packs.getJSONObject(i);
                    sb.append("  ").append(p.optString("name"))
                      .append("  ").append(p.optInt("remain")).append('\n');
                }
            }
        } catch (Exception e) {
            sb.append(json);
        }
        return sb.toString();
    }

    private void claimTrial(String action) {
        new Thread(() -> {
            String result = httpPost("/admin/trial/" + action, "{}");
            ui.post(() -> {
                toast(result == null ? "领取失败" : "已提交领取");
                refresh();
            });
        }, "wb2hub-trial-claim").start();
    }

    // ------------------------------------------------------------ 定时巡检

    private void showSchedulerDialog() {
        new Thread(() -> {
            String json = httpGet("/admin/scheduler");
            ui.post(() -> {
                if (json == null) {
                    toast(getString(R.string.toast_gateway_not_running));
                    return;
                }
                new AlertDialog.Builder(this)
                        .setTitle(R.string.section_scheduler)
                        .setMessage(renderScheduler(json))
                        .setNegativeButton("关闭", null)
                        .setPositiveButton("立即签到", (d, w) -> runJob("checkin"))
                        .setNeutralButton("立即旅行", (d, w) -> runJob("travel"))
                        .show();
            });
        }, "wb2hub-sched").start();
    }

    private String renderScheduler(String json) {
        try {
            JSONObject root = new JSONObject(json);
            if (!root.optBoolean("enabled")) {
                return getString(R.string.scheduler_disabled);
            }
            StringBuilder sb = new StringBuilder();
            sb.append("排程      ").append(root.optString("mode")).append('\n');
            sb.append("下次执行  ").append(root.optString("next_run")).append('\n');
            sb.append("上次执行  ").append(root.optString("last_run")).append('\n');
            sb.append("状态      ").append(root.optBoolean("running") ? "正在执行" : "空闲").append('\n');

            JSONObject last = root.optJSONObject("last_result");
            if (last != null) {
                sb.append("\n最近一次结果\n");
                sb.append("  任务  ").append(last.optString("job")).append('\n');
                sb.append("  成功 ").append(last.optInt("success"))
                  .append(" · 跳过 ").append(last.optInt("skip"))
                  .append(" · 失败 ").append(last.optInt("failure")).append('\n');

                JSONArray outcomes = last.optJSONArray("outcomes");
                if (outcomes != null) {
                    for (int i = 0; i < outcomes.length(); i++) {
                        JSONObject o = outcomes.getJSONObject(i);
                        String uid = o.optString("uid");
                        if (uid.length() > 12) uid = uid.substring(0, 10) + "…";
                        String mark = o.optBoolean("ok") ? "✓" : (o.optBoolean("skip") ? "–" : "✗");
                        String why = o.optString("note");
                        if (why == null || why.isEmpty()) why = o.optString("reason");
                        sb.append("  ").append(mark).append(' ').append(uid);
                        if (why != null && !why.isEmpty()) {
                            if (why.length() > 40) why = why.substring(0, 38) + "…";
                            sb.append("  ").append(why);
                        }
                        sb.append('\n');
                    }
                }
            }
            return sb.toString();
        } catch (Exception e) {
            return json;
        }
    }

    private void runJob(String job) {
        new Thread(() -> {
            String result = httpPost("/admin/scheduler/run?job=" + job, null);
            ui.post(() -> {
                if (result == null) {
                    toast("执行失败");
                } else {
                    try {
                        JSONObject r = new JSONObject(result);
                        toast("成功 " + r.optInt("success") + " · 跳过 " + r.optInt("skip")
                                + " · 失败 " + r.optInt("failure"));
                    } catch (Exception e) {
                        toast("已提交");
                    }
                }
                refresh();
            });
        }, "wb2hub-run").start();
    }

    // ------------------------------------------------------------ 出口代理

    private void showProxyDialog() {
        new Thread(() -> {
            String json = httpGet("/admin/proxy/slots");
            String discover = null;
            ui.post(() -> {
                if (json == null) {
                    toast(getString(R.string.toast_gateway_not_running));
                    return;
                }
                showProxyList(json);
            });
        }, "wb2hub-proxy").start();
    }

    private void showProxyList(String json) {
        StringBuilder sb = new StringBuilder();
        try {
            JSONArray slots = new JSONObject(json).optJSONArray("slots");
            if (slots == null || slots.length() == 0) {
                sb.append(getString(R.string.empty_slots)).append('\n');
            } else {
                for (int i = 0; i < slots.length(); i++) {
                    JSONObject s = slots.getJSONObject(i);
                    sb.append(s.optString("id")).append("  ").append(s.optString("name")).append('\n');
                    sb.append("   ").append(s.optString("url")).append('\n');

                    String exit = s.optString("exit_ip");
                    if (exit != null && !exit.isEmpty()) {
                        sb.append("   出口 ").append(exit);
                        String country = s.optString("country");
                        if (country != null && !country.isEmpty()) sb.append(" · ").append(country);
                        String kind = s.optString("ip_type");
                        if ("residential".equals(kind)) sb.append(" · 住宅");
                        else if ("datacenter".equals(kind)) sb.append(" · 机房");
                        int latency = s.optInt("latency_ms");
                        if (latency > 0) sb.append(" · ").append(latency).append("ms");
                        sb.append('\n');
                        String isp = s.optString("isp");
                        if (isp != null && !isp.isEmpty()) sb.append("   ").append(isp).append('\n');
                    } else {
                        sb.append("   ").append(getString(R.string.proxy_unprobed)).append('\n');
                        String err = s.optString("last_error");
                        if (err != null && !err.isEmpty()) {
                            if (err.length() > 70) err = err.substring(0, 68) + "…";
                            sb.append("   错误：").append(err).append('\n');
                        }
                    }
                    sb.append("   ").append(getString(R.string.proxy_bound, s.optInt("bound"))).append("\n\n");
                }
            }
        } catch (Exception e) {
            sb.append(json);
        }

        new AlertDialog.Builder(this)
                .setTitle(R.string.section_proxy)
                .setMessage(sb.toString())
                .setNeutralButton("自动发现", (d, w) -> discoverProxies())
                .setNegativeButton("关闭", null)
                .setPositiveButton("新增", (d, w) -> showAddSlotDialog())
                .show();
    }

    private void showAddSlotDialog() {
        LinearLayout box = dialobBox();
        box.addView(dialogHint("填写出口代理地址，例如 http://127.0.0.1:7890 或 socks5://主机:端口"));

        EditText name = field("备注名（可选）", false, 1);
        EditText url = field("代理地址", false, 1);
        box.addView(name);
        box.addView(url);

        new AlertDialog.Builder(this)
                .setTitle("新增出口代理")
                .setView(box)
                .setNegativeButton(R.string.dialog_cancel, null)
                .setPositiveButton(R.string.dialog_save, (d, w) -> new Thread(() -> {
                    String body = "{"
                            + "\"name\":" + JSONObject.quote(name.getText().toString().trim()) + ","
                            + "\"url\":" + JSONObject.quote(url.getText().toString().trim())
                            + "}";
                    httpPost("/admin/proxy/slots", body);
                    ui.post(() -> {
                        toast("已保存");
                        refresh();
                    });
                }, "wb2hub-slot").start())
                .show();
    }

    private void discoverProxies() {
        new Thread(() -> {
            String json = httpGet("/admin/proxy/discover");
            ui.post(() -> {
                if (json == null) {
                    toast(getString(R.string.toast_gateway_not_running));
                    return;
                }
                String text = "探测完成。";
                try {
                    JSONObject root = new JSONObject(json);
                    text = "发现 " + root.optInt("reachable") + " 个可用出口（共扫描 "
                            + root.optJSONArray("ports").length() + " 个端口）。";
                } catch (Exception ignored) {
                    // 保持上面的兜底文案
                }
                toast(text);
                refresh();
            });
        }, "wb2hub-discover").start();
    }

    // ------------------------------------------------------------ 密钥

    private void showKeysDialog() {
        new Thread(() -> {
            String json = httpGet("/admin/keys");
            ui.post(() -> {
                if (json == null) {
                    toast(getString(R.string.toast_gateway_not_running));
                    return;
                }
                StringBuilder sb = new StringBuilder();
                try {
                    JSONArray keys = new JSONObject(json).optJSONArray("keys");
                    if (keys == null || keys.length() == 0) {
                        sb.append(getString(R.string.empty_keys));
                    } else {
                        for (int i = 0; i < keys.length(); i++) {
                            JSONObject k = keys.getJSONObject(i);
                            sb.append(k.optString("name")).append("  ")
                              .append(k.optBoolean("enabled") ? "启用" : "停用").append('\n');
                            sb.append("   ").append(k.optString("masked")).append('\n');

                            String realm = k.optString("realm");
                            sb.append("   区域 ").append(realm == null || realm.isEmpty()
                                    ? "跟随全局" : realm).append('\n');

                            JSONObject usage = k.optJSONObject("usage");
                            if (usage != null) {
                                sb.append("   请求 ").append(usage.optLong("requests"))
                                  .append("  令牌 ").append(usage.optLong("total_tokens")).append('\n');
                            }
                            sb.append('\n');
                        }
                    }
                } catch (Exception e) {
                    sb.append(json);
                }

                new AlertDialog.Builder(this)
                        .setTitle(R.string.section_keys)
                        .setMessage(sb.toString())
                        .setNegativeButton("关闭", null)
                        .setPositiveButton("新建密钥", (d, w) -> createKey())
                        .show();
            });
        }, "wb2hub-keys").start();
    }

    private void createKey() {
        LinearLayout box = dialobBox();
        box.addView(dialogHint("新密钥只会显示这一次，请立即复制保存。"));

        EditText name = field("用途备注，如 笔记本", false, 1);
        EditText realm = field("区域（intl / cn，留空跟随全局）", false, 1);
        box.addView(name);
        box.addView(realm);

        new AlertDialog.Builder(this)
                .setTitle("新建 API 密钥")
                .setView(box)
                .setNegativeButton(R.string.dialog_cancel, null)
                .setPositiveButton("生成", (d, w) -> new Thread(() -> {
                    String body = "{"
                            + "\"name\":" + JSONObject.quote(name.getText().toString().trim()) + ","
                            + "\"realm\":" + JSONObject.quote(realm.getText().toString().trim())
                            + "}";
                    String result = httpPost("/admin/keys", body);
                    ui.post(() -> {
                        if (result == null) {
                            toast("生成失败");
                            return;
                        }
                        String secret = "";
                        try {
                            secret = new JSONObject(result).optString("secret");
                        } catch (Exception ignored) {
                            // 拿不到明文就只提示失败
                        }
                        if (!secret.isEmpty()) {
                            setClipboard(secret);
                            new AlertDialog.Builder(this)
                                    .setTitle("新密钥已复制")
                                    .setMessage(secret)
                                    .setPositiveButton("好", null)
                                    .show();
                        } else {
                            toast("已生成");
                        }
                        refresh();
                    });
                }, "wb2hub-newkey").start())
                .show();
    }

    // ------------------------------------------------------------ 网络

    private String httpGet(String path) {
        return request("GET", path, null);
    }

    private String httpPost(String path, String body) {
        return request("POST", path, body);
    }

    /** 统一走本地网关，处理鉴权与错误分支。 */
    private String request(String method, String path, String body) {
        HttpURLConnection conn = null;
        try {
            GatewayService svc = GatewayService.get();
            String key = svc != null ? svc.apiKey() : "";

            conn = (HttpURLConnection) new URL(
                    "http://127.0.0.1:" + GatewayService.HTTP_PORT + path).openConnection();
            conn.setConnectTimeout(3000);
            conn.setReadTimeout(20000);
            conn.setRequestMethod(method);
            conn.setRequestProperty("Accept", "application/json");
            if (key != null && !key.isEmpty()) {
                conn.setRequestProperty("Authorization", "Bearer " + key);
            }
            if (body != null) {
                conn.setDoOutput(true);
                conn.setRequestProperty("Content-Type", "application/json");
                try (OutputStream os = conn.getOutputStream()) {
                    os.write(body.getBytes("UTF-8"));
                }
            }

            int code = conn.getResponseCode();
            java.io.InputStream in = code >= 400 ? conn.getErrorStream() : conn.getInputStream();
            if (in == null) return null;

            StringBuilder out = new StringBuilder();
            try (BufferedReader r = new BufferedReader(new InputStreamReader(in))) {
                String line;
                while ((line = r.readLine()) != null) out.append(line).append('\n');
            }
            // 写操作只看成败，读操作需要正文；这里统一返回正文，失败返回 null。
            if (code >= 400 && out.length() == 0) return null;
            return out.toString();
        } catch (Exception e) {
            return null;
        } finally {
            if (conn != null) conn.disconnect();
        }
    }

    // ------------------------------------------------------------ 控件助手

    private LinearLayout dialobBox() {
        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(16), dp(8), dp(16), dp(8));
        return box;
    }

    private TextView dialogHint(String text) {
        TextView t = new TextView(this);
        t.setText(text);
        t.setTextSize(12);
        t.setTextColor(color(R.color.muted));
        t.setPadding(0, dp(4), 0, dp(4));
        return t;
    }

    private EditText field(String hint, boolean multiline, int minLines) {
        EditText e = new EditText(this);
        e.setHint(hint);
        e.setInputType(InputType.TYPE_CLASS_TEXT
                | (multiline ? InputType.TYPE_TEXT_FLAG_MULTI_LINE : 0));
        e.setMinLines(minLines);
        return e;
    }

    private void toast(String message) {
        ui.post(() -> Toast.makeText(this, message, Toast.LENGTH_SHORT).show());
    }

    /** 预留：底部导航尚未实现，保留常量以免位置计算散落在各处。 */
    @SuppressWarnings("unused")
    private int bottomGravity() { return Gravity.BOTTOM; }
}
