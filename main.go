package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// --- 数据结构定义 ---

// IndexEntry 对应 index.jsonl 中的行
type IndexEntry struct {
	ID           string          `json:"id"`
	RawLyricFile string          `json:"rawLyricFile"`
	MetadataRaw  [][]interface{} `json:"metadata"`
	SearchBlob   string          // 预处理的全文本索引（小写）
}

// SearchResult 对应 API 文档中的搜索结果格式
type SearchResult struct {
	ID           string          `json:"id"`
	RawLyricFile string          `json:"rawLyricFile"`
	Metadata     [][]interface{} `json:"metadata"`
	Platforms    []string        `json:"platforms"`
}

// --- 全局变量 ---

var (
	// 命令行参数
	repoURL      = "https://github.com/Steve-xmh/amll-ttml-db.git"
	noSync       = flag.Bool("no-sync", false, "Disable git sync and use local data only")
	noDownload   = flag.Bool("no-download", false, "Disable the download API")
	inputDataDir = flag.String("data-dir", "lyric-data", "Preferred path to the data directory")
	syncInterval = flag.Duration("interval", 10*time.Minute, "Interval for automatic sync")
	port         = flag.String("port", "43594", "Server port")

	// 内存数据库
	dataStore      = make(map[string][]IndexEntry)
	platformPaths  = make(map[string]string)
	platforms      = []string{"ncm", "qq", "am", "spotify", "raw"}
	actualDataDir  string
	lastUpdateTime time.Time

	// 并发控制
	mu    sync.RWMutex // 保护数据索引
	gitMu sync.Mutex   // 保护 Git 操作

	// 查询缓存
	queryCache     = make(map[string][]SearchResult)
	queryCacheMu   sync.RWMutex
	queryCacheTTL  = 5 * time.Minute
	queryTimestamp = make(map[string]time.Time)
)

// --- 路径嗅探逻辑 ---

func isDataDir(path string) bool {
	indicators := []string{"ncm-lyrics", "qq-lyrics", "metadata"}
	for _, ind := range indicators {
		if info, err := os.Stat(filepath.Join(path, ind)); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

func findValidDataDir() string {
	if isDataDir(*inputDataDir) {
		p, _ := filepath.Abs(*inputDataDir)
		return p
	}
	if isDataDir(".") {
		p, _ := filepath.Abs(".")
		return p
	}
	if isDataDir("..") {
		p, _ := filepath.Abs("..")
		return p
	}
	subDirs := []string{"lyric-data", "amll-ttml-db", "data"}
	for _, sub := range subDirs {
		if isDataDir(sub) {
			p, _ := filepath.Abs(sub)
			return p
		}
	}
	return ""
}

// --- Git 同步与索引加载 ---

func syncRepo() bool {
	if *noSync {
		return false
	}
	gitMu.Lock()
	defer gitMu.Unlock()

	absTarget, _ := filepath.Abs(*inputDataDir)
	if _, err := os.Stat(filepath.Join(absTarget, ".git")); os.IsNotExist(err) {
		log.Printf("Repository not found. Initializing clone to %s...", absTarget)
		cmd := exec.Command("git", "clone", "--depth", "1", repoURL, absTarget)
		if err := cmd.Run(); err != nil {
			log.Printf("Git clone failed: %v", err)
			return false
		}
		return true
	}

	log.Println("Performing incremental update (git pull)...")
	cmd := exec.Command("git", "-C", absTarget, "pull")
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Git pull failed: %v", err)
		return false
	}
	return !strings.Contains(string(output), "Already up to date")
}

func loadMetadata() {
	root := findValidDataDir()
	if root == "" {
		log.Println("Warning: No valid data directory found. API will return empty results.")
		return
	}
	actualDataDir = root

	configs := map[string]string{
		"ncm":     filepath.Join(root, "ncm-lyrics", "index.jsonl"),
		"qq":      filepath.Join(root, "qq-lyrics", "index.jsonl"),
		"am":      filepath.Join(root, "am-lyrics", "index.jsonl"),
		"spotify": filepath.Join(root, "spotify-lyrics", "index.jsonl"),
		"raw":     filepath.Join(root, "metadata", "raw-lyrics-index.jsonl"),
	}

	tempStore := make(map[string][]IndexEntry)
	tempPaths := make(map[string]string)

	for key, path := range configs {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		tempPaths[key] = filepath.Dir(path)
		
		// 优化：预分配容量以减少扩容
		var entries []IndexEntry
		scanner := bufio.NewScanner(file)
		
		// 优化：增大缓冲区以提高读取性能
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)
		
		for scanner.Scan() {
			var entry IndexEntry
			if err := json.Unmarshal(scanner.Bytes(), &entry); err == nil {
				// 预处理 SearchBlob
				var sb strings.Builder
				sb.Grow(len(entry.ID) + len(entry.RawLyricFile) + 256) // 预分配容量
				
				sb.WriteString(strings.ToLower(entry.ID))
				sb.WriteString(" ")
				sb.WriteString(strings.ToLower(entry.RawLyricFile))
				sb.WriteString(" ")
				
				for _, pair := range entry.MetadataRaw {
					if len(pair) >= 2 {
						if values, ok := pair[1].([]interface{}); ok {
							for _, v := range values {
								if s, ok := v.(string); ok {
									sb.WriteString(strings.ToLower(s))
									sb.WriteString(" ")
								}
							}
						}
					}
				}
				entry.SearchBlob = sb.String()
				entries = append(entries, entry)
			}
		}
		file.Close()
		tempStore[key] = entries
	}

	mu.Lock()
	dataStore = tempStore
	platformPaths = tempPaths
	lastUpdateTime = time.Now()
	mu.Unlock()
	
	total := getTotalCount()
	log.Printf("Metadata reloaded. Root: %s, Total entries: %d", actualDataDir, total)
}

func getTotalCount() int {
	count := 0
	for _, v := range dataStore {
		count += len(v)
	}
	return count
}

// --- 查询缓存管理 ---

func getFromCache(query string) ([]SearchResult, bool) {
	queryCacheMu.RLock()
	defer queryCacheMu.RUnlock()
	
	if results, ok := queryCache[query]; ok {
		if time.Since(queryTimestamp[query]) < queryCacheTTL {
			return results, true
		}
	}
	return nil, false
}

func saveToCache(query string, results []SearchResult) {
	queryCacheMu.Lock()
	defer queryCacheMu.Unlock()
	
	queryCache[query] = results
	queryTimestamp[query] = time.Now()
	
	// 清理过期缓存
	if len(queryCache) > 1000 {
		now := time.Now()
		for k, t := range queryTimestamp {
			if now.Sub(t) > queryCacheTTL {
				delete(queryCache, k)
				delete(queryTimestamp, k)
			}
		}
	}
}

func clearCache() {
	queryCacheMu.Lock()
	defer queryCacheMu.Unlock()
	
	queryCache = make(map[string][]SearchResult)
	queryTimestamp = make(map[string]time.Time)
	log.Println("Query cache cleared")
}

// --- 中间件 ---

func Middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next(w, r)
		log.Printf("[%s] %s %s %v", r.Method, r.URL.Path, r.RemoteAddr, time.Since(start))
	}
}

// --- 接口处理器 ---

func statusHandler(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	defer mu.RUnlock()

	stats := make(map[string]int)
	for k, v := range dataStore {
		stats[k] = len(v)
	}

	queryCacheMu.RLock()
	cacheSize := len(queryCache)
	queryCacheMu.RUnlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":           "active",
		"last_update_time": lastUpdateTime.Format("2006-01-02 15:04:05"),
		"total_entries":    getTotalCount(),
		"platform_stats":   stats,
		"repo_url":         repoURL,
		"cache_size":       cacheSize,
	})
}

func searchHandler(w http.ResponseWriter, r *http.Request) {
	// 添加上下文超时控制
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var query string
	var targetPlatforms []string

	if r.Method == http.MethodPost {
		var body struct {
			Query     string   `json:"query"`
			Platforms []string `json:"platforms"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		query = body.Query
		targetPlatforms = body.Platforms
	} else {
		query = r.URL.Query().Get("query")
		targetPlatforms = r.URL.Query()["platforms"]
	}

	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "success", "count": 0, "results": []SearchResult{}})
		return
	}
	if len(targetPlatforms) == 0 {
		targetPlatforms = platforms
	}

	// 尝试从缓存获取
	if cachedResults, ok := getFromCache(query); ok {
		log.Printf("Cache hit for query: %s", query)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "success",
			"count":   len(cachedResults),
			"results": cachedResults,
			"cached":  true,
		})
		return
	}

	// 预分配结果通道容量
	resultChan := make(chan []SearchResult, len(targetPlatforms))
	var wg sync.WaitGroup

	// 并行搜索每个平台
	for _, p := range targetPlatforms {
		wg.Add(1)
		go func(pName string) {
			defer wg.Done()

			// 检查上下文是否已取消
			select {
			case <-ctx.Done():
				resultChan <- []SearchResult{}
				return
			default:
			}

			mu.RLock()
			data := dataStore[pName]
			mu.RUnlock()

			// 预分配结果切片容量（假设匹配率约5-10%）
			estimatedSize := len(data) / 20
			if estimatedSize < 10 {
				estimatedSize = 10
			}
			found := make([]SearchResult, 0, estimatedSize)

			// 使用strings.Index替代strings.Contains以获得更好性能
			for _, entry := range data {
				if strings.Index(entry.SearchBlob, query) >= 0 {
					found = append(found, SearchResult{
						ID:           entry.ID,
						RawLyricFile: entry.RawLyricFile,
						Metadata:     entry.MetadataRaw,
						Platforms:    []string{pName},
					})
				}
			}
			resultChan <- found
		}(p)
	}

	// 等待所有goroutine完成
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	// 超时控制
	select {
	case <-done:
	case <-ctx.Done():
		w.WriteHeader(http.StatusRequestTimeout)
		json.NewEncoder(w).Encode(map[string]string{"error": "Search timeout"})
		return
	}

	close(resultChan)

	// 更高效的结果合并和去重
	// 预分配map容量以减少扩容
	estimatedResults := getTotalCount() / 50
	if estimatedResults < 100 {
		estimatedResults = 100
	}
	finalMap := make(map[string]*SearchResult, estimatedResults)

	for list := range resultChan {
		for i := range list {
			item := &list[i]
			if existing, ok := finalMap[item.RawLyricFile]; ok {
				// 避免重复分配，直接append到existing.Platforms
				existing.Platforms = append(existing.Platforms, item.Platforms...)
			} else {
				finalMap[item.RawLyricFile] = item
			}
		}
	}

	// 预分配最终结果切片
	finalResults := make([]SearchResult, 0, len(finalMap))
	for _, v := range finalMap {
		finalResults = append(finalResults, *v)
	}

	// 保存到缓存
	if len(finalResults) > 0 {
		saveToCache(query, finalResults)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"count":   len(finalResults),
		"results": finalResults,
	})
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	if *noDownload {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "Download API is disabled by server configuration"})
		return
	}

	var platform, musicId, format string
	if r.Method == http.MethodPost {
		var body struct {
			Platform string `json:"platform"`
			MusicID  string `json:"musicId"`
			Format   string `json:"format"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		platform, musicId, format = body.Platform, body.MusicID, body.Format
	} else {
		platform = r.URL.Query().Get("platform")
		musicId = r.URL.Query().Get("musicId")
		format = r.URL.Query().Get("format")
	}

	if format == "" {
		format = "ttml"
	}

	mu.RLock()
	dir, ok := platformPaths[platform]
	mu.RUnlock()

	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid platform"})
		return
	}

	filePath := filepath.Join(dir, musicId+"."+format)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "Lyric file not found"})
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filepath.Base(filePath)))
	http.ServeFile(w, r, filePath)
}

func formatsHandler(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode([]string{"ttml", "lrc", "yrc", "qrc", "lys"})
}

func updateHandler(w http.ResponseWriter, r *http.Request) {
	if *noSync {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "Git sync is disabled by server configuration"})
		return
	}

	updated := syncRepo()
	if updated {
		loadMetadata()
		clearCache() // 清除缓存以使用新数据
		json.NewEncoder(w).Encode(map[string]string{"message": "Update successful and metadata reloaded"})
	} else {
		json.NewEncoder(w).Encode(map[string]string{"message": "Already up to date"})
	}
}

// --- 主程序入口 ---

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags)
	log.Println("Starting AMLL TTML API Server (Optimized)...")

	// 1. 初始化 Git 同步
	if !*noSync {
		syncRepo()
	}

	// 2. 加载元数据
	loadMetadata()

	// 3. 启动定时更新协程
	if !*noSync {
		go func() {
			ticker := time.NewTicker(*syncInterval)
			for range ticker.C {
				if syncRepo() {
					loadMetadata()
					clearCache()
				}
			}
		}()
	}

	// 4. 路由注册
	http.HandleFunc("/api/status", Middleware(statusHandler))
	http.HandleFunc("/api/search", Middleware(searchHandler))
	http.HandleFunc("/api/download", Middleware(downloadHandler))
	http.HandleFunc("/api/formats", Middleware(formatsHandler))
	http.HandleFunc("/api/update", Middleware(updateHandler))

	// 5. 启动服务
	log.Printf("Server is listening on :%s", *port)
	if err := http.ListenAndServe(":"+*port, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}