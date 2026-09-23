package provider

import (
	"context"
	"strconv"
	"strings"

	"github.com/zhidian-cmd/metasearch_cli/internal/model"
)

// metasoProvider —— 密塔（metaso.cn）官方搜索 API。
//
// 官方 JSON 接口 + Bearer 鉴权：无爬虫、无渲染、无翻页，一次请求要多少给多少。
// 实测（2026-09-19）：size=20 稳定回满 20 条；size=50 与 100 **都不报错**，
// 上游按自己的结果集封顶（某 query total=47 → 返回 47）。
//
// ⚠️ `score` 是**字符串等级**（high / medium / low），不是数值。必须落 Signals.ScoreLevel ——
// 按 float64 解会让**整个引擎解析失败**（当年 metaso 的 `score:"high"` 就是这个坑，
// model.Signals.ScoreLevel 的注释就是为它写的）。改字段前先 dump 原始响应确认类型。
type metasoProvider struct{}

const metasoEndpoint = "https://metaso.cn/api/v1/search"

func init() { Register(metasoProvider{}) }

func (metasoProvider) Name() string { return "metaso" }

func (metasoProvider) Search(ctx context.Context, opt Options) ([]model.RawItem, error) {
	q := strings.TrimSpace(opt.Query)
	if q == "" {
		return nil, nil
	}
	if opt.APIKey == "" {
		return nil, errMissingKey("Metaso", "METASO_API_KEY")
	}
	payload := map[string]any{
		"q":     q,
		"scope": "webpage",
		// ponytail: 不设条数上限 —— 实测 50/100 均不报错，上游自己封顶；凭空编一个反而会静默截断。
		"size": strconv.Itoa(max(1, opt.Limit)),
		// conciseSnippet=false 才给完整摘要，与"摘要不截断"契约一致；
		// includeSummary/includeRawContent 关掉：它们要的是正文/概括，本 CLI 只取 snippet。
		"includeSummary": false, "includeRawContent": false, "conciseSnippet": false,
	}
	var body struct {
		Webpages []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Score   string `json:"score"`
			Snippet string `json:"snippet"`
			Date    string `json:"date"`
		} `json:"webpages"`
	}
	if err := doJSON(ctx, opt, "Metaso", "POST", metasoEndpoint, nil,
		map[string]string{"Authorization": "Bearer " + opt.APIKey}, payload, &body); err != nil {
		return nil, err
	}
	var out []model.RawItem
	for _, it := range body.Webpages {
		if it.Link == "" {
			continue
		}
		out = append(out, model.RawItem{
			Engine:  "metaso",
			Rank:    len(out) + 1, // 切片顺序即上游 position 顺序（实测一致）
			Title:   strings.TrimSpace(it.Title),
			URL:     it.Link,
			Snippet: cleanSnippet(it.Snippet),
			Date:    strings.TrimSpace(it.Date),
			// score 是等级字符串（high/medium/low）→ ScoreLevel，不是 Score。
			Signals: signalOf(model.Signals{ScoreLevel: it.Score}),
		})
	}
	return out, nil
}
