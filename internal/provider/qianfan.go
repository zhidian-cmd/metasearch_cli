package provider

import (
	"context"
	"strings"

	"github.com/zhidian-cmd/metasearch_cli/internal/model"
)

// qianfanProvider 移植自参考实现 providers/qianfan.py（百度千帆 AI 搜索）。
//
// 取代原 baidu 爬虫版：从"渲染 HTML 再解析"改为官方 API，
// 无反爬、无验证码、时延 1~2s，且每条结果自带发布日期。
// top_k 官方上限 50；edition 用 standard（专业版计费更高，收益不明确）。
//
// ⚠️ 已知数据质量噪声：部分 GBK 老页面服务端解码失败出现乱码标题、
// 以及 SEO 站群内容 —— provider 只透传不做二次判断，交由调用方筛查。
type qianfanProvider struct{}

const (
	qianfanEndpoint = "https://qianfan.baidubce.com/v2/ai_search/web_search"
	qianfanTopKMax  = 50
)

func init() { Register(qianfanProvider{}) }

func (qianfanProvider) Name() string { return "qianfan" }

func (qianfanProvider) Search(ctx context.Context, opt Options) ([]model.RawItem, error) {
	q := strings.TrimSpace(opt.Query)
	if q == "" {
		return nil, nil
	}
	if opt.APIKey == "" {
		return nil, errMissingKey("Qianfan", "QIANFAN_API_KEY")
	}
	payload := map[string]any{
		"messages":             []map[string]any{{"content": q, "role": "user"}},
		"resource_type_filter": []map[string]any{{"type": "web", "top_k": trimLimit(opt.Limit, qianfanTopKMax)}},
		"edition":              "standard",
	}
	var body struct {
		References []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Snippet string `json:"snippet"`
			Content string `json:"content"`
			Date    string `json:"date"`
			// 以下三个是上游自带、此前被丢弃的筛选信号（2026-09-17 取证接入）：
			// rerank_score 实测为 int、authority_score 实测为 float，均可直接解到 float64。
			RerankScore    float64 `json:"rerank_score"`
			AuthorityScore float64 `json:"authority_score"`
			Website        string  `json:"website"` // 发布平台名，如"百家号"
		} `json:"references"`
	}
	if err := doJSON(ctx, opt, "Qianfan", "POST", qianfanEndpoint, nil,
		map[string]string{"Authorization": "Bearer " + opt.APIKey}, payload, &body); err != nil {
		return nil, err
	}
	var out []model.RawItem
	for _, it := range body.References {
		if it.URL == "" {
			continue
		}
		out = append(out, model.RawItem{
			Engine: "qianfan",
			Rank:   len(out) + 1,
			Title:  strings.TrimSpace(it.Title),
			URL:    it.URL,
			// snippet（短摘要）优先，缺失回落 content（页面正文，可能很长）
			Snippet: cleanSnippet(firstNonEmpty(it.Snippet, it.Content)),
			Date:    strings.TrimSpace(it.Date),
			Signals: signalOf(model.Signals{
				RerankScore:    it.RerankScore,
				AuthorityScore: it.AuthorityScore,
				Source:         strings.TrimSpace(it.Website),
			}),
		})
	}
	return out, nil
}
