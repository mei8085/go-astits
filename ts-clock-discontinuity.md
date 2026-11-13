# MPEG-TS 时钟契约分析：PCR 与 PTS/DTS 跨回绕及流中断

本文基于 go-astits 当前代码（demuxer.go / muxer.go / packet.go / data_pes.go /
clock_reference.go / packet_pool.go / cmd/astits-es-split）分析解复用与复用两端
在时钟回绕（wraparound）和显式不连续（discontinuity）下的时间契约。

## 1. 代码中的时间表示事实

### 1.1 统一的时间结构，不统一的位宽

所有时间值都解析为 `ClockReference`（clock_reference.go:9）：

```go
type ClockReference struct {
    Base, Extension int64
}
```

但三类时间戳的有效位宽不同：

| 时间戳 | Base | Extension | 解析位置 |
|---|---|---|---|
| PCR / OPCR | 33 bit（90 kHz） | 9 bit（27 MHz） | packet.go:314 `parsePCR` |
| PTS / DTS | 33 bit（90 kHz） | 恒为 0 | data_pes.go:412 `parsePTSOrDTS` |
| ESCR | 33 bit + 9 bit | 同 PCR | data_pes.go `parseESCR` |

回绕周期为 2^33 / 90000 ≈ 95443.7 s ≈ 26.5 小时。PCR 的 9 bit 扩展把分辨率
提到 27 MHz，但**不改变回绕周期**——回绕只发生在 33 bit base 上。

### 1.2 解复用端：逐位保真，零状态

`parsePCR` 和 `parsePTSOrDTS` 只做位拼接，把线上值原样放进 int64。Demuxer
不维护任何每 PID 的 PCR 历史、不做 unwrap、不做抖动修正、不校验 PCR 与
PTS/DTS 的一致性。`PacketAdaptationField.DiscontinuityIndicator`
（packet.go:57）被解析和透传，但**语义完全留给调用方**。

唯一的隐式时钟相关行为在 packet_pool.go:26：检测到 continuity counter 不连续
时清空该 PID 的半组装队列。注意 `hasDiscontinuity`（packet_pool.go:130）对
`DiscontinuityIndicator == true` 的包显式返回 false，即规范意义上"接下来的
CC/PCR 可以不连续"的包不会触发清队列——这是正确的，但副作用是：CC 真的跳变
时，已缓存的半个 PES 被**静默丢弃**，不向上层报错，其 PTS 随之丢失。

### 1.3 复用端：隐式取模，零校验

`writePCR`（packet.go:386）写 `Base` 的低 33 位、`Extension` 的低 9 位；
`writePTSOrDTS` 同理写 33 位。底层 `astikit.BitsWriter.writeBitsN`
（binary.go:200）先执行 `toWrite &= ^uint64(0) >> (64 - n)`，即**静默截断到
低 n 位，等价于 mod 2^33 / mod 512**，不报错。

`Muxer.WriteData`（muxer.go:186）不生成 PCR、不重写 PTS/DTS、不检查 PCR 与
PES 时间戳是否同属一个时基。`generatePMT`（muxer.go:404）只校验 PCRPID 是
某个已注册的 ES PID。时间戳的正确性完全是调用方的责任。

cmd/astits-es-split/main.go:202-220 是调用方责任的现实样本：它丢弃原 PCR，
用 `PCR = PTS`（无 DTS 时）或 `PCR = DTS`（有 DTS 时）合成新 PCR。这隐含两个
假设：PTS/DTS 与 PCR 同属一个 90 kHz 时基（同一 program 内成立）；27 MHz
的 extension 可以丢弃（引入 ≤ 1/90000 s ≈ 11 µs 的量化误差，对解码器
PLL 可接受，但对需要 PCR 精度的抖动测量不可接受）。

### 1.4 Duration()/Time() 的语义边界

`ClockReference.Duration()`（clock_reference.go:23）把模 2^33 的值线性映射为
`time.Duration`。base 最大约 8.59e9，`base*1e9/90000` ≈ 9.5e16 ns，远在
int64 上限（约 292 年）之内，不会溢出；但结果是**环上的位置，不是单调时间**。
`Time()` 把它当作 Unix epoch 起的绝对时间，仅适合展示，不适合跨回绕比较。

## 2. 时间契约：两端各自承诺什么

综合代码行为，这个库的实际契约是：

1. **值域契约**：时间戳是 [0, 2^33) 环上的不透明整数（PCR 另有 [0, 512) 的
   extension，规范要求 ≤ 299，代码不校验）。Demuxer 保证保真，Muxer 保证
   取模后保真。两端在"模 2^33 等价类"上达成一致。
2. **时基契约**：同一 program 内 PCR、PTS、DTS 共享同一 90 kHz 时基；不同
   program（不同 PCR PID）可以有独立时基。库不建立也不检查这个关联——
   `streamMap` 只记录 ES PID → program number，调用方须自己通过 PMT 的
   PCRPID 把 ES 归到正确的时钟域。
3. **顺序契约**：库不保证 PCR 与 PTS/DTS 的任何交错顺序。PCR 在 adaptation
   field 中按包到达顺序出现，PTS/DTS 在 PES 首包出现；B 帧重排使 PES 按
   解码序（DTS 序）到达而 PTS 乱序；PCR 与其"对应"PES 的先后由编码器
   复用策略决定。任何基于"先看到 PCR 再看到 PTS"的假设都是调用方自己
   引入的，库不背书。
4. **不连续契约**：`DiscontinuityIndicator` 和 CC 跳变被如实暴露/触发清队，
   但"之后的时间戳属于新时基"这一语义由调用方解释。

## 3. 原始值何时必须保留

**透传、remux、转封装场景必须保留原始模值**，原因是模算术的自洽性：

对任意两个同环时间戳 t1、t2，只要真实间隔 < 2^32（半环，约 13.2 小时），

```
(t2 - t1) mod 2^33
```

在回绕点两侧给出相同的有符号差。音视频 PTS 都在同一个 33 bit 环上，回绕对
两路**同时**发生，唇同步差 `(vPTS - aPTS) mod 2^33` 在回绕点连续。PCR 与
PTS 的差同理。也就是说：**不连续的是数值，连续的是差值**——只要所有比较都
用模差，原始值就是完整信息，任何改写都是纯风险。

Demuxer 的现状（不 unwrap）和 Muxer 的现状（隐式取模）恰好构成闭环：即使
调用方在中间做了 unwrap 得到 > 2^33 的 int64，喂回 Muxer 时写出的低 33 位
仍与原始值模等价。这是一个相当宽容的契约——但它宽容的前提是调用方**不在
不同流之间使用不一致的 unwrap 状态**（见下节）。

## 4. 连续化何时破坏音画关系

"连续化"（unwrap：给回绕后的值加 2^33 的整数倍，得到单调时间轴）本身合法，
破坏音画的是**不一致的连续化**：

1. **按流独立 unwrap**。视频 PTS 先回绕、音频 PTS 后回绕（两路回绕时刻相同
   但各自状态机检测到回绕的时机依赖各自下一个时间戳的到达），若视频已加
   2^33 而音频未加，唇同步差瞬间错开 26.5 小时。安全的做法是以 program
   （PCR PID）为域维护**一个**偏移量，域内所有流共享。
2. **跨 discontinuity 沿用旧偏移**。`discontinuity_indicator = 1` 之后时基
   允许任意跳变（规范只约束同一 program 内 PCR 与 PTS/DTS 一起跳）。旧的
   unwrap 偏移对新时基无意义，继续套用会把新值平移到错误位置。偏移状态机
   必须在 discontinuity 边界重置。
3. **跨 program / 跨 PCR PID 统一 unwrap**。不同 program 的时钟域独立，
   回绕时刻和跳变历史都不同，共享偏移必然错位。
4. **用 PCR 到达顺序给 PTS 定标**。PCR 与 PES 无顺序保证（§2.3），用
   "最近看到的 PCR 的圈数"推断 PTS 圈数，在 discontinuity 后的过渡窗口
   （新 PCR 已到、旧 PTS 的 PES 还在流水线里）会把旧 PTS 错划到新时基。

## 5. 场景推演

### 5.1 多流跨回绕

设视频 PID 0x101、音频 PID 0x102，PCR PID = 0x101，回绕点 2^33 = 8589934592
（90 kHz tick）。可观察序列（库交付给调用方的原始值）：

| 到达序 | 类型 | 原始值 | 模差（相对上一行同类） |
|---|---|---|---|
| 1 | PCR | 8589934400 | — |
| 2 | 音频 PTS | 8589934500 | — |
| 3 | 视频 DTS/PTS | 8589934520 / 108 | — |
| 4 | PCR | 12 | (12−8589934400) mod 2^33 = 204 |
| 5 | 音频 PTS | 200 | 292 |
| 6 | 视频 PTS | 308 | 200 |

观察要点：

- 第 3 行视频 PTS 的线上值是 108（编码器已取模），demuxer 如实交付 108。
- 第 4 行 PCR 数值"倒流"约 26.5 小时，但模差 204 tick ≈ 2.3 ms，与码率
  一致。任何把 PCR 画成单调曲线的工具若不做模差，会在这里画出一根
  垂直下探的线——这是展示层的 bug，不是数据的异常。
- 唇同步差：回绕前 (8589934700−8589934500) mod 2^33 = 200；回绕后
  (308−200) mod 2^33 = 108 的邻域内同样连续。**音画关系在原始值上从未
  断裂**，断裂只会由不一致的 unwrap 引入。
- Muxer 侧：若调用方 unwrap 后得到 8589934700+2^33 = 17179869292 再写入，
  `writePTSOrDTS` 取低 33 位仍得 108，输出码流与透传完全一致。

### 5.2 显式中断与恢复

编码器重启或拼接点，PCR PID 上出现 `discontinuity_indicator = 1` 的 adaptation
field，随后 PCR 跳到新时基（例如从 26 小时处跳回接近 0）。可观察序列：

| 到达序 | 事件 | 库的行为 |
|---|---|---|
| 1 | 旧时基 PCR = 8589930000 | 透传 |
| 2 | AF: discontinuity_indicator=1，无 PCR | 透传标志；packet_pool 不清队（packet_pool.go:134） |
| 3 | 新时基 PCR = 3000 | 透传；库不标记"时基已变" |
| 4 | 旧时基排队的视频 PES，PTS = 8589930100 | 照常交付，无任何异常标记 |
| 5 | 新时基视频 PES，PTS = 3600 | 照常交付 |
| 6 | 若中断伴随 CC 跳变 | 该 PID 半组装的 PES 被静默丢弃（packet_pool.go:26） |

关键观察：

- **存在一个歧义窗口**（第 3 行到第 5 行之间）：调用方看到新 PCR，但无法
  仅从库交付的数据判定第 4 行的 PES 属于旧时基还是新时基。规范上
  discontinuity_indicator 声明的是"本 PID 下一个 PCR/CC 起不连续"，PES
  时间戳的归属须由调用方按"discontinuity 之后第一个 PCR 建立新基准，
  之后到达的 PTS 若与旧基准的模差超过半环则归入新时基"之类的启发式
  判定——这是调用方的责任，且本质上是启发式（见 §6）。
- 恢复期的兼容边界：解码器通常以 discontinuity 为界丢弃旧基准、重新锁定
  PLL。库把全部信息（标志、原始 PCR、原始 PTS）都交给了调用方，因此
  **信息上无损**；但 CC 跳变导致的静默丢 PES（第 6 行）是信息有损的，
  调用方只能从上层序列的空洞推断。
- es-split 式重复用在中断后的行为：PCR = DTS 合成继续工作（新时基的 DTS
  直接成为新 PCR），数值上自洽；但如果调用方在 discontinuity 前后做了
  全局 unwrap 再写入，旧偏移会污染新时基——这正是 §4.2 的失效模式。

## 6. 无法从输入确定的情形

以下信息原理上不在码流/库交付的数据中，任何实现都只能靠假设：

1. **单个时间戳的圈数（epoch）**。33 bit 环上的位置不携带圈数。唯一可
   unwrap 的前提是"相邻同域时间戳真实间隔 < 半环（约 13.2 小时）"。对
   稀疏流（字幕、低帧率幻灯片流、长时间只有音频的节目）该假设可能
   失效，此时 unwrap 有 2^k 歧义，码流本身无法仲裁。
2. **discontinuity 前后时基的偏移量**。规范允许任意跳变；只有拼接场景
   的 `DTSNextAccessUnit`（packet.go:68，splice 扩展）携带预告。一般
   discontinuity 后，新旧时基的差不可恢复，也不应恢复——它们本就属于
   两条独立时间轴。
3. **PCR 的 27 MHz 相位**。PTS/DTS 只有 90 kHz 分辨率，es-split 用
   PTS/DTS 合成 PCR 时 extension 恒为 0，原 PCR 的亚 tick 相位不可
   恢复（影响 PCR 抖动测量，不影响解码）。
4. **被静默丢弃的残缺 PES**。CC 不连续触发 packet_pool 清队时，已缓存
   片段的 PTS（在首包中）随之丢失，demuxer 不报错，调用方无法区分
   "丢了一个 PES"与"源就没有这段"。
5. **跨 program 的时基关系**。两个 program 的 PCR 是否同源（同一调制器
   时钟）码流不声明；它们的 PTS 数值即使接近也不能互算差值。
6. **中断的时长**。discontinuity 只标记"断了"，不携带断了多久；从
   PCR 数值差推算时长在时基跳变时无意义。

## 7. 结论：两端的责任划分

**Demuxer（已实现，符合契约）**：逐位保真解析；透传 discontinuity_indicator、
OPCR、splice 信息；不做 unwrap、不做重排、不做"修正"。改进空间在于把
packet_pool 的静默清队以错误或事件形式上抛，消除 §6.4 的盲区。

**Muxer（已实现，符合契约）**：按位宽隐式取模写出；不生成、不改写时间戳；
PMT 只校验 PCRPID 存在性。隐式取模使"unwrap 后写入"与"透传写入"输出
一致，这是对调用方友好的退化行为，但应在文档中明示而非依赖 BitsWriter
的实现细节。

**调用方（契约的另一半）**：

- 透传/remux：原样转发，比较一律用模差；
- 需要单调时间轴（画图、对齐、录制索引）：以 (PCR PID, discontinuity 段)
  为域做 unwrap，域内所有 ES 共享同一偏移，段边界重置；
- 重打时间戳（es-split 模式）：确认 PTS/DTS 与目标 PCR 域同时基，接受
  90 kHz 量化，discontinuity 后重新锚定；
- 永远不要用 PCR 与 PES 的到达顺序推断时基归属，不要用单一数值推断
  圈数，不要跨 program 比较时间戳。
