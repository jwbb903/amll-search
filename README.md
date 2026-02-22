# AMLL TTML API 服务器

AMLL TTML API 服务器是为 [amll-ttml-db](https://github.com/Steve-xmh/amll-ttml-db) 数据仓库设计的高性能搜索与下载服务端。它提供歌词元数据的快速检索以及对应格式歌词文件的下载功能。

## 特性

- **快速全文检索**：基于预处理文本索引，实现毫秒级响应。
- **多平台支持**：支持 `ncm`、`qq`、`am`、`spotify`、`raw` 五种平台的歌词元数据。
- **智能缓存**：搜索结果缓存 5 分钟，相同查询直接命中，显著提升响应速度。
- **自动同步**：定时从 GitHub 拉取最新数据，无需手动干预。
- **并行搜索**：多平台并发查询，结果合并去重后返回。
- **下载 API**：支持获取 TTML、LRC、YRC、QRC、LYS 等格式的原始歌词文件（可配置禁用）。
- **状态监控**：实时查看各平台条目数、上次更新时间、缓存大小等信息。

## 快速开始

### 环境要求

- Go 1.16 或更高版本

### 安装与运行

```bash
# 克隆仓库（如果有源码）
git clone https://github.com/yourname/amll-api.git
cd amll-api

# 编译
go build -o amll-api main.go

# 运行
./amll-api
```

默认会在 `43594` 端口启动服务，数据目录会自动探测（优先使用当前目录下的 `lyric-data`，若不存在则从 GitHub 克隆）。

## 命令行参数

| 参数 | 默认值 | 说明 |
| :--- | :--- | :--- |
| `-no-sync` | `false` | 禁止 Git 同步，仅使用本地已有数据 |
| `-no-download` | `false` | 禁用 `/api/download` 接口 |
| `-data-dir` | `lyric-data` | 指定数据目录路径（绝对或相对） |
| `-interval` | `10m` | 自动同步间隔，例如 `30s`、`5m`、`1h` |
| `-port` | `43594` | 服务监听端口 |

**示例：**

```bash
# 使用自定义数据目录，关闭自动同步，端口 8080
./amll-api -data-dir /mnt/data/amll -no-sync -port 8080
```

## API 文档

所有接口返回 JSON 格式，并支持跨域请求（CORS）。

### 基础 URL

```
http://<服务器地址>:<端口>
```
例如：`http://localhost:43594`

---

### 1. 状态查询

**端点**：`GET /api/status`

返回服务器状态、各平台条目数、上次更新时间等。

**响应示例**：

```json
{
  "status": "active",
  "last_update_time": "2025-03-20 15:04:05",
  "total_entries": 123456,
  "platform_stats": {
    "ncm": 50000,
    "qq": 40000,
    "am": 15000,
    "spotify": 10000,
    "raw": 8456
  },
  "repo_url": "https://github.com/Steve-xmh/amll-ttml-db.git",
  "cache_size": 128
}
```

---

### 2. 搜索歌词

**端点**：`GET /api/search` 或 `POST /api/search`

**查询参数 (GET)**：

- `query`：搜索关键词（必填）
- `platforms`：限定平台，可重复。例如 `platforms=ncm&platforms=qq`（不传则搜索全部）

**请求体 (POST)**：

```json
{
  "query": "周杰伦",
  "platforms": ["ncm", "qq"]
}
```

**响应**：

```json
{
  "status": "success",
  "count": 2,
  "results": [
    {
      "id": "12345",
      "rawLyricFile": "七里香.lrc",
      "metadata": [["artist", ["周杰伦"]], ["title", ["七里香"]]],
      "platforms": ["ncm", "qq"]
    }
  ],
  "cached": false
}
```

> **注意**：搜索基于 ID、文件名和元数据文本进行全小写模糊匹配。`platforms` 字段表示该歌曲在哪些平台存在匹配。

---

### 3. 下载歌词文件

**端点**：`GET /api/download` 或 `POST /api/download`

*如果服务器启动时添加了 `-no-download` 参数，此接口将返回 403。*

**参数 (GET)**：

- `platform`：平台名（如 `ncm`）
- `musicId`：歌曲 ID（例如 `12345`）
- `format`：文件格式，可选 `ttml`, `lrc`, `yrc`, `qrc`, `lys`，默认 `ttml`

**请求体 (POST)**：

```json
{
  "platform": "ncm",
  "musicId": "12345",
  "format": "lrc"
}
```

**成功响应**：直接返回文件内容（`application/octet-stream`）。

**失败响应 (JSON)**：

```json
{ "error": "Lyric file not found" }
```

---

### 4. 获取支持的格式列表

**端点**：`GET /api/formats`

返回所有可下载的歌词格式。

**响应**：

```json
["ttml", "lrc", "yrc", "qrc", "lys"]
```

---

### 5. 手动触发更新

**端点**：`GET /api/update` 或 `POST /api/update`

*如果启用了 `-no-sync`，此接口返回 403。*

执行 `git pull` 并重新加载索引。

**响应**：

```json
{ "message": "Update successful and metadata reloaded" }
```
或
```json
{ "message": "Already up to date" }
```

## 缓存机制

- **查询缓存**：相同关键词的搜索结果会缓存 5 分钟，减少重复计算。
- **缓存大小限制**：超过 1000 条时会自动清理过期条目。
- **数据更新后**：自动清空缓存，确保搜索使用最新数据。

## 数据目录结构

服务会按以下优先级查找数据目录：

1. 命令行 `-data-dir` 指定的路径
2. 当前工作目录
3. 上级目录
4. 子目录 `lyric-data`、`amll-ttml-db`、`data`

数据目录应包含类似如下的子目录：

```text
lyric-data/
├── ncm-lyrics/
│   ├── index.jsonl
│   └── 歌曲ID.ttml
├── qq-lyrics/
├── am-lyrics/
├── spotify-lyrics/
└── metadata/
    └── raw-lyrics-index.jsonl
```

## 贡献

欢迎提交 Issue 或 Pull Request。
本项目旨在为 AMLL 社区提供便捷的歌词搜索服务，感谢 [Steve-xmh](https://github.com/Steve-xmh) 维护的数据仓库。

## 许可证

本项目采用 **MIT 许可证**。
你可以自由使用、修改和分发本软件，但需保留版权声明和许可声明。详细条款请参见项目根目录下的 `LICENSE` 文件。