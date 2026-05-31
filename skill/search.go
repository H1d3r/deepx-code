package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// === Clawhub skill 搜索 ===
//
// SearchSkills 跨所有 enabled 源搜(目前只有内置 Clawhub)。各源单独 8s timeout,
// 一个源失败不影响其他源。保留 sourceID 形参以便未来加多源。
//
// Clawhub adapter:GET /api/v1/search?q= → 并发 enrich /api/v1/skills/<slug> 拿
// stars/downloads,最后按 downloads 倒序。

const (
	httpClientTimeout = 8 * time.Second
	httpMaxBodyBytes  = 5 << 20 // 5MB,任何响应体 hardcap
)

var httpClient = &http.Client{Timeout: httpClientTimeout}

func userAgent() string { return "deepx-skill-client" }

// SearchSkills 跨所有 enabled 源搜索。sourceID 非空时只搜该源。
// 单个源失败被吞(返回 results 可能比 enabled sources 少)。
func SearchSkills(ctx context.Context, query, sourceID string) ([]RemoteSkillInfo, error) {
	pool := make([]SkillSource, 0)
	for _, s := range ListSources() {
		if !s.Enabled {
			continue
		}
		if sourceID != "" && s.ID != sourceID {
			continue
		}
		pool = append(pool, s)
	}
	type chunk struct {
		infos []RemoteSkillInfo
	}
	out := make([]chunk, len(pool))
	var wg sync.WaitGroup
	for i, s := range pool {
		wg.Add(1)
		go func(i int, src SkillSource) {
			defer wg.Done()
			if src.Type != "clawhub" {
				return
			}
			if infos, err := clawhubSearch(ctx, src, query); err == nil {
				out[i].infos = infos
			}
		}(i, s)
	}
	wg.Wait()
	var all []RemoteSkillInfo
	for _, c := range out {
		all = append(all, c.infos...)
	}
	return all, nil
}

// ---------- Clawhub adapter ----------

type clawhubSearchResp struct {
	Results []struct {
		Slug        string  `json:"slug"`
		DisplayName string  `json:"displayName"`
		Summary     string  `json:"summary"`
		Version     *string `json:"version"`
		OwnerHandle string  `json:"ownerHandle"`
	} `json:"results"`
}

type clawhubDetailResp struct {
	Skill struct {
		Stats struct {
			Downloads int `json:"downloads"`
			Stars     int `json:"stars"`
		} `json:"stats"`
	} `json:"skill"`
	Owner struct {
		Handle      string `json:"handle"`
		DisplayName string `json:"displayName"`
	} `json:"owner"`
	LatestVersion struct {
		Version string `json:"version"`
	} `json:"latestVersion"`
}

func clawhubSearch(ctx context.Context, src SkillSource, query string) ([]RemoteSkillInfo, error) {
	base := src.URL
	if base == "" {
		base = clawhubBase
	}
	q := query
	if strings.TrimSpace(q) == "" {
		q = "skill"
	}
	u := fmt.Sprintf("%s/api/v1/search?q=%s", strings.TrimRight(base, "/"), url.QueryEscape(q))
	body, err := httpGet(ctx, u, "application/json")
	if err != nil {
		return nil, err
	}
	var resp clawhubSearchResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("Clawhub 搜索响应: %w", err)
	}
	infos := make([]RemoteSkillInfo, 0, len(resp.Results))
	for _, r := range resp.Results {
		info := RemoteSkillInfo{
			Name:        r.DisplayName,
			Description: r.Summary,
			SourceID:    src.ID,
			RemoteRef:   r.Slug,
			Author:      r.OwnerHandle,
		}
		if r.Version != nil {
			info.Version = *r.Version
		}
		if r.OwnerHandle != "" {
			info.URL = fmt.Sprintf("%s/%s/%s", clawhubWeb, r.OwnerHandle, r.Slug)
		}
		infos = append(infos, info)
	}
	// 并发 enrich 详情,失败的留原信息不掉
	var wg sync.WaitGroup
	for i := range infos {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			du := fmt.Sprintf("%s/api/v1/skills/%s",
				strings.TrimRight(base, "/"), url.PathEscape(infos[i].RemoteRef))
			data, err := httpGet(ctx, du, "application/json")
			if err != nil {
				return
			}
			var d clawhubDetailResp
			if err := json.Unmarshal(data, &d); err != nil {
				return
			}
			if infos[i].Version == "" {
				infos[i].Version = d.LatestVersion.Version
			}
			if d.Owner.DisplayName != "" {
				infos[i].Author = d.Owner.DisplayName
			}
			infos[i].Downloads = d.Skill.Stats.Downloads
			infos[i].Stars = d.Skill.Stats.Stars
		}(i)
	}
	wg.Wait()
	sort.SliceStable(infos, func(i, j int) bool {
		return infos[i].Downloads > infos[j].Downloads
	})
	return infos, nil
}

// ---------- shared HTTP ----------

func httpGet(ctx context.Context, urlStr, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent())
	req.Header.Set("Accept", accept)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, httpMaxBodyBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
