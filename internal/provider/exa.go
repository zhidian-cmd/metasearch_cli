package provider

import (
	"context"
	"strings"

	"github.com/zhidian-cmd/metasearch_cli/internal/model"
)

// exaProvider 移植自参考实现 providers/exa.py（Exa AI 语义搜索）。
//
// numResults 官方上限 100，跟随 limit 下发（聚合层另有 15 条上限兜底）。
// 发布日期字段为 publishedDate，RFC3339 形如 "2026-09-08T15:53:15.000Z"，
// 是三个带日期的引擎里格式最规整的。
type exaProvider struct{}

const (
	exaEndpoint = "https://api.exa.ai/search"
	exaMaxChars = 10000
)

func init() { Register(exaProvider{}) }

func (exaProvider) Name() string { return "exa" }

func (exaProvider) Search(ctx context.Context, opt Options) ([]model.RawItem, error) {
	q := strings.TrimSpace(opt.Query)
	if q == "" {
		return nil, nil
	}
	if opt.APIKey == "" {
		return nil, errMissingKey("Exa", "EXA_API_KEY")
	}
	payload := map[string]any{
		"query":      q,
		"numResults": trimLimit(opt.Limit, 100),
		"contents":   map[string]any{"text": map[string]any{"maxCharacters": exaMaxChars}},
	}
	var body struct {
		Results []struct {
			Title         string `json:"title"`
			URL           string `json:"url"`
			Text          string `json:"text"`
			PublishedDate string `json:"publishedDate"`
		} `json:"results"`
	}
	if err := doJSON(ctx, opt, "Exa", "POST", exaEndpoint, nil,
		map[string]string{"x-api-key": opt.APIKey}, payload, &body); err != nil {
		return nil, err
	}
	var out []model.RawItem
	for _, it := range body.Results {
		if it.URL == "" {
			continue
		}
		out = append(out, model.RawItem{
			Engine:  "exa",
			Rank:    len(out) + 1,
			Title:   strings.TrimSpace(it.Title),
			URL:     it.URL,
			Snippet: cleanSnippet(it.Text),
			Date:    strings.TrimSpace(it.PublishedDate),
		})
	}
	return out, nil
}

// errMissingKey 统一"缺密钥"错误文案（参考实现为 ValueError）。
func errMissingKey(provider, envVar string) error {
	return &HTTPError{Provider: provider, Status: 0, Reason: "缺少 API Key", Body: "请配置 " + envVar}
}
