package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/zhidian-cmd/metasearch_cli/internal/aggregate"
	"github.com/zhidian-cmd/metasearch_cli/internal/config"
	"github.com/zhidian-cmd/metasearch_cli/internal/dedup"
	"github.com/zhidian-cmd/metasearch_cli/internal/model"
	"github.com/zhidian-cmd/metasearch_cli/internal/reach"
)

// cmdSearch 是第一个命令：搜索。
//
// 流水线（**没有排序步骤**）：
//
//  1. 并发跑全部候选引擎
//  2. 归一化：精确 URL 相等即合并，并集引擎、保留各引擎最靠前排名
//  3. 保守去重：规范化 URL 全等无条件合并；标题/摘要候选对再进结构判决（含否决链）
//  4. 批量 TCP 握手，只留可连接的 URL（保持相对顺序）
//  5. 结构化输出
//
// ⚠️ 第 2 步与第 3 步是**两件事**：第 2 步只认"URL 一个字节都不差"，属归一化；
// 第 3 步才处理"同一页的两种写法 / 同站换子域 / 标题挂不同站点名"。
func cmdSearch(args []string) int {
	fs := newFlagSet("metasearch_cli")
	fQuery := fs.String("q", "", "关键词")
	fQueryLong := fs.String("query", "", "关键词")
	fEngine := fs.String("engine", "", "引擎列表")
	fLimit := fs.Int("limit", config.DefaultLimit, "每引擎请求条数")
	fEngineTimeout := fs.Int("engine-timeout", config.DefaultEngineTimeout, "单引擎超时(秒)")
	fGlobalTimeout := fs.Int("global-timeout", config.DefaultGlobalTimeout, "整轮熔断(秒)")
	fConcurrency := fs.Int("concurrency", 0, "并发引擎数上限")
	fTracking := fs.String("tracking", string(config.TrackingDefault), "追踪参数剥离档位")
	fPrecheck := fs.Bool("precheck", true, "TCP 可达性预检")
	fPrecheckTimeout := fs.Int("precheck-timeout", config.PrecheckTimeout, "单域名握手超时(秒)")
	fPrecheckWorkers := fs.Int("precheck-workers", config.PrecheckWorkers, "并发探测数")
	fFormat := fs.String("format", string(formatJSON), "输出格式")
	fPretty := fs.Bool("pretty", true, "JSON 缩进")
	fOut := fs.String("o", "", "输出文件")
	fVerbose := fs.Bool("v", false, "引擎级日志")
	fEnv := fs.String("env", "", "指定 .env")
	fVerifySSL := fs.Bool("verify-ssl", false, "校验 TLS 证书")
	fProxy := fs.String("proxy", "", "显式代理")
	fAnyVertical := fs.Bool("anysearch-vertical", true, "AnySearch 垂直解析")
	fMaxPages := fs.Int("max-pages", 0, "quark 内部翻页最大页数（0=默认 3；调小更快但条数少）")

	help, err := parseFlags(fs, args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "参数错误:", err)
		return 2
	}
	if help {
		usage()
		return 0
	}

	// 关键词：-q/-query 优先，否则位置参数以空格拼接
	query := strings.TrimSpace(*fQuery)
	if query == "" {
		query = strings.TrimSpace(*fQueryLong)
	}
	if query == "" {
		query = strings.TrimSpace(strings.Join(fs.Args(), " "))
	}
	if query == "" {
		fmt.Fprintln(os.Stderr, "缺少关键词。用法: metasearch_cli <关键词> [参数]（-h 看完整帮助）")
		return 2
	}

	format := outputFormat(strings.ToLower(strings.TrimSpace(*fFormat)))
	if !format.valid() {
		fmt.Fprintf(os.Stderr, "未知输出格式 %q（可选 json / jsonl / text）\n", *fFormat)
		return 2
	}
	tracking, okTrack := parseTracking(*fTracking)
	if !okTrack {
		fmt.Fprintf(os.Stderr, "未知 tracking 档位 %q（可选 default / minimal / none）\n", *fTracking)
		return 2
	}

	keys, err := config.LoadAPIKeys(*fEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取密钥失败:", err)
		return 1
	}
	engines, err := resolveEngines(*fEngine, keys)
	if err != nil {
		fmt.Fprintln(os.Stderr, "引擎选择错误:", err)
		return 2
	}

	logf := func(string, ...any) {}
	if *fVerbose {
		logf = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "[metasearch] "+format+"\n", args...)
		}
	}
	logf("引擎: %s", strings.Join(engines, ", "))
	logf("密钥来源: %s", describeEnvSource(keys))

	start := time.Now()

	// 全局硬熔断覆盖**整条链路**（并发检索 → 去重 → 敲门），
	// 而不是只管聚合阶段，否则慢站会把它拖过 -global-timeout 还没人管。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(*fGlobalTimeout)*time.Second)
	defer cancel()

	// 敲门预热器：需在聚合**之前**建好，聚合每收完一个引擎就把它结果的域名
	// 丢进来先探着 —— 这样敲门阶段与聚合重叠，而不是等聚合全跑完再串行敲门。
	// precheck 关掉时不建（不做任何多余连接）。
	var prober *reach.Prefetcher
	if *fPrecheck {
		prober = reach.NewPrefetcher(ctx,
			time.Duration(*fPrecheckTimeout)*time.Second, *fPrecheckWorkers, nil)
	}

	// ---------- 1~2. 并发跑引擎 + 归一化 ----------
	aggOpt := aggregate.Options{
		Query:         query,
		Engines:       engines,
		Limit:         *fLimit,
		EngineTimeout: time.Duration(*fEngineTimeout) * time.Second,
		Concurrency:   *fConcurrency,
		APIKeys:       keys,

		Insecure:          !*fVerifySSL,
		Proxy:             *fProxy,
		AnySearchVertical: *fAnyVertical,
		MaxPages:          *fMaxPages,

		Logf: logf,
	}
	if prober != nil {
		aggOpt.OnEngineResult = prober.AddURLs
	}
	out := aggregate.Collect(ctx, aggOpt)

	// ---------- 3. 保守去重 ----------
	// Raw/Final 只描述**这一层**的进与出，与敲门阶段的减量分开记 ——
	// 免得把"可达性剔除"误读成"去重删掉的"。
	// ⚠️ 刻意**不给外部任何入口**去关掉或调松这一层：判据强度固定在 DefaultOptions 的
	// conservative 预设。曾有过 -dedup / -dedup-threshold 两个开关，已按要求从源码删除
	// （理由见 docs/decisions.md「为什么没有去重开关」）。-tracking 是 URL 规范化档位，
	// 与"去重开关"是两回事，故仍从命令行透传。
	hits, dedupStat := dedup.Run(out.Hits, tracking)
	if dedupStat.TotalMerged > 0 {
		logf("去重: %d → %d（%s 档，合并 %d 条）",
			dedupStat.Raw, dedupStat.Final, dedupStat.Mode, dedupStat.TotalMerged)
	}

	candidates := len(hits)

	// ---------- 4. 批量 TCP 握手，只留可连接的 URL ----------
	reachStat := model.ReachStat{Enabled: *fPrecheck}
	if *fPrecheck && len(hits) > 0 {
		res := reach.Filter(ctx, hits, prober)
		hits = res.Kept
		reachStat.HostsProbed = res.HostsProbed
		reachStat.HostsOK = res.HostsOK
		reachStat.DroppedItems = len(res.Dropped)
		reachStat.Prefetched = res.Prefetched
		reachStat.Unreachable = res.Unreachable
		logf("敲门: 探测 %d 域名（其中 %d 已在聚合阶段预热完成），可达 %d，剔除 %d 条",
			res.HostsProbed, res.Prefetched, res.HostsOK, len(res.Dropped))
		if len(res.Unreachable) > 0 {
			// 明确列出被判死的域名：敲门只有"剔除"一个动作，误杀是不可见的，
			// 留一行日志便于人工核对（-precheck=false 可完全跳过）。
			logf("敲门: 判死域名 %s", strings.Join(res.Unreachable, ", "))
		}
	}
	reachStat.Kept = len(hits)

	// ---------- 5. 输出前收尾 ----------
	// 这里**刻意不做摘要截断**（参考实现有 aggregate.SNIPPET_MAX=400）：
	//   1. 截断点在去重之后，去重用的是未截断原文，所以不截断**不影响判重质量**；
	//   2. 上游正文的流量已经付过了，截断纯粹是让调用方少掉一段可用的筛选依据 ——
	//      下游要做精细筛选/二次打分，靠 400 字是不够的；
	//   3. 代价（长正文含页面噪声、JSON 体积变大）由调用方自己权衡，不由本层代劳。
	// 控制台 text 格式仍按显示宽度截断（见 formatText），那只影响人眼看的那一列。
	// merged_from 同理恒定输出，不由开关控制。
	if hits == nil {
		hits = []model.Hit{}
	}

	report := model.Report{
		Query:       query,
		Engines:     engines,
		GeneratedAt: time.Now(),
		ElapsedMS:   time.Since(start).Milliseconds(),
		EngineStat:  out.Stats,
		Dedup:       dedupStat,
		Reach:       reachStat,
		Stats: model.Stats{
			Candidates: candidates,
			Returned:   len(hits),
		},
		Results: hits,
	}

	var w io.Writer = os.Stdout
	// -o - 表示 stdout（通行约定）；不特判会真的建出一个名为 "-" 的文件
	if p := strings.TrimSpace(*fOut); p != "" && p != "-" {
		f, ferr := os.Create(p)
		if ferr != nil {
			fmt.Fprintln(os.Stderr, "创建输出文件失败:", ferr)
			return 1
		}
		defer f.Close()
		w = f
	}
	if err := writeReport(w, report, format, *fPretty); err != nil {
		fmt.Fprintln(os.Stderr, "输出失败:", err)
		return 1
	}
	if outPath := strings.TrimSpace(*fOut); outPath != "" && outPath != "-" && format != formatJSONL {
		fmt.Fprintf(os.Stderr, "已写入 %s（%d 条）\n", outPath, len(hits))
	}
	return 0
}

// resolveEngines 解析 -engine：
//   - 指定了：逐个校验
//   - 未指定：免费引擎 + 已配 key 的付费引擎
func resolveEngines(spec string, keys config.APIKeys) ([]string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return config.DefaultEngines(keys.HasKey), nil
	}
	all := config.AllEngines() // 一处算好（内部要排序+分配），不在循环里重算
	var out []string
	seen := map[string]bool{}
	for _, raw := range strings.Split(spec, ",") {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		if !slices.Contains(all, name) {
			return nil, fmt.Errorf("引擎 %q 不存在；已实现: %s", name, strings.Join(all, ", "))
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("未解析出任何引擎")
	}
	return out, nil
}

// parseTracking 解析 -tracking 档位。
func parseTracking(s string) (config.TrackingMode, bool) {
	switch config.TrackingMode(strings.ToLower(strings.TrimSpace(s))) {
	case config.TrackingDefault:
		return config.TrackingDefault, true
	case config.TrackingMinimal:
		return config.TrackingMinimal, true
	case config.TrackingNone:
		return config.TrackingNone, true
	}
	return "", false
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return "" // 防御：负/零截断长度会让 r[:n-1] 变成负索引
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}

// describeEnvSource 说明密钥最终来自哪些 .env 文件（诊断"为什么改了 .env 没生效"）。
// 按 provider 名排序遍历，保证输出顺序可复现（map 遍历顺序随机）。
func describeEnvSource(keys config.APIKeys) string {
	providers := make([]string, 0, len(keys.Sources))
	for p := range keys.Sources {
		providers = append(providers, p)
	}
	sort.Strings(providers)
	var used []string
	seen := map[string]bool{}
	for _, p := range providers {
		label := keys.Sources[p]
		if label == "" {
			label = "<进程环境变量>"
		}
		if !seen[label] {
			seen[label] = true
			used = append(used, label)
		}
	}
	if len(used) == 0 {
		return "未找到任何密钥来源"
	}
	return strings.Join(used, ", ")
}

// jsonPrint 小工具：把任意值以 UTF-8 友好的 JSON 打到 stdout。
func jsonPrint(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// ========== 输出渲染 ==========

// outputFormat 输出格式。
type outputFormat string

const (
	formatJSON  outputFormat = "json"
	formatJSONL outputFormat = "jsonl"
	formatText  outputFormat = "text"
)

func (f outputFormat) valid() bool {
	switch f {
	case formatJSON, formatJSONL, formatText:
		return true
	}
	return false
}

func writeReport(w io.Writer, rep model.Report, format outputFormat, pretty bool) error {
	switch format {
	case formatJSONL:
		return writeJSONL(w, rep)
	case formatText:
		return writeText(w, rep)
	default:
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		if pretty {
			enc.SetIndent("", "  ")
		}
		return enc.Encode(rep)
	}
}

// writeJSONL 第一行是 meta（不含 results），随后每行一条结果 —— 便于流式消费。
func writeJSONL(w io.Writer, rep model.Report) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	meta := rep
	meta.Results = nil
	if err := enc.Encode(meta); err != nil {
		return err
	}
	for _, r := range rep.Results {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

func writeText(w io.Writer, rep model.Report) error {
	var b strings.Builder
	fmt.Fprintf(&b, "查询: %s\n", rep.Query)
	fmt.Fprintf(&b, "引擎: %s\n", strings.Join(rep.Engines, ", "))
	fmt.Fprintf(&b, "耗时: %d ms\n", rep.ElapsedMS)
	// 三段数字分开报：候选 → 去重后 → 可达。
	// 只报首尾会把"去重合了几条"和"敲门剔了几条"混成一个数，看不出是哪一层干的。
	fmt.Fprintf(&b, "候选 %d → 去重后 %d → 可达 %d\n",
		rep.Dedup.Raw, rep.Dedup.Final, len(rep.Results))
	fmt.Fprintf(&b, "去重: %s 档（恒开，标题阈值 %.2f）", rep.Dedup.Mode, rep.Dedup.Threshold)
	for _, s := range rep.Dedup.Stages {
		fmt.Fprintf(&b, " | %s %d→%d(合并%d)", s.Stage, s.Before, s.After, s.Merged)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "可达性预检: 探测 %d 个域名，可达 %d，剔除 %d 条\n",
		rep.Reach.HostsProbed, rep.Reach.HostsOK, rep.Reach.DroppedItems)
	b.WriteString(strings.Repeat("=", 78) + "\n")

	if len(rep.Results) == 0 {
		b.WriteString("没有结果。\n")
	} else {
		for i, r := range rep.Results {
			fmt.Fprintf(&b, "[%d] %s\n", i+1, r.URL)
			fmt.Fprintf(&b, "    标题: %s\n", truncateRunes(r.Title, 90))
			fmt.Fprintf(&b, "    命中: %s  %s\n",
				strings.Join(r.Engine, " / "), positionsText(r.Positions))
			if r.Snippet != "" {
				fmt.Fprintf(&b, "    摘要: %s\n", truncateRunes(r.Snippet, 120))
			}
			if len(r.MergedFrom) > 0 {
				keys := make([]string, 0, len(r.MergedFrom))
				for u := range r.MergedFrom {
					keys = append(keys, u)
				}
				sort.Strings(keys) // map 无序遍历 → 排序，保证控制台输出稳定可读
				for _, u := range keys {
					alt := r.MergedFrom[u]
					fmt.Fprintf(&b, "    备用: %s", truncateRunes(u, 70))
					if alt.Title != "" {
						fmt.Fprintf(&b, "  《%s》", truncateRunes(alt.Title, 40))
					}
					fmt.Fprintf(&b, "  [%s] %s\n", strings.Join(alt.Engine, "/"), alt.Reason)
				}
			}
			b.WriteString("\n")
		}
	}

	if len(rep.EngineStat) > 0 {
		b.WriteString(strings.Repeat("-", 78) + "\n引擎状态:\n")
		for _, s := range rep.EngineStat {
			state := "OK"
			if !s.OK {
				state = "FAIL"
			}
			line := fmt.Sprintf("  %-10s %-4s %3d 条  %5d ms", s.Engine, state, s.Count, s.LatencyMS)
			if s.Error != "" {
				line += "  " + truncateRunes(s.Error, 90)
			}
			b.WriteString(line + "\n")
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// positionsText 把 {引擎:排名} 渲染成 "bing#1, exa#3"（键升序，输出可复现）。
func positionsText(m map[string]int) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // map 无序遍历 → 排序，保证输出可复现
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s#%d", k, m[k]))
	}
	return "(" + strings.Join(parts, ", ") + ")"
}
