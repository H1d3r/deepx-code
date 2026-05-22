package tui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// 版本相关常量。repo 写死指向上游,后续 fork 自己跑的话改这里。
const (
	githubRepoOwner = "itmisx"
	githubRepoName  = "deepx-code"
	upgradeCheckTTL = 6 * time.Hour // 缓存 6 小时,避免频繁打 GitHub API
)

// upgradeCheckResult 是版本检查结果,goroutine 完成后通过 tea.Msg 发回主模型。
type upgradeCheckResult struct {
	LatestVersion string // 最新发布的 tag(去掉 v 前缀,如 "0.2.0")
	URL           string // release 页 URL,给"去查看"用
	Err           error  // 网络 / API 失败时非 nil,model 视为"未知"忽略掉
}

// checkForUpgradeCmd 返回一个 tea.Cmd 在后台异步检查新版本,完成后发 upgradeCheckResult。
// 缓存(~/.deepx/meta.json)在 TTL 内且 latest 仍比当前新时直接复用,否则打 GitHub API。
// 失败(timeout / 4xx / 5xx)静默,不弹错误。
//
// "缓存 latest ≤ currentVersion" 视同过期重拉 —— 既然当前已经 ≥ 缓存里那个,缓存给不了
// "是不是有更新版"的信息;不重拉的话发了新版用户重启也不会被提醒,得等 6 小时 TTL。
func checkForUpgradeCmd(currentVersion string) tea.Cmd {
	return func() tea.Msg {
		m := metaGet()
		fresh := !m.UpgradeCheckedAt.IsZero() && time.Since(m.UpgradeCheckedAt) <= upgradeCheckTTL
		if fresh && versionNewer(m.LatestVersion, currentVersion) {
			return upgradeCheckResult{LatestVersion: m.LatestVersion, URL: m.UpgradeURL}
		}
		ver, url, err := fetchLatestRelease()
		if err != nil {
			return upgradeCheckResult{Err: err}
		}
		metaUpdate(func(m *meta) {
			m.UpgradeCheckedAt = time.Now()
			m.LatestVersion = ver
			m.UpgradeURL = url
		})
		return upgradeCheckResult{LatestVersion: ver, URL: url}
	}
}

// fetchLatestRelease 打 GitHub Releases API 拿最新 tag。3s 超时避免拖累启动。
func fetchLatestRelease() (string, string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", githubRepoOwner, githubRepoName)
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "deepx-upgrade-check")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("github api status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", "", err
	}
	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return "", "", err
	}
	return strings.TrimPrefix(rel.TagName, "v"), rel.HTMLURL, nil
}

// versionNewer 比较两个语义化版本字符串(已去 v 前缀,允许后缀 -rc1 / -beta 等)。
// latest > current 返回 true。pre-release 后缀走字符串比较,够用即可,不严格遵循 semver。
func versionNewer(latest, current string) bool {
	if latest == "" || current == "" || current == "dev" {
		return false
	}
	la, lpre := splitVersion(latest)
	ca, cpre := splitVersion(current)
	for i := 0; i < 3; i++ {
		var lv, cv int
		if i < len(la) {
			lv = la[i]
		}
		if i < len(ca) {
			cv = ca[i]
		}
		if lv != cv {
			return lv > cv
		}
	}
	// 主版本相同:无 pre-release 视为更"新"(0.1.0 > 0.1.0-rc1)
	if lpre == "" && cpre != "" {
		return true
	}
	if lpre != "" && cpre == "" {
		return false
	}
	return lpre > cpre
}

func splitVersion(v string) (nums []int, pre string) {
	if idx := strings.IndexAny(v, "-+"); idx >= 0 {
		pre = v[idx+1:]
		v = v[:idx]
	}
	for _, s := range strings.Split(v, ".") {
		n, _ := strconv.Atoi(s)
		nums = append(nums, n)
	}
	return
}
