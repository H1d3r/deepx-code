package skill

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 假 Clawhub server:支持 /api/v1/search 和 /api/v1/skills/<slug>。
func fakeClawhubSearchServer(t *testing.T) (*httptest.Server, SkillSource) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		v := "0.1.0"
		results := []map[string]any{}
		if strings.Contains(q, "bash") {
			results = []map[string]any{
				{
					"slug": "bash-debug", "displayName": "bash-debug",
					"summary": "Debug shell scripts", "version": &v, "ownerHandle": "alice",
				},
				{
					"slug": "bash-helper", "displayName": "bash-helper",
					"summary": "Common bash patterns", "version": &v, "ownerHandle": "bob",
				},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	})
	mux.HandleFunc("/api/v1/skills/", func(w http.ResponseWriter, r *http.Request) {
		slug := strings.TrimPrefix(r.URL.Path, "/api/v1/skills/")
		stars, downloads := 5, 10
		if slug == "bash-helper" {
			downloads = 99 // 让 helper 排前面
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"skill": map[string]any{
				"stats": map[string]any{"stars": stars, "downloads": downloads},
			},
			"owner":         map[string]any{"handle": "alice", "displayName": "Alice"},
			"latestVersion": map[string]any{"version": "1.2.3"},
		})
	})
	srv := httptest.NewServer(mux)
	return srv, SkillSource{
		ID: "test-clawhub", Name: "test", Type: "clawhub", URL: srv.URL, Enabled: true,
	}
}

func TestClawhubSearch_ReturnsAndEnrichesAndSorts(t *testing.T) {
	srv, src := fakeClawhubSearchServer(t)
	defer srv.Close()

	got, err := clawhubSearch(context.Background(), src, "bash")
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应有 2 条结果,got %d (%+v)", len(got), got)
	}
	// downloads 倒序:bash-helper(99) 在前
	if got[0].RemoteRef != "bash-helper" {
		t.Errorf("应按 downloads 倒序,but got[0]=%q", got[0].RemoteRef)
	}
	if got[0].Downloads != 99 {
		t.Errorf("downloads enrich 失败:%d", got[0].Downloads)
	}
	if got[0].Stars == 0 {
		t.Errorf("stars enrich 失败:%d", got[0].Stars)
	}
	if got[0].Author != "Alice" {
		t.Errorf("author 应被 owner.displayName 覆盖到 Alice,got %q", got[0].Author)
	}
	// 空查询不报错(给 server 发 q=skill)
	if _, err := clawhubSearch(context.Background(), src, ""); err != nil {
		t.Errorf("空 query 不应报错: %v", err)
	}
}

func TestClawhubSearch_EmptyResults(t *testing.T) {
	srv, src := fakeClawhubSearchServer(t)
	defer srv.Close()

	got, err := clawhubSearch(context.Background(), src, "no-such-thing")
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("无匹配应返回空,got %d", len(got))
	}
}
