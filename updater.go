package main

// 自动更新：启动时去 GitHub Releases 看有没有新版本，有就下载新程序替换自己并重启。
// 发新版用 ./release.sh v1.2.3 "更新说明"（会把版本号编进程序、上传到 GitHub 发布页）。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Version 由编译参数 -ldflags "-X main.Version=v1.2.3" 写入；本地直接 go build 出来是 dev，不检查更新
var Version = "dev"

const (
	updateRepo     = "QWQ-potato/bug-doodle-game"
	updateUA       = "bug-doodle-game-updater"
	checkTimeout   = 10 * time.Second
	downloadTimout = 5 * time.Minute
)

// updateAssetName 每个平台在发布页上对应的文件名
func updateAssetName() string {
	switch runtime.GOOS {
	case "windows":
		return "bug-doodle-game.exe"
	case "darwin":
		return "bug-doodle-server-mac"
	}
	return ""
}

type ghRelease struct {
	TagName string `json:"tag_name"`
	Body    string `json:"body"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

// parseVersion "v1.2.3" -> [1 2 3]
func parseVersion(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	var out []int
	for _, p := range strings.Split(v, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// isNewerVersion latest 比 current 新就返回 true
func isNewerVersion(latest, current string) bool {
	a, b := parseVersion(latest), parseVersion(current)
	if a == nil || b == nil {
		return false
	}
	for i := 0; i < len(a) || i < len(b); i++ {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// cleanupOldExe 删掉上次更新留下的旧程序（刚重启时旧进程可能还没完全退出，多试几次）
func cleanupOldExe(exePath string) {
	old := exePath + ".old"
	if _, err := os.Stat(old); err != nil {
		return
	}
	go func() {
		for i := 0; i < 20; i++ {
			if err := os.Remove(old); err == nil || os.IsNotExist(err) {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
}

// checkAndUpdate 有新版就下载替换并重启（重启成功不会返回）；任何一步失败都只打日志，继续用当前版本
func checkAndUpdate(exePath string) {
	if Version == "dev" {
		log.Printf("[更新] 本地开发版，不检查更新")
		return
	}
	asset := updateAssetName()
	if asset == "" {
		return
	}
	log.Printf("[更新] 当前版本 %s，正在检查新版本…", Version)

	rel, err := fetchLatestRelease()
	if err != nil {
		log.Printf("[更新] 检查更新失败（不影响使用，继续用当前版本）: %v", err)
		return
	}
	if !isNewerVersion(rel.TagName, Version) {
		log.Printf("[更新] 已经是最新版本")
		return
	}

	var url, sumURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case asset:
			url = a.URL
		case asset + ".sha256":
			sumURL = a.URL
		}
	}
	if url == "" {
		log.Printf("[更新] 发现新版本 %s，但发布页里没有 %s，跳过", rel.TagName, asset)
		return
	}

	log.Printf("[更新] 发现新版本 %s → %s，正在下载…", Version, rel.TagName)
	if notes := strings.TrimSpace(rel.Body); notes != "" {
		log.Printf("[更新] 更新内容：\n%s", notes)
	}
	newPath := exePath + ".new"
	if err := downloadUpdate(url, sumURL, newPath); err != nil {
		os.Remove(newPath)
		log.Printf("[更新] 下载失败（不影响使用，继续用当前版本）: %v", err)
		return
	}

	// Windows 不能覆盖正在运行的 exe，但可以给它改名：旧的改成 .old，新的放到原位置
	oldPath := exePath + ".old"
	os.Remove(oldPath)
	if err := os.Rename(exePath, oldPath); err != nil {
		os.Remove(newPath)
		log.Printf("[更新] 替换程序失败（继续用当前版本）: %v", err)
		return
	}
	if err := os.Rename(newPath, exePath); err != nil {
		os.Rename(oldPath, exePath) // 还原
		os.Remove(newPath)
		log.Printf("[更新] 替换程序失败（继续用当前版本）: %v", err)
		return
	}
	os.Chmod(exePath, 0o755)

	log.Printf("[更新] ✅ 已更新到 %s，正在重启…", rel.TagName)
	if err := restartSelf(exePath); err != nil {
		log.Printf("[更新] 自动重启失败，请手动重新打开程序: %v", err)
		os.Exit(1)
	}
}

func fetchLatestRelease() (*ghRelease, error) {
	client := &http.Client{Timeout: checkTimeout}
	api := "https://api.github.com/repos/" + updateRepo + "/releases/latest"
	if v := os.Getenv("BUG_DOODLE_UPDATE_API"); v != "" { // 仅本地测试更新流程用
		api = v
	}
	req, _ := http.NewRequest("GET", api, nil)
	req.Header.Set("User-Agent", updateUA)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub 返回 %s", resp.Status)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func httpGet(url string, timeout time.Duration) (*http.Response, error) {
	client := &http.Client{Timeout: timeout}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", updateUA)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("下载 %s 返回 %s", filepath.Base(url), resp.Status)
	}
	return resp, nil
}

// downloadUpdate 下载新程序到 dst，有 .sha256 就校验，防止下到一半的坏文件
func downloadUpdate(url, sumURL, dst string) error {
	resp, err := httpGet(url, downloadTimout)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	f.Close()
	if err != nil {
		return err
	}
	if resp.ContentLength > 0 && n != resp.ContentLength {
		return fmt.Errorf("文件没下完整（%d/%d 字节）", n, resp.ContentLength)
	}
	if n < 1<<20 {
		return fmt.Errorf("下载的文件太小（%d 字节），可能不是程序", n)
	}

	if sumURL != "" {
		sr, err := httpGet(sumURL, checkTimeout)
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(io.LimitReader(sr.Body, 1024))
		sr.Body.Close()
		want := strings.Fields(string(b))
		got := hex.EncodeToString(h.Sum(nil))
		if len(want) == 0 || !strings.EqualFold(want[0], got) {
			return fmt.Errorf("文件校验不通过，可能下载损坏")
		}
	}
	return nil
}
