package provider

import (
	"context"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/zhidian-cmd/metasearch_cli/internal/model"
)

// serpapiProvider 移植自参考实现 providers/serpapi.py
// —— Google 结果的 JSON 化通道（取代原 serper.dev）。
//
// gl/hl 按 query 语言自动切换：中文 query 走 cn/zh-cn，其余走 us/en，
// 避免英文 query 拿回一堆中文结果。
//
// 端点：GET https://serpapi.com/search（api_key 走查询串）。
// 选型理由：无 HTML 反爬，不受验证码/JS 渲染影响，是 Google 结果的稳定通道。
//
// ⚠️ 关于单次条数：**`num` 是被忽略的**，首页永远只有约 10 条。
// 取证（2026-09-17，原始响应体落盘 + 对照实验，见 internal/provider/rawdump_probe_test.go）：
//
//	· 请求 `num=15` 与 `num=100`，响应 `search_parameters` 回显里**都没有 num**
//	  （只回显 engine/q/google_domain/hl/gl/device）；
//	· 两者 `organic_results` 都只有 **10** 条（另一 query 为 8 条）；
//	· 响应自带 `serpapi_pagination.next_link`（`...&start=10`），说明下一页必须靠 `start`。
//
// ⇒ 所以**要凑到 15 条就必须翻页**，`start>0` 是**默认路径下会走到的代码**，
// 不是死配置。默认 `-limit=15` 下 serpapi 通常发 **2 次**请求（start=0 拿 10 条 → start=10 补足）。
//
// （2026-09-17 上午此处曾写"num 生效、首页即回满 15 条、后两页从未被请求"，**该结论是错的**，
// 已按上述取证撤回。当初的误判来自"maxPages 设 1/2/3 均返回 15 条"——
// 但 `-max-pages` **只对 quark 生效**（bing 按 limit 自算、serpapi 读自己的常量），
// 三次其实是同一条路径，那 15 条正是**内部翻页凑出来的**，不能用来证明 num 生效。）
//
// 步长自适应：第 1 页条数 >10 才按实际条数推进偏移（实测首页恰为 10，故该分支通常不触发）。
// 停止条件**不能**用"某页条数 < 步长即末页"（首页可不满页而后续页仍有结果），
// 只在**空页**、凑够条数或达上限时停。
//
// ⚠️ 时延波动极大（实测单次 1.8s ~ 32.5s，同一 query 同一 key），
// 故超时默认放宽到 30s（config.EngineTimeoutFloor）。
type serpapiProvider struct{}

const (
	serpapiEndpoint = "https://serpapi.com/search"

	// ⚠️ 读这段前先记住一句话：`num` 被忽略，首页只有约 10 条，**默认就会翻一次页**。
	// 所以下面这些**不是**死代码，默认路径（-limit=15）就会走到：
	//   - `start>0` 分支（第 2 次请求带 start=10）
	//   - `page>0` 的 `break`（第 2 页失败则保留已抓的 10 条）
	// 而 `step` 更新与 `page*step` 里的"按实际条数推进"通常不触发（首页恰为 10，不满足 >10）。
	// 别再把它们当兜底余量 —— 见上方 2026-09-17 取证。

	// serpapiMaxPages 内部翻页上限（含首页）。
	//
	// 默认 `-limit=15` 下实际用 2 页：首页 10 条 + start=10 补足到 15。
	serpapiMaxPages = 3
)

// cjkRe CJK 统一表意文字 + 日文假名 + 韩文音节：命中即按中文区检索
var cjkRe = regexp.MustCompile(`[\x{4e00}-\x{9fff}\x{3040}-\x{30ff}\x{ac00}-\x{d7af}]`)

func init() { Register(serpapiProvider{}) }

func (serpapiProvider) Name() string { return "serpapi" }

func serpapiLocale(q string) (gl, hl string) {
	if cjkRe.MatchString(q) {
		return "cn", "zh-cn"
	}
	return "us", "en"
}

// serpapiResult 一条 organic result（只取用得到的字段）。
type serpapiResult struct {
	Title   string
	Link    string
	Snippet string
	Date    string
	Source  string
}

// serpapiFetchPage 取一页。**单次请求、失败即返回**：重试退避曾有一套
// attempts/delay 装置，但 attempts 恒为 1、退避分支恒不进入，2026-09-19 已整体删除。
func serpapiFetchPage(ctx context.Context, opt Options, q string, start, num int, gl, hl string) ([]serpapiResult, error) {
	params := url.Values{
		"engine":  {"google"},
		"q":       {q},
		"num":     {strconv.Itoa(num)},
		"gl":      {gl},
		"hl":      {hl},
		"api_key": {opt.APIKey},
	}
	if start > 0 {
		params.Set("start", strconv.Itoa(start))
	}
	var body struct {
		OrganicResults []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
			Date    string `json:"date"`
			// source 是发布方名称（实测取值如"商务部财务司"），不是结果类型，
			// 但对下游过滤很有用（可区分官网/百科 vs 导购页），故一并带出。
			Source string `json:"source"`
		} `json:"organic_results"`
	}
	if err := doJSON(ctx, opt, "SerpApi", "GET", serpapiEndpoint, params, nil, nil, &body); err != nil {
		return nil, err
	}
	out := make([]serpapiResult, 0, len(body.OrganicResults))
	for _, it := range body.OrganicResults {
		// 用 link（真实 URL），不用 redirect_link（那是 Google 跳转链接）
		if it.Link == "" {
			continue
		}
		out = append(out, serpapiResult{
			Title: it.Title, Link: it.Link,
			Snippet: it.Snippet, Date: it.Date, Source: it.Source,
		})
	}
	return out, nil
}

func (serpapiProvider) Search(ctx context.Context, opt Options) ([]model.RawItem, error) {
	q := strings.TrimSpace(opt.Query)
	if q == "" {
		return nil, nil
	}
	if opt.APIKey == "" {
		return nil, errMissingKey("SerpApi", "SERPAPI_API_KEY")
	}
	limit := trimLimit(opt.Limit, 100)
	gl, hl := serpapiLocale(q)

	// step 偏移步长，默认 10（Google 首页的固定条数）。
	// 仅当首页返回 **>10** 条时才按实际条数改写 —— 实测首页恰为 10，故通常保持 10。
	step := 10

	var out []model.RawItem
	seen := map[string]bool{}
	// ⚠️ 默认（-limit=15）下本循环**通常迭代 2 次**：首页拿 ~10 条不足 15，
	// 于是带 `start=10` 再取一页补足。故下列分支**默认路径都会走到**：
	//   - `page*step` 乘法（page=1 时得 start=10）→ serpapiFetchPage 的 `start>0` 分支
	//   - `page>0` 的 `break` —— 即**存在"翻页中途失败但保留已抓结果"的能力**：
	//     第 2 页失败时不会把已拿到的 10 条全丢掉，只是退出循环。
	//     真正让整引擎归零的只有**首页失败**（下面的 `page == 0` return）。
	//   - `len(raw)==0` 的 break：第 2 页空（结果已尽）时会走到。
	// 唯一"通常不触发"的是 `step` 改写（需首页 >10 条）。
	for page := 0; page < serpapiMaxPages; page++ {
		raw, err := serpapiFetchPage(ctx, opt, q, page*step, limit, gl, hl)
		if err != nil {
			if page == 0 {
				return nil, err // 首页失败 = 整引擎判死，由聚合层降级
			}
			break // 第 2 页起失败：保留已抓结果退出
		}
		if len(raw) == 0 {
			break // 本页无结果 = 结果已尽
		}
		if page == 0 && len(raw) > 10 {
			step = len(raw) // 首页超 10 条才按实际条数推进偏移（实测通常不触发）
		}
		for _, it := range raw {
			if it.Link == "" || seen[it.Link] {
				continue
			}
			seen[it.Link] = true
			out = append(out, model.RawItem{
				Engine:  "serpapi",
				Rank:    len(out) + 1,
				Title:   strings.TrimSpace(it.Title),
				URL:     it.Link,
				Snippet: cleanSnippet(it.Snippet),
				Date:    strings.TrimSpace(it.Date),
				Signals: signalOf(model.Signals{Source: strings.TrimSpace(it.Source)}),
			})
			if len(out) >= limit {
				break
			}
		}
		// 只有两种情况停：凑够条数，或本页空（结果已尽）
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}
