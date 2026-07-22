# Changelog

## 1.3.4

### 未分类形态标签：只显示代码，中文仅拆分时手填
- `ShapeOf` 形态列表 label 改为机器码：`402·personal-team-blocked:spending-limit` / `unexpected EOF` 等，**不再**自动译成「消费限额」「连接中断」。
- 拆分弹窗默认不预填中文；显示名 / 错误信息由用户手填。完整样本仍可点预览。

## 1.3.3

### 状态标签与未分类样本显示修复
- 活跃账号展示连续错误时忽略全局 `any_error` 计数，优先显示真实信号；未分类显示为「正常·观察·未分类×N」。
- 未分类错误形态返回真实失败 body，并在形态行显示可点击的一行预览与完整样本弹层。

## 1.3.1

### 永禁幂等：状态不变不打动作日志
- **现象**：已 `user_manual` 永久禁用的号，巡查/usage 再命中 disable 策略时每小时重复写 `manual_disable`（「策略阶梯永久禁用 · 连续≥1」刷屏）。
- **修复**：`applyPermanentDisable` 若当前已是 `State=user_manual` 且 `DisableSource=user_manual`，只补关 CPA 文件 + `StampLastAction`，**不**再追加动作日志（对齐冷却幂等）。首次进永禁、以及 `cpa_file_disabled` 路径仍正常记日志。

## 1.1.83

### 修 403 候删循环 + 阶梯严重度 + 候删文案
- **根因**：`permission_403` 阶梯写成 `disable@1 + candidate@5` 时，旧 `Decide` 按「最大 streak 命中」取最后一档 → streak≥5 永远候删；候删又带 30m `recover_at` 自动 reenable 且 streak 保留 → 半小时一轮 `候删↔恢复`。面板把所有 `candidate_dead` 写死成「401·候删」。
- **policy**：多阶梯在已达标档位中取**动作严重度最高**（disable > trash > candidate > cool > observe），同分再比 streak。
- **guard**：仅真实 `auth_401` 候删写 `recover_at`；非 401 候删清 recover；tick 对存量非 401 候删 `candidate_hold` 取消自动恢复。
- **panel/UI**：`candidate_dead` 按 `last_signal` 显示 `401·候删` / `403·候删` / …；筛选标签改为「候删」。

## 1.1.82

### 巡查 Guard 刷新 + 分类收紧 + 文档对齐
- **patrol**：`GetGuard` 热更后刷新 Guard 引用；`IsRunning` 含结束后 30s settle，避免 tick prune 误删刚落盘账号。
- **match**：`permission_403` 仅匹配 xAI `permission-denied` / chat endpoint denied；泛化 `Access Denied` 不进权限阶梯（补回归测试）。
- **docs**：README / `registry.json` 对齐当前 **v1.1.82**（注册 Tab、永禁探活、拆分类阶梯、双写配置、严格 403 匹配）。

> 注：1.1.60–1.1.81 明细以 git log / 面板版本为准；本文件自上而下为近期高信号变更，更早条目自 1.1.59 起保留。


## 1.1.59

### 报错日志「模型」补齐
- **根因**：CPAMP `usage.sqlite` 的 model 列实际有值（24h 失败 1547 条全有），但哨兵 hit 环大量空：`cpamp_backfill` 完全没读 model；live usage 字段空时也不从 body 回填。
- **修复**：
  - `FetchRecentFailures` 读 `model/requested_model/resolved_model` + body 解析 `for model X`
  - backfill / usage / raw usage 空 model 时 `ModelFromFailBody`
  - 错误策略表展示：hit 无 model 时从 sample 再解析一次（旧 hit 也能显示）
  - 详情时间线 `FetchAuthRecentEvents` 同步 coalesce 三列 model

## 1.1.58

### 长稳 / 逻辑健壮性（UI 与产品功能不变）
- **P0 回补先于 heal**：tick 周期内先 `cpamp_backfill` 再 `Tick`；回补前 `MarkPendingBackfillAuths`，heal 跳过即将冷却的号，消除同秒「强制打开→冷却」。
- **P0 冷却幂等**：已 `plugin_auto` 冷却且同族信号 + recover 未到 → 只补关文件，不重打【冷却】、不拉长 recover_at。
- **P0 usage 短窗去重**：同 auth 在 8s 内重复 fail/冷却动作直接跳过，消 3s 双冷却日志。
- **P1 last_signal**：`any_error` 连续计数不再覆盖主信号（`free_usage_429`/`permission_403` 等）。
- **P1 日志环**：满 2000 时优先丢掉 `heal_summary` 等维护噪声，保留冷却/永禁/到期恢复。
- **P1 list 熔断**：连续 ≥5 次 list 校验失败不再 blind trust-PATCH。
- **P2 到期海啸**：每 tick 最多 40 个到期恢复 ensure-open。
- **P2 日池 floor**：analytics 过瘦时用 usage.sqlite 日汇总回补。
- **P2 rebuild 交接**：热更前 Save + 继承 `LastCPAMPFailMS` watermark。

## 1.1.57

### 热修：主页面 JS 语法错误白屏
- 账号列 `title` 拼接漏了引号，导致整页 script 解析失败进不了主页面。

## 1.1.56

### 报错日志只点错误信息；弹层去重；文案默认中文
- 账号列不可点；仅「错误信息」打开最近15次时间线；去掉弹层里重复的错误样本块。
- `cpamp/backfill` 统一中文「用量回补」。
- 降回未分类黄底；显示名称/错误信息/计数与输入框同一行；错误信息文案预填系统默认短中文。

## 1.1.55

### 弹层灰屏/滚不动 + 详情按钮重复 + 策略默认收起
- 弹层 z-index 盖过遮罩（910>900），居中 flex，body 独立滚动。
- 去掉报错日志「详情」按钮（与点账号/错误信息重复）。
- 错误策略**始终默认收起**；清 localStorage 展开缓存；切到该页强制重渲。

## 1.1.54

### 详情时间线修好 + 未分类排序 + 文案可自定义
- **根因**：`/accounts/recent` 未注册到 host 管理路由表 → 详情 404 无历史。
- 注册 GET `accounts/recent`；错误信息列高亮可点；未分类卡片排最后。
- ShapeOf 识别 402/429 为可拆形态；拆分时可填显示名称+错误信息文案+key。
- 策略卡可改「显示名称」「错误信息文案」并保存（不再硬编码覆盖 label）。

## 1.1.53

### 错误策略卡片布局
- 「降回未分类」挪到**启用开关旁**；顶栏：启用+降回 | 计数方式 | 保存。
- 按错误类型上色（429 琥珀 / 402 橙 / 403 紫 / 401 红 / 404 灰 / 未分类虚线 / 其他蓝）。
- 阶梯行栅格：连续N · 动作 · 冷却秒 · 删；**仅观察/永久禁用/垃圾箱**时隐藏冷却秒。

## 1.1.52

### 去掉禁删硬保护，是否进垃圾箱完全按配置
- 删除 `HardNeverTrash` 硬拦（402/spending-limit 等）；`Validate` 不再从 `delete_signals` 剔 402。
- 策略阶梯里配「垃圾箱」+ 全局自动垃圾箱开启即可进箱；`never_trash` 仅当面板显式字段为 true 时才挡。
- 加载 state 时清掉历史 `never_trash=true`（旧硬规则残留）。

## 1.1.51

### 内置策略可真正降回未分类；标题去掉「禁删/内置」噪音
- **根因**：`EnsureBuiltinPolicies` 每次 `/errors` 把空内置卡种回来；无命中的内置降回会 `source key not found`。
- **修复**：`hidden_policy_keys` 记住用户降回的 key，不 re-seed；无命中也能降回；新真实命中仍会经 HandleUsage 重新建卡。
- 卡片标题行去掉「内置/动态」「禁删」标签（禁删是硬规则防自动垃圾箱，不是面板阶梯配置项）。

## 1.1.50

### 错误策略「详情」= 该号最近 15 次请求时间线
- 点账号/错误信息/详情：弹窗拉 `GET /accounts/recent`，展示该 auth **最近 15 次请求**（成功+失败，含模型/状态码/错误摘要）+ 哨兵动作日志。
- 不再只是看不懂的字段堆；无 CPAMP 数据时回退 recent15 色条。

## 1.1.49

### 错误策略：默认折叠 + 内置可降回未分类 + 拆分自定义字段
- 每条错误策略卡片**默认折叠**（点标题展开）；展开状态记 localStorage。
- 原内置分类（429/402/401/403/404 等）也可「降回未分类」；仅 `unmatched`/`any_error` 除外。
- 未分类「拆成此类」时先填**显示名称**和**策略 key**（内部字段），不再只能用系统 suggest_key。

## 1.1.48

### 错误策略：报错日志「模型」列 + 任意错误总设置
- 报错日志表新增 **模型** 列（最近错误请求的 model）；详情弹层同步显示。usage 从 `UsageRecord.Model` 写入 hit 环。
- 新增内置策略 `any_error`（**总设置 · 任意错误·连续**，默认关闭）：不管错误类型，连续失败达到阶梯 N 即按更强动作覆盖单类策略；成功请求清零连续计数。

## 1.1.47

### 强制打开：打开意图后 settle 2 分钟
- **现象**：2k 日志里 heal 大头跟在 `manual_enable` 后 60–120s（p50≈80s）；07-15 21 点批量启用 → 346 次强制打开。
- **判断**：与冷却补关对称的 CPA list/hotload 滞后，不是文件真 sticky 关。
- **修复**：`manual_enable` / `reenable` / `patrol_alive*` / `reopen_foreign` / `clear_cpa_disabled_tag` 后 **2 分钟内** 跳过 `heal_active_file`（不 TouchLastHealAt）；settle 后仍关才强制打开。不含 `*_file_still_closed`（已确认开失败应尽快 heal）。

## 1.1.46

### 冷却补关：冷却后 settle 2 分钟
- **现象**：冷却后约 30–60s 几乎必出一条【冷却补关】；2k 日志里 cool→reassert p90≈61s，且 `cooldown_file_still_open=0`。
- **判断**：多为 CPA list/hotload 滞后，不是外部重开；主路径 `ensureAuthDisabled` 已关过。
- **修复**：冷却/候删/手动禁用后 **2 分钟内** 跳过 `cooldown_reassert`（不 PATCH、不写补关日志）；settle 后文件仍开才补关。

## 1.1.45

### 强制打开过多：闭环疏漏
- **根因1**：heal 频控只看 `LastAction==heal_*`，被 cooldown/patrol 覆盖后每 tick 再强制打开（例 gcrv9… 约每 60s 连开 28 次）。
- **根因2**：面板批量「手动启用」只 `SetDisabled(false)` 不校验 → 下一 tick 大量 `heal_active_file`（21 点 316 次 manual_enable → 346 次强制打开）。
- **修复**：`LastHealAt` 硬限 15 分钟；heal 统一 `ensureAuthEnabled`；`ManualEnable` 开文件二次校验，失败写 `manual_enable_file_still_closed`。

## 1.1.44

### failover 中间 403 漏记 + 到期恢复文件不粘
- **现象**：CPAMP 请求失败有 `hx9…` 23:32 **403**，哨兵动作/策略无记录；随后 23:33 才【强制打开】。
- **usage 打点**：每条失败/非 2xx 写 host 日志 `usage_in source/auth/status/body`，便于对照插件是否收到。
- **CPAMP 失败回补**：tick 读 `usage.sqlite` 中 watermark 之后的 401/402/403/429，经 `HandleUsage(source=cpamp_backfill)` 补策略；`alreadyHandledRecently` 防双计。
- **到期恢复校验**：`ensureAuthEnabled` 开文件后二次 list；仍关写 `reenable_file_still_closed`，状态仍前进，由 heal 继续开。

## 1.1.43

### 巡查四种范围（手动 + 定时）
- **全部** `all`：有 token 的 xAI 全探  
- **启用** `enabled`（旧 `full`）：哨兵未禁用/可接流  
- **冷却** `cooldown`：仅冷却中  
- **永久禁用** `permanent`：仅 user_manual 永禁  
- 面板：四个手动按钮 + 配置「自动范围」`patrol_mode`（定时巡查使用）  
- 新增定时巡查 ticker（此前仅配置开关、未真正周期启动）  
- 永禁账号探测报错不会被策略降级成冷却  

## 1.1.42

### 手机：点账号/错误信息看详情
- 策略报错日志表：账号、错误信息仍截断 + 仅 `title` 悬停 → 手机看不到完整信息。
- 修复：点 **账号 / 错误信息 /「详情」** 弹出底部抽屉，展示完整邮箱、auth、状态、连续N、错误正文/JSON；遮罩点击关闭。

## 1.1.41

### 错误策略「当前状态」大量空白
- 根因：状态列只查面板 `LAST_ACCOUNTS`（常为「问题账号」子集 / 未加载全量），命中环账号对不上就显示「—」。
- 修复：`/errors` 的 `account_hits` 直接附带 `state / disable_source / pending_observe / streaks`（全量 state 索引）；前端优先用后端字段渲染状态。

## 1.1.40

### 错误策略·报错日志：垃圾箱 + 当前状态列
- 操作列补 **垃圾箱**（与启用/冷却/永久禁用并列，可恢复）。
- 新增 **当前状态** 列：对齐账号列表 `stateTag`（正常·可用 / 冷却 / 永禁 / 待观察…）。

## 1.1.39

### 错误策略：手机可点看完整错误样本
- 现象：错误信息只在桌面 `title` 悬停可见，手机无法移动指针查看。
- 修复：错误策略卡片标题旁增加 **「错误样本」** 按钮（点标题亦可）；弹出层展示 code / message / 原始 body；窄屏底部抽屉式展示。

## 1.1.38

### 动作日志：强制打开分账号 + 汇总不再把补关叫「自愈」
- 现象：只有 `【维护汇总】自愈打开9 · 强制打开2`，搜不到分账号 `【强制打开】email` / `【自愈打开】email`。
- 根因1：成功 `heal_active_file` 只 `StampLastAction`，不进动作日志环。
- 根因2：周期 tick **不会**跑 foreign 自愈；`syncDisabledFromCPA` 返回的 n 含 **冷却补关**，被误标成「自愈打开」。
- 修复：成功强制打开写分账号 `Log`；计数拆成 `foreignOpened` / `reasserted`；汇总正确显示「冷却补关N」；真正的自愈打开只在立即维护且仍写分账号 `reopen_foreign`。

## 1.1.37

### 巡查历史：保留多次任务（别被同 id 挤掉）
- 现象：先「手动查冷却」再「手动全量」后，面板只剩全量任务。
- 根因：job id 重启后从 `job-1` 重计，两次任务都叫 `job-1`；UI `if (p.id===h.id) skip` 把冷却任务整条丢掉。磁盘 `patrol-history.json` 实际有 2 条。
- 修复：job id = `job-YYYYMMDD-HHMMSS-seq`；UI 按 `started_at|finished_at|mode` 去重，不再单靠 id 过滤历史。

## 1.1.36

### 巡查：探活即打开 + 逐条实时日志
- 根因1：探活 HTTP 200 只走 `HandleUsage` 成功分支，**不会**退出冷却、也**不会** `SetDisabled(false)` → 「探活了还不打开」。
- 修复：`source=patrol` 且 2xx → `reopenAfterProbeAlive`：冷却/候删则开文件 + `ResetToActive` + `【探活打开】`；已 Active 则确保文件开 + `【探活确认】`。永禁/垃圾箱不碰。全量/仅冷却同一路径。
- 根因2：任务明细日志在 `Run` 整批结束后才 `appendLog`，时间戳挤在同一秒。
- 修复：每个 probe 结束立刻 `appendProbeResultLive`；面板巡查中 800ms 刷进度 + 账号表 + 右侧动作日志。

## 1.1.35

### 冷却补关：限频 + 关文件二次校验
- 现象：`gr5qewyhpz` / `gticvvbjvt` 等冷却后每 30s 一条【冷却补关】，且中途仍漏流再 429。
- `ensureAuthDisabled`：`SetDisabled(true)` 后 ~1.2s 再 list；仍开则重试 1 次；仍开记 `cooldown_file_still_open`。
- 同号 `cooldown_reassert*` / `cooldown_file_still_open` **15 分钟内只打 1 条叙事日志**（其余只 stamp）。
- `applyCooldown` 同样走校验；关不牢写 `cooldown_file_still_open`，状态仍保持冷却。

### heal 不再膨胀「恢复待观察」
- clean Active 强制打开 **不再** `ResetToActive`（不再强打 `pending_observe`）。
- tick 立即清掉 `last_action=heal_active_file` 且无阶梯信号的 pending 气球。

## 1.1.34

### 强制打开治本
- 打开后 **~1.2s 再读 CPA 列表校验**；仍关则记失败 streak，**连续 ≥3 次** → 标 `CPA已禁用`（停空转 heal）。
- 成功 heal **不再逐条写动作日志**（只 `StampLastAction`），改由 tick **【维护汇总】** 聚合；失败/卡死仍逐条。
- 仍保留同号 **15 分钟** heal 冷却。

### 恢复待观察 TTL + 分级过滤
- 新增 `pending_since`：进入 pending 时写入，heal 重触 **不刷新**。
- **空闲 6 小时**无成功请求 → 清 pending；额度 residual signal 一并清；**403/401 阶梯保留**。
- 过滤器拆分：`空闲待观察` / `有信号观察`（原「恢复待观察」仍为合集）。

## 1.1.33

### 强制打开日志限频（防刷屏）
- 现象：个别号每 30s 一条【强制打开】（如 gcrv9ol0kv 连续 20+ 次）。
- 根因：Active+文件关 每 tick 都 heal；文件随后又被关（或列表仍显示关）→ 无限循环写日志。
- 修复：同一账号 `heal_active_file*` 后 **15 分钟内不再 heal/不再打日志**。



## 1.1.32

### 闭环：Active + 文件仍关 → 强制打开（消未归类）
- 根因：到期恢复/自愈后哨兵 Active，但 CPA 文件仍 disabled（文件重写/外部再关/open 未钉死）→ KPI「未归类」。
- 新增 `healActiveFileDisabled`：每次 tick（定时+立即维护）扫描 **Active 且无 cool 所有权** 但文件 disabled 的号 → `SetDisabled(false)` + 日志 `【强制打开】`。
- **不**打开自有冷却/永禁/候删（shouldProtectDisable）。
- 打开后 `pending_observe`，UI 显示恢复待观察直到成功请求。



## 1.1.31

### 状态文案对齐：额度 residual 一律「恢复待观察」
- 禁止 `观察·429×1`：free_usage_429 / spending residual 统一显示 **恢复待观察**。
- **观察·403×N / 观察·401×N** 仅用于仍在接流的权限/凭证阶梯进度（未进冷却）。
- 兼容旧数据：`last_action=reenable|reopen_foreign` 也显示恢复待观察。

### 立即维护：必有汇总动作日志
- 点「立即维护」无论有无变更，都写一条 `【立即维护】…` 汇总日志。
- 有变更时带数量：到期恢复N · 自愈打开N；无变更时写明「无到期冷却、无非自有禁用需处理」。



## 1.1.30

### Overview KPI：CPA 口径可加总
- 第一行：可接流量(CPA 已开启) / 冷却中 / 候删 / **禁用**。
- **禁用** = xAI总量 − 可接 − 冷却 − 候删，强制加总闭合；副文案 `永禁N+未归类M`（未归类 = CPA 已关但哨兵未标冷却/候删/永禁）。
- xAI 总量副文案改为「可接+冷却+候删+禁用」。

### 状态文案：冷却恢复 → 恢复待观察 → 正常
- 新增 `pending_observe`：冷却/候删 `ResetToActive` 后置 true；面板手动启用清零。
- UI/API 不再显示易误解的「正常·429×1」：恢复后统一 **恢复待观察**，真实成功请求后 → **正常·可用**。
- 仍在接流且有阶梯连续计数时显示 **观察·403×N / 观察·429×N**（未进冷却的策略进度）。
- 成功路径清除 `pending_observe` + residual `last_signal`。



## 1.1.29

### Docs / ops: CPAMP usage mount must include SQLite WAL
- **Symptom:** account shows 429 cooldown, but 「最近15次」 stays all-green / request counts lag CPAMP.
- **Cause:** mounting only `usage.sqlite` hides `usage.sqlite-wal` / `-shm` (CPAMP uses WAL mode).
- **Fix:** mount whole dir `./cpa-manager-data:/data:ro` (documented in INSTALL + README).
- No plugin logic change required; deploy + recreate container after compose update.


## 1.1.28

### Overview: 日池剩余 subtitle
- Show remaining pool percent under 日池剩余 (mirrors 日池已用 %).


## 1.1.27

### Overview 候删 count = candidate + trash
- KPI 候删 = `candidate_dead` + `trashed` (候选删除 + 移入垃圾箱).
- Subtitle shows split: `候选N+箱M`.


## 1.1.26

### Overview KPIs: two rows + 候删/永久禁用
- Layout: 4 columns × 2 rows.
- Row1: 可接流量 / 冷却中 / 候删 / 永久禁用.
- Row2: xAI 总量 / 日池总量 / 日池已用 / 日池剩余.


## 1.1.25

### Panel header badges + table density
- Split runtime status into 3 badges: 维护 / 巡查 / 密钥.
- Requests column: `今日/总数`; success rate: `今日%/总%`.
- Last-action column: time only (action name on hover, no second line).


## 1.1.24

### Panel layout polish
- Runtime status `维护每30s · 巡查… · 密钥` sits **inline with version** as a soft badge (not a second line).
- Account table numeric columns single-line + right-aligned (no stacked 今日 sub-rows).


## 1.1.23

### Domain mask: first label only
- `jnge8hzlj6@lovc.eu.cc` → `jng***j6@l***.eu.cc` (keep `.eu.cc`).

## 1.1.22

### Action logs honor mask toggle + domain masking
- Mask button now rewrites emails inside action log / patrol log text.
- Domain labels masked too (e.g. `jng***j6@l***.eu.cc`), not only local-part.


## 1.1.21

### Policy reason strings without parentheses
- e.g. `策略阶梯永久禁用 · 连续≥6` instead of `策略阶梯=永久禁用(≥6)`.
- Historical logs humanized on read.


## 1.1.20

### Action log copy: permanent disable + no parentheses
- `manual_disable` now `【永久禁用】账号 · 原因：…` like cooldown/reenable.
- Reasons: 面板手动永久禁用 / 策略阶梯触发永久禁用.
- Narrative logs no longer use `()` / `（）` wrappers.


## 1.1.19

### Bidirectional config sync (last-writer-wins)
- **Panel save** → host `plugins.configs` + `runtime-overrides.json` (unchanged dual-write).
- **Host reconfigure** (official plugin page / `config.yaml`) → **host wins**, then mirrors into
  `runtime-overrides.json` so panel no longer stomps host with stale overrides.
- Removes one-way "overrides always override YAML" that blocked official-page edits.


## 1.1.18

### Align config persistence with official CLIProxyAPI plugin model
- Panel save dual-writes:
  1. `runtime-overrides.json` (local)
  2. CPA host `PUT /v0/management/plugins/cpa-xai-sentry/config` (GET+merge+PUT)
- Keeps existing host `enabled`/`priority`/secrets via merge.


## 1.1.17

### Broader runtime-overrides + persist viewer
- Persist ops knobs: `tick_seconds`, permission/401 cool-down seconds, `max_reset_seconds`,
  `reopen_foreign_disabled`, trash/cpamp floor flags (in addition to patrol fields).
- Panel: save writes disk; **查看持久化** shows overrides path + on-disk JSON + live values.
- Secrets remain YAML-only.


## 1.1.16

### Persist patrol config + job history across restarts
- Root cause: `patrol_batch_size` etc. only lived in memory; `runtime-overrides.json` only saved bool switches, so restart reloaded YAML `50`.
- Root cause: patrol job history was package memory only — docker/plugin restart wiped the 巡查 list.
- Now: all patrol knobs saved in `runtime-overrides.json` and reapplied on load.
- Now: finished jobs saved to `patrol-history.json` next to state and reloaded on start.


## 1.1.15

### Panel layout copy
- Runtime line `维护每30s · 巡查… · 密钥` next to version badge.
- Schedule `巡查：上次… · 下次…` under 巡查中心 actions.
- Unified wording: 巡检 → 巡查.


## 1.1.14

### Patrol probe
- Default base: `https://cli-chat-proxy.grok.com/v1` when auth file has no base_url.
- Probe order: **`/responses` first** (`input` + `max_output_tokens`, never `max_tokens`), then `/chat/completions` on 404/405/shape errors.
- Grok CLI headers: `x-authenticateresponse`, `x-grok-client-identifier`, `x-grok-client-version=0.2.93`, `x-xai-token-auth`.
- Chat fallback content `"hi"`.


## 1.1.13

### Full patrol = sentry-not-disabled (schedulable) accounts
- **Full** no longer means "all CPA files" or "CPA-enabled only".
- Full = accounts **not** in sentry cool-down / 候删 / 永久禁用 / 垃圾箱 (still may receive traffic).
- CPA file `disabled` does **not** exclude a target if sentry still treats it as open — probe with token, real HTTP decides.
- No synthetic disabled→429 alignment.
- Cooldown mode = sentry cool-down accounts only.


## 1.1.12

### Patrol: real probe only (no synthetic disabled→429)
- Removed `align_disabled_quota` heuristic (disabled file ≠ proven free-usage).
- Full patrol includes **disabled** xAI auths when token is available; probe uses token directly.
- Policy follows **real** HTTP status/body only (200/429/403/…).


## 1.1.11

### Fix: same-tick 「到期恢复」vs「冷却补关」fight
- Tick order: **recover due cool-downs first**, then CPA file reassert/sync.
- Do not reassert-close when `recover_at` is already due (account should open).
- Stops duplicate opposite logs in the same second for one account.


## 1.1.10

### Patrol summary: count 401/403 as 权限信号
- Previously 401/403 were logged but not included in 冷却/异常, so
  `探测N · 存活A · 冷却0 · 异常0` could leave N-A accounts unaccounted.
- Summary now: 探测 · 存活 · 冷却信号 · **权限信号** · 异常 (+ 已禁用额度对齐).


## 1.1.9

### Fix: disabled-quota align actually stamps cool-down
- Direct `cooldown_quota` write (not only HandleUsage synthetic) when CPA file disabled + sentry Active.
- Skip 401/403-signaled accounts so they are not rebranded as free-usage.

## 1.1.8

### Fix: full patrol skips CPA-disabled 429 accounts
- Full mode only probes **enabled** files, so accounts already disabled for free-usage never get cooled in sentry state.
- Patrol now **aligns** CPA-disabled + sentry-Active xAI accounts as `free_usage_429` cool-down (no HTTP probe).
- Example: `jc3f4dnnjh@…` file disabled, panel still active → becomes cooldown_quota on next full patrol.


## 1.1.7

### Fix: duplicate cool-down action logs
- `applyCooldown` logged `cooldown` twice (before + after CPA SetDisabled).
- Single log entry now; `cooldown_failed` still logged on disable failure.
- Patrol targets deduped by email/file.


## 1.1.6

### Fix: patrol all accounts HTTP 400
- Root cause: probe body sent `max_tokens` to `/v1/responses`, which rejects it.
- Use endpoint-specific payloads (`chat/completions` → `max_tokens`; `responses` → `max_output_tokens` only).
- Prefer `/chat/completions` first; on shape-related 400, fall through to the other path.
- Do not treat request-shape 400 as account death.


## 1.1.5

### Fix: permission_403 streak stuck at 3
- Cool-down **recover** (`ResetToActive`) no longer clears `streaks`.
- Streak ladders (e.g. ≥3 cooldown, ≥15 disable) now accumulate across cool-down cycles.
- Streaks still clear on **successful request** (streak mode) and **panel 启用**.
- Day fail count is not the same as continuous N (hits vs streak).


## 1.1.4

### Action log readability
- Human labels: 冷却补关 / 到期恢复 / 自愈打开 / CPA禁用对齐 …
- Full sentences with 【标签】prefix for key events.
- `owned_disable_was_enabled` → 「自有冷却期间文件被打开，已重新关闭」.
- `permission_denied` → 「权限拒绝(403)」; `recover_at` → 「冷却到期自动恢复」.


## 1.1.3

### Foreign-disable scan only on manual maintenance
- Periodic Tick: recover cool-downs, trash purge, **owned reassert only** — no `file_disabled_sync` / unowned reopen spam.
- Panel **立即维护** (`TickManual`): one-shot full scan — open unowned (if enabled) or mark CPA已禁用.
- Rationale: unowned disables are rare after one audit; continuous scan only creates noise and races.


## 1.1.2

### Ops preference restored: open unowned disables
- Default `reopen_foreign_disabled=true` again (open non-owned disables; wait next error).
- **Still hard-protects** owned cool-downs / panel permanent disable (mutex, ownership map, reassert, no ResetToActive on cool-downs).
- Production config set `reopen_foreign_disabled: true`.


## 1.1.1

### Fixed: conservative mode overwriting cool-down as CPA已禁用
- `file_disabled_sync` must not run when account is owned cool-down/manual.
- Triple-check identity ownership before tagging cpa_file_disabled.
- Lock bulk cooldown path.

## 1.1.0

### Hard fix: stop cool-down being reopened as "非自有禁用"
Root cause was structural, not one-account matching:
1. Self-heal reopen default ON + `ResetToActive` wiped cool-downs after 429/403.
2. HandleUsage and Tick raced on ownership.

Changes:
- **Default `reopen_foreign_disabled=false`** (safe). Unknown disables stay closed; only `recover_at` / panel enable open files.
- Serialize **HandleUsage / Tick / ManualEnable/Disable** with a mutex.
- Self-heal (if explicitly enabled) **never** opens owned cool-downs and **never** `ResetToActive` on protectable accounts; file enable only.
- Owned cool-down with file enabled still **re-disables** (`cooldown_reassert`).
- Production config sets `reopen_foreign_disabled: false`.


## 1.0.11

### Fixed: owned cool-down file left enabled after false reopen
- Precompute owned disable identities (auth_index/file/email).
- If CPA file is enabled but account is owned cool-down/manual → **re-disable** (`cooldown_reassert`).
- Self-heal only runs for non-owned disables.


## 1.0.10

### Fixed: cooldown then immediate "打开非自有禁用"
- Protect if **any** matched identity row is owned (not only pickAcc winner).
- 2-minute grace after `cooldown`/`candidate`/`manual_disable` action even if state briefly Active.
- Persist ownership (Log+Save) before CPA SetDisabled to shrink tick race.
- Do not ResetToActive owned cool-downs during self-heal.


## 1.0.9

### Panel sync: dual time columns + parallel refresh
- Account table: **最后请求** (CPAMP) and **最后动作** (sentry action log).
- Stamp `last_action` / `last_action_at` on every action log; backfill from retained logs on load.
- Sort by max(request, action) activity.
- Refresh loads `/state` + `/logs` in parallel; interval 8s; log timestamps include date.


## 1.0.8

### Fixed: unowned reopen despite auth_index cool-down
- Parse CPA auth-files `auth_index` / `id` / `path` / `account`.
- Match disabled files to sentry cool-downs by **auth_index first**, then file/email.
- Last-chance guard: never reopen if any owned cool-down shares email/file/auth_index.


## 1.0.7

### Action log pagination
- `/logs?limit=&offset=&q=` returns newest-first pages (`page.total/has_more/next_offset`).
- Panel loads 80 lines first; **更早** loads older pages; search uses server `q`.
- Log expiry still independent of counters (see below).


## 1.0.6

### Fixed: false "unowned" reopen on owned cool-downs
- Match CPA list entries to sentry accounts by **filename basename**, path suffix, and email derived from `xai-<email>.json` when list omits email.
- Prevents 429/`plugin_auto` cool-downs from being treated as untracked and reopened (e.g. `xai-5w4ggr8txx@...json`).


## 1.0.4

### Self-heal unowned disables (operator preference)
- Default `reopen_foreign_disabled=true`: if a CPA file is disabled but sentry cannot prove ownership (`plugin_auto` cool-down / panel `user_manual`), **reopen** it and `ResetToActive`.
- Next real usage/patrol error re-applies policy and re-stamps ownership (self-heal).
- Still **protects** real cool-downs, 候删, and panel permanent disables.
- `cpa_file_disabled` tags are no longer sticky protect — they self-heal on the next tick.
- Set `reopen_foreign_disabled=false` for the old conservative "keep closed + mark CPA已禁用" mode.


## 1.0.3

### Auth closed-loop hardening
- `CanAutoReenable`: `plugin_auto` sufficient; never open manual/CPA-file locks
- Candidate path also disables CPA file + stamps ownership/recover_at
- Permanent disable stamps ownership before CPA I/O
- Duplicate-account prune prefers cool-down/ownership rows over empty Active shells
- Regression tests for cooldown→recover, manual never auto-open, prune, legacy owner


## 1.0.2

### Fixed (plugin_auto ownership holes)
- Protect **any** cool-down/候删/manual/plugin_auto residue from foreign-reopen, not only exact state+source pairs.
- Pick the best matching account row when multiple state keys map to one auth file (email/file duplicates).
- Do not scrub away `plugin_auto` on Active; if `recover_at` is still in the future, **repair** cool-down state instead.
- Stamp cool-down ownership before CPA disable to close race with concurrent ticks.


## 1.0.1

### Fixed
- **Stop auto-reopening operator disables**: maintenance no longer re-enables CPA auth files that are disabled outside sentry cool-down/manual locks.
- Disabled files are synced to panel state as `CPA已禁用` (`cpa_file_disabled`) so they stay locked until you click **启用**.
- New config `reopen_foreign_disabled` (default `false`) restores the old reopen behaviour only if explicitly enabled.

# Changelog

## 1.1.45

### 强制打开过多：闭环疏漏
- **根因1**：heal 频控只看 `LastAction==heal_*`，被 cooldown/patrol 覆盖后每 tick 再强制打开（例 gcrv9… 约每 60s 连开 28 次）。
- **根因2**：面板批量「手动启用」只 `SetDisabled(false)` 不校验 → 下一 tick 大量 `heal_active_file`（21 点 316 次 manual_enable → 346 次强制打开）。
- **修复**：`LastHealAt` 硬限 15 分钟；heal 统一 `ensureAuthEnabled`；`ManualEnable` 开文件二次校验，失败写 `manual_enable_file_still_closed`。

## 1.1.29

### Docs / ops: CPAMP usage mount must include SQLite WAL
- **Symptom:** account shows 429 cooldown, but 「最近15次」 stays all-green / request counts lag CPAMP.
- **Cause:** mounting only `usage.sqlite` hides `usage.sqlite-wal` / `-shm` (CPAMP uses WAL mode).
- **Fix:** mount whole dir `./cpa-manager-data:/data:ro` (documented in INSTALL + README).
- No plugin logic change required; deploy + recreate container after compose update.


## 1.1.28

### Overview: 日池剩余 subtitle
- Show remaining pool percent under 日池剩余 (mirrors 日池已用 %).


## 1.1.27

### Overview 候删 count = candidate + trash
- KPI 候删 = `candidate_dead` + `trashed` (候选删除 + 移入垃圾箱).
- Subtitle shows split: `候选N+箱M`.


## 1.1.26

### Overview KPIs: two rows + 候删/永久禁用
- Layout: 4 columns × 2 rows.
- Row1: 可接流量 / 冷却中 / 候删 / 永久禁用.
- Row2: xAI 总量 / 日池总量 / 日池已用 / 日池剩余.


## 1.1.25

### Panel header badges + table density
- Split runtime status into 3 badges: 维护 / 巡查 / 密钥.
- Requests column: `今日/总数`; success rate: `今日%/总%`.
- Last-action column: time only (action name on hover, no second line).


## 1.1.24

### Panel layout polish
- Runtime status `维护每30s · 巡查… · 密钥` sits **inline with version** as a soft badge (not a second line).
- Account table numeric columns single-line + right-aligned (no stacked 今日 sub-rows).


## 1.1.23

### Domain mask: first label only
- `jnge8hzlj6@lovc.eu.cc` → `jng***j6@l***.eu.cc` (keep `.eu.cc`).

## 1.1.22

### Action logs honor mask toggle + domain masking
- Mask button now rewrites emails inside action log / patrol log text.
- Domain labels masked too (e.g. `jng***j6@l***.eu.cc`), not only local-part.


## 1.1.21

### Policy reason strings without parentheses
- e.g. `策略阶梯永久禁用 · 连续≥6` instead of `策略阶梯=永久禁用(≥6)`.
- Historical logs humanized on read.


## 1.1.20

### Action log copy: permanent disable + no parentheses
- `manual_disable` now `【永久禁用】账号 · 原因：…` like cooldown/reenable.
- Reasons: 面板手动永久禁用 / 策略阶梯触发永久禁用.
- Narrative logs no longer use `()` / `（）` wrappers.


## 1.1.19

### Bidirectional config sync (last-writer-wins)
- **Panel save** → host `plugins.configs` + `runtime-overrides.json` (unchanged dual-write).
- **Host reconfigure** (official plugin page / `config.yaml`) → **host wins**, then mirrors into
  `runtime-overrides.json` so panel no longer stomps host with stale overrides.
- Removes one-way "overrides always override YAML" that blocked official-page edits.


## 1.1.18

### Align config persistence with official CLIProxyAPI plugin model
- Panel save dual-writes:
  1. `runtime-overrides.json` (local)
  2. CPA host `PUT /v0/management/plugins/cpa-xai-sentry/config` (GET+merge+PUT)
- Keeps existing host `enabled`/`priority`/secrets via merge.


## 1.1.17

### Broader runtime-overrides + persist viewer
- Persist ops knobs: `tick_seconds`, permission/401 cool-down seconds, `max_reset_seconds`,
  `reopen_foreign_disabled`, trash/cpamp floor flags (in addition to patrol fields).
- Panel: save writes disk; **查看持久化** shows overrides path + on-disk JSON + live values.
- Secrets remain YAML-only.


## 1.1.16

### Persist patrol config + job history across restarts
- Root cause: `patrol_batch_size` etc. only lived in memory; `runtime-overrides.json` only saved bool switches, so restart reloaded YAML `50`.
- Root cause: patrol job history was package memory only — docker/plugin restart wiped the 巡查 list.
- Now: all patrol knobs saved in `runtime-overrides.json` and reapplied on load.
- Now: finished jobs saved to `patrol-history.json` next to state and reloaded on start.


## 1.1.15

### Panel layout copy
- Runtime line `维护每30s · 巡查… · 密钥` next to version badge.
- Schedule `巡查：上次… · 下次…` under 巡查中心 actions.
- Unified wording: 巡检 → 巡查.


## 1.1.14

### Patrol probe
- Default base: `https://cli-chat-proxy.grok.com/v1` when auth file has no base_url.
- Probe order: **`/responses` first** (`input` + `max_output_tokens`, never `max_tokens`), then `/chat/completions` on 404/405/shape errors.
- Grok CLI headers: `x-authenticateresponse`, `x-grok-client-identifier`, `x-grok-client-version=0.2.93`, `x-xai-token-auth`.
- Chat fallback content `"hi"`.


## 1.1.13

### Full patrol = sentry-not-disabled (schedulable) accounts
- **Full** no longer means "all CPA files" or "CPA-enabled only".
- Full = accounts **not** in sentry cool-down / 候删 / 永久禁用 / 垃圾箱 (still may receive traffic).
- CPA file `disabled` does **not** exclude a target if sentry still treats it as open — probe with token, real HTTP decides.
- No synthetic disabled→429 alignment.
- Cooldown mode = sentry cool-down accounts only.


## 1.1.12

### Patrol: real probe only (no synthetic disabled→429)
- Removed `align_disabled_quota` heuristic (disabled file ≠ proven free-usage).
- Full patrol includes **disabled** xAI auths when token is available; probe uses token directly.
- Policy follows **real** HTTP status/body only (200/429/403/…).


## 1.1.11

### Fix: same-tick 「到期恢复」vs「冷却补关」fight
- Tick order: **recover due cool-downs first**, then CPA file reassert/sync.
- Do not reassert-close when `recover_at` is already due (account should open).
- Stops duplicate opposite logs in the same second for one account.


## 1.1.10

### Patrol summary: count 401/403 as 权限信号
- Previously 401/403 were logged but not included in 冷却/异常, so
  `探测N · 存活A · 冷却0 · 异常0` could leave N-A accounts unaccounted.
- Summary now: 探测 · 存活 · 冷却信号 · **权限信号** · 异常 (+ 已禁用额度对齐).


## 1.1.9

### Fix: disabled-quota align actually stamps cool-down
- Direct `cooldown_quota` write (not only HandleUsage synthetic) when CPA file disabled + sentry Active.
- Skip 401/403-signaled accounts so they are not rebranded as free-usage.

## 1.1.8

### Fix: full patrol skips CPA-disabled 429 accounts
- Full mode only probes **enabled** files, so accounts already disabled for free-usage never get cooled in sentry state.
- Patrol now **aligns** CPA-disabled + sentry-Active xAI accounts as `free_usage_429` cool-down (no HTTP probe).
- Example: `jc3f4dnnjh@…` file disabled, panel still active → becomes cooldown_quota on next full patrol.


## 1.1.7

### Fix: duplicate cool-down action logs
- `applyCooldown` logged `cooldown` twice (before + after CPA SetDisabled).
- Single log entry now; `cooldown_failed` still logged on disable failure.
- Patrol targets deduped by email/file.


## 1.1.6

### Fix: patrol all accounts HTTP 400
- Root cause: probe body sent `max_tokens` to `/v1/responses`, which rejects it.
- Use endpoint-specific payloads (`chat/completions` → `max_tokens`; `responses` → `max_output_tokens` only).
- Prefer `/chat/completions` first; on shape-related 400, fall through to the other path.
- Do not treat request-shape 400 as account death.


## 1.1.5

### Fix: permission_403 streak stuck at 3
- Cool-down **recover** (`ResetToActive`) no longer clears `streaks`.
- Streak ladders (e.g. ≥3 cooldown, ≥15 disable) now accumulate across cool-down cycles.
- Streaks still clear on **successful request** (streak mode) and **panel 启用**.
- Day fail count is not the same as continuous N (hits vs streak).


## 1.1.4

### Action log readability
- Human labels: 冷却补关 / 到期恢复 / 自愈打开 / CPA禁用对齐 …
- Full sentences with 【标签】prefix for key events.
- `owned_disable_was_enabled` → 「自有冷却期间文件被打开，已重新关闭」.
- `permission_denied` → 「权限拒绝(403)」; `recover_at` → 「冷却到期自动恢复」.


## 1.1.3

### Foreign-disable scan only on manual maintenance
- Periodic Tick: recover cool-downs, trash purge, **owned reassert only** — no `file_disabled_sync` / unowned reopen spam.
- Panel **立即维护** (`TickManual`): one-shot full scan — open unowned (if enabled) or mark CPA已禁用.
- Rationale: unowned disables are rare after one audit; continuous scan only creates noise and races.


## 1.1.2

### Ops preference restored: open unowned disables
- Default `reopen_foreign_disabled=true` again (open non-owned disables; wait next error).
- **Still hard-protects** owned cool-downs / panel permanent disable (mutex, ownership map, reassert, no ResetToActive on cool-downs).
- Production config set `reopen_foreign_disabled: true`.


## 1.1.1

### Fixed: conservative mode overwriting cool-down as CPA已禁用
- `file_disabled_sync` must not run when account is owned cool-down/manual.
- Triple-check identity ownership before tagging cpa_file_disabled.
- Lock bulk cooldown path.

## 1.1.0

### Hard fix: stop cool-down being reopened as "非自有禁用"
Root cause was structural, not one-account matching:
1. Self-heal reopen default ON + `ResetToActive` wiped cool-downs after 429/403.
2. HandleUsage and Tick raced on ownership.

Changes:
- **Default `reopen_foreign_disabled=false`** (safe). Unknown disables stay closed; only `recover_at` / panel enable open files.
- Serialize **HandleUsage / Tick / ManualEnable/Disable** with a mutex.
- Self-heal (if explicitly enabled) **never** opens owned cool-downs and **never** `ResetToActive` on protectable accounts; file enable only.
- Owned cool-down with file enabled still **re-disables** (`cooldown_reassert`).
- Production config sets `reopen_foreign_disabled: false`.


## 1.0.11

### Fixed: owned cool-down file left enabled after false reopen
- Precompute owned disable identities (auth_index/file/email).
- If CPA file is enabled but account is owned cool-down/manual → **re-disable** (`cooldown_reassert`).
- Self-heal only runs for non-owned disables.


## 1.0.10

### Fixed: cooldown then immediate "打开非自有禁用"
- Protect if **any** matched identity row is owned (not only pickAcc winner).
- 2-minute grace after `cooldown`/`candidate`/`manual_disable` action even if state briefly Active.
- Persist ownership (Log+Save) before CPA SetDisabled to shrink tick race.
- Do not ResetToActive owned cool-downs during self-heal.


## 1.0.9

### Panel sync: dual time columns + parallel refresh
- Account table: **最后请求** (CPAMP) and **最后动作** (sentry action log).
- Stamp `last_action` / `last_action_at` on every action log; backfill from retained logs on load.
- Sort by max(request, action) activity.
- Refresh loads `/state` + `/logs` in parallel; interval 8s; log timestamps include date.


## 1.0.8

### Fixed: unowned reopen despite auth_index cool-down
- Parse CPA auth-files `auth_index` / `id` / `path` / `account`.
- Match disabled files to sentry cool-downs by **auth_index first**, then file/email.
- Last-chance guard: never reopen if any owned cool-down shares email/file/auth_index.


## 1.0.7

### Action log pagination
- `/logs?limit=&offset=&q=` returns newest-first pages (`page.total/has_more/next_offset`).
- Panel loads 80 lines first; **更早** loads older pages; search uses server `q`.
- Log expiry still independent of counters (see below).


## 1.0.6

### Fixed: false "unowned" reopen on owned cool-downs
- Match CPA list entries to sentry accounts by **filename basename**, path suffix, and email derived from `xai-<email>.json` when list omits email.
- Prevents 429/`plugin_auto` cool-downs from being treated as untracked and reopened (e.g. `xai-5w4ggr8txx@...json`).


## 1.0.4

### Self-heal unowned disables (operator preference)
- Default `reopen_foreign_disabled=true`: if a CPA file is disabled but sentry cannot prove ownership (`plugin_auto` cool-down / panel `user_manual`), **reopen** it and `ResetToActive`.
- Next real usage/patrol error re-applies policy and re-stamps ownership (self-heal).
- Still **protects** real cool-downs, 候删, and panel permanent disables.
- `cpa_file_disabled` tags are no longer sticky protect — they self-heal on the next tick.
- Set `reopen_foreign_disabled=false` for the old conservative "keep closed + mark CPA已禁用" mode.


## 1.0.3

### Auth closed-loop hardening
- `CanAutoReenable`: `plugin_auto` sufficient; never open manual/CPA-file locks
- Candidate path also disables CPA file + stamps ownership/recover_at
- Permanent disable stamps ownership before CPA I/O
- Duplicate-account prune prefers cool-down/ownership rows over empty Active shells
- Regression tests for cooldown→recover, manual never auto-open, prune, legacy owner


## 1.0.2

### Fixed (plugin_auto ownership holes)
- Protect **any** cool-down/候删/manual/plugin_auto residue from foreign-reopen, not only exact state+source pairs.
- Pick the best matching account row when multiple state keys map to one auth file (email/file duplicates).
- Do not scrub away `plugin_auto` on Active; if `recover_at` is still in the future, **repair** cool-down state instead.
- Stamp cool-down ownership before CPA disable to close race with concurrent ticks.


## 1.0.0 — production release

### Added
- Error policy **escalation ladder** (e.g. 403: ≥3 cooldown, ≥15 permanent disable)
- Count modes: `streak` (success clears) / `total` (success keeps)
- Dynamic account state filters with live counts
- Live active labels (`正常·可用` / `正常·403×N` / …)
- Patrol job history with expandable per-job logs
- Unmatched error **shape split** into new policy keys
- Single version package `internal/version`

### Fixed
- Cooldown ↔ maintenance re-sync loop
- Scrub loop clearing policy streaks on active accounts
- Concurrent safety: `Get` / `AccountsSnapshot` return deep copies
- Patrol completion message race on unlocked `jobStatus`
- Policy ops permanent-disable button visibility
- State filter options not populated (`fillStateFilter` hook)

### Safety defaults
- Trash retention 7d; 402 never auto-trash; Super/Heavy protected
- Permanent disable requires `sentry_enabled`
- Atomic state save (tmp + rename, 0600)
