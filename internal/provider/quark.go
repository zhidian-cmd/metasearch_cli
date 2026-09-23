package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/JSLEEKR/scrapling-go/pkg/fetcher"
	"github.com/JSLEEKR/scrapling-go/pkg/parser"
	"github.com/JSLEEKR/scrapling-go/pkg/selector"
	"github.com/zhidian-cmd/metasearch_cli/internal/model"
)

// quarkProvider —— 夸克搜索，走**移动站 + 纯 HTTP**。
//
// ============ 为什么不是浏览器渲染 ============
//
// 参考实现用 scrapling 的 DynamicFetcher（CDP 等 `#scs > *` 出现）。
// 本实现照做过浏览器渲染（headless --dump-dom + 虚拟时间预算），实测三处硬伤：
//
//  1. `--virtual-time-budget` 与 SPA 的交互是**不确定**的：单独跑 8s 预算能出
//     1.05MB 完整结果；与另外 7 个引擎**并发**时，同样的 8s 预算只渲染出 626KB
//     空壳（无结果容器）→ 0 条。
//  2. 加大预算不是解法，反而更糟：页面一直不 idle，虚拟时间耗不尽，进程挂到
//     被外部超时杀掉 —— 实测 20s/30s 预算均返回 **0 字节**。
//  3. 成本高：重试一档后单引擎耗时 14.5~16.5s，却产出 0 条，把整轮拖到 20s+。
//
// 换成移动站后全部消失：**m.quark.cn 的结果页是服务端渲染（SSR）的**，
// 原始 HTML 里就带完整结果卡片，不需要 JS、不需要浏览器、不需要等待。
//
// ============ 桌面站已不可用（2026-09-16 实测）============
//
// 用参考实现的 DynamicFetcher 重新渲染 www.quark.cn，拿到 546KB HTML 但：
// `#scs` 0 个、`qk-title-text` 0 次、`data-log` 0 次、`data-openpageurl` 0 次，
// 正文是"请先登录 / 请使用手机号码登录" —— **桌面站已整体落到登录墙后面**。
// 参考实现那条 `_real_url()`（读 `data-log` 的 ext.nu 还原直链）因此形同虚设。
// 所以本实现只有移动站这一条通道，且它是可用的。
//
// ============ 结果形态与处理策略（2026-09-16 实测）============
//
// 移动站给的是 `a.qk-link-wrapper[data-openpageurl]`，取值分几类：
//
//	external  https://baijiahao.baidu.com/s?id=...   原始目标站点，可直接抓
//	external  https://m.chem17.com/tech_news/...     同上
//	聚合壳    https://baike.quark.cn/baike?id=...    夸克百科（自家**文本**内容，保留）
//	聚合壳    https://vt.quark.cn/.../preview?...     夸克文档预览（保留）
//	聚合壳    https://page.sm.cn/blm/midpage-*/...    神马文本聚合页（保留）
//	媒体壳    https://m.quark.cn/vsearch/picture?...  夸克图片/视频垂直频道（剔除）
//	媒体壳    https://page.sm.cn/blm/video-page-*/... 神马视频/图片页（剔除，⚠️ 属于另一个域）
//	中转壳    https://m.quark.cn/s/<id>              点击中转，302 跳外部（还原直链）
//
// 处理策略（用户 2026-09-16 明确）：
//  1. **翻页**：首跳 form1 会被 302 到带 session 的 URL，之后在 session URL 上追加
//     `snum=2..N` 翻页，跨页去重合并，凑够 limit（默认 15）即停。
//  2. **剔除媒体页**：`m.quark.cn/vsearch` 与 `page.sm.cn/blm/*-page-*`（图片/视频页）
//     都不是单篇网页，从源头丢弃。⚠️ 两个域都要判，只判一个会漏（见 isQuarkMedia 注释）。
//  3. **聚合壳保留**：百科/文档预览/神马 midpage 是文本、极有价值 → 保留，仅清洗 URL 跟踪参数
//     （还原到原生 URL）；外部站点 URL 一个字节都不动（宁放过不杀错）。
//  4. **中转壳直接丢弃**：`m.quark.cn/s/<id>` 背后虽是外部直链，但还原它要多发请求、
//     还会撞反爬，这笔账不划算（用户 2026-09-16 拍板）—— 直接丢弃，条数由其他引擎补。
//
// ============ 「微生物」为什么超过 3 秒（2026-09-16 实测定位）============
//
// quark 单引擎实测 4.0~6.0s，**整轮耗时几乎全由它决定**（其他 7 个引擎都在 0.7~2.2s）。
// 逐页探针（关键词"微生物"）：
//
//	首跳       2.3~2.5s   470~710KB   首页 9~10 张卡片（已含结果，SSR）
//	snum=2     1.5~1.8s   450~504KB   新增 0~2 条   ← 收益已经很小
//	snum=3     1.5s       504KB       新增 0 条     ← 纯亏
//
// 结论：**成本全在"翻页"**。每页 1.5~2.5s 且串行，页数直接线性叠加到整轮耗时。
// 而且本关键词只需 2~3 页即可凑满，`snum=3` 常常零新增（代码里"零新增即 break"
// 已能兜住，但 snum=2 的 1.5~1.8s 是实打实付掉了）。
//
// 因此 `defaultQuarkPages` 从 6 收敛到 3（详见该常量注释）—— 6 页的上限从没跑到过，
// 但留着它意味着一旦某 query 持续有新结果就会一路翻到 6 页 ≈ 7s+，
// 与用户"接近就可以，不用硬凑 15 条"的取向冲突。需要更多条数用 `-max-pages` 调大。
//
// 另注：`resp.URL` 现在**不带 `queryId`**（旧记录里的 session URL 未复现），
// 但 `snum` 在该 URL 上仍然生效（snum=2 确有新卡片），故翻页机制不变。
type quarkProvider struct{}

const (
	quarkEndpoint = "https://m.quark.cn/s"
	// quarkUCParam 移动站要求的 UC 参数串（缺了会被重定向到首页）
	quarkUCParam = "ntnwvepffrbiprsvchutosstxs"
	quarkUA      = "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36"
	// minQuarkHTMLBytes 低于此值基本可判定为错误页/空壳页（正常结果页实测 620~890KB）。
	minQuarkHTMLBytes = 4096
	// quarkPunishMarker 阿里系反爬惩罚页的特征串。
	//
	// ⚠️ 2026-09-16 实测定性：短响应（858~890B）的正文是一句 JS 跳转——
	//	window.location.replace("https://m.quark.cn//s/_____tmd_____/punish?x5secdata=...")
	// 注意 **HTTP 状态码仍是 200**，所以 `resp.OK()` 拦不住，只能靠长度 + 特征串兜底。
	// 触发条件：短时间密集请求（实测连打 5~6 次即中），惩罚持续约 20~30s 后自动恢复。
	// 因此错误信息必须把"被限流"与"页面模板变了"区分开，否则调用方会误改解析代码。
	quarkPunishMarker = "_____tmd_____"
	// defaultQuarkPages 默认最多翻到第几页（含首页）。
	//
	// ⚠️ 这是"条数 vs 耗时"的直接旋钮，2026-09-16 实测（关键词"微生物"）：
	//   - 翻到 2 页（首页+snum2）→ 约 4.0~4.3s，产出 6~8 条
	//   - 翻到 3 页              → 约 5.4~6.0s，产出 11 条
	//   - 翻到 6 页（原值）      → 耗时可到 7s+，而且**多翻的页常常零新增**
	// 每页成本 1.5~2.5s 且串行，所以页数直接线性叠加到整轮耗时上。
	// 用户明确"接近就可以，不用硬凑 15 条"，故默认收敛到 3 页（约 11 条，够用）。
	// 需要更多条数再调大；想更快可设 2。
	defaultQuarkPages = 3
)

func init() { Register(quarkProvider{}) }

func (quarkProvider) Name() string { return "quark" }

// quarkTarget 构造移动站结果页 URL。
func quarkTarget(q string) string {
	u, err := url.Parse(quarkEndpoint)
	if err != nil {
		return quarkEndpoint + "?q=" + url.QueryEscape(q)
	}
	v := url.Values{}
	v.Set("q", q)
	v.Set("uc_param_str", quarkUCParam)
	v.Set("by", "submit")
	v.Set("from", "kkframenew")
	u.RawQuery = v.Encode()
	return u.String()
}

func (quarkProvider) Search(ctx context.Context, opt Options) ([]model.RawItem, error) {
	q := strings.TrimSpace(opt.Query)
	if q == "" {
		return nil, nil
	}
	endpoint := quarkTarget(q)

	headers := http.Header{}
	headers.Set("User-Agent", quarkUA)
	headers.Set("Accept", "text/html,application/xhtml+xml")
	headers.Set("Accept-Language", "zh-CN,zh;q=0.9")
	// 与 bing 同理：显式声明 identity，避免 Transport 在调用方已设
	// Accept-Encoding 时不自动解压导致乱码（bing 那边有同类注释，含兜底被删的原因）。
	headers.Set("Accept-Encoding", "identity")

	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	opts := []fetcher.Option{
		fetcher.WithTimeout(timeout),
		fetcher.WithMaxRetries(0),
		fetcher.WithHeaders(headers),
	}
	if px := proxyFor(endpoint, opt); px != "" {
		opts = append(opts, fetcher.WithProxy(px))
	}
	f, err := fetcher.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("Quark: 初始化 fetcher 失败: %w", err)
	}
	defer f.Close()

	// 1) 首跳：form1 通常 302 重定向到带 session 的 URL，resp.URL 即翻页基准。
	resp, err := f.Get(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("Quark: 抓取失败: %w", err)
	}
	if !resp.OK() {
		return nil, &HTTPError{
			Provider: "Quark", Status: resp.StatusCode,
			Reason: http.StatusText(resp.StatusCode), Body: bodySnippet(resp.Body),
		}
	}
	firstHTML := string(resp.Body)
	if len(firstHTML) < minQuarkHTMLBytes {
		return nil, quarkShortResponseErr("首跳", firstHTML)
	}
	base := resp.URL // 翻页基准（session URL）

	limit := trimLimit(opt.Limit, 50)
	seen := map[string]bool{}
	var collected []quarkItem

	// 把一页解析结果并入 collected：跳过空标题/空链接、剔除媒体垂直、跨页去重。
	addPage := func(html string) {
		for _, it := range parseQuarkMobile(html) {
			if it.url == "" || strings.TrimSpace(it.title) == "" {
				continue
			}
			// 媒体/无正文卡剔除 = **URL 形状规则**。
			//
			// ⚠️ 卡片自带的声明式标记（`data-sc.sc` / class 的 `sc_*`）曾作为**兜底并集**参与判定，
			// 2026-09-19 已删除：原始 HTML 取证（4 组 query、37 张卡）证明它与 URL 规则
			// **完全重叠、零增量** —— `nature_result` 既有正常网页也有媒体页（无区分力），
			// `ss_pic` 是卡片模板而实测全指向正常图片素材站（剔了就是误杀）。
			// 若 quark 改了 URL 形状导致漏网，按那时的原始 HTML 重建，别凭直觉复原。
			if isQuarkMedia(it.url) {
				continue
			}
			if isTransitShell(it.url) { // 中转壳直接丢弃：还原要多发请求且易撞反爬，不划算
				continue
			}
			key := normalizeForDedup(it.url)
			if seen[key] {
				continue
			}
			seen[key] = true
			collected = append(collected, it)
		}
	}
	addPage(firstHTML)

	// 2) 翻页：snum=2..maxPages，够 limit 或某页零新增即停。
	//
	// ⚠️ 这里是整轮耗时的主要来源：翻页串行，每页 1.5~2.5s（实测）。
	// 凑到 15 条往往要 3 页 ≈ 5.4s；凑到 8 条通常 2 页 ≈ 4.0s。
	// 可用 -max-pages 调小换取速度（条数同比减少）。
	maxQuarkPages := defaultQuarkPages
	if opt.MaxPages > 0 {
		maxQuarkPages = opt.MaxPages
	}
	for snum := 2; snum <= maxQuarkPages; snum++ {
		if len(collected) >= limit {
			break
		}
		pr, err := f.Get(ctx, withSnum(base, snum))
		if err != nil || !pr.OK() {
			break
		}
		ph := string(pr.Body)
		if len(ph) < minQuarkHTMLBytes { // 多半是中途撞上限流惩罚页，保留已抓结果
			break
		}
		before := len(collected)
		addPage(ph)
		if len(collected) == before { // 该页零新增 → snum 失效，停止翻页
			break
		}
	}

	if len(collected) == 0 {
		return nil, fmt.Errorf("Quark: 页面未解析出结果卡片（模板可能已变或全为媒体结果）")
	}

	// 3) 聚合壳清洗跟踪参数（还原原生 URL）。中转壳与媒体垂直已在上游丢弃。
	for i := range collected {
		// cleanQuarkShellURL 内部已按 host 守卫：quark 自家域剥跟踪参数，外部站点原样。
		collected[i].url = cleanQuarkShellURL(collected[i].url)
	}

	out := make([]model.RawItem, 0, limit)
	for _, it := range collected {
		if len(out) >= limit {
			break
		}
		out = append(out, model.RawItem{
			Engine:  "quark",
			Rank:    len(out) + 1,
			Title:   normalizeSpace(it.title),
			URL:     it.url,
			Snippet: cleanSnippet(it.snippet),
			// card_type 是 quark 卡片自带的声明式类型（ss_text / ss_pic / doc_jgh …），
			// 交给下游做精细筛选 —— 本包只带出、不消费（不做剔除、不做内容质量判断）。
			Signals: signalOf(model.Signals{CardType: it.cardType}),
		})
	}
	return out, nil
}

// ============ 壳页 / 媒体判定与处理 ============

// quarkShortResponseErr 把"响应过短"翻译成可操作的错误信息。
//
// 关键是把两种完全不同的原因区分开（2026-09-16 实测定性）：
//   - 命中反爬惩罚页（正文含 `_____tmd_____/punish`）→ **限流**，解析代码没问题，不要改解析。
//     恢复时间**不固定**：轻度触发（连打 5~6 次）约 20~30s；高频猛打会显著拉长（实测 >8 分钟）；
//   - 不含惩罚特征 → 才可能是模板变更 / 真错误页，需要查解析。
func quarkShortResponseErr(stage, html string) error {
	if strings.Contains(html, quarkPunishMarker) {
		return fmt.Errorf("Quark: %s被反爬限流（%dB 惩罚页，非解析问题；密集请求触发，恢复时间不定，请降低请求频率后重试）", stage, len(html))
	}
	return fmt.Errorf("Quark: %s响应过短（%dB），疑似错误页/空壳页或页面模板已变", stage, len(html))
}

// quark 自家域（聚合壳或中转壳都算）。外部站点一律按"宁放过不杀错"不动。
// ⚠️ 用带前导点的后缀匹配，避免 `zimgs.cn` 被 `.sm.cn` 误吃；裸域另行精确匹配。
var quarkShellSuffixes = []string{".quark.cn", ".sm.cn", ".uc.cn", ".ucweb.com"}

var quarkShellHosts = map[string]bool{"quark.cn": true, "sm.cn": true, "uc.cn": true}

// isQuarkHost 是否为 quark/神马自家的域。
func isQuarkHost(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return true // 解析失败按壳处理，避免坏链接当外部放出
	}
	h := strings.ToLower(u.Hostname())
	if h == "" {
		return true
	}
	if quarkShellHosts[h] {
		return true
	}
	for _, suffix := range quarkShellSuffixes {
		if strings.HasSuffix(h, suffix) {
			return true
		}
	}
	return false
}

// isTransitShell 是否为中转壳（m.quark.cn/s/<id> 点击中转）。
// 处理策略：直接丢弃 —— 背后虽是外部直链，但还原要多发一次请求且易撞反爬，
// 这笔账不划算（用户 2026-09-16 拍板），缺的条数由其他引擎补。
func isTransitShell(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), "m.quark.cn") && strings.HasPrefix(u.Path, "/s/")
}

// isQuarkMedia 是否为媒体页 / 无正文卡片页（图片视频频道、视频图片页、字词卡片），
// 不是单篇网页，从源头剔除。
//
// ⚠️ 判定范围必须覆盖**三个位置**（2026-09-16 两轮修正）：
//   - `m.quark.cn`：夸克自家媒体垂直（`/vsearch/picture`、`qtab=picture|video|image`）
//   - `page.sm.cn`（神马，夸克兄弟域）：`/blm/video-page-*`、`/blm/image-page-*` 等媒体页
//   - `p.quark.cn`：**字词/实体卡片页**（`/xxx/char?entity=`、`/xxx/word?entity=`）。
//     这类是纯前端组件页（带 `force_uc_biz_str`、`content_id`），没有可抓的正文，
//     实测"风景/猫"等词会返回它，混进结果里就是一条空壳。
//
// 早期版本第一道就 `host != m.quark.cn → return false`，导致
// `page.sm.cn/blm/video-page-710/video?h=www.bilibili.com` 这类视频页整条漏网放出
// （isTransitShell 也要求 m.quark.cn，两道防线都没拦）。
//
// 策略是**按类型剔除、未知类型保留**（不是白名单）：
// page.sm.cn/blm/ 下是混合域，文本聚合页（`midpage-*`）必须留，
// 只剔路径里明确带媒体页标识的、或 `h=` 参数指向视频站的；
// p.quark.cn 同理 —— 只剔 `char`/`word` 卡片，其余（如 `baike.quark.cn/baike`）是
// 真百科内容，必须保留。
func isQuarkMedia(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	p := strings.ToLower(u.Path)

	// 1) 夸克自家媒体垂直
	if h == "m.quark.cn" {
		if strings.HasPrefix(p, "/vsearch") {
			return true
		}
		if t := strings.ToLower(u.Query().Get("qtab")); t == "picture" || t == "video" || t == "image" {
			return true
		}
		return false
	}

	// 2) 神马（page.sm.cn）下的媒体页：路径含媒体页标识即剔
	if h == "page.sm.cn" {
		for _, kw := range quarkMediaPathKeywords {
			if strings.Contains(p, kw) {
				return true
			}
		}
		// 兜底：h= 参数指向视频站/视频源（视频页常常路径不带标识，只能靠这个）
		hv := strings.ToLower(u.Query().Get("h"))
		if hv != "" {
			for _, vs := range quarkVideoHosts {
				if strings.Contains(hv, vs) {
					return true
				}
			}
		}
		return false
	}

	// 3) p.quark.cn 字词/实体卡片页：无正文的纯组件页。
	//    路径形如 /<hash>/char、/<hash>/word；也兼容直接 /char、/word。
	if h == "p.quark.cn" {
		segs := strings.Split(strings.Trim(p, "/"), "/")
		last := ""
		if len(segs) > 0 {
			last = segs[len(segs)-1]
		}
		if last == "char" || last == "word" {
			return true
		}
		// 兜底：带 entity= 的卡片查询（/xxx/char?entity=猫）
		if u.Query().Get("entity") != "" {
			return true
		}
		return false
	}

	return false
}

// quarkMediaPathKeywords page.sm.cn 上媒体页的路径标识（小写比较）。
// 只列"明确是媒体页"的标识——未知类型保留，避免误杀文本聚合页。
var quarkMediaPathKeywords = []string{
	"video-page", "image-page", "picture-page", "audio-page",
	"video_", "pic-page", "gallery-page",
}

// quarkVideoHosts 视频站/视频源域名片段（用于识别 h= 参数指向视频页的情况）。
// 含 `user_auth_video` —— 夸克自家的用户视频源（实测出现过
// `h=v1.user_auth_video.quark.cn`）。
var quarkVideoHosts = []string{
	"bilibili.com", "douyin.com", "v.qq.com", "youku.com", "iqiyi.com",
	"mgtv.com", "ixigua.com", "kuaishou.com", "xigua.com", "sohu.com/v",
	"user_auth_video", "video.quark.cn",
}

// quarkTrackParams 壳页 URL 上的跟踪/实验参数——与内容无关，剥掉不影响可达性与正文。
//
// ⚠️ `sid` 是**翻页会话 id**，同一张卡片在 p1/p2/p3 里**只差它**（2026-09-19 实测：
// 「跨境电商 政策」的 `sc_doc_sc_new` 精选资料卡三页各出现一次，URL 仅 sid 不同）。
// 不剥的后果有两个，都不是小事：
//  1. 同一批结果在输出里**重复 2~3 次**（条目级 seen 去重失效）；
//  2. **「某页零新增就停翻页」失效** —— 每页都看着像有新东西，白付 2 页请求（约 3~4s）。
//
// 剥它安全：带/不带 sid 的 `vt.quark.cn/.../preview` 都返回 200（实测两条 URL 各拉一次），
// 文档由 `id=` 定位，sid 只是搜索会话。
var quarkTrackParams = map[string]bool{
	"uc_param_str":    true,
	"uc_biz_str":      true,
	"docexpbucketstr": true,
	"zuowentagstatus": true,
	"fp_from":         true,
	"entry":           true,
	"x_render_type":   true,
	"sc_doc_level":    true,
	"perf_format":     true,
	"perf_exttype":    true,
	"perf_doc_cls_v5": true,
	"bucket":          true,
	"from":            true,
	"sid":             true,
}

// cleanQuarkShellURL 只剥 quark 自家域 URL 上的跟踪参数，**不重新 Encode**
// （保住其余参数原始顺序与编码，个别服务端对顺序敏感）。外部站点 URL 原样返回。
func cleanQuarkShellURL(raw string) string {
	if !isQuarkHost(raw) {
		return raw // 外部站点：宁放过不杀错，一个字节都不动
	}
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return raw
	}
	parts := strings.Split(u.RawQuery, "&")
	kept := parts[:0]
	for _, p := range parts {
		if p == "" {
			continue
		}
		name := p
		if i := strings.IndexByte(p, '='); i >= 0 {
			name = p[:i]
		}
		if quarkTrackParams[strings.ToLower(name)] {
			continue
		}
		kept = append(kept, p)
	}
	u.RawQuery = strings.Join(kept, "&")
	return u.String()
}

// normalizeForDedup 去重键：quark 自家 URL 先剥跟踪参数再比，避免同一条 baike
// 因跟踪参数不同被判成两条。外部站点原样。
func normalizeForDedup(raw string) string {
	if isQuarkHost(raw) {
		return cleanQuarkShellURL(raw)
	}
	return raw
}

// resolveTransitShells / followRedirect 已删除（2026-09-16）：中转壳改为源头直接丢弃，
// 不再为还原直链多发请求 —— 见 isTransitShell 注释。

// withSnum 在基准 URL 上设置翻页参数 snum（先清掉旧的，避免重复）。
func withSnum(base string, snum int) string {
	u, err := url.Parse(base)
	if err != nil {
		sep := "&"
		if !strings.Contains(base, "?") {
			sep = "?"
		}
		return base + sep + "snum=" + strconv.Itoa(snum)
	}
	q := u.Query()
	q.Del("snum")
	q.Set("snum", strconv.Itoa(snum))
	u.RawQuery = q.Encode()
	return u.String()
}

// quarkItem 一条 quark 原始结果。
//
// cardType 是卡片自带的**声明式语义类型**（`div.sc` 上 `data-sc` JSON 的 `sc` 字段，
// 实测取值 ss_text / ss_pic / general_entity_* / nature_result / news_uchq / doc_sc_* …）。
//
// ⚠️ 它**只作为 Signals.CardType 带出**给下游，不参与任何剔除判定 ——
// 这个标记与"是否媒体页"没有区分力，故不能拿它当判据（见 isQuarkMedia 处的注释）。
type quarkItem struct {
	title, url, snippet string
	cardType            string
}

// quarkCardMarks 从卡片节点上取 `data-sc` 的 sc token。
//
// 标记来源已由 2026-09-17 原始 HTML 取证确认（4 组 query），**不需要猜**：
// `data-sc` 直接写在 `div.sc` 上、是 JSON 串。取不到时返回空串 ——
// 调用方按"未知类型"处理（带出的 Signals.CardType 为空，整个 signals 对象省略）。
func quarkCardMarks(card *parser.Adaptable) string {
	raw := strings.TrimSpace(card.Attr("data-sc"))
	if raw == "" {
		return ""
	}
	var m struct {
		SC string `json:"sc"`
	}
	if json.Unmarshal([]byte(raw), &m) != nil {
		return ""
	}
	return strings.TrimSpace(m.SC)
}

// parseQuarkMobile 解析移动站结果页（DOM 顺序即引擎内排名），**原样返回所有卡片**
// （外部 + 壳页 + 媒体都返回），去重 / 媒体剔除 / 壳页清洗由 Search 统一处理。
//
// 卡片 = `div.sc`；卡片内每个**带标题**的 `a.qk-link-wrapper` 是一条结果：
//   - 标题 `.qk-title-text`（含 <em> 高亮，AllText 无分隔符拼接避免多余空格）
//   - 链接 `data-openpageurl`（移动站直接给地址），兜底用 href
//   - 摘要见 quarkSnippet —— **不在同一个 <a> 里**，这是最容易取错的一处
//
// 同一条结果在 DOM 里常以多个并列 `<a>` 出现（标题 a 与摘要 a 平行），且 `div.sc`
// 可能嵌套，所以这里不做去重，交给 Search 按清洗后的 URL 跨页去重。
//
// 类型标记按**卡片**取、再挂到该卡片内的每条结果上（一条卡片可能装多条结果）。
func parseQuarkMobile(doc string) []quarkItem {
	root, err := parser.Parse(doc)
	if err != nil {
		return nil
	}
	cards, err := selector.CSS(root, "div.sc")
	if err != nil {
		return nil
	}
	var out []quarkItem
	for _, card := range cards {
		anchors, err := selector.CSS(card, "a.qk-link-wrapper")
		if err != nil {
			continue
		}
		cardType := quarkCardMarks(card)
		for _, a := range anchors {
			titleEl, err := selector.CSSFirst(a, ".qk-title-text")
			if err != nil || titleEl == nil {
				continue
			}
			title := titleEl.AllText()
			link := quarkRealURL(a)
			if link == "" {
				continue
			}
			out = append(out, quarkItem{
				title: title, url: link, snippet: quarkSnippet(a),
				cardType: cardType,
			})
		}
	}
	return out
}

// quarkRealURL 取结果的地址：优先 data-openpageurl，其次 http(s) 形式的 href。
func quarkRealURL(a *parser.Adaptable) string {
	v := strings.TrimSpace(a.Attr("data-openpageurl"))
	if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		return v
	}
	// href 常见为 "javascript:;"（点击由前端接管），此时 data-openpageurl 才是真地址
	href := strings.TrimSpace(a.Attr("href"))
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return href
	}
	return ""
}

// quarkSnippet 摘要：按优先级分三步取。
//
// 移动版实测的卡片形态是**标题和摘要在两个并列的 `<a.qk-link-wrapper>` 里**：
//
//	<div class="qk-card">
//	  <a class="qk-link-wrapper">   ← 带 .qk-title-text
//	     ...<div class="qk-paragraph-text">化工仪器网</div>   ← 短标签，不是摘要
//	  </a>
//	  <a class="qk-link-wrapper">   ← 没有标题
//	     ...<div class="qk-paragraph-text">食品微生物检验的核心目的围绕…  ← 真正的摘要
//	  </a>
//	</div>
//
// 只在标题所在的 `<a>` 内部找会取错（拿到短标签）或取空 —— 早期实现实测
// 11 条里 7 条摘要为空。故：
//
//  1. **先往后找兄弟节点**：摘要就在标题 `<a>` 的下一个兄弟里。这条同时天然
//     防串 —— 遇到下一个含标题的兄弟（即下一条结果）就停，所以"一张卡片装两条
//     结果"时各取各的。
//  2. 再**向上找**"祖先内只出现一个标题"的容器（应对模板把摘要挪进外层的情况）。
//  3. 兜底才用标题 `<a>` 自身内的段落。实测那里通常是"化工仪器网"这类短标签，
//     所以放在最后 —— 有总比空着强，但绝不优先。
func quarkSnippet(anchor *parser.Adaptable) string {
	// 1) 标题 <a> 之后的兄弟
	for sib := anchor.NextSibling(); sib != nil; sib = sib.NextSibling() {
		if titles, err := selector.CSS(sib, ".qk-title-text"); err == nil && len(titles) > 0 {
			break // 撞到下一条结果的标题，说明本条没有摘要
		}
		if texts, err := selector.CSS(sib, ".qk-paragraph-text"); err == nil && len(texts) > 0 {
			if s := longestText(texts); s != "" {
				return s
			}
		}
	}
	// 2) 向上找只含本条结果的容器
	for node, depth := anchor.Parent(), 0; node != nil && depth < 8; node, depth = node.Parent(), depth+1 {
		if titles, err := selector.CSS(node, ".qk-title-text"); err == nil && len(titles) > 1 {
			break // 该祖先已覆盖多条结果，再往上只会更串
		}
		if texts, err := selector.CSS(node, ".qk-paragraph-text"); err == nil && len(texts) > 0 {
			if s := longestText(texts); s != "" {
				return s
			}
		}
	}
	// 3) 兜底：标题 <a> 自身
	if texts, err := selector.CSS(anchor, ".qk-paragraph-text"); err == nil {
		return longestText(texts)
	}
	return ""
}

// longestText 取一组节点里文本最长的一段（参考实现对摘要也是取 max by len）。
func longestText(nodes []*parser.Adaptable) string {
	best := ""
	for _, n := range nodes {
		if s := n.AllText(); len(s) > len(best) {
			best = s
		}
	}
	return best
}
