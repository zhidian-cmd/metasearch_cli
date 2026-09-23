package provider

import (
	"context"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/zhidian-cmd/metasearch_cli/internal/model"
)

// anysearchProvider 移植自参考实现 providers/anysearch.py。
//
// AnySearch 是外部统一搜索服务，API Key 可选、匿名低限流可用。
// 除通用搜索外支持 17 个垂直 domain（finance/academic/legal/...），
// 自动流程：match_domain(query) 命中 → 拉 sub-domains 目录 → 选 sub_domain
// → 必填 params 空串兜底 → 带 tag 垂直检索；任一环节失败则回落通用搜索。
//
// ⚠️ max_results 官方规定 1~10（硬上限），故本引擎**永远拿不到 15 条**。
type anysearchProvider struct{}

const (
	anysearchBaseDefault = "https://api.anysearch.com"
	anysearchSearchPath  = "/v1/search"
	anysearchSubPath     = "/v1/sub-domains"
	anysearchClientID    = "metasearch-cli/1.0 (go)"
)

func init() { Register(anysearchProvider{}) }

func (anysearchProvider) Name() string { return "anysearch" }

func anysearchBase() string {
	if v := strings.TrimSpace(os.Getenv("ANYSEARCH_API_BASE_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return anysearchBaseDefault
}

func anysearchHeaders(apiKey string) map[string]string {
	h := map[string]string{
		"Content-Type":       "application/json",
		"X-Anysearch-Client": anysearchClientID,
	}
	if apiKey != "" {
		h["Authorization"] = "Bearer " + apiKey
	}
	return h
}

func (anysearchProvider) Search(ctx context.Context, opt Options) ([]model.RawItem, error) {
	q := strings.TrimSpace(opt.Query)
	if q == "" {
		return nil, nil
	}
	payload := map[string]any{
		"query": q,
		// max_results 官方上限 10，超了会被拒；与参考实现 min(limit, 10) 一致
		"max_results": trimLimit(opt.Limit, 10),
	}
	if opt.AnySearchVertical {
		if tag, params := resolveVertical(ctx, opt, q); tag != "" {
			payload["tag"] = tag
			if len(params) > 0 {
				payload["params"] = params
			}
			// 垂直领域命中：通过 -v 可见，便于确认"垂直搜索是否发挥了作用"
			if opt.Logf != nil {
				opt.Logf("AnySearch: 垂直领域命中 tag=%s", tag)
			}
		} else if opt.Logf != nil {
			opt.Logf("AnySearch: 未命中垂直领域，回落通用搜索")
		}
	}

	var body struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Results []struct {
				Title   string `json:"title"`
				Name    string `json:"name"`
				URL     string `json:"url"`
				Link    string `json:"link"`
				Content string `json:"content"`
				Snippet string `json:"snippet"`
			} `json:"results"`
		} `json:"data"`
	}
	if err := doJSON(ctx, opt, "AnySearch", "POST", anysearchBase()+anysearchSearchPath,
		nil, anysearchHeaders(opt.APIKey), payload, &body); err != nil {
		return nil, err
	}
	// 错误：HTTP>=400 已由 do 处理；此处补 code!=0 的业务错误
	if body.Code != 0 {
		return nil, &HTTPError{Provider: "AnySearch", Status: 0, Reason: "业务错误码", Body: body.Message}
	}

	var out []model.RawItem
	for _, it := range body.Data.Results {
		title := firstNonEmpty(it.Title, it.Name)
		link := firstNonEmpty(it.URL, it.Link)
		if title == "" || link == "" {
			continue
		}
		out = append(out, model.RawItem{
			Engine:  "anysearch",
			Rank:    len(out) + 1,
			Title:   strings.TrimSpace(title),
			URL:     link,
			Snippet: cleanSnippet(firstNonEmpty(it.Content, it.Snippet)),
		})
	}
	return out, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---------- 垂直领域解析 ----------

var anysearchDomains = []string{
	"general", "resource", "social_media", "finance", "academic", "legal",
	"health", "business", "security", "ip", "code", "energy",
	"environment", "agriculture", "travel", "film", "gaming",
}

var anysearchDomainKeywords = map[string][]string{
	"resource":     {"资源", "下载", "资料", "素材", "模板", "工具", "pdf", "文档"},
	"social_media": {"微博", "小红书", "抖音", "快手", "知乎", "推特", "x平台", "社交", "热搜", "网红"},
	"finance":      {"股票", "基金", "股市", "财经", "金融", "汇率", "期货", "债券", "理财", "股价", "上市公司", "财报", "a股", "美股"},
	"academic":     {"论文", "期刊", "学术", "研究", "文献", "学报", "doi", "引文", "课题", "博士", "硕士", "科研"},
	"legal":        {"法律", "法规", "律师", "条款", "合规", "诉讼", "判决", "仲裁", "知识产权法", "刑法", "民法"},
	"health":       {"健康", "医疗", "疾病", "药物", "医生", "症状", "医院", "疫苗", "治疗", "癌症", "新冠", "体检"},
	"business":     {"公司", "企业", "商业", "创业", "融资", "营收", "利润", "市场", "行业", "品牌", "营销", "管理"},
	"security":     {"安全", "漏洞", "攻击", "防护", "防火墙", "加密", "黑客", "勒索", "病毒", "网络战", "cve"},
	"ip":           {"专利", "商标", "版权", "知识产权", "发明", "著作权", "ip", "外观设计"},
	"code":         {"代码", "编程", "开发", "函数", "api", "python", "java", "javascript", "git", "算法", "调试", "部署", "开源", "github", "程序"},
	"energy":       {"能源", "电力", "电池", "石油", "天然气", "光伏", "风电", "核电", "碳中和", "储能"},
	"environment":  {"环境", "污染", "生态", "气候", "环保", "碳排放", "温室", "垃圾分类", "绿色"},
	"agriculture":  {"农业", "种植", "养殖", "农产品", "粮食", "农药", "化肥", "农机", "畜牧业"},
	"travel":       {"旅游", "旅行", "景点", "酒店", "机票", "签证", "攻略", "民宿", "行程", "自驾"},
	"film":         {"电影", "影视", "票房", "导演", "演员", "剧集", "影评", "预告片", "奥斯卡", "戛纳"},
	"gaming":       {"游戏", "电竞", "玩家", "手游", "主机", "steam", "ps5", "xbox", "switch", "游戏机", "通关", "外挂"},
}

// matchDomain 按关键词自动匹配最合适的垂直领域；无命中返回空串。
func matchDomain(query string) string {
	q := strings.ToLower(query)
	for _, d := range anysearchDomains {
		if d == "general" {
			continue
		}
		for _, kw := range anysearchDomainKeywords[d] {
			if strings.Contains(q, strings.ToLower(kw)) {
				return d
			}
		}
	}
	return ""
}

type subDomainInfo struct {
	SubDomain   string                    `json:"sub_domain"`
	Description string                    `json:"description"`
	Params      map[string]map[string]any `json:"params"`
}

// subDomainsCache 目录进程内缓存（按 domain），避免每次搜索都拉目录
// —— 原 skill 明确要求 "Do NOT call repeatedly"。
var (
	subDomainsMu    sync.Mutex
	subDomainsCache = map[string][]subDomainInfo{}
)

func fetchSubDomains(ctx context.Context, opt Options, domain string) []subDomainInfo {
	subDomainsMu.Lock()
	if v, ok := subDomainsCache[domain]; ok {
		subDomainsMu.Unlock()
		return v
	}
	subDomainsMu.Unlock()

	var body struct {
		Data struct {
			Domains []struct {
				Domain     string          `json:"domain"`
				SubDomains []subDomainInfo `json:"sub_domains"`
			} `json:"domains"`
		} `json:"data"`
	}
	err := doJSON(ctx, opt, "AnySearch", "GET", anysearchBase()+anysearchSubPath,
		url.Values{"domain": {domain}}, anysearchHeaders(opt.APIKey), nil, &body)
	if err != nil {
		return nil
	}
	var subs []subDomainInfo
	for _, d := range body.Data.Domains {
		if d.Domain == domain {
			subs = d.SubDomains
			break
		}
	}
	subDomainsMu.Lock()
	subDomainsCache[domain] = subs
	subDomainsMu.Unlock()
	return subs
}

// pickSubDomain 从目录中选最匹配的 sub_domain：query 词命中名称/描述者优先，否则取第一个。
func pickSubDomain(subs []subDomainInfo, query string) (subDomainInfo, bool) {
	if len(subs) == 0 {
		return subDomainInfo{}, false
	}
	var words []string
	for _, w := range strings.Fields(strings.ReplaceAll(strings.ToLower(query), "，", " ")) {
		if len([]rune(w)) >= 2 {
			words = append(words, w)
		}
	}
	for _, sd := range subs {
		name := strings.ToLower(sd.SubDomain)
		desc := strings.ToLower(sd.Description)
		for _, w := range words {
			if strings.Contains(name, w) || strings.Contains(desc, w) {
				return sd, true
			}
		}
	}
	return subs[0], true
}

// requiredParams 必填参数全部带上、无适用值传空字符串（原 skill 规则：漏带会校验报错）。
func requiredParams(sd subDomainInfo) map[string]any {
	out := map[string]any{}
	for name, info := range sd.Params {
		if req, _ := info["required"].(bool); req {
			out[name] = ""
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveVertical 解析垂直检索上下文；任一环节失败返回空（调用方回落通用搜索）。
func resolveVertical(ctx context.Context, opt Options, query string) (string, map[string]any) {
	domain := matchDomain(query)
	if domain == "" {
		return "", nil
	}
	sd, ok := pickSubDomain(fetchSubDomains(ctx, opt, domain), query)
	if !ok {
		return "", nil
	}
	tag := sd.SubDomain
	if tag == "" {
		tag = domain
	}
	return tag, requiredParams(sd)
}
