package codegraph

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// Status 是索引的生命周期状态,给状态栏展示用。用 atomic 存,渲染线程可无锁读取。
type Status int32

const (
	StatusIdle    Status = iota // 未构建(还没查询过)
	StatusLoading               // 加载:正在遍历解析
	StatusReady                 // 就绪:已构建且最新
	StatusStale                 // 更新:文件已变、缓存失效,待下次查询重建
)

// Token 返回稳定的状态标识串,供 i18n / 上层映射(不直接耦合渲染)。
func (s Status) Token() string {
	switch s {
	case StatusLoading:
		return "loading"
	case StatusReady:
		return "ready"
	case StatusStale:
		return "stale"
	default:
		return "idle"
	}
}

// 遍历时跳过的目录名(版本控制 / 依赖 / 构建产物 / deepx 自身数据)。
var skipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "vendor": true, ".deepx": true,
	"dist": true, "build": true, "target": true, ".next": true,
}

// 单文件大小上限:超过视为生成 / 压缩产物,跳过,避免拖慢索引。
const maxFileSize = 1 << 20 // 1 MiB

// Index 持有某个 workspace 根的图谱,懒构建 + 进程内缓存。Reindex 可强制重建。
//
// 两段式:Graph() 同步出"快图"(语法层,瞬间可用,Go 调用边是近似版);随后异步在后台跑
// go/types 精确解析(慢,~秒级,要类型检查依赖树),跑完原子换上"精确图"(Go 同名方法不再合并)。
// 精确结果按 Go 文件签名缓存:只在 .go 真变了才重算,非 Go 编辑直接复用,避免反复付那份开销。
// 内存态、单进程;落盘增量索引留作后续优化。
type Index struct {
	root string
	mu   sync.Mutex
	g    *Graph
	st   atomic.Int32 // Status,无锁读供状态栏用

	gPrecise       bool   // 当前 ix.g 是否已是精确图
	goSig          string // ix.g 精确图对应的 Go 文件签名
	cachedPrecise  []Edge // 上次算出的精确 Go 调用边(按 cachedSig 缓存)
	cachedSig      string
	preciseRunning bool // 后台精确解析是否在跑(避免并发重复)
}

// Status 返回当前索引状态(无锁,供 TUI 每帧读取)。
func (ix *Index) Status() Status { return Status(ix.st.Load()) }

func (ix *Index) setStatus(s Status) { ix.st.Store(int32(s)) }

// NewIndex 创建绑定到 root(workspace 绝对路径)的索引,此时还未构建。
func NewIndex(root string) *Index { return &Index{root: root} }

// Prewarm 后台预热:开机只建"快图"(语法层,便宜),不阻塞调用方。
// 刻意不在此跑精确解析(go/packages 较重、可能联网)—— 那留到模型真正调用 CodeGraph 时
// 才后台升级,避免"用户根本没用代码图谱却在每次启动后台跑 go list"的浪费。
func (ix *Index) Prewarm() {
	ix.setStatus(StatusLoading)
	go func() {
		ix.mu.Lock()
		if ix.g == nil {
			g, err := ix.assemble(nil, false)
			if err != nil {
				ix.mu.Unlock()
				ix.setStatus(StatusIdle)
				return
			}
			ix.g = g
			ix.gPrecise = false
		}
		ix.mu.Unlock()
		ix.setStatus(StatusReady)
	}()
}

// Graph 返回图谱(快图,首次调用时构建并缓存),并在后台异步把 Go 调用边升级为精确版。
func (ix *Index) Graph() (*Graph, error) {
	ix.mu.Lock()
	if ix.g != nil {
		g := ix.g
		ix.mu.Unlock()
		ix.maybePrecise()
		return g, nil
	}
	g, err := ix.assemble(nil, false) // 快图:语法近似
	if err != nil {
		ix.mu.Unlock()
		ix.setStatus(StatusIdle)
		return nil, err
	}
	ix.g = g
	ix.gPrecise = false
	ix.mu.Unlock()
	ix.setStatus(StatusReady)
	ix.maybePrecise()
	return g, nil
}

// Reindex 丢弃缓存并立即重建快图,返回符号数(精确升级仍走后台)。
func (ix *Index) Reindex() (int, error) {
	ix.mu.Lock()
	g, err := ix.assemble(nil, false)
	if err != nil {
		ix.mu.Unlock()
		return 0, err
	}
	ix.g = g
	ix.gPrecise = false
	n := len(g.Symbols)
	ix.mu.Unlock()
	ix.setStatus(StatusReady)
	ix.maybePrecise()
	return n, nil
}

// Invalidate 标记缓存失效,下次 Graph() 时重建。供文件被编辑后调用(增量刷新的简化版)。
// 只有"已就绪"的图谱被改动才降级成"待更新";从没构建过(idle)的维持 idle —— 否则
// 模型还没用过图谱、只是改了个文件,状态就会错误地跳成"更新"。
func (ix *Index) Invalidate() {
	ix.mu.Lock()
	ix.g = nil
	ix.gPrecise = false
	ix.mu.Unlock()
	ix.st.CompareAndSwap(int32(StatusReady), int32(StatusStale))
}

// maybePrecise 在后台把当前快图升级成精确图(若有 Go 代码且尚未精确)。
// 命中 Go 签名缓存就复用,否则跑 go/types(慢);全程不阻塞调用方,跑完原子换图。
func (ix *Index) maybePrecise() {
	sig := ix.goSignature()
	if sig == "" {
		return // 没有 Go 文件,免跑
	}
	ix.mu.Lock()
	if ix.preciseRunning || (ix.gPrecise && ix.goSig == sig) {
		ix.mu.Unlock()
		return
	}
	ix.preciseRunning = true
	reuse := ix.cachedSig == sig && ix.cachedPrecise != nil
	cached := ix.cachedPrecise
	ix.mu.Unlock()

	go func() {
		edges, ok := cached, reuse
		if !ok {
			edges, ok = goPreciseCallEdges(ix.root) // 慢:类型检查依赖树
		}
		if ok {
			if g2, err := ix.assemble(edges, true); err == nil {
				ix.mu.Lock()
				ix.g = g2
				ix.gPrecise = true
				ix.goSig = sig
				ix.cachedPrecise = edges
				ix.cachedSig = sig
				ix.preciseRunning = false
				ix.mu.Unlock()
				return
			}
		}
		ix.mu.Lock()
		ix.preciseRunning = false
		ix.mu.Unlock()
	}()
}

// assemble 遍历 workspace 构图。usePrecise=true 时 Go 调用边用传入的精确边,否则用语法近似边。
func (ix *Index) assemble(preciseGoCalls []Edge, usePrecise bool) (*Graph, error) {
	g := newGraph()
	var goApproxCalls []Edge // 语法近似的 Go 调用边(快图用)
	err := filepath.WalkDir(ix.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 单个条目出错就跳过,别中断整次遍历
		}
		name := d.Name()
		if d.IsDir() {
			if path != ix.root && (skipDirs[name] || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		p := parserFor(path)
		if p == nil {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxFileSize {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(ix.root, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		res, perr := p.Parse(rel, src)
		if perr != nil {
			return nil // 坏文件跳过,不污染整体
		}
		for _, s := range res.Symbols {
			g.addSymbol(s)
		}
		for _, r := range res.Refs {
			g.addRef(r)
		}
		for _, e := range res.Edges {
			if usePrecise && e.Kind == EdgeCall && strings.HasSuffix(e.File, ".go") {
				continue // 精确模式下丢弃 Go 语法近似边,改用传入的精确边
			}
			if !usePrecise && e.Kind == EdgeCall && strings.HasSuffix(e.File, ".go") {
				goApproxCalls = append(goApproxCalls, e)
				continue
			}
			g.addEdge(e)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if usePrecise {
		for _, e := range preciseGoCalls {
			g.addEdge(e)
		}
	} else {
		for _, e := range goApproxCalls {
			g.addEdge(e)
		}
	}
	return g, nil
}

// goSignature 扫 workspace 里所有 .go 文件的大小+修改时间,拼成签名;Go 没变签名就不变。
// 只 stat 不读内容,便宜。
func (ix *Index) goSignature() string {
	var b strings.Builder
	_ = filepath.WalkDir(ix.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != ix.root && (skipDirs[name] || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			if info, e := d.Info(); e == nil {
				fmt.Fprintf(&b, "%s:%d:%d;", path, info.Size(), info.ModTime().UnixNano())
			}
		}
		return nil
	})
	return b.String()
}
