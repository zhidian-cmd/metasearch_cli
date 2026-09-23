package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/zhidian-cmd/metasearch_cli/internal/config"
	"github.com/zhidian-cmd/metasearch_cli/internal/provider"
)

// cmdAPIKey 是第二个（也是最后一个）命令：密钥配置。
//
//	apikey list                 列出各 provider 密钥状态（脱敏）
//	apikey set <provider> <key> 写入/覆盖密钥
//	apikey set <provider> -     同上，但密钥从 stdin 读一行（避免泄漏到进程列表）
//	apikey unset <provider>     删除密钥
//	apikey path                 显示 .env 搜索路径与持久化位置
//	apikey test [provider...]   实测密钥可用性
//
// 持久化：set/unset 默认落盘到**与当前工作目录无关**的固定路径
// （见 config.PersistentEnvPath），换个目录、换个 shell 都读得到。
func cmdAPIKey(args []string) int {
	if len(args) == 0 {
		apiKeyUsage()
		return 0
	}
	sub := strings.ToLower(strings.TrimSpace(args[0]))
	rest := args[1:]
	if isHelpSub(sub) {
		apiKeyUsage()
		return 0
	}

	fs := newFlagSet("metasearch_cli apikey")
	fEnv := fs.String("env", "", "指定 .env")
	fJSON := fs.Bool("json", false, "JSON 输出")
	fTimeout := fs.Int("timeout", config.DefaultEngineTimeout, "test 单引擎超时(秒)")
	help, err := parseFlags(fs, rest)
	if err != nil {
		fmt.Fprintln(os.Stderr, "参数错误:", err)
		return 2
	}
	if help {
		apiKeyUsage()
		return 0
	}
	pos := fs.Args()

	switch sub {
	case "list", "ls":
		return apiKeyList(*fEnv, *fJSON)
	case "set", "add":
		key, rc := resolveSetKey(pos)
		if rc != 0 {
			return rc
		}
		return apiKeySet(pos[0], key, *fEnv, *fJSON)
	case "unset", "rm", "del", "delete":
		if len(pos) < 1 {
			fmt.Fprintln(os.Stderr, "用法: metasearch_cli apikey unset <provider> [provider...]")
			return 2
		}
		return apiKeyUnset(pos, *fEnv, *fJSON)
	case "path":
		return apiKeyPath(*fEnv, *fJSON)
	case "test", "check":
		return apiKeyTest(pos, *fEnv, time.Duration(*fTimeout)*time.Second, *fJSON)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n\n", sub)
		apiKeyUsage()
		return 2
	}
}

func isHelpSub(s string) bool {
	return s == "help" || s == "-h" || s == "--help"
}

// setUsage 参数缺失时的两条用法提示。
const setUsage = "用法: metasearch_cli apikey set <provider> <key>\n" +
	"      metasearch_cli apikey set <provider> -    # 从 stdin 读一行（不进进程列表）"

// resolveSetKey 取出 set 子命令的密钥：支持两种写法。
//
//	apikey set <provider> <key>   key 作为位置参数（简单，但会出现在进程列表里）
//	apikey set <provider> -       key 从 stdin 读一行（GUI / 脚本应走这条）
//
// 为什么要有 stdin 通道：命令行参数在 Windows 任务管理器、Linux `ps aux`、macOS
// `ps` 里都是**明文可见**的，同机其他用户能读到完整密钥。走 stdin 就不进进程列表。
//
// 返回 (key, 0)；参数缺失返回 ("", 2)。
func resolveSetKey(pos []string) (string, int) {
	// 只有显式的 `-` 才走 stdin；其余一律按位置参数取，避免"少写参数"变成静默读 stdin 等输入。
	if len(pos) >= 2 {
		if pos[1] != "-" {
			return pos[1], 0
		}
		key, err := readKeyFromStdin()
		if err != nil {
			fmt.Fprintln(os.Stderr, "从 stdin 读取密钥失败:", err)
			return "", 2
		}
		if key == "" {
			fmt.Fprintln(os.Stderr, "stdin 读到的密钥为空")
			return "", 2
		}
		return key, 0
	}
	fmt.Fprintln(os.Stderr, setUsage)
	return "", 2
}

// readKeyFromStdin 读标准输入的第一行（去掉行尾 CR/LF 与首尾空白）。
// 用 Scanner 而非 ReadAll：`echo key | ...` 与交互输入都能在换行后立刻返回，
// 不必等 EOF。
func readKeyFromStdin() (string, error) {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 8*1024), 1<<20) // 密钥不可能这么长，给足余量防超长行
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("stdin 未提供内容")
	}
	return strings.TrimSpace(sc.Text()), nil
}

func apiKeyUsage() {
	fmt.Fprint(os.Stdout, `metasearch_cli apikey —— 密钥配置

用法:
  metasearch_cli apikey list                    列出各 provider 密钥状态（脱敏）
  metasearch_cli apikey set <provider> <key>    写入/覆盖密钥（原位替换，不产生重复行）
  metasearch_cli apikey set <provider> -        同上，密钥从 stdin 读一行（推荐：不进进程列表）
  metasearch_cli apikey unset <provider> [...]  删除密钥
  metasearch_cli apikey path                    显示 .env 搜索路径与持久化位置
  metasearch_cli apikey test [provider...]      实测密钥可用性（实发一次最小请求，消耗配额）

参数:
  -env <文件>    指定目标 .env（不指定则写入持久文件，见 apikey path）
  -json          以 JSON 输出结果
  -timeout <秒>  test 子命令的单引擎超时（默认 `+fmt.Sprint(config.DefaultEngineTimeout)+`）

provider 取值（也可直接写环境变量名）:
  exa        -> EXA_API_KEY
  tavily     -> TAVILY_API_KEY
  anysearch  -> ANYSEARCH_API_KEY  （可选：不填也可匿名使用，限流较低）
  serpapi    -> SERPAPI_API_KEY
  qianfan    -> QIANFAN_API_KEY
  metaso     -> METASO_API_KEY     （密塔搜索，官方 API，按 credits 计费）

⚠️ 密钥写在命令行里会暴露在进程列表（任务管理器 / ps aux），同机其他用户可见。
   GUI 与脚本请用「-」形式从 stdin 传，例如:
     echo "mk-xxxx" | metasearch_cli apikey set qianfan -

示例:
  metasearch_cli apikey list
  metasearch_cli apikey set qianfan mk-4B2E...
  metasearch_cli apikey set qianfan -            # 交互输入或管道传入
  metasearch_cli apikey set EXA_API_KEY mk-4B2E...
  metasearch_cli apikey unset qianfan
  metasearch_cli apikey test exa,tavily,qianfan
`)
}

// apiKeyRow 一行密钥状态（list / set / unset / test 共用）。
type apiKeyRow struct {
	Provider   string `json:"provider"`
	EnvVar     string `json:"env_var"`
	Configured bool   `json:"configured"`
	Masked     string `json:"masked,omitempty"`
	Source     string `json:"source,omitempty"`
	Note       string `json:"note,omitempty"`
}

func apiKeyList(explicit string, asJSON bool) int {
	keys, err := config.LoadAPIKeys(explicit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取密钥失败:", err)
		return 1
	}
	rows := buildRows(keys)
	if asJSON {
		jsonPrint(map[string]any{
			"write_target": config.WriteTargetPath(explicit),
			"layers":       describeLayers(keys),
			"keys":         rows,
		})
		return 0
	}
	fmt.Printf("写入目标（set/unset 落盘位置）: %s\n", config.WriteTargetPath(explicit))
	fmt.Printf("密钥来源分层（高 → 低，前者覆盖后者）:\n")
	for _, l := range describeLayers(keys) {
		mark := "    "
		if !l.Exists {
			mark = "  ✗ "
		}
		fmt.Printf("  %s%s%s\n", mark, l.Path, layerTag(l.Writable))
	}
	fmt.Println()
	fmt.Printf("%-10s %-20s %-8s %s\n", "PROVIDER", "ENV VAR", "状态", "密钥")
	for _, r := range rows {
		state := "未配置"
		if r.Configured {
			state = "已配置"
		}
		line := fmt.Sprintf("%-10s %-20s %-8s %s", r.Provider, r.EnvVar, state, r.Masked)
		if r.Source != "" {
			line += "   来源: " + r.Source
		}
		if r.Note != "" {
			line += "   (" + r.Note + ")"
		}
		fmt.Println(line)
	}
	return 0
}

func buildRows(keys config.APIKeys) []apiKeyRow {
	providers := make([]string, 0, len(config.APIKeyVars))
	for p := range config.APIKeyVars {
		providers = append(providers, p)
	}
	sort.Strings(providers)
	rows := make([]apiKeyRow, 0, len(providers))
	for _, p := range providers {
		row := apiKeyRow{
			Provider:   p,
			EnvVar:     config.APIKeyVars[p],
			Configured: keys.HasKey(p),
			Masked:     config.Mask(keys.Get(p)),
			Source:     keys.Sources[p],
		}
		if row.Source == "" && row.Configured {
			row.Source = "<进程环境变量>"
		}
		if p == "anysearch" {
			row.Note = "可选，匿名亦可用（限流较低）"
		}
		rows = append(rows, row)
	}
	return rows
}

// layerInfo 供 JSON 输出使用。
type layerInfo struct {
	Path     string `json:"path"`
	Exists   bool   `json:"exists"`
	Writable bool   `json:"writable"`
	Keys     int    `json:"keys"`
}

// layerTag 层来源的标签（console 输出用）：只剩"持久写入目标"这一档。
func layerTag(writable bool) string {
	if writable {
		return "  [持久写入目标]"
	}
	return ""
}

func describeLayers(keys config.APIKeys) []layerInfo {
	out := make([]layerInfo, 0, len(keys.Layers))
	for _, l := range keys.Layers {
		out = append(out, layerInfo{
			Path: l.Path, Exists: l.Exists,
			Writable: l.Writable, Keys: len(l.Vars),
		})
	}
	return out
}

// resolveProvider 把 provider 名或环境变量名统一成 (provider, envVar)。
func resolveProvider(s string) (string, string, error) {
	s = strings.TrimSpace(s)
	up := strings.ToUpper(s)
	if p, ok := config.ProviderOfEnvVar(up); ok {
		return p, up, nil
	}
	if envVar, ok := config.APIKeyVars[strings.ToLower(s)]; ok {
		return strings.ToLower(s), envVar, nil
	}
	known := make([]string, 0, len(config.APIKeyVars))
	for p := range config.APIKeyVars {
		known = append(known, p)
	}
	sort.Strings(known)
	return "", "", fmt.Errorf("未知 provider %q；可用: %s（或直接写环境变量名，如 EXA_API_KEY）",
		s, strings.Join(known, ", "))
}

func apiKeySet(providerArg, key, explicit string, asJSON bool) int {
	p, envVar, err := resolveProvider(providerArg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	key = strings.TrimSpace(key)
	if key == "" {
		fmt.Fprintln(os.Stderr, "密钥不能为空")
		return 2
	}
	target := config.WriteTargetPath(explicit)
	created, err := config.SetKey(target, envVar, key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "写入失败:", err)
		return 1
	}
	action := "已更新（原位替换）"
	if created {
		action = "已新建文件并写入"
	}

	// 写后复核：密钥可能被**更高优先级的层**遮蔽（进程环境变量 / 显式 -env /
	// exe同目录 / 当前目录），那样本次写入不会生效，必须当场说清楚。
	// 复核本身失败时不能拿零值 keys 下结论（那会把"读取失败"误判成"被环境变量遮蔽"），
	// 只能如实报告复核失败。
	keys, kerr := config.LoadAPIKeys(explicit)
	effective, shadowed := "", false
	warn := ""
	if kerr != nil {
		warn = fmt.Sprintf("写后复核失败（%v），无法确认 %s 是否被更高优先级来源遮蔽。", kerr, envVar)
	} else {
		effective = keys.Sources[p]
		shadowed = effective != target
		if shadowed {
			switch {
			case effective == "":
				warn = fmt.Sprintf("%s 已由**进程环境变量**提供，优先级高于 .env，本次写入不生效。", envVar)
			default:
				warn = fmt.Sprintf("%s 实际生效来源是 %s（优先级高于写入目标），本次写入被遮蔽。", envVar, effective)
			}
		}
	}

	if asJSON {
		jsonPrint(map[string]any{
			"provider": p, "env_var": envVar, "file": target,
			"created": created, "action": action, "masked": config.Mask(key),
			"effective_source": effective, "shadowed": shadowed, "warning": warn,
		})
		return 0
	}
	fmt.Printf("%s: %s = %s\n", action, envVar, config.Mask(key))
	fmt.Printf("文件: %s\n", target)
	if warn != "" {
		fmt.Fprintf(os.Stderr, "⚠️  注意: %s\n", warn)
	}
	return 0
}

func apiKeyUnset(providers []string, explicit string, asJSON bool) int {
	target := config.WriteTargetPath(explicit)
	type result struct {
		Provider string `json:"provider"`
		EnvVar   string `json:"env_var"`
		Removed  bool   `json:"removed"`
		// StillFrom 非空表示删掉本层后，该 key 仍由更低优先级的层提供
		StillFrom string `json:"still_from,omitempty"`
	}
	var results []result
	exit := 0
	for _, pa := range providers {
		p, envVar, err := resolveProvider(pa)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			exit = 2
			continue
		}
		removed, err := config.UnsetKey(target, envVar)
		if err != nil {
			fmt.Fprintln(os.Stderr, "删除失败:", err)
			exit = 1
			continue
		}
		r := result{Provider: p, EnvVar: envVar, Removed: removed}

		// 删后复核：更底层的 .env（或进程环境变量）可能仍在提供这个 key。
		// 不说清楚的话，用户会以为"删了怎么还在"。
		if keys, lerr := config.LoadAPIKeys(explicit); lerr == nil && keys.HasKey(p) {
			from := keys.Sources[p]
			if from == "" {
				from = "<进程环境变量>"
			}
			r.StillFrom = from
			fmt.Fprintf(os.Stderr, "⚠️  %s 已从 %s 删除，但仍由 %s 提供；要彻底移除请对那个文件执行 apikey unset -env <文件>。\n",
				envVar, target, from)
		} else if !removed {
			fmt.Fprintf(os.Stderr, "%s 未在 %s 中找到（可能来自其它 .env 或进程环境变量）。\n", envVar, target)
			if exit == 0 {
				exit = 3
			}
		}
		results = append(results, r)
	}
	if asJSON {
		jsonPrint(map[string]any{"file": target, "results": results})
		return exit
	}
	fmt.Printf("已处理 %d 项，写入目标: %s\n", len(results), target)
	return exit
}

func apiKeyPath(explicit string, asJSON bool) int {
	keys, err := config.LoadAPIKeys(explicit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取失败:", err)
		return 1
	}
	if asJSON {
		jsonPrint(map[string]any{
			"write_target": config.WriteTargetPath(explicit),
			"layers":       describeLayers(keys),
		})
		return 0
	}
	fmt.Printf("写入目标（set/unset 落盘位置）: %s\n\n", config.WriteTargetPath(explicit))
	fmt.Println("搜索顺序（高 → 低，前者覆盖后者；进程环境变量优先级最高）:")
	for i, l := range keys.Layers {
		state := "不存在"
		if l.Exists {
			state = fmt.Sprintf("存在，%d 个变量", len(l.Vars))
		}
		fmt.Printf("  %d. %s\n     %s%s\n", i+1, l.Path, state, layerTag(l.Writable))
	}
	return 0
}

// apiKeyTest 实测密钥可用性：对每个 provider 发一次最小请求（limit=1）。
//
// 只测指定的 provider；未指定则测全部"已配置"的（没 key 的跳过，不浪费时间）。
func apiKeyTest(providers []string, explicit string, timeout time.Duration, asJSON bool) int {
	keys, err := config.LoadAPIKeys(explicit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取密钥失败:", err)
		return 1
	}

	var targets []string
	if len(providers) > 0 {
		for _, pa := range providers {
			p, _, err := resolveProvider(pa)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
			targets = append(targets, p)
		}
	} else {
		for _, p := range config.AllEngines() {
			if keys.HasKey(p) || slices.Contains(config.FreeEngines, p) {
				targets = append(targets, p)
			}
		}
	}
	sort.Strings(targets)

	type testResult struct {
		Provider  string `json:"provider"`
		OK        bool   `json:"ok"`
		Count     int    `json:"count"`
		LatencyMS int64  `json:"latency_ms"`
		Error     string `json:"error,omitempty"`
	}
	var results []testResult
	exit := 0

	for _, name := range targets {
		p, ok := provider.Get(name)
		if !ok {
			results = append(results, testResult{Provider: name, Error: "未实现的引擎"})
			exit = 1
			continue
		}
		if !keys.HasKey(name) && !slices.Contains(config.FreeEngines, name) {
			results = append(results, testResult{Provider: name, Error: "未配置密钥，跳过"})
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		start := time.Now()
		items, err := p.Search(ctx, provider.Options{
			Query:             "test",
			Limit:             1,
			APIKey:            keys.Get(name),
			Timeout:           timeout,
			AnySearchVertical: false,
		})
		cancel()
		r := testResult{Provider: name, LatencyMS: time.Since(start).Milliseconds(), Count: len(items)}
		if err != nil {
			r.Error = err.Error()
			exit = 1
		} else {
			r.OK = true
		}
		results = append(results, r)
		if !asJSON {
			if r.OK {
				fmt.Printf("  %-10s ✓  OK   %3d 条  %5d ms\n", r.Provider, r.Count, r.LatencyMS)
			} else {
				fmt.Printf("  %-10s ✗  FAIL %s\n", r.Provider, r.Error)
			}
		}
	}
	if asJSON {
		jsonPrint(map[string]any{"results": results})
	} else {
		fmt.Println()
		fmt.Println("提示: 429/quota 类错误通常是账户额度问题，不是代码问题。")
	}
	return exit
}
