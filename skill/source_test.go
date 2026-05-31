package skill

import "testing"

// 当前 ListSources 永远只有内置 Clawhub。
// 这个测试是个回归 guard:以后如果有人重新加 source 管理,会破这个断言提醒留意。
func TestListSources_OnlyBuiltinClawhub(t *testing.T) {
	got := ListSources()
	if len(got) != 1 {
		t.Fatalf("应只有一条 source(内置 Clawhub),got %d: %+v", len(got), got)
	}
	c := got[0]
	if c.ID != SourceIDClawhub {
		t.Errorf("ID 应是 %q,got %q", SourceIDClawhub, c.ID)
	}
	if c.Type != "clawhub" {
		t.Errorf("Type 应是 clawhub,got %q", c.Type)
	}
	if c.URL == "" {
		t.Errorf("URL 不应为空")
	}
	if !c.Enabled {
		t.Errorf("内置 Clawhub 应 Enabled")
	}
}
