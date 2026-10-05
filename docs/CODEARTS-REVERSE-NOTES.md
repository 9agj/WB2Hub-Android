# CodeArts 上游逆向笔记

来源：`wb2hub.apk` → `lib/arm64-v8a/libwb2hub.so`（go1.22.5，带完整 `.debug_*` 与 `.strtab`）
方法：`strtab`(415227 B) 符号表 + `rodata`(1752171 B) 字符串扫描 + Go 类型字符串提取

---

## 一、端点全图

### 华为云 STS 与门户
```
https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens     STS 换票（OAuth2 token 端点）
https://codearts.huaweicloud.com/portal/login                 登录页
https://codearts.huaweicloud.com/portal/authorize             授权页
```

### snap-access（CodeArts 侧网关）
```
https://snap-access.cn-north-4.myhuaweicloud.com/snap-manager/v1/login/ticket      ticket 换取
https://snap-access.cn-north-4.myhuaweicloud.com/snap-manager/v1/statistics/plugin 用量统计
https://snap-access.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens                  令牌
https://snap-access.cn-north-4.myhuaweicloud.com/v1/model/builtin                  内建模型列表
https://snap-access.cn-north-4.myhuaweicloud.com/api/v2/chat/completions           对话
https://snap-access.cn-north-4.myhuaweicloud.com/v1/ops/claim                      运营领取
https://snap-access.cn-north-4.myhuaweicloud.com/v1/ops/confirm                    确认
https://snap-access.cn-north-4.myhuaweicloud.com/v1/ops/delivery                   发放
https://snap-access.cn-north-4.myhuaweicloud.com/v1/stats                          统计
```

### 开发网关
```
https://opengw.developer.huaweicloud.com/api/v1/gateway/config
```

---

## 二、签名方案

字符串证据：
```
SDK-HMAC-SHA256
x-security-token
20060102T150405Z          ← ISO8601 basic 时间戳（华为 SDK 标准）
Authorization
```

这是**华为云 SDK-HMAC-SHA256** 签名，不是 AWS SigV4。对应符号：
`codearts.SignRequestHuawei`、`codearts.buildCanonicalRequest`、`codearts.hmacSha256Hex`

加上 **DPoP** 层（RFC 9449）：
`codearts.SignDpopJws`、`codearts.GenerateDpopKeyPair`
JWK 字段：`kty` / `crv` / `DpopPrivateJwk json:"dpop_private_key_jwk,omitempty"`

---

## 三、OAuth + PKCE

```
&code_challenge=
&code_challenge_method=SHA-256
```
结构体字段：`CodeChallenge` / `CodeVerifier`（`json:"code_challenge"` / `json:"code_verifier"`）

符号：`codearts.GeneratePkcePair`、`codearts.StartOAuthFlow`、`codearts.listenOnCallbackPort`、
`codearts.pollForCredential`、`codearts.exchangeCodeForToken`

流程：起本地回调端口 → 拼 authorize URL（PKCE S256）→ 用户在浏览器授权 →
回调收到 code → `exchangeCodeForToken` 换 token → `pollForCredential` 轮询拿凭证

---

## 四、凭证 JSON 结构（从 Go 类型字符串完整提取）

### 账号文件（`SaveAuthFile` 落盘格式）
```json
{
  "auth": {
    "accessToken":  "string",
    "refreshToken": "string",
    "expiresAt":    0,
    "domain":       "string",
    "realm":        "string"
  },
  "account": {
    "uid":          "string",
    "enterpriseId": "string",
    "nickname":     "string"
  },
  "device_token": "string",
  "codearts": {}
}
```

扁平变体（旧版）：
```json
{
  "accessToken":  "string",
  "refreshToken": "string",
  "expiresAt":    0,
  "domain":       "string",
  "realm":        "string",
  "uid":          "string",
  "enterpriseId": "string",
  "nickname":     "string",
  "device_token": "string",
  "codearts":     {}
}
```

### CodeArts 凭证块（`codearts.Credential`，塞在上面的 `codearts` 字段里）
```json
{
  "access_key_id":     "string",
  "secret_access_key": "string",
  "security_token":    "string",
  "expires_at":        "string",
  "user_name":         "string",
  "user_id":           "string",
  "domain_id":         "string"
}
```

精简变体（`tokenClient.requestToken` 返回值）：
```json
{ "access_key_id": "...", "secret_access_key": "...", "security_token": "...", "expiration": "..." }
```

**关键结论**：你给我的 AK/SK 要放进 `access_key_id` / `secret_access_key`，
而 `security_token` 是 STS 换回来的临时令牌（有效期短），需要刷新。

---

## 五、登录态轮询

符号：`codearts.parseTicketResponse`、`codearts.pollForCredential`、
`codearts.claimedStatuses`、`codearts.IsRefreshTokenExpired`、`codearts.isAuthError`

错误串：
```
codearts: credential not usable (re-login required)
codearts: refresh materials missing (re-login required)
codearts credential missing after refresh
codearts: parse credential: %w
codearts: decode dpop d: %w
codearts: sign dpop: %w
codearts: dpop keypair missing
codearts: invalid dpop jwk
codearts: auth dir not set
codearts: no credential on account
codearts: no models fetched
codearts: chat transport: %w
codearts queue wait timed out after 30 minutes
```

---

## 六、仍然未知、需要真实凭证才能确定的

这几项**必须**用真实 AK/SK 跑一次才能确定，不能靠猜：

1. **ticket 轮询的响应体结构** —— `ticketResponse` 类型字段没进 typelink，
   只挖到 `parseTicketResponse` 函数名。需要看真实返回。
2. **STS 换票请求体** —— `/v1/oauth2/tokens` 的 form/JSON 参数名。
   华为云 OAuth2 通常要 `grant_type` / `client_id` / `client_secret`，但没有证据。
3. **snap-manager 的 ticket 换取参数** —— `?ticket_id=trial` 后面还带什么。
4. **DPoP 签名覆盖范围** —— `htu`(URL) / `htm`(方法) / `jti` / `ath` 用哪些。
5. **请求签名的 canonical request 组装顺序** —— 华为 SDK 有标准实现，但
   这里 `buildCanonicalRequest` 可能是定制版。
6. **`/v1/ops/*` 三个端点的请求体**。

拿到真实凭证后，跑一次登录抓包就能全部确定。

---

## 七、其它上游（同样来自二进制）

### 成长体系端点
```
/v2/activity/growth/tasks              任务列表
/v2/activity/growth/tasks/accept       接取
/activity/growth/tasks/{code}/claim    领奖
/v2/activity/growth/energy             能量
/activity/growth/streak                连续打卡
/activity/growth/heatmap               热力图
/activity/growth/lottery/chances       抽奖机会
/activity/growth/lottery/draw          抽奖
/activity/growth/redeem                阶梯兑换
/activity/growth/makeup-cards          补签卡
/activity/growth/makeup-cards/use      使用补签卡
/activity/growth/buddy/info            猫猫信息
/activity/growth/buddy/first           领养
/activity/growth/buddy/agreement       协议
/activity/growth/buddy/travel/status   旅行状态
/activity/growth/buddy/travel/depart   派出
/activity/growth/buddy/travel/claim    领奖
/v2/report                             行为上报
```

### 计费 / 试用
```
/v2/billing/meter/daily-checkin
/v2/billing/meter/get-user-resource
/v2/billing/meter/claim-compensation
/v2/billing/meter/claim-gift
/billing/ide/trial
```

### 相关类型
`GrowthStreakWithCards`、`GrowthLotteryDrawResult`、`GrowthTierSpec`、
`GrowthRedemptionStatus`、`GrowthMakeupCards`、`GrowthRedeemResult`、
`GrowthRewardState`、`HeatmapCell`、`TravelState`、`Buddy`

---

## 八、已知的业务硬约束（Python 版实测校准，2026-09）

- **桌面专有任务**（伪造 `/v2/report` 无效，进度永远 0/1）：
  `RichMeow_Chat`、`Library_read`、`Buddy_App`、`Buddy_App_QQ`
- **夜猫子任务** `black_cat` 只在 23:00–08:00 上报才计数，每天 1 次、累计 3 天
- **专家/团队事件按 (eventCode, id) 去重**，重复同一 id 进度不动
- **默认签到时间**：09:00 / 21:00 签到+旅行，22:00 保活，01:00 夜猫
