package reach

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhidian-cmd/metasearch_cli/internal/model"
)

func hit(url string) model.Hit {
	return model.Hit{URL: url, Engine: []string{"e"}, Positions: map[string]int{"e": 1}}
}

// TestTargetOfPortByScheme 端口必须按 scheme 取，不能一律 443。
//
// 理由（参考实现踩过的坑）：http 站点未提供 443 时会被误判为不可达而整条剔除。
func TestTargetOfPortByScheme(t *testing.T) {
	cases := []struct {
		url      string
		host     string
		port     string
		wantZero bool
	}{
		{"https://a.com/x", "a.com", "443", false},
		{"http://a.com/x", "a.com", "80", false},
		{"http://a.com:8080/x", "a.com", "8080", false},
		{"https://a.com:8443/x", "a.com", "8443", false},
		{"https://a.com:abc/x", "", "", true}, // 非数字端口 → 视为无法探测
		{"not a url", "", "", true},
	}
	for _, c := range cases {
		got := targetOf(c.url)
		if c.wantZero {
			if got.host != "" {
				t.Errorf("%s: 期望无法解析出探测目标，实际 %+v", c.url, got)
			}
			continue
		}
		if got.host != c.host || got.port != c.port {
			t.Errorf("%s: 期望 %s:%s，实际 %s:%s", c.url, c.host, c.port, got.host, got.port)
		}
	}
}

// fakeDial 按预置的可达集合伪造拨号，并记录每个地址被拨了几次。
type fakeDial struct {
	reachable map[string]bool
	mu        sync.Mutex
	calls     map[string]int
}

func newFakeDial(reachable ...string) *fakeDial {
	d := &fakeDial{reachable: map[string]bool{}, calls: map[string]int{}}
	for _, a := range reachable {
		d.reachable[a] = true
	}
	return d
}

func (d *fakeDial) dial(_ context.Context, _, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.calls[addr]++
	ok := d.reachable[addr]
	d.mu.Unlock()
	if !ok {
		return nil, errors.New("connection refused")
	}
	c1, c2 := net.Pipe()
	_ = c2.Close()
	return c1, nil
}

func (d *fakeDial) callCount(addr string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[addr]
}

// TestFilterKeepsOrderAndDropsUnreachable 编排语义：保持原顺序、剔除不可达、统计正确。
func TestFilterKeepsOrderAndDropsUnreachable(t *testing.T) {
	d := newFakeDial("a.com:443", "c.com:443")
	items := []model.Hit{
		hit("https://a.com/1"),
		hit("https://dead.com/1"), // 不可达 → 剔除
		hit("https://c.com/1"),
		hit("https://a.com/2"), // 同域名，应复用同一判定
	}
	pf := NewPrefetcher(context.Background(), time.Second, 4, d.dial)
	res := Filter(context.Background(), items, pf)

	if len(res.Kept) != 3 {
		t.Fatalf("期望保留 3 条，实际 %d 条: %v", len(res.Kept), urls(res.Kept))
	}
	if len(res.Dropped) != 1 || res.Dropped[0].URL != "https://dead.com/1" {
		t.Errorf("期望剔除 dead.com，实际 %v", urls(res.Dropped))
	}
	// 顺序必须保持：a.com/1、c.com/1、a.com/2
	want := []string{"https://a.com/1", "https://c.com/1", "https://a.com/2"}
	for i, w := range want {
		if res.Kept[i].URL != w {
			t.Errorf("顺序被破坏，第 %d 条期望 %s，实际 %s", i, w, res.Kept[i].URL)
		}
	}
	if res.HostsProbed != 3 || res.HostsOK != 2 {
		t.Errorf("期望探测 3 域名/可达 2，实际 %d/%d", res.HostsProbed, res.HostsOK)
	}
	// 同域名只探一次
	if got := d.callCount("a.com:443"); got != 1 {
		t.Errorf("同域名应只探一次，实际 %d 次", got)
	}
}

// TestFilterDropsUnparsableURL 无法解析出 host 的条目一律剔除（参考实现同口径：
// 敲门阶段拿不到目标就没有保留理由）。
func TestFilterDropsUnparsableURL(t *testing.T) {
	d := newFakeDial()
	items := []model.Hit{hit(""), hit("::not-a-url::"), hit("https://a.com/x")}
	pf := NewPrefetcher(context.Background(), time.Second, 2, d.dial)
	res := Filter(context.Background(), items, pf)
	if len(res.Kept) != 0 {
		t.Errorf("全部不可达时应零保留，实际 %v", urls(res.Kept))
	}
	if res.HostsProbed != 1 { // 只有 a.com 是可解析目标
		t.Errorf("期望只探测 1 个域名，实际 %d", res.HostsProbed)
	}
}

// TestFilterConcurrencyBounded -precheck-workers 必须真的限制并发度。
func TestFilterConcurrencyBounded(t *testing.T) {
	var inflight, peak int64
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		cur := atomic.AddInt64(&inflight, 1)
		for {
			old := atomic.LoadInt64(&peak)
			if cur <= old || atomic.CompareAndSwapInt64(&peak, old, cur) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		atomic.AddInt64(&inflight, -1)
		return nil, errors.New("nope")
	}
	var items []model.Hit
	for i := 0; i < 20; i++ {
		items = append(items, hit("https://h"+string(rune('a'+i))+".com/x"))
	}
	Filter(context.Background(), items, NewPrefetcher(context.Background(), time.Second, 4, dial))
	if got := atomic.LoadInt64(&peak); got > 4 {
		t.Errorf("并发度应被限制在 4，实际峰值 %d", got)
	}
}

// TestFilterRespectsContextCancel 全局熔断后不再空转等待。
func TestFilterRespectsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	items := []model.Hit{hit("https://a.com/x")}
	done := make(chan struct{})
	go func() {
		Filter(ctx, items, NewPrefetcher(ctx, 30*time.Second, 2, dial))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 已取消却仍在等待，说明没有把 ctx 传下去")
	}
}

func urls(hits []model.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.URL)
	}
	return out
}

// TestPrefetchThenFilterNoDeadlock 回归：聚合阶段预热过的域名，敲门阶段必须
// 直接命中缓存并**正常返回**。
//
// 曾经的 bug：AddURLs 先把域名登记进 inflight，随后启动的 verdict 看到这个
// 登记，误以为"别人正在探"，于是 `<-ch` 等一个永远不会 close 的 channel。
// 每个域名泄漏一个 goroutine，敲门阶段彻底死锁（实测整轮挂死 4 分钟以上）。
// 这里给一次显式超时，把死锁变成一条明确的失败信息。
func TestPrefetchThenFilterNoDeadlock(t *testing.T) {
	d := newFakeDial("a.com:443", "b.com:443")
	pf := NewPrefetcher(context.Background(), time.Second, 4, d.dial)
	pf.AddURLs([]string{
		"https://a.com/1",
		"https://b.com/2",
		"https://dead.com/3", // 不可达
	})
	// 预热是异步的，先等它落定再敲门 —— 这样才是在验证"命中缓存"这条路径。
	// （生产里敲门要等最后一个引擎返回，预热早就落定了。）
	waitSettled(t, pf, 3)

	done := make(chan Result, 1)
	go func() {
		done <- Filter(context.Background(), []model.Hit{
			hit("https://a.com/1"),
			hit("https://b.com/2"),
			hit("https://dead.com/3"),
		}, pf)
	}()

	select {
	case res := <-done:
		if len(res.Kept) != 2 {
			t.Fatalf("期望保留 2 条可达，实际 %d 条: %v", len(res.Kept), urls(res.Kept))
		}
		if res.Prefetched != 3 {
			t.Errorf("3 个域名都应命中预热缓存，实际命中 %d", res.Prefetched)
		}
		if len(res.Unreachable) != 1 || res.Unreachable[0] != "dead.com:443" {
			t.Errorf("期望列出 dead.com:443 为不可达，实际 %v", res.Unreachable)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("预热后敲门超时未返回：疑似 inflight 登记与在途等待互相咬死")
	}

	// 预热 + 敲门合计，同域名仍只应握手一次
	for _, addr := range []string{"a.com:443", "b.com:443", "dead.com:443"} {
		if got := d.callCount(addr); got != 1 {
			t.Errorf("%s 应只探一次，实际 %d 次（预热与敲门重复发起了连接）", addr, got)
		}
	}
}

// TestPrefetcherSameHostOnceUnderConcurrency 多个引擎回调里出现同一域名时，
// 预热也绝不能重复握手。
func TestPrefetcherSameHostOnceUnderConcurrency(t *testing.T) {
	d := newFakeDial("same.com:443", "other.com:443")
	pf := NewPrefetcher(context.Background(), time.Second, 8, d.dial)

	// 模拟多个引擎陆续回来，每个都带同一批域名
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pf.AddURLs([]string{"https://same.com/x", "https://other.com/y"})
		}()
	}
	wg.Wait()
	waitSettled(t, pf, 2)

	Filter(context.Background(), []model.Hit{hit("https://same.com/x")}, pf)
	if got := d.callCount("same.com:443"); got != 1 {
		t.Errorf("同域名并发预热应只探一次，实际 %d 次", got)
	}
}

// waitSettled 等预热把至少 n 个域名**落定**（done 里有结论、且没有在途），
// 超时即失败，避免测试挂死。
//
// ⚠️ 判据是"落定"而不是"发起过握手"：probe 返回之后才写 done，只看发起次数会
// 在"握手已发出、结论未写回"的窗口里提前返回，让随后的 Filter 走成"未命中缓存"。
func waitSettled(t *testing.T, pf *Prefetcher, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pf.mu.Lock()
		settled, inflight := len(pf.done), len(pf.inflight)
		pf.mu.Unlock()
		if settled >= n && inflight == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待预热落定超时：期望至少 %d 个域名有结论，实际 %d", n, len(pf.done))
}
