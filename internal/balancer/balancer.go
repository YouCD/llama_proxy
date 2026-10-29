// Package balancer 封装后端加权负载均衡（wrr/rr）。
package balancer

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"llama_proxy/internal/config"

	"github.com/fufuok/balancer"
)

type Balancer struct {
	mu       sync.RWMutex
	bal      balancer.Balancer
	backends map[string]*config.BackendConfig
	// models 模型 ID → 后端名 的反查表，用于具体模型 ID 的直连路由
	// （启动时已校验模型 ID 全局唯一，这里每个模型最多对应一个后端）。
	models   map[string]string
	strategy string
	// logger 为 nil 时不打池变更日志（测试默认静默）。
	logger func(format string, args ...any)
	// pools 按标签划分的后端子池：routing 规则的 pool 字段引用标签名，命中规则的
	// 请求只从对应子池中选择（如 "tool_call" 子池即 tags 含 "tool_call" 的后端）。
	pools map[string]balancer.Balancer
	// inflight 每节点在途（已转发、尚未完成）请求计数，用于负载感知选点：
	// 在途最少的节点优先，平局时沿用策略轮换。直连本地模型的流量也经 Enter/Leave
	// 计入，使轮询请求能避开正被直连占用的节点。
	inflight map[string]int
}

func New(backends []config.BackendConfig, strategy string) *Balancer {
	bb := &Balancer{
		backends: make(map[string]*config.BackendConfig),
		strategy: strategy,
	}
	bb.updateBalancer(backends)
	return bb
}

func (bb *Balancer) updateBalancer(backends []config.BackendConfig) {
	bb.mu.Lock()
	defer bb.mu.Unlock()

	bb.backends = make(map[string]*config.BackendConfig, len(backends))
	bb.models = make(map[string]string)
	wNodes := map[string]int{}
	tagNodes := map[string]map[string]int{}

	for i := range backends {
		b := &backends[i]
		bb.backends[b.Name] = b
		wNodes[b.Name] = b.Weight
		if m := strings.TrimSpace(b.Model); m != "" {
			bb.models[m] = b.Name
		}
		for tag := range b.EffectiveTags() {
			if tagNodes[tag] == nil {
				tagNodes[tag] = map[string]int{}
			}
			tagNodes[tag][b.Name] = b.Weight
		}
	}

	bb.bal = newStrategyBalancer(bb.strategy, wNodes)
	bb.pools = make(map[string]balancer.Balancer, len(tagNodes))
	for tag, nodes := range tagNodes {
		bb.pools[tag] = newStrategyBalancer(bb.strategy, nodes)
	}

	// 池每秒同步（本地节点就绪状态变化触发 Update）：保留仍存活节点的在途计数，
	// 只清除已出池节点的计数——整体清零会丢掉正在转发的请求负载。
	if bb.inflight == nil {
		bb.inflight = make(map[string]int)
	}
	for name := range bb.inflight {
		if _, ok := bb.backends[name]; !ok {
			delete(bb.inflight, name)
		}
	}

	bb.emitPoolLog()
}

// SetLogger 注入日志函数：池初始化/变更时输出 strategy 与每个标签池的成员及权重。
func (bb *Balancer) SetLogger(f func(format string, args ...any)) *Balancer {
	bb.logger = f
	return bb
}

// LogPools 主动输出一次当前池摘要（启动时使用，此时尚无 Update 事件）。
func (bb *Balancer) LogPools() {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	bb.emitPoolLogLocked()
}

// emitPoolLog 输出池摘要。调用方需持有写锁（updateBalancer）或读锁（LogPools）。
func (bb *Balancer) emitPoolLog() {
	bb.emitPoolLogLocked()
}

// emitPoolLogLocked 在已持锁状态下输出池摘要；未注入 logger 时静默。
func (bb *Balancer) emitPoolLogLocked() {
	if bb.logger == nil {
		return
	}
	bb.logger("backend pool: %s", bb.poolSummary())
}

// poolSummary 生成确定性的池摘要：strategy，随后是全量池（all）与每个标签池的
// 成员及权重（按名字典序排序）。
func (bb *Balancer) poolSummary() string {
	names := make([]string, 0, len(bb.backends))
	for name := range bb.backends {
		names = append(names, name)
	}
	sort.Strings(names)

	formatMembers := func(has func(*config.BackendConfig) bool) string {
		var m []string
		for _, name := range names {
			b := bb.backends[name]
			if has(b) {
				m = append(m, fmt.Sprintf("%s(w=%d)", name, b.Weight))
			}
		}
		if len(m) == 0 {
			return "(empty)"
		}
		return strings.Join(m, ",")
	}

	parts := make([]string, 0, len(bb.pools)+2)
	parts = append(parts, "strategy="+bb.strategy)
	parts = append(parts, "all="+formatMembers(func(*config.BackendConfig) bool { return true }))
	tags := make([]string, 0, len(bb.pools))
	for tag := range bb.pools {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	for _, tag := range tags {
		parts = append(parts, tag+"="+formatMembers(func(b *config.BackendConfig) bool {
			return b.EffectiveTags()[tag]
		}))
	}
	return strings.Join(parts, " ")
}

// newStrategyBalancer 按策略构造对应的 balancer。
func newStrategyBalancer(strategy string, wNodes map[string]int) balancer.Balancer {
	switch strategy {
	case "swrr":
		return balancer.NewSmoothWeightedRoundRobin(wNodes)
	case "wr":
		return balancer.NewWeightedRand(wNodes)
	case "rr":
		return balancer.NewRoundRobin(sortedNames(wNodes))
	case "random":
		return balancer.NewRandom(sortedNames(wNodes))
	default: // wrr or unknown
		return balancer.NewWeightedRoundRobin(wNodes)
	}
}

// sortedNames 返回按字典序排序的节点名，保证 rr/random 的节点顺序确定。
func sortedNames(m map[string]int) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Enter 登记某节点一条在途请求（转发前调用）；未知节点名（不在池内）忽略。
func (bb *Balancer) Enter(name string) {
	if name == "" {
		return
	}
	bb.mu.Lock()
	defer bb.mu.Unlock()
	if _, ok := bb.backends[name]; ok {
		bb.inflight[name]++
	}
}

// Leave 注销某节点一条在途请求（请求完成/失败后调用）；计数下限为 0。
func (bb *Balancer) Leave(name string) {
	if name == "" {
		return
	}
	bb.mu.Lock()
	defer bb.mu.Unlock()
	if _, ok := bb.backends[name]; ok {
		if bb.inflight[name] > 0 {
			bb.inflight[name]--
		}
	}
}

// InflightCount 返回某节点当前在途请求数（未知节点为 0），供面板/测试观测。
func (bb *Balancer) InflightCount(name string) int {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	return bb.inflight[name]
}

// allEligible 返回全量池节点名集合。
func (bb *Balancer) allEligible() map[string]bool {
	m := make(map[string]bool, len(bb.backends))
	for name := range bb.backends {
		m[name] = true
	}
	return m
}

// selectLeastLoaded 在 elig 集合内做负载感知选点：最小化 在途/权重 比率（整型
// 交叉相乘精确比较，避免浮点噪声破坏平局判定）——权重表达容量，均衡目标是各节点
// 的负载密度（单位权重的在途数）相等，而非绝对在途数相等；全空闲（比率同为 0）时
// tied 为全池，按策略轮换保留 WRR/rr 的加权公平。找到最优比率后，按策略 balancer
// 的轮换在 tied 中选取；若在 maxTries 内未落到 tied 节点（如 tied 节点权重为 0 被
// 轮换跳过），回退到字典序首个 tied 节点保证终止。调用方需持有读锁。
func (bb *Balancer) selectLeastLoaded(bal balancer.Balancer, elig map[string]bool) *config.BackendConfig {
	if bal == nil || len(bb.backends) == 0 {
		return nil
	}
	// 最优比率以 bestNum/bestDen 表示（-1/1 为未设置）；正权重节点优先参与比较，
	// 全为零权重时退化为按绝对在途数比较。
	bestNum, bestDen := -1, 1
	for name := range bb.backends {
		if !elig[name] || bb.backends[name].Weight <= 0 {
			continue
		}
		if bestNum < 0 || bb.inflight[name]*bestDen < bestNum*bb.backends[name].Weight {
			bestNum, bestDen = bb.inflight[name], bb.backends[name].Weight
		}
	}
	if bestNum < 0 {
		bestNum, bestDen = -1, 1
		for name := range bb.backends {
			if !elig[name] {
				continue
			}
			if bestNum < 0 || bb.inflight[name] < bestNum {
				bestNum, bestDen = bb.inflight[name], 1
			}
		}
		if bestNum < 0 {
			return nil
		}
	}
	tied := make(map[string]bool)
	for name := range bb.backends {
		if !elig[name] {
			continue
		}
		w := bb.backends[name].Weight
		if w > 0 {
			if bb.inflight[name]*bestDen == bestNum*w {
				tied[name] = true
			}
		} else if bestDen == 1 && bb.inflight[name] == bestNum {
			tied[name] = true
		}
	}
	// 一个完整策略周期必然覆盖池内全部正权重节点；maxTries 取全池权重和作上界。
	maxTries := 0
	for _, b := range bb.backends {
		if b.Weight > 0 {
			maxTries += b.Weight
		}
	}
	if maxTries < len(bb.backends) {
		maxTries = len(bb.backends)
	}
	for i := 0; i < maxTries; i++ {
		name := bal.Select()
		if name == "" {
			break
		}
		if tied[name] {
			if c, ok := bb.backends[name]; ok {
				return c
			}
		}
	}
	// 回退：字典序首个 tied 节点（处理权重为 0 被轮换跳过的边界）。
	var tiedNames []string
	for name := range tied {
		tiedNames = append(tiedNames, name)
	}
	sort.Strings(tiedNames)
	if len(tiedNames) > 0 {
		return bb.backends[tiedNames[0]]
	}
	return nil
}

// Select 从全量池选在途最少（平局按策略轮换）的后端；无可用后端时返回 nil。
func (bb *Balancer) Select() *config.BackendConfig {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	return bb.selectLeastLoaded(bb.bal, bb.allEligible())
}

// SelectFromTag 从指定标签子池选在途最少（平局按策略轮换）的后端；该标签无后端时返回 nil。
func (bb *Balancer) SelectFromTag(tag string) *config.BackendConfig {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	return bb.selectLeastLoaded(bb.pools[tag], bb.tagEligible(tag, nil))
}

func (bb *Balancer) Update(backends []config.BackendConfig) {
	bb.updateBalancer(backends)
}

func (bb *Balancer) GetBackendByName(name string) *config.BackendConfig {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	return bb.backends[name]
}

// GetBackendByModel 按模型 ID 反查池内后端（具体模型直连路由用）；
// 池内无该模型 ID 的后端时返回 nil。
func (bb *Balancer) GetBackendByModel(modelID string) *config.BackendConfig {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	name, ok := bb.models[strings.TrimSpace(modelID)]
	if !ok {
		return nil
	}
	return bb.backends[name]
}

// Names 返回池内所有后端名（按字典序排序）。
func (bb *Balancer) Names() []string {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	names := make([]string, 0, len(bb.backends))
	for name := range bb.backends {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Len 返回池内后端数量。
func (bb *Balancer) Len() int {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	return len(bb.backends)
}

func (bb *Balancer) GetStrategy() string {
	return strings.TrimSpace(bb.strategy)
}

// tagEligible 返回带 tag 标签且在 exclude 之外的节点名集合（调用方持读锁）。
func (bb *Balancer) tagEligible(tag string, exclude map[string]bool) map[string]bool {
	m := make(map[string]bool)
	for name, c := range bb.backends {
		if c.EffectiveTags()[tag] && !exclude[name] {
			m[name] = true
		}
	}
	return m
}

// excludeEligible 返回全量池中不在 exclude 内的节点名集合（调用方持读锁）。
func (bb *Balancer) excludeEligible(exclude map[string]bool) map[string]bool {
	m := make(map[string]bool, len(bb.backends))
	for name := range bb.backends {
		if !exclude[name] {
			m[name] = true
		}
	}
	return m
}

// SelectExcluding 选取一个不在 exclude 中、在途最少（平局按策略轮换）的后端，用于
// 某后端请求失败后的自动故障转移。exclude 传入已尝试过的后端名；没有可用后端时返回 nil。
func (bb *Balancer) SelectExcluding(exclude map[string]bool) *config.BackendConfig {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	return bb.selectLeastLoaded(bb.bal, bb.excludeEligible(exclude))
}

// SelectFromTagExcluding 与 SelectExcluding 相同，但只在指定标签的子池中选取，
// 用于命中路由规则后的自动故障转移。
func (bb *Balancer) SelectFromTagExcluding(tag string, exclude map[string]bool) *config.BackendConfig {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	return bb.selectLeastLoaded(bb.pools[tag], bb.tagEligible(tag, exclude))
}
