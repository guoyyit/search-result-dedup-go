# search-result-dedup-go

本地检索结果的**近重复合并**与**多样性重排**服务：对结果集去重、打散，
使同一页面不会反复出现内容高度相似的条目，并在游标翻页过程中保持顺序稳定。

## 功能总览

| 需求 | 实现 |
| --- | --- |
| 近重复识别与合并，阈值可配置，保留被合并 ID 集合 | `internal/similarity`（k-shingle + Jaccard）+ `internal/dedup`（并查集聚簇） |
| 多样性重排，相邻不同簇/不同来源，不丢条目 | `internal/diversity`（确定性分层贪心） |
| 游标分页，同一快照稳定、无重复遗漏，并发更新不跳变 | `internal/service`（不可变快照 + 游标） |
| 增量增/删/覆盖，与合并一致，代表项不抖动 | `internal/index` + `internal/dedup`（插入序代表项、删桥拆分） |
| 崩溃恢复，重放后同查询逐条一致 | `internal/storage`（CRC32 校验的 WAL，尾部截断） |
| 资源上限与确定降级 | `internal/limits`（候选数/重排数/并发数/内容大小/快照数与 TTL） |
| 自动化测试 | 全部包自带 `_test.go`，含 `-race` 并发场景 |

## 包结构

```
internal/
  model/       核心数据类型 Document
  similarity/  文本归一化、k-shingle、FNV-1a 指纹、Jaccard、查询分词
  dedup/       近重复聚簇（并查集）、代表项选择、删除后拆分、覆盖更新
  diversity/   确定性多样性重排
  storage/     追加式 WAL（magic + len + CRC32 + JSON），崩溃尾部处理
  index/       线程安全的增量索引，WAL 落盘与恢复
  limits/      资源上限与默认值
  service/     查询、快照、游标分页、降级、LRU+TTL 快照管理
cmd/dedupd/    最小宿主程序（flag 配置 + 启动自检）
```

## 相似度语义

1. **归一化**：Unicode 小写化；所有非字母/数字字符折叠为一个空格。
2. **Shingle**：把归一化文本按 rune 切成重叠的长度 `K` 滑动窗口（默认
   `K=5`），每个窗口用 FNV-1a (64-bit) 指纹化，得到去重集合。文本短于
   `K` 时整体作为一个 shingle，使短文档仍可比较；纯标点/空文本产生空集。
   基于 rune 切分，对中文等不用空格分词的语言同样有效。
3. **相似度**：两个 shingle 集合的 Jaccard 指数

   ```
   sim(A, B) = |A ∩ B| / |A ∪ B| ∈ [0, 1]
   ```

   两个空集视为相同（1），一空一非空为 0。
4. **聚簇**：`sim >= 阈值` 的两篇文档连边，簇是该近重复图的**连通分量**。
   新文档若同时相似于多个簇，会把这些簇传递性合并；删除"桥接"文档后，
   会在原簇内重算连通分量，簇会正确拆分。

阈值通过 `index.Config.SimilarityThreshold`（0,1] 配置（非正值按 1 处理，
即只合并指纹完全一致的文档）；`ShingleK` 配置窗口大小。

### 代表项稳定性

簇代表项是**最早加入的成员**（插入序，平局按 ID），簇 ID 为
`c:<代表ID>`。因此：

- 新增文档永远不会改变已有簇的代表项；
- 覆盖某成员内容时保留其插入序，只要仍处于同一簇，代表项不变；
- 只有删除代表项本身、或删除导致簇拆分时，代表项才会变化（这是任何
  确定性代表规则下不可避免的最小抖动）。

输出项 `Item.MergedIDs` 给出该代表项所在簇的其余全部成员 ID（升序），
供调用方追溯。

## 多样性重排语义

对簇代表项执行确定性贪心重排：

1. 每个簇维护一个内部按 `分数降序、ID 升序` 的队列；
2. 每个位置在各簇队首中分层挑选：
   - tier 0：与上一条**簇不同**（优先）；
   - tier 1：同簇但**来源不同**（次选）；
   - tier 2：同簇同源（仅在没有别的可选时使用，**保证不丢条目**）；
3. 层内按分数降序、ID 升序。

该排列是输入的纯函数：相同输入逐字节一致。相邻位置保证"不同簇，或至少
不同来源"。

## 分页与一致性模型

- `Search` 把**合并 + 排序 + 重排后的完整排列**冻结为不可变快照，返回首页；
- 游标是不透明字符串，内容为 `{snapshotID, offset}`（base64(JSON)）；
- `ReadPage` 永远从同一快照切片：**不会因为后台并发的增删改而重排、
  重复或遗漏**；
- 快照按 `MaxSnapshots`（LRU）与 `SnapshotTTL` 有界保留；被淘汰或过期后
  使用旧游标确定式返回 `ErrSnapshotExpired`，调用方应从第一页重新开始；
- 检索打分（确定性）：
  - 文本查询：分数 = 查询去重词项命中文档（标题+正文）的比例，
    次键为文档自带 `Score`，末键 ID；
  - 空文本（浏览）：按文档自带 `Score` 降序、ID 升序。

## 崩溃恢复

- 每次 Add/Delete/Upsert 先 `write + fsync` 追加 WAL，然后内存状态才可见；
- WAL 帧格式：`magic "DWL1"(4B) | payload len(4B BE) | CRC32(4B BE) | JSON`；
- 启动时 `Open` 调用 `Recover`：顺序重放已提交帧（`OpAdd` 按 Upsert 语义，
  `OpDelete` 删除），并把崩溃留下的**撕裂尾部截断**，之后的追加仍接在
  完整日志之后；
- 归一化、指纹、阈值比较、代表项选择、重排全部确定性，因此恢复后的索引
  与崩溃前等价，同一查询的输出逐条一致（测试 `TestRecoverySameQueryOutput`
  / `TestRecoveryEquivalence` 保证）。

## 资源上限与降级（`limits.Limits`）

| 配置项 | 默认值 | 超限行为（确定式） |
| --- | --- | --- |
| `MaxCandidates` | 10000 | 按确定性检索顺序扫描到上限即停止，页 `Degraded=true`，warning `candidates_limited` |
| `MaxRerankItems` | 2000 | 超出代表项按分数/ID 截断后再重排（不重排的不返回），warning `rerank_items_limited` |
| `MaxConcurrentQueries` | 64 | 超限立即返回 `ErrTooManyConcurrent`，不排队（无 goroutine 无界增长） |
| `MaxContentBytes` | 1 MiB | 超大文档写入直接失败 `ErrContentTooLarge`，不入索引 |
| `MaxSnapshots` | 1024 | LRU 淘汰最久未用快照，旧游标得到 `ErrSnapshotExpired` |
| `SnapshotTTL` | 30m | 到期快照惰性清理，旧游标得到 `ErrSnapshotExpired` |

所有零值字段经 `Normalize()` 替换为默认值。

## 使用示例

```go
idx, _ := index.Open(index.Config{
    DataDir:             "./data",
    SimilarityThreshold: 0.8,
    ShingleK:            5,
    MaxContentBytes:     1 << 20,
})
defer idx.Close()

idx.Upsert(model.Document{ID: "1", Source: "web", Content: "..."})

svc := service.New(idx, limits.Default())
defer svc.Close()

page, _ := svc.Search(ctx, service.Query{Text: "关键词", PageSize: 20})
for page.NextCursor != "" {
    page, _ = svc.ReadPage(ctx, page.NextCursor, 20)
}
```

## 运行

```bash
go build ./...
go test ./... -race
go run ./cmd/dedupd -data-dir ./data -threshold 0.8 -shingle-k 5
```
