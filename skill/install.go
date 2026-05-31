package skill

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// === /skill-add 和 /skill-delete 的纯逻辑层 ===
//
// 设计原则:
//  1. 安装目标永远是 ~/.deepx/skills/<dir>/,不动用户其他工具拥有的 ~/.claude / ~/.agents
//  2. 删除只允许操作 ~/.deepx/skills 下的目录,白名单校验 dir 名,杜绝 ../ 越界
//  3. 安装时不执行任何下载内容,SKILL.md 是 markdown 文本,LLM 读取后自己理解,
//     工具层零代码执行风险

// InstalledDir 返回 ~/.deepx/skills 绝对路径,目录不存在不报错(Install 时会自动创建)。
func InstalledDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("无法定位用户目录: %w", err)
	}
	return filepath.Join(home, ".deepx", "skills"), nil
}

// InstalledList 列出已安装在 ~/.deepx/skills/ 下的 skill 元数据(name 字典序)。
// **跟 Loader.List() 不同 —— 这只看 deepx 自己装的,不混进 ~/.claude / ~/.agents 那些**,
// 给 /skill-delete 防误删别人工具的 skill 用。
func InstalledList() ([]Metadata, error) {
	dir, err := InstalledDir()
	if err != nil {
		return nil, err
	}
	seen := map[string]Metadata{}
	scanDir(dir, "global", seen)
	out := make([]Metadata, 0, len(seen))
	for _, m := range seen {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// safeName 校验 skill 目录名:仅字母数字 + `_-.`,长度 1~64。
// 用于 Delete 时拒绝任何包含 / 或 .. 的输入(防越界 rm)。
var safeName = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

// Delete 删除 ~/.deepx/skills/<name>/ 整个目录。**永不动其他目录**。
// name 必须通过 safeName 校验;skill 不在 ~/.deepx/skills 下时报"不属于 deepx 管理"。
func Delete(name string) error {
	name = strings.TrimSpace(name)
	if !safeName.MatchString(name) {
		return fmt.Errorf("非法 skill 名 %q(仅允许字母数字 . _ -)", name)
	}
	root, err := InstalledDir()
	if err != nil {
		return err
	}
	target := filepath.Join(root, name)
	// 防御性二次校验:Clean 后必须仍在 root 下,否则可能被 .. 钻空
	clean := filepath.Clean(target)
	if !strings.HasPrefix(clean+string(filepath.Separator), root+string(filepath.Separator)) {
		return fmt.Errorf("非法路径(疑似越界):%s", clean)
	}
	if _, err := os.Stat(filepath.Join(target, "SKILL.md")); err != nil {
		return fmt.Errorf("skill %q 不在 ~/.deepx/skills/ 下,deepx 不管理它,无法删除", name)
	}
	return os.RemoveAll(target)
}

// InstallFromSource 按源 ID 装一个 skill。当前只支持内置 Clawhub。
// remoteRef = Clawhub slug,落到 ~/.deepx/skills/<remoteRef>/。
//
// 安全防御:
//   - remoteRef 走 safeName 白名单,杜绝 / 和 ..
//   - Clawhub:ZIP 解压时拒 ..,总文件数 ≤ 256,总解压字节 ≤ 10MB
//   - 整个过程在 staging 目录里做,成功才 rename 到 target,失败 cleanup
//
// 同名已装报错,**不**静默覆盖(用户先 /skill-delete 再装语义清楚)。
func InstallFromSource(ctx context.Context, sourceID, remoteRef string) (string, error) {
	if !safeName.MatchString(remoteRef) {
		return "", fmt.Errorf("非法 skill ref %q(仅允许字母数字 . _ -)", remoteRef)
	}
	var source SkillSource
	for _, s := range ListSources() {
		if s.ID == sourceID && s.Enabled {
			source = s
			break
		}
	}
	if source.ID == "" {
		return "", fmt.Errorf("未找到源 id=%s", sourceID)
	}
	root, err := InstalledDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("创建 %s 失败: %w", root, err)
	}
	target := filepath.Join(root, remoteRef)
	if _, err := os.Stat(target); err == nil {
		return "", fmt.Errorf("skill 目录已存在:%s(请先 /skill-delete %s)", target, remoteRef)
	}
	if source.Type != "clawhub" {
		return "", fmt.Errorf("未知源类型: %s", source.Type)
	}
	return installFromClawhub(ctx, source, remoteRef, root)
}

// 安全限制:防 zip slip / zip bomb。
const (
	maxInstallBytes = 10 * 1024 * 1024 // 10MB
	maxInstallFiles = 256
)

// installFromClawhub 下 ZIP → 防 zip slip/bomb 解压到 staging → atomic rename 到 target。
// SKILL.md 可能在 zip 根或一层子目录,自动剥层落到 ~/.deepx/skills/<remoteRef>/;两者都没就 fail。
func installFromClawhub(ctx context.Context, src SkillSource, remoteRef, root string) (string, error) {
	base := src.URL
	if base == "" {
		base = clawhubBase
	}
	target := filepath.Join(root, remoteRef)
	staging := filepath.Join(root, fmt.Sprintf(".%s.staging-%d-%d",
		remoteRef, os.Getpid(), time.Now().UnixNano()))
	cleanup := func() { _ = os.RemoveAll(staging) }

	ctx2, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	dl := fmt.Sprintf("%s/api/v1/download?slug=%s",
		strings.TrimRight(base, "/"), url.QueryEscape(remoteRef))
	req, err := http.NewRequestWithContext(ctx2, "GET", dl, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent())
	req.Header.Set("Accept", "application/zip")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("Clawhub 下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("Clawhub 下载 HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	// 全文读进内存(zip.NewReader 需要随机访问)。LimitReader 多读 1 字节用来检测超限。
	zipBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxInstallBytes+1))
	if err != nil {
		return "", fmt.Errorf("读取下载: %w", err)
	}
	if len(zipBytes) > maxInstallBytes {
		return "", fmt.Errorf("压缩包超过 %dMB 上限(疑似异常)", maxInstallBytes/1024/1024)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return "", fmt.Errorf("ZIP 解析: %w", err)
	}

	// 找 SKILL.md:zip 根 / 一层子目录;都没就 fail
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, "/") {
			names = append(names, f.Name)
		}
	}
	strip := ""
	hasRoot := false
	for _, n := range names {
		if n == "SKILL.md" {
			hasRoot = true
			break
		}
	}
	if !hasRoot {
		nested := ""
		for _, n := range names {
			parts := strings.Split(n, "/")
			if len(parts) == 2 && parts[1] == "SKILL.md" {
				nested = n
				break
			}
		}
		if nested == "" {
			return "", fmt.Errorf("Clawhub 包里没有 SKILL.md(包含: %s)", strings.Join(names, ", "))
		}
		strip = nested[:strings.Index(nested, "/")+1]
	}

	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", err
	}

	var totalBytes int64
	var fileCount int
	if err := func() error {
		for _, f := range zr.File {
			name := f.Name
			if strip != "" {
				if !strings.HasPrefix(name, strip) {
					continue
				}
				name = name[len(strip):]
			}
			if name == "" || strings.HasSuffix(name, "/") {
				continue
			}
			// 安全:拒 .. 和绝对路径
			if strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
				return fmt.Errorf("ZIP 含可疑路径: %s", f.Name)
			}
			fileCount++
			if fileCount > maxInstallFiles {
				return fmt.Errorf("ZIP 文件数超 %d", maxInstallFiles)
			}
			dst := filepath.Join(staging, name)
			// Clean 后必须仍在 staging 下,防符号链接 / 各种构造越界
			clean := filepath.Clean(dst)
			if !strings.HasPrefix(clean+string(filepath.Separator), staging+string(filepath.Separator)) {
				return fmt.Errorf("ZIP 路径越界: %s", f.Name)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			rc, err := f.Open()
			if err != nil {
				return err
			}
			w, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				rc.Close()
				return err
			}
			remaining := int64(maxInstallBytes) - totalBytes + 1
			n, copyErr := io.Copy(w, io.LimitReader(rc, remaining))
			w.Close()
			rc.Close()
			if copyErr != nil {
				return copyErr
			}
			totalBytes += n
			if totalBytes > maxInstallBytes {
				return fmt.Errorf("解压总大小超 %dMB 上限", maxInstallBytes/1024/1024)
			}
		}
		return nil
	}(); err != nil {
		cleanup()
		return "", err
	}

	if err := os.Rename(staging, target); err != nil {
		cleanup()
		return "", fmt.Errorf("rename staging → target: %w", err)
	}
	return remoteRef, nil
}

// Install 把 src 安装到 ~/.deepx/skills/<dir>/,返回最终的 <dir>(skill 名)。
//
// 支持两种 src:
//   - GitHub URL(https://github.com/<owner>/<repo>(.git)?)→ git clone --depth 1
//   - 本地目录路径(/abs/path 或 ./rel 或 ~/...)→ 递归拷贝
//
// 安装前会查 SKILL.md 是否存在(repo 根或拷贝源根)。同名 skill 已存在则报错,
// 不静默覆盖(用户自己 /skill-delete 后再装,语义最清晰)。
func Install(src string) (string, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return "", fmt.Errorf("source 不能为空")
	}
	root, err := InstalledDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("创建 %s 失败: %w", root, err)
	}

	switch {
	case strings.HasPrefix(src, "https://github.com/"), strings.HasPrefix(src, "http://github.com/"):
		return installFromGitHub(src, root)
	case strings.HasPrefix(src, "/"),
		strings.HasPrefix(src, "./"),
		strings.HasPrefix(src, "../"),
		strings.HasPrefix(src, "~"):
		return installFromLocal(src, root)
	}
	return "", fmt.Errorf("无法识别 source(支持 https://github.com/... 或本地路径):%s", src)
}

// parseGitHubInstallURL 解析两种形式的 GitHub URL:
//
//	https://github.com/<owner>/<repo>                          → 单 skill 仓库,SKILL.md 在根
//	https://github.com/<owner>/<repo>/tree/<branch>/<subpath>  → 多 skill 仓库,装其中 <subpath> 子目录
//
// .git 后缀容忍。返回 cloneURL(剥掉 tree/... 给 git 用)、branch(可空)、subpath(可空)、
// baseName(最终装到的目录名 = subpath 的最后一段,或 repo 名)。
//
// 限制:branch 只取一段(tree/<branch>/...),所以名字带 `/` 的分支没法配子路径。这是
// URL 自身的歧义(github web 也是这样),无解。
func parseGitHubInstallURL(raw string) (cloneURL, branch, subpath, baseName string, err error) {
	s := strings.TrimSuffix(strings.TrimRight(raw, "/"), ".git")
	low := strings.ToLower(s)
	idx := strings.Index(low, "github.com/")
	if idx < 0 {
		return "", "", "", "", fmt.Errorf("不是合法 GitHub URL: %s", raw)
	}
	head := s[:idx+len("github.com/")]
	rest := s[idx+len("github.com/"):]
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", "", fmt.Errorf("不是合法 GitHub URL: %s", raw)
	}
	cloneURL = head + parts[0] + "/" + parts[1]
	baseName = parts[1]
	if len(parts) > 3 && parts[2] == "tree" {
		branch = parts[3]
		if len(parts) > 4 {
			subpath = strings.Join(parts[4:], "/")
			baseName = filepath.Base(subpath)
		}
	} else if len(parts) > 2 && parts[2] == "blob" {
		return "", "", "", "", fmt.Errorf("URL 指向文件而非目录,请改成 /tree/<branch>/<dir> 形式")
	}
	return cloneURL, branch, subpath, baseName, nil
}

// installFromGitHub 从 GitHub 装 skill。两种形式见 parseGitHubInstallURL。
//   - 无 subpath:直接 clone --depth 1 [--branch X] 到 root/<repo>/,根必须有 SKILL.md
//   - 有 subpath:clone 到 /tmp 临时目录 → 校验 subpath 下有 SKILL.md → 拷贝到 root/<basename>/
func installFromGitHub(rawURL, root string) (string, error) {
	cloneURL, branch, subpath, baseName, err := parseGitHubInstallURL(rawURL)
	if err != nil {
		return "", err
	}
	if !safeName.MatchString(baseName) {
		return "", fmt.Errorf("从 URL 推出的目录名 %q 不合法", baseName)
	}
	target := filepath.Join(root, baseName)
	if _, err := os.Stat(target); err == nil {
		return "", fmt.Errorf("skill 目录已存在:%s(请先 /skill-delete %s)", target, baseName)
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("未找到 git 可执行文件,请先安装(macOS:`brew install git`;Ubuntu:`sudo apt install git`)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if subpath == "" {
		// 单 skill 仓库:直接 clone 到 target
		args := []string{"clone", "--depth", "1"}
		if branch != "" {
			args = append(args, "--branch", branch)
		}
		args = append(args, cloneURL, target)
		out, cerr := exec.CommandContext(ctx, "git", args...).CombinedOutput()
		if cerr != nil {
			os.RemoveAll(target) // 半残目录会让下次 install 误判"已存在"
			return "", fmt.Errorf("git clone 失败:%w\n%s", cerr, strings.TrimSpace(string(out)))
		}
		if _, err := os.Stat(filepath.Join(target, "SKILL.md")); err != nil {
			os.RemoveAll(target)
			return "", fmt.Errorf("仓库根目录没有 SKILL.md。如果是多 skill 仓库(例 anthropics/skills),请改 URL 指定子目录:.../tree/<branch>/<skill-dir>")
		}
		return baseName, nil
	}

	// 多 skill 仓库:staging clone → 校验 subpath → 拷到 target
	staging, err := os.MkdirTemp("", "deepx-clone-*")
	if err != nil {
		return "", fmt.Errorf("创建临时目录: %w", err)
	}
	defer os.RemoveAll(staging)
	cloneDir := filepath.Join(staging, "repo")
	args := []string{"clone", "--depth", "1"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, cloneURL, cloneDir)
	out, cerr := exec.CommandContext(ctx, "git", args...).CombinedOutput()
	if cerr != nil {
		return "", fmt.Errorf("git clone 失败:%w\n%s", cerr, strings.TrimSpace(string(out)))
	}
	// 子路径越界防御:Clean 后必须在 cloneDir 下
	skillDir := filepath.Clean(filepath.Join(cloneDir, filepath.FromSlash(subpath)))
	if !strings.HasPrefix(skillDir+string(filepath.Separator), cloneDir+string(filepath.Separator)) {
		return "", fmt.Errorf("子路径越界:%s", subpath)
	}
	if _, err := os.Stat(filepath.Join(skillDir, "SKILL.md")); err != nil {
		return "", fmt.Errorf("仓库子路径 %q 下没有 SKILL.md", subpath)
	}
	if err := copyTree(skillDir, target); err != nil {
		os.RemoveAll(target)
		return "", err
	}
	return baseName, nil
}

// installFromLocal 把 src 目录递归拷贝到 root/<basename>/。
// src 必须是个含 SKILL.md 的目录;同名已装则报错。
func installFromLocal(rawSrc, root string) (string, error) {
	src := rawSrc
	if strings.HasPrefix(src, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("解析 ~ 失败: %w", err)
		}
		src = filepath.Join(home, strings.TrimPrefix(src, "~"))
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		return "", fmt.Errorf("解析路径失败: %w", err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("路径不存在或不可访问:%s", abs)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("只支持目录拷贝(目录里要含 SKILL.md):%s", abs)
	}
	if _, err := os.Stat(filepath.Join(abs, "SKILL.md")); err != nil {
		return "", fmt.Errorf("源目录里没有 SKILL.md:%s", abs)
	}
	name := filepath.Base(abs)
	if !safeName.MatchString(name) {
		return "", fmt.Errorf("目录名 %q 不合法(仅允许字母数字 . _ -)", name)
	}
	target := filepath.Join(root, name)
	if _, err := os.Stat(target); err == nil {
		return "", fmt.Errorf("skill 目录已存在:%s(请先 /skill-delete %s)", target, name)
	}
	if err := copyTree(abs, target); err != nil {
		os.RemoveAll(target)
		return "", err
	}
	return name, nil
}

// copyTree 递归拷贝 src 到 dst。文件保留 0644,目录 0755,符号链接按目标内容跟随。
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(p, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
