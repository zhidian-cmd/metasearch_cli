package provider

import (
	"context"
	"strings"

	"github.com/zhidian-cmd/metasearch_cli/internal/model"
)

// tavilyProvider 移植自参考实现 providers/tavily.py。
//
// max_results 真实生效（跟随 limit），实测可回满 15 条。
// 参考实现里那段引用未定义变量 _VALID_TIME 的 time_range 死代码已删除
// —— 时间过滤功能已整体下线（2026-09-11 用户裁决）。
type tavilyProvider struct{}

const tavilyEndpoint = "https://api.tavily.com/search"

func init() { Register(tavilyProvider{}) }

func (tavilyProvider) Name() string { return "tavily" }

func (tavilyProvider) Search(ctx context.Context, opt Options) ([]model.RawItem, error) {
	q := strings.TrimSpace(opt.Query)
	if q == "" {
		return nil, nil
	}
	if opt.APIKey == "" {
		return nil, errMissingKey("Tavily", "TAVILY_API_KEY")
	}
	payload := map[string]any{"query": q, "max_results": trimLimit(opt.Limit, 20)}
	var body struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
			Snippet string `json:"snippet"`
			// score 实测为 float（0~1，如 0.84858394），可直接解。
			Score float64 `json:"score"`
		} `json:"results"`
	}
	if err := doJSON(ctx, opt, "Tavily", "POST", tavilyEndpoint, nil,
		map[string]string{"Authorization": "Bearer " + opt.APIKey}, payload, &body); err != nil {
		return nil, err
	}
	var out []model.RawItem
	for _, it := range body.Results {
		if it.URL == "" {
			continue
		}
		out = append(out, model.RawItem{
			Engine:  "tavily",
			Rank:    len(out) + 1,
			Title:   strings.TrimSpace(it.Title),
			URL:     it.URL,
			Snippet: cleanSnippet(firstNonEmpty(it.Content, it.Snippet)),
			Signals: signalOf(model.Signals{Score: it.Score}),
		})
	}
	return out, nil
}
