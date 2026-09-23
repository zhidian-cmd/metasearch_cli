// metasearch_cli —— 多引擎并发元搜索 CLI（Go 版）
//
// 借鉴 deep_search MCP（Python 版）的元搜索实现，
// 用 Go 的并发能力重写整条链路：
//
//	并发请求全部候选引擎 → 批量 TCP 握手剔除不可达 → 输出结构化 JSON。
//
// **不做排序**：参考实现那套 RRF 融合 + 引擎权重 + 内容质量分全部未移植。
// 本 CLI 只负责"把能连上的 URL 和它们的多引擎命中情况如实摆出来"，
// 排序与取舍交给调用方。
//
// 只提供两个命令：
//
//	metasearch_cli <关键词> [参数]        搜索
//	metasearch_cli apikey <子命令> [参数] 密钥配置
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/zhidian-cmd/metasearch_cli/internal/config"
)

// version 版本号（-version 打印）。
const version = "1.1.0"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "apikey", "key", "keys":
			return cmdAPIKey(args[1:])
		case "search", "s":
			return cmdSearch(args[1:])
		case "version", "-version", "--version", "v":
			fmt.Printf("metasearch_cli %s\n", version)
			return 0
		case "help", "-h", "--help":
			usage()
			return 0
		}
	}
	// 默认命令：整串当搜索参数（metasearch_cli "关键词" -engine bing,quark）
	return cmdSearch(args)
}

func usage() {
	fmt.Fprintf(os.Stdout, `metasearch_cli %s —— 多引擎并发元搜索（只检索，不排序）

用法:
  metasearch_cli <关键词> [参数]                 搜索（默认命令）
  metasearch_cli apikey <子命令> [参数]          密钥配置

搜索参数:
  -q, -query <词>        关键词（也可直接用位置参数；多个位置参数以空格拼接）
  -engine <列表>         逗号分隔的引擎；默认=免费引擎 + 已配 key 的付费引擎
                         可用: %s
  -limit <n>             每引擎请求条数（默认 15，与参考实现 text_sources_per_query 一致）
  -engine-timeout <秒>   单引擎超时（默认 20；serpapi 有 30s 下限，见下）
  -global-timeout <秒>   整轮硬熔断（默认 60）
  -concurrency <n>       同时运行的引擎数上限（默认 0=不限）

去重与 URL 规范化（去重**恒开、无开关**：宁可漏杀，不可误杀）:
  判据要点: 同页 URL 全等无条件合并；其余候选对必须"同站 + 同栏目"且
            标题/摘要够像，任一否决项命中就放过。
  ⚠️ 刻意**不提供档位/阈值开关**：调用方只给关键词，判据强度不由外部下调
     （曾有一个把去重整层关掉的开关，已从源码删除；理由见 docs/decisions.md）。
  -tracking <档位>       剥哪些追踪参数：default(默认) / minimal / none
                         注: 合并恒定留痕 —— 被吞并的 URL、来源引擎与判定理由
                             写在该条的 merged_from 里；去重减量与不可达剔除
                             分开统计在 dedup / reachability 两块，别混着看。

可达性预检（批量 TCP 握手）:
  -precheck <bool>       是否敲门剔除不可达（默认 true）
  -precheck-timeout <秒> 单域名握手超时（默认 2）
                         可达域名实测最慢 287ms，而不可达要吃满本值才判死，
                         32 并发下整段敲门时长基本等于本值。调大只会拉长尾部。
  -precheck-workers <n>  并发探测数（默认 32；预热与敲门共用这一上限）
  注: 域名探测在聚合阶段就开始（谁先返回就先探它的域名），敲门时多数直接命中缓存。
      -v 会打印命中数与被判死的域名清单。

输出与其它:
  -format <json|jsonl|text>
  -pretty <bool>         JSON 缩进（默认 true）
  -o <文件>              写入文件（默认 stdout；-o - 亦表示 stdout）
  -v                     把引擎级日志打到 stderr
  注: 摘要恒定输出**完整正文**，不做长度截断。
      同一 URL 被多个引擎命中时会合并成一条，engine 列全部命中方、
      positions 保留各引擎内的原始排名。
  -env <文件>            指定 .env（默认按 显式 → 环境变量 → exe同目录 → 当前目录 → 持久文件 分层合并）
  -verify-ssl <bool>     true 则校验 TLS 证书（默认 false，与参考实现 verify_ssl=False 一致）
  -proxy <url>           显式代理；默认跟随 HTTP_PROXY/HTTPS_PROXY
  -anysearch-vertical    启用 AnySearch 垂直领域自动解析（默认 true）
  -max-pages <n>         quark 内部翻页最大页数，0 = 默认 3。
                         翻页是串行的，每页 1.5~2.5s；调小更快但条数减少。
                         （仅 quark 读此参数：bing 按 limit 自算页数，
                           serpapi 读自己的常量、且其 num 被忽略，默认会翻一页。）

apikey 子命令:
  metasearch_cli apikey list                   列出各 provider 密钥状态（脱敏）
  metasearch_cli apikey set <provider> <key>   写入/覆盖密钥
  metasearch_cli apikey set <provider> -       同上，密钥从 stdin 读一行（推荐，不进进程列表）
  metasearch_cli apikey unset <provider>       删除密钥
  metasearch_cli apikey path                   显示 .env 搜索路径与持久化位置
  metasearch_cli apikey test [provider...]     实测密钥可用性（实发一次最小请求）

示例:
  metasearch_cli "食品微生物检验 目的 任务"
  metasearch_cli "量子计算 进展" -engine bing,quark -format text
  metasearch_cli "site:arxiv.org transformer" -engine bing,exa -precheck=false
  metasearch_cli apikey set qianfan mk-xxxx
  echo "mk-xxxx" | metasearch_cli apikey set qianfan -   # 密钥不进进程列表
`, version, strings.Join(config.AllEngines(), ", "))
}

// ========== 参数解析 ==========
//
// 用标准库 flag，但允许**位置参数与参数任意混排**
// （`metasearch_cli 关键词 -engine bing` 这种自然写法，标准库原生不支持：
// 它遇到第一个位置参数就停止解析）。做法是先把参数重排到前面再交给标准库。

// newFlagSet 建一个 FlagSet：输出吞掉（错误由我们自己打），Usage 置空。
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// parseFlags 重排参数后交给标准库解析；help=true 表示用户请求帮助。
func parseFlags(fs *flag.FlagSet, args []string) (help bool, err error) {
	isBool := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			isBool[f.Name] = true
		}
	})

	flags := make([]string, 0, len(args))
	pos := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		name, hasVal, isFlag := splitFlagToken(tok)
		if !isFlag {
			pos = append(pos, tok)
			continue
		}
		flags = append(flags, tok)
		// 非布尔参数且写成 `-name value`（没带 =）：把下一个 token 当它的取值。
		// 未知参数不吞下一个 token —— 让标准库去报"参数不存在"，错误更准确。
		if !hasVal && !isBool[name] && fs.Lookup(name) != nil && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}

	err = fs.Parse(append(flags, pos...))
	if errors.Is(err, flag.ErrHelp) {
		return true, nil
	}
	return false, err
}

// splitFlagToken 判断 token 是否为参数，返回参数名与是否自带 =value。
func splitFlagToken(tok string) (name string, hasVal, isFlag bool) {
	if len(tok) < 2 || tok[0] != '-' {
		return "", false, false
	}
	name = strings.TrimLeft(tok, "-")
	if name == "" { // 单个 "-"，当位置参数
		return "", false, false
	}
	if i := strings.IndexByte(name, '='); i >= 0 {
		return name[:i], true, true
	}
	return name, false, true
}
