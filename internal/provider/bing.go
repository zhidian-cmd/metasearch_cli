package provider

import (
	"context"
	"encoding/base64"
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

// bingProvider 移植自参考实现 providers/bing.py。
//
// 选型理由（保留原注释）：DuckDuckGo 国内无法直连、依赖本地代理出海，
// 改用 Bing —— 国内访问 www.bing.com 会 302 → cn.bing.com，可直连抓取。
//
// 抓取通道：scrapling-go 的 `pkg/fetcher`（浏览器风格请求头 + 重试 + cookie jar），
// 解析通道：scrapling-go 的 `pkg/selector`（cascadia CSS 选择器）。
// 解析规则与参考实现**完全一致**，只是把 BeautifulSoup 换成 scrapling 的选择器：
// li.b_algo 容器 → h2 a[href] 标题/链接 → .b_caption p 摘要。
//
// ⚠️ 翻页：实测不带 Cookie 时 first 翻页参数被忽略（p1 与 p2 结果 100% 重叠），
// 带 `Cookie: _EDGE_V=1` 后 first 翻页生效（p1∩p2=0%）。移动 UA 反而返回 0 条，
// 故坚持桌面 UA + 该 cookie，循环 first=11/21/... 翻页聚合到 limit 条。
type bingProvider struct{}

const (
	bingEndpoint = "https://www.bing.com/search"
	bingUA       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
	// bing 每页固定约 10 条；翻页用 first=(page-1)*bingPerPage+1。
	bingPerPage = 10
)

func init() { Register(bingProvider{}) }

func (bingProvider) Name() string { return "bing" }

func (bingProvider) Search(ctx context.Context, opt Options) ([]model.RawItem, error) {
	q := strings.TrimSpace(opt.Query)
	if q == "" {
		return nil, nil
	}

	headers := http.Header{}
	headers.Set("User-Agent", bingUA)
	headers.Set("Accept", "text/html,application/xhtml+xml")
	headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	// ⚠️ 必须显式声明 identity：scrapling-go 的 fetcher 默认带 gzip/deflate，
	// 但 Go 的 Transport 在"调用方自己设了 Accept-Encoding"时**不会**自动解压，
	// 压缩体回来就是乱码。
	// （曾另有一个 maybeGunzip 魔数兜底，2026-09-23 删除：它防的是"上游无视 identity
	//   仍压缩"，而这种情形**从未被观测到**，属恒不触发的分支。将来若真出现整页乱码
	//   正文，先按原始响应取证再重建兜底，别凭直觉加回来。）
	headers.Set("Accept-Encoding", "identity")
	// ⚠️ 翻页关键：不带这个 cookie 时 first 翻页参数被忽略（p1∩p2=100%），
	// 带上后 first 翻页生效（p1∩p2=0%）。移动 UA 反而返回 0 条，故坚持桌面 UA。
	headers.Set("Cookie", "_EDGE_V=1")

	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	limit := trimLimit(opt.Limit, 50)
	// 翻页页数：每页 bingPerPage 条，向上取整；limit 上限 50 对应最多 5 页，兜底封顶。
	maxPages := (limit + bingPerPage - 1) / bingPerPage
	if maxPages < 1 {
		maxPages = 1
	}
	if maxPages > 5 {
		maxPages = 5
	}

	opts := []fetcher.Option{
		fetcher.WithTimeout(timeout),
		fetcher.WithMaxRetries(0), // 与参考实现一致：单发不重试
		fetcher.WithHeaders(headers),
	}
	if px := proxyFor(bingEndpoint, opt); px != "" {
		opts = append(opts, fetcher.WithProxy(px))
	}
	f, err := fetcher.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("Bing: 初始化 fetcher 失败: %w", err)
	}
	defer f.Close()

	var out []model.RawItem
	seen := map[string]bool{}
	for page := 1; page <= maxPages; page++ {
		if len(out) >= limit {
			break
		}
		resp, err := f.Get(ctx, bingPageURL(q, page))
		if err != nil {
			// 翻页中途失败：第 1 页必须成功，后续页失败则放弃翻页（已抓结果仍返回）
			if page == 1 {
				return nil, fmt.Errorf("Bing: 抓取失败: %w", err)
			}
			break
		}
		if !resp.OK() {
			if page == 1 {
				return nil, &HTTPError{
					Provider: "Bing", Status: resp.StatusCode,
					Reason: http.StatusText(resp.StatusCode), Body: bodySnippet(resp.Body),
				}
			}
			break
		}
		for _, it := range parseBingHTML(string(resp.Body)) {
			u := unwrapBingURL(it.url)
			if u == "" || seen[u] {
				continue
			}
			seen[u] = true
			out = append(out, model.RawItem{
				Engine:  "bing",
				Rank:    len(out) + 1,
				Title:   strings.TrimSpace(it.title),
				URL:     u,
				Snippet: cleanSnippet(it.snippet),
			})
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// bingPageURL 构造分页 URL：第 1 页只带 q；后续页用 first=(page-1)*bingPerPage+1 翻页，
// 并带 form=QBLH（Bing 自身翻页链接的同款参数）。实测 first 翻页需配合 Cookie: _EDGE_V=1 才生效。
func bingPageURL(q string, page int) string {
	base := bingEndpoint + "?q=" + url.QueryEscape(q)
	if page <= 1 {
		return base
	}
	return base + "&first=" + strconv.Itoa((page-1)*bingPerPage+1) + "&form=QBLH"
}

type bingItem struct{ title, url, snippet string }

// parseBingHTML 用 scrapling-go 的选择器解析 Bing 结果页，
// 规则与参考实现 providers/bing.py 的 _parse_page 一一对应。
func parseBingHTML(doc string) []bingItem {
	root, err := parser.Parse(doc)
	if err != nil {
		return nil
	}
	blocks, err := selector.CSS(root, "li.b_algo")
	if err != nil {
		return nil
	}
	var out []bingItem
	for _, block := range blocks {
		a, err := selector.CSSFirst(block, "h2 a[href]")
		if err != nil || a == nil {
			continue
		}
		href := strings.TrimSpace(a.Attr("href"))
		if !strings.HasPrefix(href, "http://") && !strings.HasPrefix(href, "https://") {
			continue
		}
		// 摘要优先级与参考实现一致：.b_caption p → .b_snippet → 任意 p
		snippet := selectorText(block, ".b_caption p")
		if snippet == "" {
			snippet = selectorText(block, ".b_snippet")
		}
		if snippet == "" {
			snippet = selectorText(block, "p")
		}
		out = append(out, bingItem{
			title:   a.AllText(),
			url:     href,
			snippet: snippet,
		})
	}
	return out
}

// selectorText 取首个匹配节点的全部文本（递归拼接），无匹配返回空串。
func selectorText(root *parser.Adaptable, sel string) string {
	el, err := selector.CSSFirst(root, sel)
	if err != nil || el == nil {
		return ""
	}
	return el.AllText()
}

// unwrapBingURL 把 Bing 的 /ck/a 跳转壳还原为真实目标 URL（u=a1<base64(url)>）。
// www/cn.bing.com 大部分结果直接给真实直链，仅少量（多为推广/特殊条目）走包装。
// 还原失败时原样返回。
func unwrapBingURL(raw string) string {
	if raw == "" || !strings.Contains(raw, "/ck/a") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	payload := u.Query().Get("u")
	if payload == "" {
		return raw
	}
	if strings.HasPrefix(payload, "a1") {
		payload = payload[2:]
	}
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	for _, enc := range []*base64.Encoding{base64.URLEncoding, base64.StdEncoding} {
		if b, err := enc.DecodeString(payload); err == nil {
			s := string(b)
			if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
				return s
			}
		}
	}
	return raw
}

func normalizeSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
