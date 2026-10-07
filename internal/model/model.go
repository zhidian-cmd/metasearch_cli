// Package model 定义贯穿全流程的数据结构。
package model

import "time"

// RawItem 是单个搜索引擎返回的一条原始结果。
//
// Rank 是该结果在**本引擎内部**的排名（1 起），是输出 positions 字段的唯一依据
// —— 一旦赋值不得重排。
type RawItem struct {
	Engine  string
	Rank    int // 引擎内排名，1 起
	Title   string
	URL     string
	Snippet string
	Date    string // 发布日期原文；空串 = 该来源未提供（不是"未知"）
	// Signals 引擎响应里自带的筛选信号（可为 nil）。内部传递用，不直接序列化。
	Signals *Signals
}

// Signals 上游引擎**本来就有**、但对下游筛选有用的信号。
//
// 为什么要有它：这些值早就在引擎响应里，此前解析时被直接丢弃 —— 调用方只能
// 看到"引擎名 + 排名"，没有任何内容质量/权威性依据。带出来之后，调用方可以做
// 精细筛选（例如剔除导购页、优先政府站），而这正是"检索内核"该交给外层的东西。
//
// 各引擎能给的不一样，给不出的留空；同一 URL 被多引擎命中时按聚合层与去重层的
// 既有约定合并：字符串取首个非空、数值取更大者。
type Signals struct {
	// Score 引擎自带的相关性**数值**分（tavily.score，0~1）
	Score float64 `json:"score,omitempty"`
	// ScoreLevel 引擎自带的相关性**等级**（字符串形态，实测取值 "high" / "medium" / "low"）。
	//
	// ⚠️ 必须与 Score 分开：等级是字符串而不是数字（metaso 的 `score` 就是它），
	// 若按 float64 解码会**整个引擎解析失败**（已实测确认类型，勿合并这两个字段）。
	ScoreLevel string `json:"score_level,omitempty"`
	// RerankScore qianfan 重排分：上游对"与 query 相关程度"的打分
	RerankScore float64 `json:"rerank_score,omitempty"`
	// AuthorityScore qianfan 权威性分
	AuthorityScore float64 `json:"authority_score,omitempty"`
	// Source 发布方（serpapi.source "商务部财务司" / qianfan.website "百家号"）
	Source string `json:"source,omitempty"`
	// CardType 结果卡片类型（quark：声明式类型标记，如 ss_text / ss_pic / doc_jgh）
	CardType string `json:"card_type,omitempty"`
}

// MergeSignals 把 src 里非空的信号并入 dst（dst 可为 nil），返回合并后的值。
//
// 合并约定与去重层 mergeInto 对标题/摘要/日期的处理同风格：**字符串取首个非空、
// 数值取更大者**。返回的是新对象（不复用 src 的指针），避免调用方之间共享可变状态。
func MergeSignals(dst, src *Signals) *Signals {
	if src == nil {
		return dst
	}
	out := Signals{}
	if dst != nil {
		out = *dst
	}
	if src.Score > out.Score {
		out.Score = src.Score
	}
	if src.RerankScore > out.RerankScore {
		out.RerankScore = src.RerankScore
	}
	if src.AuthorityScore > out.AuthorityScore {
		out.AuthorityScore = src.AuthorityScore
	}
	for _, f := range []struct {
		dst *string
		src string
	}{
		{&out.ScoreLevel, src.ScoreLevel},
		{&out.Source, src.Source},
		{&out.CardType, src.CardType},
	} {
		if *f.dst == "" && f.src != "" {
			*f.dst = f.src
		}
	}
	if out == (Signals{}) {
		return nil
	}
	return &out
}

// Hit 是去重后的一条结果，也是最终对外输出的一项。
//
// Engine / Positions 是"同时命中"的载体：同一 URL 被多个引擎检索到时，
// Engine 收齐全部命中的引擎名，Positions 保留每个引擎内部的精确排名。
type Hit struct {
	// Engine 命中的引擎列表（升序去重）
	Engine []string `json:"engine"`
	// Positions 各引擎内部排名，如 {"bing":1,"exa":3}
	Positions map[string]int `json:"positions"`
	// Title 标题（多引擎命中时取最长的一条）
	Title string `json:"title"`
	// Snippet 摘要（多引擎命中时取最长的一条，完整输出、不截断）
	Snippet string `json:"snippet"`
	// URL 目标链接
	URL string `json:"url"`
	// Date 发布日期原文，空串 = 未提供
	Date string `json:"date,omitempty"`
	// MergedFrom **备用 URL 字典**：键 = 被本条吞并的那条 URL，值 = 它的标题、
	// 命中引擎与合并理由。
	//
	// 恒定输出、不由开关控制 —— 这是"删了也不丢信息"的保险：合并掉的是**另一个 URL**
	// （同页换参 / 换子域），那条对调用方可能是更好的入口，写在这里随时可回取，
	// 而不是被无声吞掉。
	//
	// ⚠️ 每个 AltRef 的 Engine 是**那条 URL 自己**的命中引擎，不是本条的并集。
	// JSON 对象键由 encoding/json 升序输出，顺序稳定、可直接 diff。
	MergedFrom map[string]AltRef `json:"merged_from,omitempty"`
	// Signals 上游引擎自带的筛选信号（见 Signals 注释）。全部为空时整个对象省略。
	Signals *Signals `json:"signals,omitempty"`
}

// AltRef 一条备用 URL 的随附信息 —— URL 本身是外层字典的键，这里不重复出现。
//
// Engine 是**这条 URL** 的命中引擎，不是本条的并集 —— 有了它调用方才能判断
// "这个备用入口是哪个引擎给的、值不值得换用"。
type AltRef struct {
	// Title 那条 URL 的标题（**合并前**的原值，不参与"取更长者"）
	Title string `json:"title,omitempty"`
	// Engine 该 URL 的命中引擎（升序去重）
	Engine []string `json:"engine"`
	// Reason 判定为同一页的依据（如 "d1:url_exact" / "url+title_high,same_host"）。
	// 供人工复核"这条为什么被合并"—— 少了它就只剩结论、没有依据。
	Reason string `json:"reason,omitempty"`
}

// EngineStat 单个引擎的执行情况（无论成功失败都会有一条，便于诊断"为什么少了个源"）。
type EngineStat struct {
	Engine    string `json:"engine"`
	OK        bool   `json:"ok"`
	Count     int    `json:"count"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
	// Coherence 桶相干度（V10.6）：query 去首词后的 term 在本桶 title+snippet+url
	// 中的覆盖率。nil = 无法判定（桶内 <4 条或切不出 anchor/rest），调用方按健康
	// 处理。只打标不删——"过滤 = 删结果一律不做"契约红线，删不删由调用方决定。
	// 指针形态：0.0 是真实的诱饵分值，不能用 omitempty 丢掉。
	Coherence *float64 `json:"coherence,omitempty"`
	// Decoy true = 该桶判为诱饵（Coherence < 0.3 且存在 witness 桶 ≥0.5）。
	// 判定算法见 internal/coherence（移植自 free-search-mcp，MIT）。
	Decoy bool `json:"decoy,omitempty"`
}

// DedupStage 单个去重阶段的减量（阶段名自描述，避免"哪一档砍了几条"要靠猜）。
type DedupStage struct {
	Stage  string `json:"stage"`
	Before int    `json:"before"`
	After  int    `json:"after"`
	Merged int    `json:"merged"`
}

// DedupStat 去重统计。
//
// 字段口径：
//   - Raw / Final 是**去重这一层**的进与出，与敲门阶段的减量分开记，
//     避免把"可达性剔除"误读成"去重删掉的"。
//   - Veto 记录候选对**被否决**的次数分布（`veto:cross_site` 等）。
//     它是"差点被误杀"的直接证据：否决多说明判据在拦，不是没跑。
type DedupStat struct {
	Mode        string         `json:"mode"`
	Threshold   float64        `json:"threshold"`
	Raw         int            `json:"raw"`
	Final       int            `json:"final"`
	TotalMerged int            `json:"total_merged"`
	Stages      []DedupStage   `json:"stages"`
	Veto        map[string]int `json:"veto,omitempty"`
	Pass        map[string]int `json:"pass,omitempty"`
}

// ReachStat 敲门（TCP 可达性预检）统计。
type ReachStat struct {
	Enabled      bool `json:"enabled"`
	HostsProbed  int  `json:"hosts_probed"`
	HostsOK      int  `json:"hosts_ok"`
	DroppedItems int  `json:"dropped_items"`
	Kept         int  `json:"kept"`
	// Prefetched 命中聚合阶段预热缓存的域名数（预热与聚合重叠的收益面）。
	Prefetched int `json:"prefetched"`
	// Unreachable 判死的域名（"host:port"，字典序），留档以便人工核对是否误杀。
	Unreachable []string `json:"unreachable,omitempty"`
}

// Stats 汇总数字。
type Stats struct {
	Candidates int `json:"candidates"` // 去重后、敲门前的候选数
	Returned   int `json:"returned"`   // 最终返回条数
}

// Report 是 CLI 的完整输出信封。
type Report struct {
	Query       string       `json:"query"`
	Engines     []string     `json:"engines"`
	GeneratedAt time.Time    `json:"generated_at"`
	ElapsedMS   int64        `json:"elapsed_ms"`
	EngineStat  []EngineStat `json:"engine_status"`
	Dedup       DedupStat    `json:"dedup"`
	Reach       ReachStat    `json:"reachability"`
	Stats       Stats        `json:"stats"`
	Results     []Hit        `json:"results"`
}
