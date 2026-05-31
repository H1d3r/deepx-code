package skill

// === Skill 源 ===
//
// 简化版:只内置 Clawhub registry,不暴露 source 增删改 API。
//
// 这里保留 SkillSource 类型 + ListSources() 抽象,是为了:
//   - 后续如果加多源(企业内部 registry / 自托管 Clawhub)只用扩 ListSources()
//   - search/install 内部按 source.Type 分派,代码结构不动
//
// 当前 ListSources() 永远返回 [builtinClawhub] 一条。

const (
	SourceIDClawhub = "clawhub"
	clawhubBase     = "https://wry-manatee-359.convex.site"
	clawhubWeb      = "https://clawhub.ai"
)

// SkillSource 一个 skill 源描述。
type SkillSource struct {
	ID      string
	Name    string
	Type    string // "clawhub"
	URL     string
	Enabled bool
}

// RemoteSkillInfo 搜索结果里的一条 skill。
// RemoteRef:Clawhub 的 slug;安装后落到 ~/.deepx/skills/<RemoteRef>/。
type RemoteSkillInfo struct {
	Name        string
	Description string
	Version     string
	SourceID    string
	RemoteRef   string
	Author      string
	URL         string
	Downloads   int
	Stars       int
}

var builtinClawhub = SkillSource{
	ID:      SourceIDClawhub,
	Name:    "Clawhub",
	Type:    "clawhub",
	URL:     clawhubBase,
	Enabled: true,
}

// ListSources 当前永远是 [builtinClawhub]。
// 保留函数签名给 search/install 迭代用,扩展点留给后续。
func ListSources() []SkillSource {
	return []SkillSource{builtinClawhub}
}
