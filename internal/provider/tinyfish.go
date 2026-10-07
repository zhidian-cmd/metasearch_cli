package provider

import (
	"context"
	"net/url"
	"strings"

	"github.com/zhidian-cmd/metasearch_cli/internal/model"
)

// tinyfishProvider TinyFish Search API（https://docs.tinyfish.ai/search-api）。
//
// 2026-10-07 实测（V10.6-A 接入依据，6 请求全 200）：
//   - GET 端点 + X-API-Key 头，免费额度 12,000 次/天，延迟 2.3~7.1s；
//   - 中文直发质量优秀（食品包装/高血压两题 20 条几乎全切题，权威密度高：
//     cas.cn / WHO / UpToDate 中文 / 新华网）→ 不需要翻译桥、不需要语言参数；
//   - date 原样透传：web 类型多为绝对式（"Jun 4, 2025"），news 类型为相对式
//     （"55 months ago"）——解析归一交给 deep_search 的 helpers.normalize_date
//     （两种都认），本层不做日期解析；
//   - 响应 page 字段的翻页语义文档未说明，首版单页（每引擎 10 条）；
//   - 端点声明为 var 仅供测试注入（httptest），生产勿改。
type tinyfishProvider struct{}

var tinyfishEndpoint = "https://api.search.tinyfish.ai"

func init() { Register(tinyfishProvider{}) }

func (tinyfishProvider) Name() string { return "tinyfish" }

func (tinyfishProvider) Search(ctx context.Context, opt Options) ([]model.RawItem, error) {
	q := strings.TrimSpace(opt.Query)
	if q == "" {
		return nil, nil
	}
	if opt.APIKey == "" {
		return nil, errMissingKey("TinyFish", "TINYFISH_API_KEY")
	}
	params := url.Values{"query": []string{q}}
	var body struct {
		Query        string `json:"query"`
		TotalResults int    `json:"total_results"`
		Page         int    `json:"page"`
		Results      []struct {
			Position int    `json:"position"`
			SiteName string `json:"site_name"`
			Title    string `json:"title"`
			Snippet  string `json:"snippet"`
			URL      string `json:"url"`
			Date     string `json:"date"`
		} `json:"results"`
	}
	if err := doJSON(ctx, opt, "TinyFish", "GET", tinyfishEndpoint, params,
		map[string]string{"X-API-Key": opt.APIKey}, nil, &body); err != nil {
		return nil, err
	}
	var out []model.RawItem
	for _, it := range body.Results {
		if it.URL == "" {
			continue
		}
		rank := it.Position
		if rank <= 0 {
			rank = len(out) + 1 // 上游 position 异常时的兜底口径（与聚合层一致）
		}
		out = append(out, model.RawItem{
			Engine:  "tinyfish",
			Rank:    rank,
			Title:   strings.TrimSpace(it.Title),
			URL:     it.URL,
			Snippet: cleanSnippet(it.Snippet),
			Date:    strings.TrimSpace(it.Date),
			// Source = 发布方站点名（qianfan.website/serpapi.source 同语义）；
			// news 响应另有 publisher 字段，这里取 site_name 口径统一。
			Signals: signalOf(model.Signals{Source: it.SiteName}),
		})
	}
	return out, nil
}
