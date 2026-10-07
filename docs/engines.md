# 引擎细节参考

> 本文件是 `internal/provider` 各引擎的**细节参考**（DOM 事实、死配置清单、实测数字）。  
> 面向"要改某个引擎"的读者；**使用方式与设计取舍**分别见 [`../README.md`](../README.md)  
> 与 [`decisions.md`](decisions.md)，性能形态见 [`perf.md`](perf.md)。
>
> 引擎集固定 8 个：**bing / anysearch / quark（免费）+ exa / tavily / serpapi / qianfan / tinyfish（付费）**。  
> `bocha` 与 `zhipu` 已按要求删除，不要加回来。

---

## 〇、tinyfish（V10.6-A 新增）

- 端点 `GET https://api.search.tinyfish.ai`，请求头 `X-API-Key`，免费额度 **12,000 次/天**。
- 2026-10-07 实测 6 请求全 200，延迟 2.3~7.1s；**中文直发质量优秀**（食品包装/高血压两题
  20 条几乎全切题，权威密度高：cas.cn / WHO / UpToDate 中文 / 新华网）→ 不带语言参数、
  不做翻译桥；英文 query 返回的多是市场报告工厂页（同质化）。
- `date` 原样透传（web 绝对式 "Jun 4, 2025"、news 相对式 "55 months ago"），归一交给
  调用方；`site_name` 进 Signals.Source；响应 `page` 字段翻页语义未说明，首版单页 10 条。
- 测试 `tinyfish_test.go` 用 httptest 注入端点（端点是包级 var，仅测试可改）。

---

## 一、quark

### 1.1 路线：必须走移动站 + 纯 HTTP

**明确否决浏览器与滚动路线**（勿重复尝试）：

| 尝试过的路线                          | 结果                                 |
| ------------------------------- | ---------------------------------- |
| 桌面站 `www.quark.cn` + 浏览器渲染      | 实测 0 条 / 挂死                        |
| 移动站 + headless `DynamicFetcher` | 只拿到"请先登录"骨架页                       |
| 移动站 + `StealthyFetcher`         | 4/4 连续降级                           |
| 滚动加载（0→3 次，46→96 标题）            | 机制有效，但整条路被指纹降级堵死                   |
| **移动站 `m.quark.cn` + 纯 HTTP**   | **1.4~~2.5s / 11~~14 条 / 稳定 ← 采用** |

另注：`scrapling-go` v1.0.0 **没有 roll/scroll 能力**（纯 HTTP + 解析库）。

### 1.2 请求与解析

- 移动站是 SSR。请求**必须带** `uc_param_str=ntnwvepffrbiprsvchutosstxs&by=submit&from=kkframenew`，  
  缺了会被重定向到首页。**必须带移动 UA**（UA 门控，桌面 UA / 不带 UA → 0 条空壳）。
- 解析规则：`div.sc` 卡片 → `a.qk-link-wrapper[data-openpageurl]` → `.qk-title-text` / `.qk-paragraph-text`。
- ⚠️ 排查"选择器选不中"时**先 `count("qk-link-wrapper")` 判断页面是否被降级** ——  
  降级页上任何选择器都返回 0，容易误读成选择器写错。

### 1.3 DOM 真事实：标题与正文在两张平行的 `<a>` 里，不是父子

一张 `a` 装 `.qk-title-text`，另一张装 `.qk-paragraph-text`。

**正确解析**：以标题所在 `a` 为锚 → **向后遍历兄弟节点**（`anchor.NextSibling()`）→  
遇到下一个 `.qk-title-text` 就停（这是结果边界，防串条）→ 取其中最长的 `.qk-paragraph-text`；  
找不到再向上找祖先，最后兜底用锚自身。

> 曾因"从当前 a 往上找祖先"导致 7/11 条摘要为空；修后 11/11。

### 1.4 ⚠️ 反爬限流（最容易误判成代码 bug 的坑）

高频请求（实测连打 5~~6 次）后返回 \*\*858~~890B 极短响应\*\*，正文是  
`_____tmd_____/punish` 惩罚页（阿里系）。

- **HTTP 状态码仍是 200** → 只能靠**长度 + 特征串**兜底。  
  代码里 `quarkPunishMarker` + `quarkShortResponseErr` 已区分两种短响应。
- 惩罚时长不固定：轻度 20~30s 恢复；连跑 60+ 次猛打 → **8 分钟以上**未解除。
- **开发时严禁连跑压测。** 看到"反爬限流"就降频等待 20~30s，**不要改解析**。

**⚠️ 反直觉：latency 短（230~330ms）反而是坏消息**（被罚、秒返回空壳）。  
判断健康**必须看 `engine_status[].ok` / `count`，不能看 latency**：

| 形态   | latency     | 条数      |
| ---- | ----------- | ------- |
| 正常翻页 | 2800~6100ms | 8~11 条  |
| 被罚   | 230~330ms   | **0 条** |

### 1.5 耗时结构（"quark 5s+ 不是 bug"）

首跳 2.3~~2.5s + `snum` 每页 1.5~~2.5s **串行**叠加 ≈ 5.4s —— 这是**翻满 3 页的必然成本**。  
想压到 3s 内只有 `-max-pages=2`（~~4.1s / 6~~8 条）。**不要往并发方向改**（见 [`perf.md`](perf.md)）.

### 1.6 翻页：`snum` 可用，其余参数无效

- `m.quark.cn/s?q=` 首次访问会 302 跳到带 `queryId` 的 session URL，之后在该 URL 上逐页  
  `snum=2..N` 取增量（重叠约 5~6/9，确有新结果）。
- **无效参数**：`page` / `pg` / `start` / `pn` / `pageNum`（要么同 P1、要么怪结果）。
- 包级 `defaultQuarkPages = 3`，可被 `-max-pages` 覆盖。
- **不要为了凑满 15 加到 8 页**（曾加到 8 又回落，意义不大且拖耗时）。
- ⚠️ **`sid` 必须剥掉（2026-09-19 修）**：同一张卡片在 p1/p2/p3 里**只差 `sid`**
  （翻页会话 id）。不剥的后果 ① 同批结果在输出里重复 2~3 次（条目级 seen 去重失效）；
  ② **「某页零新增就停翻页」失效** —— 每页看着都像有新东西，白付 2 页请求
  （实测「跨境电商 政策」单引擎 6.91s → 2.80s）。剥它安全：带/不带 `sid` 的预览 URL 都 200。

### 1.7 壳页四类分类处理

用户改向原话："**壳页基本不是垃圾极其有价值**" —— 不再一刀切删，按域名分类：

| 类型           | 例子                                                                   | 处理                                                                                                      |
| ------------ | -------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------- |
| 外部站          | 任意非 quark 域                                                          | **原样保留**，`cleanQuarkShellURL` 不动它（受 `isQuarkHost` 保护）                                                   |
| 聚合壳          | 百科、文档预览 `vt.quark.cn/blm/quark-doc-ssr-*`、`page.sm.cn/blm/midpage-*` | **保留** + 清洗 `quarkTrackParams`（`uc_param_str`/`uc_biz_str`/`bucket`/`from`…；保留 `q`/`title`/`doc_title`） |
| 媒体垂直 / 无正文卡片 | `isQuarkMedia` 判定                                                    | **源头剔除**                                                                                                |
| 中转壳          | `m.quark.cn/s/<id>`                                                  | **源头直接丢弃**                                                                                              |

**关于中转壳**：用户拍板"这笔账不划算 —— 直接丢弃，省请求、避反爬，结果由其他引擎补"。  
曾实现过 `resolveTransitShells` 并发还原直链，**已整体删除**（连 `followRedirect` 一起）。

**关于 `s2.zimgs.cn`**：**不是**壳 —— `isQuarkHost` 用"前导点后缀 + 裸主机名"判定，不会误判。

**⚠️ 「精选资料」聚合卡没有摘要（2026-09-19 实测，别再当解析 bug 查）**：
`sc_doc_sc_new` 卡（卡片头是 `<query>-精选资料`）里的条目是 grid 形态，每条只有
**封面 + 标题 + "24阅读"**，全卡 `qk-paragraph-text` 出现 **0 次**、`data-log` 只有曝光计数
→ `quarkSnippet` 取不到东西是**对的**，上游就没给摘要。
要填只能去抓 `vt.quark.cn` 预览页（多发请求 + 撞反爬），按中转壳同一条账**不做**；
也不删这些条目（"过滤 = 删结果"是契约红线）。

**函数更名**：`isQuarkShell` 已删，换成 `isQuarkHost` / `isQuarkMedia` / `isTransitShell` /  
`cleanQuarkShellURL`。⚠️ `-drop-shell` / `url_kind` 开关**已删除**，不要再加回来。

### 1.8 ⚠️ `isQuarkMedia` 必须覆盖三个域（曾修过两个真实漏网）

| 漏网       | URL 形态                                                           | 判据                                                                                     |
| -------- | ---------------------------------------------------------------- | -------------------------------------------------------------------------------------- |
| ① 神马域视频页 | `page.sm.cn/blm/video-page-710/video?h=www.bilibili.com&id=26_*` | 路径关键词（`quarkMediaPathKeywords`）**或** `h=` 指向视频站（`quarkVideoHosts`，含 `user_auth_video`） |
| ② 夸克字词卡片 | `p.quark.cn/<hash>/char?entity=猫&...`                            | 末段为 `char`/`word`，**或**带 `entity=`                                                     |

**根因**：早期判据是 `host != m.quark.cn → return false`，凡不在 `m.quark.cn` 的媒体页全绕过  
（`isTransitShell` 同样要求 m.quark.cn，两道防线一起失效）。**现在按域分治**  
（`m.quark.cn` 看 `/vsearch` + `qtab`）。**未知类型默认保留。**

回归测试见 `internal/provider/quark_test.go`（含用户贴出的两条真实 URL）。

### 1.9 卡片自带声明式类型标记（2026-09-17 取证）——**不能替代 URL 规则**

**取证方式**：用 `rawdump_probe_test.go` 抓 4 组 query 的原始 HTML  
（跨境电商政策 / 风景图片 / 猫咪视频 / 猫），共 37 张 `div.sc` 卡片。

**标记确实存在，直接写在卡片上，不需要推算**：

| 位置                      | 含义            | 实测取值                                                                                                                                                                                                                          |
| ----------------------- | ------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `data-sc` JSON 的 `sc`   | **语义类型**      | `ss_text` / `ss_pic` / `addition` / `nature_result` / `news_uchq` / `doc_sc_0` / `doc_sc_1` / `doc_sc_new` / `baike_sc` / `text_recommend` / `life_show_general_image` / `general_entity_*` / `note_video_relevant_recommend` |
| `data-tpl`              | 模板名           | `<模板>~<槽位>`，如 `news_uchq~news`、`doc_jgh~index`                                                                                                                                                                                |
| `class` 里的 `sc_*` token | 与 data-tpl 同源 | `sc_news_uchq` / `sc_natural_result` / `sc_doc_jgh` …                                                                                                                                                                         |
| `data-sc.ads`           | 逐卡广告标记        | **实测 8 张卡全为 0**（本次样本无广告卡）                                                                                                                                                                                                     |
| `data-sc.pos` / `pg`    | 卡片位置 / 页码     | `1_1`…`pg=1`                                                                                                                                                                                                                  |

**⚠️ 但标记不能替代 URL 形状规则** —— 三条实测证据：

1. `nature_result`（自然结果）**既覆盖正常网页**（`list.iqiyi.com`、`gs.amazon.cn`），  
   **也覆盖 `page.sm.cn/blm/video-page-*` 媒体页** —— 该留的与该剔的是同一个标记，**没有区分力**。
2. `ss_pic` 名字像"图片结果"，实测指向的却是**游戏下载页 / 163 视频页 / 电视剧页 / 图片站混杂**  
   —— 它是**卡片模板（是否带配图）**，**不等于"这条是图片"**：按它剔除会误杀正常页，
   按它标 `type=image` 也会误标。
3. 反过来，`m.quark.cn/vsearch` 这张媒体垂直卡，标记是 `news_uchq`；  
   而 `news_uchq` 本身是正常新闻卡 —— 同样不能剔。

**当前实现**：只用 `isQuarkMedia(URL)` —— **URL 形状规则**。

标记兜底曾经作为**并集**参与剔除（`isQuarkMedia(URL) || isQuarkMediaByMark(卡片标记)`），
**2026-09-19 已删除**：在上面的样本上它与 URL 规则**完全重叠、零增量**，收益只是"未来
URL 形状变化时的鲁棒性"—— 那就等形状真变了、拿到新的原始 HTML 再重建，不留没人走的旁路。

标记本身**仍然带出**：`data-sc` 的 `sc` 字段进 `signals.card_type`，
交给调用方做精细筛选（`data-tpl` / `class` 两个同源字段不再解析，没有消费者）。

> ⚠️ `data-sc.sc=addition` 是**明确的推广/导购卡标记**（实测那张指向 `gs.amazon.cn`）。  
> 本 CLI **不使用它做剔除 / 过滤**（"过滤 = 删结果"是契约红线，见  
> [`decisions.md`](decisions.md#过滤--删结果一律不做)）；它只随 `signals.card_type`  
> 输出，由调用方自行决定怎么用。

---

## 二、serpapi

**端点**：`GET https://serpapi.com/search`（api_key 走查询串）。无 HTML 反爬，  
是 Google 结果的稳定通道（取代原 serper.dev）。

### 2.1 `num` 被忽略，默认会翻一次页（2026-09-17 取证后改写）

**请求 `num` 不被接受，Google 首页固定约 10 条**，所以默认 `-limit=15` 下通常**发 2 次请求**  
（首页 10 条 → `start=10` 补足到 15）。

取证（原始响应体落盘，脚本 `internal/provider/rawdump_probe_test.go`）：

| 对照                        | `num=15`    | `num=100`   |
| ------------------------- | ----------- | ----------- |
| 响应 `search_parameters` 回显 | **无 `num`** | **无 `num`** |
| `organic_results` 条数      | 10          | 10          |

回显只有 `engine/q/google_domain/hl/gl/device`；换 query 复核首页也只有 8 条；  
响应自带 `serpapi_pagination.next_link`（`...&start=10`）。

⚠️ 本节旧写"`num` 生效、首页即回满 15 条、后两页从未被请求"，**已撤回**。  
误判来源："`maxPages` 设 1/2/3 均为 15 条" —— 但 `-max-pages` **只对 quark 生效**，  
三次其实是同一路径，那 15 条正是内部翻页凑出来的。  
（注：`internal/config/config.go` 里"num 被忽略（单页 ≤10）、靠 start 翻页累积"**本来就是对的**。）

### 2.2 默认会执行的分支

- `start>0` 分支 —— **执行**（第 2 次请求带 `start=10`）
- `page*step` 乘法 —— **执行**（page=1）
- `step` 赋值 —— 通常不执行（需首页 >10 条）
- `if page==0{return}` 之下的 `break` —— **执行**（第 2 页失败）
- `len(raw)==0` 的 break —— **执行**（第 2 页空 = 结果已尽）
- `serpapiMaxPages=3` —— 被真正用到（默认用掉 2 页）

**推论**：serpapi **有**"翻页中途失败保留已抓结果"的能力（第 2 页失败只 `break`）；  
整引擎归零只发生在**首页失败**。

### 2.3 参数与历史遗留

- **重试装置已整体删除（2026-09-19）**：`serpapiAttempts`(=1，曾更名自 `serpapiRetries`)
  与 `serpapiDelay` 一起去掉，`serpapiFetchPage` 改为**单次请求、失败即返回**。  
  原有装置里 `for attempt:=1; attempt<=N` 只迭代一次、`attempt<attempts` 恒 false，  
  即 `serpapiDelay` 是**绝对死代码**。想重试就按当时的实测证据重建，别靠"留着会自动复活"。
- **`serpapiAttempts` 2→1 那次改动属"无实测支撑"**：理由仅"砍掉理论最坏 2×30s=60s"，  
  而**该最坏情形从未被观测到**（三次探针均 `err=nil`、无一次触发重试分支）。  
  **32.5s 是首次请求就慢，不是重试放大** —— 删除重试装置不影响这个尾巴。
- 单引擎超时下限 `EngineTimeoutFloor["serpapi"] = 30` **保留** —— 跟上游抖动较劲的唯一真手段。
- ⚠️ **抖动上界又刷了一次（2026-09-17）**：全引擎实跑中出现过 serpapi 单引擎  
  **40003ms**，把整轮拖到 40.1s（`DefaultEngineTimeout=20` 被 floor 抬到 30，  
  所以 40s 并非撞超时）。随后两次单引擎复测均为 **1.95s / 15 条**。  
  **属偶发，未定位** —— 但抖动区间应从"1.8s ~ 32.5s"放宽到 **1.8s ~ 40s**。

---

## 三、bing

翻页**必须带 `Cookie: _EDGE_V=1`**：

- 不带 cookie → `first` 翻页参数被忽略（p1∩p2 = **100% 重叠**）
- 带上 cookie → `first=11/21/...` 生效（重叠 **0%**）

**移动 UA 反而返回 0 条**，故坚持桌面 UA。

实测 bing 单查询从 ~9 条升到 ~13 条（limit=15）。

> ⚠️ 不要因为"旧注释说 first 被忽略"就改回单页 —— 那是**没带 cookie** 时的假象。

---

## 四、其余引擎要点

### anysearch

- `-anysearch-vertical` 默认 **true**。`matchDomain` 按关键词路由到  
  finance/academic/legal/business/… 等 domain → 拉 sub-domains 目录 → 选 sub_domain →  
  带 `tag` 做垂直检索；通用查询回落通用搜索。
- ⚠️ 路由是 **first-match-wins**（finance 在 business 之前），同时含"股票"与"公司"会命中 finance。
- ⚠️ anysearch 官方硬上限 **10，永远拿不到 15 条**。
- 垂直可能返回大量**同一个 URL** 的实体记录，被聚合层按精确 URL 并成 1 条是  
  **合法合并，非 bug**。

### qianfan

429 = `QUOTA_USER_DAILY_FREE` **日免费额度耗尽**，不是限流、不是 key 失效，换 key 或等次日。  
**与 quark 反爬限流是不同的东西，别混。**

### metaso（密塔，官方 API）

- `POST https://metaso.cn/api/v1/search`，Bearer 鉴权（`METASO_API_KEY`），
  **无需翻页**：`size` 要多少给多少。实测 `size=20` 回满 20 条，`size=50` 与 `100`
  **都不报错**，上游按自身结果集封顶（某 query `total=47` → 返回 47）。
  6 个 query 全部回满 20 条 —— 它是唯一没有"条数天花板"的引擎。
- 计费按 `credits`（实测 3 / 次，响应里回显）。⚠️ **它是本仓库唯一按次花钱的引擎**，
  压测/回归前先想清楚代价。
- 快：单次 20 条实测 0.68s / 0.79s（两次），比 quark 快一个量级，不构成整轮瓶颈。
- 响应 `{credits, total, searchParameters, webpages[]}`；本 CLI 只取每条的
  `link / title / score / snippet / date`，`Rank` 直接用切片序（实测 `position` 与切片序一致）。
  `scope` 固定 `webpage`（文档/论文等 scope 没有调用方，要时再加）。
- ⚠️ `score` 是**字符串等级**（`high` / `medium` / `low`）→ 落 `signals.score_level`，
  按 `float64` 解会**整引擎解析失败**（见第六节）。
- 质量口径实测（2026-09-19，6 关键词 × 20 条，与其它引擎同一套人工判定）：
  广告率均值 **0.37500**、跑题率 **0.00000**（并列最低，且是唯一 6 轮零跑题的引擎 ——
  「机械键盘 选购」「量子计算 进展」都没被切词切歪，而 bing 那两轮 10/10 全废）。
  广告形态集中在"仪器 / 试剂 / 检测公司软文"（微生物检验那轮 9/20）；
  商业类 query 与其它引擎同病（空气炸锅 20/20、机械键盘 11/20）。
  无效条目 2/120（1 个空壳问答页、1 个正文拼接错乱页）。

### 摘要 markdown 清洗（8 引擎统一）

`cleanSnippet()` 在 8 个引擎输出点统一调用：`![alt](url)` 整段删、`[text](url)` 降级为 `text`，  
再 `normalizeSpace`。命中面实测：exa 1/39、tavily 1/37。

**这是文本清洗不是过滤** —— 不删结果、不改 URL，与"过滤 = 删结果"契约不冲突。  
（用户曾把某条摘要里的 `![](...)` 误认成 quark 的问题 —— 判来源看 `engine` 字段。）

### exa 编码乱码 = 上游字段级缺陷（2026-09-17 定案）

直连 `api.exa.ai` 复现 → **同一份响应里 `title` 乱码率 0.00（5/5 正常），  
`text`（→ snippet）0.84 / 0.83**。

**判据：`title` 好而 `snippet` 坏 = 上游字段级缺陷；两者都坏才可能是本地解码问题。**

全引擎扫描确认**只有 exa 中招**（其余 maxS ≤0.07）。

处理：`repairMojibake()` 挂在 `cleanSnippet` 首行，**只做无损可逆修复** ——  
字符全在 Latin-1 → 映射回字节 → 必须 `utf8.Valid` 才采用；失败**原样返回**，  
绝不用 U+FFFD 凑字、**不做部分修复**。实测 exa 9/10 修复成功，剩 1 条是上游  
**真丢字节**（如 `...e7 e7...` 两前导字节相连、`â uu` 少了 `0xC3`）→ 按设计保留。

⚠️ **两个坑**（写代码时别再犯）：

1. 别用"高字节占多数"当判据 —— 混排文本会被漏，要用**存在性判据**  
   （存在合法多字节 UTF-8 序列）。
2. 别写"把解码结果再展开回 Latin-1 比对"当无损证明 ——  
   解码后是中文（rune > 0xFF），该比对必然失败，**等于永远不修复**。  
   正确的无损证明就是 `utf8.Valid` 本身。

测试见 `internal/provider/mojibake_test.go`。

---

## 五、`-max-pages <n>` 旋钮

链路：`provider.Options.MaxPages` → `aggregate.Options.MaxPages` → `search.go` flag。

⚠️ **当前实际只对 quark 生效**（0 = 默认 3）：

- bing 按 `limit` 自算页数（`(limit+9)/10` 封顶 5）
- serpapi 读自己的内部常量（首页约 10 条、默认翻一次页补足 15，**与 `-max-pages` 无关**）

四处帮助文本/注释均已标注（`search.go` / `main.go` / `provider.go` / `aggregate.go`），  
**不要再写回"各引擎默认"**。

实测（"微生物"）：

| `-max-pages` | 耗时       | 条数                        |
| ------------ | -------- | ------------------------- |
| `=2`         | 4.1~5.1s | 6~8 条                     |
| 默认 3         | 5.4~6.2s | 10~11 条                   |
| `=4`         | 6.0~6.3s | 6 条（`snum=3` 已零新增，后续页纯浪费） |

> ⚠️ **5.4~6.2s 是"quark 真翻满 3 页"的偏慢一侧，不是默认配置的常态**：  
> 多数 query 在 `snum=2` 就零新增提前 `break`（只翻 2 页）→ 整轮 **3.2~3.7s**。  
> 两处数字并列**不是自相矛盾**，是条件不同。

---

## 六、上游信号字段（输出里的 `signals`）

**动机**：这些值本来就在引擎响应里，此前解析时被直接丢弃 —— 调用方只能看到  
"引擎名 + 排名"，没有任何权威性 / 相关性依据。2026-09-17 起接入输出。

⚠️ **只做"带出来"，不做任何过滤或排序**（契约见  
[`decisions.md`](decisions.md#过滤--删结果一律不做)）：字段为空就省略整个对象，  
调用方自行决定怎么用。

| 字段                 | 来源引擎              | 实测类型与取值                                           |
| ------------------ | ----------------- | ------------------------------------------------- |
| `score`            | tavily            | float，0~1（如 `0.84858394`）                         |
| `score_level`      | metaso            | **字符串**等级，实测 `high` / `medium` / `low`（2026-09-19 随引擎回归） |
| `rerank_score`     | qianfan           | 实测为 int（值 `1`）                                    |
| `authority_score`  | qianfan           | 实测 float（`1` / `0.5`）                             |
| `source`           | serpapi / qianfan | 发布方，如 `商务部财务司` / `百家号` / `国家税务总局政策法规库`            |
| `card_type`        | quark             | 卡片类型，如 `ss_text` / `doc_sc_new` / `nature_result` |


**⚠️ 类型陷阱（已实测，勿按直觉改）**：metaso 的 `score` 是**字符串** `"high"`，  
**不是数字** —— 若按 `float64` 解码，**整个引擎会解析失败被判死**。  
故单独用 `score_level` 承载，与数字型的 `score` 分开。

**⚠️ 曾经的 `authority_type` / `authority_domain` 已删除（2026-09-23）**：它们只在原 metaso
响应里出现过（如 `government` / `zfkawlb.cq.gov.cn`），metaso 改走官方 API 后**没有任何引擎
填充**，两个字段在 JSON 里永远不出现。留着就是"挂在墙上没通过电的插座"，故连同
`MergeSignals` 里的两行一并删掉。将来若某引擎真给这两个值，**先 dump 原始响应取证再重建**
（别照这段历史记录直接加回）。

**合并约定**：同一 URL 被多引擎命中时，字符串取首个非空、数值取更大者  
（与标题 / 摘要 / 日期的处理同风格）；两边都空则不输出 `signals`。

`bing` / `exa` 的响应里没有这类字段，故它们的条目通常不带 `signals`  
（但可能因与其他引擎合并同一 URL 而"继承"信号）。
