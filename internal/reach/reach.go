// Package reach 做批量 TCP 握手预检（"敲门"）。
//
// 移植自参考实现 search.py 的 _filter_reachable / _host_reachable，
// 但修掉了参考实现的一个耗时结构问题，见下。
//
// 位置与语义：聚合归一化之后、输出之前。TCP 握手失败 → HTTP 必然失败 → 直接剔除，
// 靠后的结果自动前进补位。握手成功 ≠ HTTP 成功（反爬/404 仍在），
// 故敲门只用于排除，不替代抓取。
//
// ⚠️ 端口必须按 scheme 选取（https→443 / http→80），不能一律探 443：
// http 站点若未提供 443 会被误判为不可达而整条剔除。
//
// ============ 为什么有 Prefetcher（本实现相对参考实现的结构改动）============
//
// 参考实现里"跑引擎"和"敲门"是**串行**两段，整轮耗时 = 聚合 + 敲门。
// 实测（8 引擎 / 一个关键词）：
//
//	聚合  2.27~2.41s   ← 并发已经跑满，等于最慢引擎（quark 2.25~2.37s）
//	敲门  4.53~5.23s   ← 唯一的瓶颈
//	整轮  6.94~7.50s
//
// 敲门为什么能比聚合还慢：**可达域名实测最慢只有 287ms**（61 个全部 <300ms），
// 但只要有 1 个域名握手拿不到响应，它就要吃满 timeout 才判死。32 并发下其余
// 域名 0.3s 内全部完成，整段时长于是被那一个域名钉死在 timeout 上。
//
// 于是做两件事：
//
//  1. **降超时**（默认 2s）：可达域名最慢 287ms，2s 已有 7 倍余量，
//     而不可达的尾部从 5s 压到 2s。
//  2. **与聚合重叠预热**：域名在聚合阶段就陆续确定了 —— 每收完一个引擎，
//     它那批结果的域名立刻开始探（aggregate 的 OnEngineResult 钩子）。
//     等真正的敲门阶段开始时，绝大部分判定已经在缓存里，直接命中。
//
// 预热**不增加任何请求**：同域名只探一次，且"正在探"的在途判定会被复用。
// 剔除语义与原先逐字节一致，只是把等待时间藏进了聚合阶段。
package reach

import (
	"context"
	"net"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/zhidian-cmd/metasearch_cli/internal/model"
)

// DialFunc 可注入的拨号函数（测试用）；与 net.Dialer.DialContext 同签名。
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Result 预检结果。
type Result struct {
	Kept        []model.Hit
	Dropped     []model.Hit
	HostsProbed int
	HostsOK     int
	// Prefetched 直接命中缓存（预热阶段已探完）的域名数，用于确认重叠是否生效。
	Prefetched int
	// Unreachable 判死的域名（"host:port"），按字典序，便于人工核对是否误杀。
	Unreachable []string
}

// target 一个待探测的 (host, port)。
type target struct {
	host string
	port string
}

func (t target) addr() string { return net.JoinHostPort(t.host, t.port) }

// targetOf 解析 URL 的探测目标；失败返回零值（host 为空，调用方据此剔除）。
func targetOf(rawURL string) target {
	u, err := url.Parse(rawURL)
	if err != nil {
		return target{}
	}
	host := u.Hostname()
	if host == "" {
		return target{}
	}
	port := u.Port() // ⚠️ 非数字端口（"http://x.com:abc/"）在这里才会报错，
	// 但 url.Parse 不校验端口、u.Port() 对非法端口返回空串而非 panic，
	// 故这里补一层显式校验，避免畸形 URL 影响整轮
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	} else if _, err := strconv.Atoi(port); err != nil {
		return target{}
	}
	return target{host: host, port: port}
}

// Prefetcher 域名可达性判定的跨阶段缓存。
//
// 聚合阶段每收完一个引擎，就把它结果的域名丢进 AddURLs（非阻塞，后台探）；
// 敲门阶段 Filter 走同一个 Prefetcher，命中即返回。
//
// 并发语义：**同域名只握手一次**。"正在探"的域名被记录在 inflight，
// 后来的请求等它结束再读结果，不会重复发起连接。
type Prefetcher struct {
	baseCtx context.Context
	timeout time.Duration
	dial    DialFunc
	sem     chan struct{}

	mu       sync.Mutex
	done     map[string]bool
	inflight map[string]chan struct{}
}

// NewPrefetcher 建一个预热器。workers 同时是"预热的并发上限"与"敲门阶段的
// 并发上限"（两者共用同一个信号量，避免叠加起来打爆网络）。
//
// timeout 是单域名握手超时，同时也是**敲门阶段的时长上界**：可达域名实测最慢 287ms，
// 而不可达的域名要吃满本值才判死，32 并发下其余域名 0.3s 内全部完成，整段时长于是
// 被那一个域名钉死在 timeout 上。
func NewPrefetcher(ctx context.Context, timeout time.Duration, workers int, dial DialFunc) *Prefetcher {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	if workers <= 0 {
		workers = 16
	}
	if dial == nil {
		d := &net.Dialer{Timeout: timeout}
		dial = d.DialContext
	}
	return &Prefetcher{
		baseCtx:  ctx,
		timeout:  timeout,
		dial:     dial,
		sem:      make(chan struct{}, workers),
		done:     map[string]bool{},
		inflight: map[string]chan struct{}{},
	}
}

// AddURLs 把一批 URL 的域名加入预热（非阻塞）。已探完/已在探测中的域名跳过。
//
// ⚠️ 这里只做**只读**检查、绝不登记 inflight：inflight 的唯一登记方是
// verdict。若在这里先把 inflight 登记上，随后启动的 verdict 会把这个登记
// 误认为"别人正在探"，于是 `<-ch` 等一个永远不会 close 的 channel ——
// 每个域名泄漏一个 goroutine，敲门阶段直接死锁（实测整轮挂死 4 分钟以上）。
func (p *Prefetcher) AddURLs(urls []string) {
	for _, u := range urls {
		t := targetOf(u)
		if t.host == "" {
			continue
		}
		if p.settled(t.addr()) {
			continue
		}
		go p.verdict(p.baseCtx, t)
	}
}

// settled 只读判断该域名是否已经有结论或在探测中。
func (p *Prefetcher) settled(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.done[key]; ok {
		return true
	}
	_, ok := p.inflight[key]
	return ok
}

// verdict 取域名判定：命中缓存立即返回；在途则等它结束；都没有则自己探。
func (p *Prefetcher) verdict(ctx context.Context, t target) bool {
	key := t.addr()
	for {
		p.mu.Lock()
		if v, ok := p.done[key]; ok {
			p.mu.Unlock()
			return v
		}
		if ch, ok := p.inflight[key]; ok {
			p.mu.Unlock()
			// 等探测者收尾；它一定会把结果写进 done，所以这里重查必有命中
			<-ch
			continue
		}
		ch := make(chan struct{})
		p.inflight[key] = ch
		p.mu.Unlock()

		p.sem <- struct{}{}
		ok := probe(ctx, p.dial, p.timeout, t)
		<-p.sem

		p.mu.Lock()
		p.done[key] = ok
		delete(p.inflight, key)
		close(ch)
		p.mu.Unlock()
		return ok
	}
}

// Filter 按域名敲门，剔除不可达项；保持原有相对顺序。
//
// 域名去重后逐一探测，同域名只探一次（参考实现同口径）——
// 探测粒度是 host 而非完整 URL：同一站点上任意页面可达即视为该 host 可达。
//
// ⚠️ pf **恒非 nil**：调用方（cmdSearch）在聚合阶段就把它建好了，敲门阶段只是复用。
// 曾有过一个 `Options{Timeout, Workers, Dial, Prefetcher}` 参数包，其中 Timeout/Workers
// 只在"现场新建 Prefetcher"那条兜底分支里被读，而该分支**只有单测能走到**（生产路径
// 永远传的是预热好的 Prefetcher），2026-09-23 整体删除。
func Filter(ctx context.Context, items []model.Hit, pf *Prefetcher) Result {
	targets := make([]target, len(items))
	uniq := map[string]target{}
	for i, it := range items {
		t := targetOf(it.URL)
		targets[i] = t
		if t.host != "" {
			uniq[t.addr()] = t
		}
	}

	verdict := make(map[string]bool, len(uniq))
	prefetched := 0
	if len(uniq) > 0 {
		// 敲门阶段的并发同样受 Prefetcher 内部的信号量约束（预热与敲门共用），
		// 这里只需要把域名并发地投进去，剩下的排队由信号量负责。
		var wg sync.WaitGroup
		var mu sync.Mutex
		for k, t := range uniq {
			// 已经探完的直接命中，不计入等待
			pf.mu.Lock()
			_, cached := pf.done[k]
			pf.mu.Unlock()
			if cached {
				prefetched++
			}
			wg.Add(1)
			go func(k string, t target) {
				defer wg.Done()
				ok := pf.verdict(ctx, t)
				mu.Lock()
				verdict[k] = ok
				mu.Unlock()
			}(k, t)
		}
		wg.Wait()
	}

	res := Result{Prefetched: prefetched}
	res.HostsProbed = len(uniq)
	for k, ok := range verdict {
		if ok {
			res.HostsOK++
		} else {
			res.Unreachable = append(res.Unreachable, k)
		}
	}
	sort.Strings(res.Unreachable)

	res.Kept = make([]model.Hit, 0, len(items))
	for i, it := range items {
		t := targets[i]
		if t.host != "" && verdict[t.addr()] {
			res.Kept = append(res.Kept, it)
		} else {
			res.Dropped = append(res.Dropped, it)
		}
	}
	return res
}

// probe 单目标 TCP 握手；握手成功即认为可达。
func probe(ctx context.Context, dial DialFunc, timeout time.Duration, t target) bool {
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := dial(dctx, "tcp", t.addr())
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
