# metasearch_cli

多引擎并发元搜索 CLI（Go）。它把「检索 → 可达性过滤」这条链路重写了一遍，
参考实现是 `deep_search/search_engine/search_core`。

它主要给程序调用（配套的 `deep_search` MCP 就是调用方之一），也能直接在命令行里用。
输出是结构化 JSON，诊断字段齐全，**不做排序**（排序交给调用方）。
单文件 exe，不需要 CGo、Python 或浏览器。

参考实现里那套 RRF 融合 + 引擎权重 + 内容质量分全部未移植 —— 本 CLI 只负责把"能连上的
URL、以及它们被哪些引擎、在第几名命中"如实摆出来。理由见 [`docs/decisions.md`](docs/decisions.md)。

```
metasearch_cli <关键词> [参数]        搜索
metasearch_cli apikey <子命令> [参数] 密钥配置
```

`gui.py` 是同目录下的 tkinter 极简界面（只输入关键词，JSON 打到控制台；密钥可保存覆盖）。
用带 tkinter 的 Python 运行（`python gui.py`），部分精简版或托管版 Python 未带 tkinter。

## 获取

**一、下载现成二进制。** 到 [Releases](../../releases) 取
`metasearch_cli_windows_amd64.exe`，放到任意目录就能运行，不需要装 Go。

**二、自己编译。** 需要 Go 1.26.6 或更高版本。

```bash
git clone https://github.com/zhidian-cmd/metasearch_cli.git
cd metasearch_cli
go build -trimpath -ldflags "-s -w" -o metasearch_cli.exe .
```

`-trimpath` 裁掉编译时嵌进二进制的本机路径，`-s -w` 裁掉符号表和调试信息。
两个一起用，体积能小约三成。

国内要把模块代理指向可用的镜像，否则依赖拉不下来：

```bash
go env -w GOPROXY=https://goproxy.cn,direct
```

> **文档分工**：本文件讲**怎么用**；
> 设计取舍与"为什么不这样做" → [`docs/decisions.md`](docs/decisions.md)；
> 性能实测数据与归因 → [`docs/perf.md`](docs/perf.md)；
> 引擎质量评估算法（整体率 / 排名加权率，含公式与判定表） → [`docs/ranking.md`](docs/ranking.md)。

---

## 一、搜索流水线

```
1. 并发跑全部候选引擎        每引擎一个 goroutine；单引擎失败/超时只影响自己
   └─ 每收完一个引擎，立刻把它的域名交给敲门预热（非阻塞）
2. 归一化                    只按 URL 精确相等合并：engine 收齐命中的引擎、positions 保留各自排名
3. 保守去重                  规范化后 URL 全等 → 无条件合并；其余候选对过结构判决（含三道否决闸）
4. 批量 TCP 握手             按 host+port 去重并发探；只留可连接的 URL
                             （大多数域名已有预热结果 → 直接命中）
5. 结构化输出                json / jsonl / text
```

第 1 步与**敲门**（第 4 步）**重叠**是有意设计：域名在引擎结果回来时就确定了，没必要等 8 个引擎全跑完
再串行敲门。这一处把整轮耗时从 `聚合 + 敲门` 压到 `max(聚合, 敲门)`。
整轮实测约 **3.2~3.7s**（≈ 最慢的 quark），详见 [`docs/perf.md`](docs/perf.md)。
第 2 步与第 3 步是**两件事**：前者只认"URL 一个字节都不差"，后者才处理"同一页的两种写法"。

### 输出结构

```json
{
  "engine":    ["anysearch", "exa", "serpapi"],
  "positions": { "anysearch": 5, "exa": 13, "serpapi": 10 },
  "title":     "……",
  "snippet":   "……",
  "url":       "https://...",
  "signals":   { "rerank_score": 1, "authority_score": 0.5, "source": "百家号" }
}
```

- **同一 URL 被多个引擎命中时合并成一条**：`engine` 收齐全部命中方，`positions` 保留
  各引擎内部的原始排名。聚合层这一步只按 **URL 精确相等**合并 —— 差一个字节就是两条。
- **同页判定是去重层的活**（见下方「去重」）：规范化后 URL 全等（尾斜杠、`?utm_*`、
  百分号编码写法差异）**无条件合并**；其余候选对必须"同站 + 同栏目 + 文本够像"才合，
  中间还有三道否决闸。被合掉的 URL **不会消失** —— 作为**备用 URL**（连同它自己的标题与
  来源引擎）进该条的 `merged_from` 字典，随时可回取。
- **`snippet` 是完整正文，不做长度截断**（参考实现的 `SNIPPET_MAX=400` 已被刻意移除）——
  下游拿它做精细筛选，截断等于凭空少一段依据。代价是 JSON 变大（实测一轮 312 KB）。
- **`signals` 是上游引擎自带、但对下游筛选有用的信号**（相关性分 / 权威性 / 发布方 /
  卡片类型），此前被丢弃、现已带出。**只带出，不做过滤或排序**；没有信号的引擎
  省略整个对象。各字段来源与实测取值见
  [`docs/engines.md`](docs/engines.md#六上游信号字段输出里的-signals)。

`url` 分四类处理：

| 类别 | 处理 |
|---|---|
| **外部原始站点**（baijiahao.baidu.com 等） | 原样输出，一个字节不动 |
| **quark 聚合壳**（`baike.quark.cn` / `vt.quark.cn` / `page.sm.cn`） | **保留**（是有价值的文本），仅剥 URL 上的跟踪参数还原原生地址 |
| **quark 媒体页**（`m.quark.cn/vsearch/*`、`page.sm.cn/blm/*-page-*`） | **源头剔除**（不是单篇网页）。判据是 **URL 形状规则**（卡片自带类型标记只带出、不做判据），⚠️ 跨两个域，见 decisions |
| **quark 中转壳**（`m.quark.cn/s/<id>`） | **源头丢弃**（还原要多发请求且易撞反爬，不划算；缺的条数由其他引擎补） |

信封层另有 `engine_status`（每引擎成败/条数/耗时/错误）、
`dedup`（档位与阈值 / 进出一对数字 / 两阶段减量 / 否决与通过计数）、
`reachability`（探测域名数/可达数/剔除条数/预热命中数）、`stats`、`elapsed_ms`。

**`results` 不做任何排序**：按引擎名字母序入池，同引擎内保持其原始排名；同一 URL 后续再次
命中时并回首次出现的那条。

### 去重

**恒定开启、无开关**（判据强度固定在 conservative），偏置是**宁可漏杀，不可误杀**。
刻意不提供"关掉 / 调松"的参数 —— 调用方只给关键词，不由外部把这一层降下来；
曾有一个能整层关掉的开关，已从源码删除（理由见 [`docs/decisions.md`](docs/decisions.md)）。

```
-tracking <档位>       剥哪些追踪参数：default(默认) / minimal / none
```

两维级联 —— **先索引、后判官**：

| 步骤 | 做什么 | 判据 |
|---|---|---|
| 维度一 | 分桶**产出候选对**，本身不删任何东西 | 规范化 URL / 标题 / 摘要任一相等 |
| 契约 0 | 同页**无条件合并** | 规范化后 URL 全等（**不看文本**） |
| 维度二 | 只对候选对判决 | 同站 + 同栏目 + 标题/摘要够像，且三道否决闸全过 |

- **规范化**：URL 去 fragment、剥 tracking、**query 解码后按 (键,值) 重排再编码**、尾斜杠归并、
  host 小写、去默认端口；路径按 Go 的 `EscapedPath` 归一**并把 `%xx` 统一成大写**。于是
  `/item/紫色面具/1`、`/item/%E7%B4%AB.../1`、`/item/%e7%b4%ab.../1` 三种写法判为同一页，
  `?write` 与 `?write=`、`?a=1&b=2` 与 `?b=2&a=1` 也判为同一页。
  标题会**剥站点后缀**（`… - 知乎` / `…_百度百科`）—— 不剥的话，
  同一页被不同引擎挂上各自站点名就合不上。
- **三道否决闸**（任一命中即放过）：`veto:cross_site` 跨注册域、`veto:diff_section`
  同站但不同栏目、`veto:diff_numbers` 数字集合不重合（分页 id 不同）。计数在 `dedup.veto` 里，
  它是"**差点被误杀**"的直接证据 —— 否决多说明判据在拦，不是没跑。
- ⚠️ **否决闸不是累赘**：京东首页与品牌页的标题归一后**完全相同**（都是"紫色面具品牌及商品"），
  只有 `veto:diff_section`（路径首段 `brand` vs 空）拦得住。删掉它就会开始误杀。
- **合并恒定留痕**（不由开关控制）：`merged_from` 是**备用 URL 字典** —— 键 = 被吞并的那条 URL，
  值 = 它的标题、**它自己的**来源引擎、判定理由：
  ```json
  "merged_from": {
    "https://vt.quark.cn/…/preview?id=2AB70AC1…": {
      "title": "空气炸锅电子食谱",
      "engine": ["quark"],
      "reason": "d1:title_exact,url+title_high,same_host"
    }
  }
  ```
  用 URL 当键而不是数组：URL 本身就是天然主键，且被删的条目**只是降级为备用** ——
  调用方随时能把备用入口连同它的标题与来源一起取回。
- ⚠️ `dedup.raw → dedup.final` **只描述去重这一层**；`reachability.dropped_items` 是敲门阶段的
  剔除量。两者别混着看 —— 只有前者是"去重删掉的"。

判据的来源、为什么默认 conservative、以及"哪些看着像重复其实是不同页面"，
见 [`docs/decisions.md`](docs/decisions.md) 第五节。

### 引擎

| 引擎 | 类型 | 说明 | 默认 |
|---|---|---|---|
| `bing` | 免费 | 桌面 UA + `Cookie: _EDGE_V=1`，`first` 翻页聚合 | ✅ |
| `anysearch` | 免费 | 匿名可用；配 key 限流更宽；支持垂直检索 | ✅ |
| `quark` | 免费 | 移动站 `m.quark.cn`（SSR，纯 HTTP），`snum` 翻页聚合 | ✅ |
| `exa` / `tavily` / `serpapi` / `qianfan` / `metaso` | 付费 | 配 key 即启用 | 配 key |

条数口径**恒定为每引擎 15 条上限**（参考实现 `text_sources_per_query`），不由调用方按需
下发。各引擎真实天花板不同：anysearch 官方上限 10、exa 约 12、bing 每页约 10（翻页后
约 13）、serpapi **`num` 被忽略、首页只有约 10 条**（默认会带 `start=10` 再取一页补足，
即通常发 2 次请求）、quark 约 11~15（受自身结果集与翻页重叠限制，不保证满 15）。
**metaso** 反过来是唯一"要多少给多少"的（实测 size=20 回满 20 条、50/100 均不报错）。

各引擎的**广告率 / 跑题率 / 无效率**实测（8 引擎 × 6 关键词 × 20 条，含口径与复现命令）见
[`docs/perf.md` 第八节](docs/perf.md#八引擎质量实测广告率--跑题率--无效率)。

---

## 二、密钥配置（持久化）

```bash
metasearch_cli apikey list                    # 状态（脱敏）+ 各层来源
metasearch_cli apikey set tavily mk-xxxx      # 写入/覆盖
metasearch_cli apikey set tavily -            # 同上，密钥从 stdin 读一行（推荐）
metasearch_cli apikey unset tavily            # 删除
metasearch_cli apikey path                    # 路径与优先级
metasearch_cli apikey test exa,tavily         # 实发最小请求验证可用性
```

**为什么不直接把 key 写在命令行里**：命令行参数在 Windows 任务管理器、Linux `ps aux`、
macOS `ps` 里都是**明文可见**的，同机其他用户能读到完整密钥。所以 GUI / 脚本应走
`-` 形式（`echo "mk-xxxx" | metasearch_cli apikey set tavily -`），密钥只经过 stdin，
不进进程列表。两种写法等价，落盘结果一致；`-` 是**显式哨兵**，不写 key 参数只会报用法错误，
不会静默去等 stdin。

`set` 落盘到与当前工作目录无关的固定位置，换目录、换 shell 都读得到；
它是**原位替换**，反复设置不会产生重复行，文件里的注释和其它键原样保留。
用 `-env <文件>` 可指定写到别处。

```
Windows : %AppData%\metasearch_cli\.env
macOS   : ~/Library/Application Support/metasearch_cli/.env
Linux   : ~/.config/metasearch_cli/.env
```

**读取分层**（高 → 低，前层覆盖后层）：

1. 进程环境变量
2. `-env <文件>` / `METASEARCH_ENV_FILE`
3. 可执行文件同目录 `.env`（便携用法）
4. 当前工作目录 `.env`
5. 持久文件（`apikey set` 的默认写入目标）

**两个防呆**（分层合并最容易踩的坑）：`set` 后复核生效来源，被更高层遮蔽会直说"本次写入
不生效，实际来源是 X"；`unset` 后也复核，若 key 仍由更底层提供会告知"要对那个文件
执行 `apikey unset -env <文件>`"。不说清楚用户会以为"删了怎么还在"。

---

## 三、代码结构

```
main.go                 入口、命令路由、使用说明、参数解析
search.go               搜索流水线 + 输出渲染（json/jsonl/text）
apikey.go               密钥命令
gui.py                  tkinter 极简界面（可选，非 Go 部分）
internal/model/         数据结构
internal/config/        常量、引擎表、.env 分层加载与读写
internal/aggregate/     并发聚合（goroutine + 信号量 + 全局熔断）
internal/reach/         并发 TCP 握手（含域名级预热 Prefetcher）
internal/provider/      8 个引擎 + 公共 HTTP 通道
docs/                   decisions.md（决策） / perf.md（实测） / engines.md（引擎细节）
```

**取证工具**（不属于正式构建，`go build`/`go test` 默认都看不到它）：

```bash
# 把 8 个引擎的原始响应体各落盘一份，用于回答"上游到底返回了什么"这类问题
go test -tags probe -run TestDumpRawBodies -v ./internal/provider/
# 输出到 _rawdump/（默认目录见文件内 dumpDir()）；可用 RAWMAP_Q / RAWMAP_NUM / RAWMAP_DIR 覆盖

# 走完整 provider.Search（含引擎内部翻页），统计"某引擎实际能拿几条"，并可逐条落盘做质量判读
RAWMAP_Q="关键词" RAWMAP_ENGINE=exa,tavily,serpapi RAWMAP_LIMIT=20 RAWMAP_ITEMS=/tmp/items.jsonl \
  go test -tags probe -run TestEngineSearch -count=1 -v ./internal/provider/
# ⚠️ RAWMAP_LIMIT 可以 >15，**只有探针能这样**：正式 CLI 的聚合层把"每引擎入池"钉死在 15
#    （`-limit` 调大也不放宽，见 aggregate.go）。RAWMAP_ITEMS 逐条追加 {engine,query,rank,title,url,snippet,date,signals}
```

放在 `internal/provider/` 里且带 `probe` 构建标签，是为了能直接调用内部的 `do()`/`getClient()`，
保证发出的请求与正式代码**同一套传输层**；载荷照抄各 provider 的 `Search()`。
**凡"某字段到底存不存在 / 某分支到底走不走"的疑问，先用它取证再下结论**——
本项目的证据纪律要求如此（`serpapi num` 那次就是猜错的）。

共 **17 个业务 go 文件 + 4 个测试文件 + 2 个取证探针 / 7 个包 / 业务代码约 5.2k 行、测试与探针约 1.1k 行**
（探针 = `internal/provider/rawdump_probe_test.go` + `internal/dedup/probe_test.go`，都带 `probe`
构建标签，普通 `go build`/`go test` 看不到它们。
2026-09-19 按"只留能兑现的东西"清了一轮：删掉整包未接线的 URL 类型标注模块
`internal/provider/mediatype.go`（业务 17→16 个文件）等；2026-09-23 又按同一条判据过了一遍
（代码净 -155 行：参考实现兜底层、`reach.Options`、`looksLikeMojibake`、两个无来源的 `signals`
字段等），两轮的删除清单与理由都在 `docs/decisions.md`）。
参数解析用标准库 `flag`，
外面套一层重排以支持"关键词与参数任意混排"（标准库遇到第一个位置参数就停止解析）。

**单测覆盖 `reach` / `provider` 两包**：
- `reach` 里的 `TestPrefetchThenFilterNoDeadlock` **必须保留** —— `inflight` 只允许由
  `verdict` 注册，`AddURLs` 只能做只读查询，否则整轮挂死。
- `provider` —— `quark_test.go`（`isQuarkMedia` / `isTransitShell` / `cleanSnippet`）、
  `mojibake_test.go`（可逆乱码修复）；**其余 provider 改动只能靠实跑验证**，
  改完务必真跑一次搜索。

实现要点中的坑（并发信号量、端口按 scheme 取、quark DOM 两处事实、媒体页三处判据、
markdown 噪声清洗、bing 翻页 cookie）统一记在
[`docs/decisions.md`](docs/decisions.md#容易踩的实现细节)。

---

## 四、已知限制

- **`results` 无排序**：设计选择，不是缺陷。需要排序请调用方自行处理。
- **不做内容抓取**：只做检索元数据 + TCP 可达性预检。握手成功 ≠ HTTP 成功，
  反爬/404 仍会发生，敲门只用于排除死链。
- **`positions` 不重排**：那是各引擎内的原始排名；最终序号即列表下标。
- **quark 依赖移动站模板**：`div.sc` + 带 `.qk-title-text` 的 `a.qk-link-wrapper`
  （地址取 `data-openpageurl`）。模板变了会**显式报错**（"未解析出结果卡片"）而非静默 0 条。
- **quark 的 `snum` 翻页**：首跳 `m.quark.cn/s?q=` 会被 302 到带 session 的 URL，之后在该
  URL 上追加 `snum=2..maxPages` 翻页（**默认上限 3 页**，即 `snum=2,3`；`-max-pages` 可调）、
  跨页去重，凑够 15 条或某页零新增即停。必须带移动 UA，否则 0 条空壳。
- **quark 有反爬限流**：密集请求会返回 200 + 858~890B 惩罚页（整路 0 条）。
  看到"反爬限流"就降频等待 20~30s，**不要去改解析**。详见 [`docs/perf.md`](docs/perf.md#四-quark-反爬限流机制最容易误判成代码-bug-的坑)。
- **anysearch 上限 10**：官方硬限制，永远到不了 15 条。垂直检索可能返回大量**同一个 URL**
  的实体记录，会被聚合层按精确 URL 并成 1 条（合法，非 bug）。
- **exa 的 snippet 偶有编码乱码**：属**上游 API 缺陷**（同一响应里 `title` 正常、`text` 坏），
  本 CLI 只做**无损可逆修复**（能解则解、不能解则原样保留，不猜不删）。详见
  [`docs/decisions.md`](docs/decisions.md#容易踩的实现细节)。
- **不支持以 `-` 开头的关键词**：`--` 之后的 token 一律当位置参数，可用它转义。
- **Release 只提供 Windows x64 二进制**：源码本身没有平台绑定，实测
  `GOOS=linux GOARCH=amd64` 与 `GOOS=darwin GOARCH=arm64` 都能编译通过。
  但这两个平台没有做过实跑验证，所以不放现成二进制。要在别的平台用，请自行编译。

---

## 五、许可证

MIT，全文见 [`LICENSE`](LICENSE)。
"# metasearch_cli" 
