// Package aggregate 并行跑所有候选引擎 → 收集 → 归一化为统一结果列表。
//
// 移植自参考实现 search_core/aggregate.py 的 aggregate_search：
//
//   - 每个引擎一个 goroutine，单引擎失败/超时**不致命**（只影响自己）；
//   - 单引擎超时由 ctx 控制（等价参考实现的 asyncio.wait_for 45s 硬熔断）；
//   - 全局硬熔断：ctx 到期后仍在跑的引擎结果一律放弃，已收到的照常返回；
//   - 每引擎结果条数上限 config.DefaultLimit（参考实现 ENGINE_RESULT_CAP=15）。
//
// 与参考实现的两处**结构差异**（有意为之）：
//
//  1. 参考实现在本层就用 normalize_url_for_dedup（会剥 www./m./amp. 前缀 +
//     尾斜杠）合并同源 URL，本 CLI 改为**只按精确 URL 相等**合并 —— 放宽型
//     规范化（前缀 / 尾斜杠 / 追踪参数）一律不做，避免把不同页面归成同一条。
//  2. 参考实现在本层做广告/垃圾页关键词剔除（过滤即删结果），本 CLI 不做 ——
//     由调用方按 engine_status 与字段自行判断。
package aggregate

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zhidian-cmd/metasearch_cli/internal/coherence"
	"github.com/zhidian-cmd/metasearch_cli/internal/config"
	"github.com/zhidian-cmd/metasearch_cli/internal/model"
	"github.com/zhidian-cmd/metasearch_cli/internal/provider"
)

// Options 聚合参数。
type Options struct {
	Query         string
	Engines       []string
	Limit         int // 每引擎请求条数
	EngineTimeout time.Duration
	Concurrency   int
	APIKeys       config.APIKeys

	Insecure          bool
	Proxy             string
	AnySearchVertical bool

	// MaxPages 引擎内部翻页的最大页数（含首页）；0 = 各引擎用自己的默认值。
	// 见 provider.Options.MaxPages —— **当前实际只对 quark 生效**（详见该处注释）。
	MaxPages int

	// OnEngineResult 每收完一个引擎就回调它的结果 URL（可能被并发调用）。
	//
	// 用途：让敲门预检**与聚合重叠**。域名在本阶段就已经确定，与其等 8 个引擎
	// 全跑完再串行敲门，不如谁先回来就先探它的域名。实测整轮耗时里敲门一度占
	// 5s（聚合只要 2.4s），重叠后这段等待被藏进聚合窗口。
	OnEngineResult func(urls []string)

	// Logf 可选日志回调（nil 则静默）
	Logf func(format string, args ...any)
}

// Outcome 聚合产物。
type Outcome struct {
	Hits  []model.Hit
	Stats []model.EngineStat
}

func (o *Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// Collect 并发执行全部候选引擎并归一化结果。
func Collect(ctx context.Context, opt Options) Outcome {
	limit := opt.Limit
	if limit <= 0 {
		limit = config.DefaultLimit
	}
	// 每引擎入池上限恒为 config.DefaultLimit：-limit 调大也不会放宽它（口径见 config.DefaultLimit）。
	maxPer := config.DefaultLimit

	// 全局熔断由调用方（cmdSearch）的 ctx 携带 —— 这样 -global-timeout
	// 覆盖的是**整条链路**（含后面的敲门），而不只是聚合阶段。

	type engineResult struct {
		engine string
		items  []model.RawItem
		err    error
		took   time.Duration
	}

	// ⚠️ Concurrency<=0 表示"不限并发"：此时信号量必须给足容量（len(engines)），
	// 不能写成 max(1, 0)=1 —— 那会把全部引擎压成**串行**，慢引擎逐个吃掉
	// 全局预算，最后几个引擎必然被全局熔断（实测：8 引擎串行跑，serpapi/qianfan
	// 双双超时，整轮从 ~20s 劣化到 65s）。
	slots := opt.Concurrency
	if slots <= 0 {
		slots = len(opt.Engines)
	}
	sem := make(chan struct{}, max(1, slots))
	results := make(chan engineResult, len(opt.Engines))
	var wg sync.WaitGroup

	for _, name := range opt.Engines {
		p, ok := provider.Get(name)
		if !ok {
			results <- engineResult{engine: name, err: fmt.Errorf("未实现的引擎")}
			continue
		}
		wg.Add(1)
		go func(name string, p provider.Provider) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			popt := provider.Options{
				Query:             strings.TrimSpace(opt.Query),
				Limit:             limit,
				APIKey:            opt.APIKeys.Get(name),
				Timeout:           opt.EngineTimeout,
				Insecure:          opt.Insecure,
				Proxy:             opt.Proxy,
				AnySearchVertical: opt.AnySearchVertical,
				MaxPages:          opt.MaxPages,
				Logf:              opt.Logf,
			}
			if d, ok := config.EngineTimeoutFloor[name]; ok && popt.Timeout < time.Duration(d)*time.Second {
				// 单引擎超时下限（quark / serpapi 见 config.EngineTimeoutFloor 注释）
				popt.Timeout = time.Duration(d) * time.Second
			}

			start := time.Now()
			items, err := p.Search(ctx, popt)
			results <- engineResult{engine: name, items: items, err: err, took: time.Since(start)}
		}(name, p)
	}

	go func() { wg.Wait(); close(results) }()

	var collected []engineResult
	for r := range results {
		collected = append(collected, r)
		// 引擎结果一到就把它那批域名交给预检预热（非阻塞），
		// 使敲门与仍在跑的引擎重叠 —— 见 Options.OnEngineResult 注释。
		if opt.OnEngineResult != nil && len(r.items) > 0 {
			urls := make([]string, 0, len(r.items))
			for _, it := range r.items {
				if it.URL != "" {
					urls = append(urls, it.URL)
				}
			}
			if len(urls) > 0 {
				opt.OnEngineResult(urls)
			}
		}
	}
	// 引擎顺序稳定化，便于输出对比
	sort.SliceStable(collected, func(i, j int) bool { return collected[i].engine < collected[j].engine })

	// ---------- 桶相干度判定（V10.6：只打标不删，见 internal/coherence） ----------
	//
	// 动机：Bing 类引擎被反感时返回 200 + 十条只匹配 query 首词的格式完好结果
	//（"rust ownership borrowing" → 游戏 Rust 页），逐条闸拦不住这种"长得像成功"，
	// 融合排序把噪声插进每份答案。整桶信号才看得见。
	// witness 门控：只有另一个桶 ≥0.5 证明"回显可能"时才判诱饵，冷门 query
	// 全体弱桶不误杀。全局超时触发时跳过判定（fail-open）。
	coherenceScores := map[string]*float64{}
	decoyEngines := map[string]bool{}
	if ctx.Err() == nil {
		buckets := map[string][]coherence.Row{}
		for _, r := range collected {
			if r.err != nil {
				continue
			}
			rows := make([]coherence.Row, 0, len(r.items))
			for _, it := range r.items {
				rows = append(rows, coherence.Row{Title: it.Title, Snippet: it.Snippet, URL: it.URL})
			}
			buckets[r.engine] = rows
		}
		scores, decoy := coherence.Judge(opt.Query, buckets)
		for e, sc := range scores {
			v := sc
			coherenceScores[e] = &v
		}
		decoyEngines = decoy
		if len(decoy) > 0 {
			opt.logf("coherence: decoy buckets %v", decoy)
		}
	}

	// ---------- 归一化：精确 URL 相等合并，并集引擎、保留各引擎最靠前排名 ----------
	out := Outcome{}
	idx := map[string]int{}
	for _, r := range collected {
		stat := model.EngineStat{Engine: r.engine, LatencyMS: r.took.Milliseconds()}
		if r.err != nil {
			if ctx.Err() != nil {
				stat.Error = "已放弃（全局超时）"
			} else {
				stat.Error = r.err.Error()
			}
			opt.logf("engine %s failed/skipped: %s", r.engine, stat.Error)
			out.Stats = append(out.Stats, stat)
			continue
		}
		stat.OK = true
		// 桶相干度（V10.6）：分数与 decoy 标记透出，删不删由调用方决定。
		stat.Coherence = coherenceScores[r.engine]
		stat.Decoy = decoyEngines[r.engine]
		itemCount := 0
		for rank, it := range r.items {
			// 每引擎入池上限 maxPer(= config.DefaultLimit)。注意它**不等于** opt.Limit(-limit)：
			//   · -limit <= 15（默认档）：各 provider 已按 trimLimit(opt.Limit, …) 截断过，
			//     交上来的条数必 ≤ limit ≤ 15 → **本分支永不触发**，纯防御；
			//   · -limit > 15：provider 会按更大的 limit 请求，本分支才成为**真正的硬上限**
			//     （2026-09-17 实测：-limit 25 时 6 个引擎各请求 25、入池全部被砍到 15）。
			// 即"每引擎 15 条"是恒定口径，-limit 调大不会放宽它 —— 这是有意为之，不是 bug。
			if itemCount >= maxPer {
				break
			}
			if it.URL == "" {
				continue
			}
			itemCount++
			rankNo := it.Rank
			if rankNo <= 0 {
				rankNo = rank + 1
			}
			if j, ok := idx[it.URL]; ok {
				h := &out.Hits[j]
				if !slices.Contains(h.Engine, r.engine) {
					h.Engine = append(h.Engine, r.engine)
				}
				if cur, ok := h.Positions[r.engine]; !ok || rankNo < cur {
					h.Positions[r.engine] = rankNo
				}
				if len(it.Title) > len(h.Title) {
					h.Title = it.Title
				}
				if len(it.Snippet) > len(h.Snippet) {
					h.Snippet = it.Snippet
				}
				if h.Date == "" && it.Date != "" {
					h.Date = it.Date
				}
				h.Signals = model.MergeSignals(h.Signals, it.Signals)
				continue
			}
			idx[it.URL] = len(out.Hits)
			out.Hits = append(out.Hits, model.Hit{
				Engine:    []string{r.engine},
				Positions: map[string]int{r.engine: rankNo},
				Title:     it.Title,
				Snippet:   it.Snippet,
				URL:       it.URL,
				Date:      it.Date,
				Signals:   model.MergeSignals(nil, it.Signals),
			})
		}
		stat.Count = itemCount
		out.Stats = append(out.Stats, stat)
	}
	for i := range out.Hits {
		sort.Strings(out.Hits[i].Engine)
	}
	return out
}
