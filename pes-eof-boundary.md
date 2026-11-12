# MPEG-TS 流结尾 PES 交付边界分析

## 结论

当前实现把“读取层是否还能给出完整 188 字节传输包”和“PES 组装层是否已经形成可交付数据”分成两个状态机处理。干净 EOF 会进入 pool 排空流程，因此合法的视频零长度 PES 可以在流结尾交付；声明了长度的 PES 只有累计到声明长度后才会在常规路径交付，若流结束时仍欠长，会在 pool 排空时解析失败并被跳过，最终只返回 `ErrNoMorePackets`。传输包被截断时，`io.ReadFull` 返回的 `io.ErrUnexpectedEOF` 被统一转换成 `ErrNoMorePackets`，因此它和干净 EOF 走同一条排空路径，调用方无法区分“输入正常结束”和“最后一个传输包损坏/截断”。

真正的上游读取错误（既不是 `io.EOF`，也不是 `io.ErrUnexpectedEOF`）不会触发 pool 排空，而是包装后直接返回；这类错误目前没有被误当成正常结束。问题集中在 `io.ErrUnexpectedEOF` 这一类“读到部分字节后结束”的情况。

## 两层状态如何相互影响

### 1. 原始包读取层：`packetBuffer`

`packetBuffer.next()` 每次用 `io.ReadFull` 读取固定大小的传输包：

- `io.EOF`：一个字节都没有读到，表示干净输入结束。
- `io.ErrUnexpectedEOF`：读到了 1 到 `packetSize-1` 个字节，表示最后一个传输包不完整。
- 其他错误：上游 I/O 故障。

当前代码在 `packet_buffer.go` 中把前两种情况都转换成同一个哨兵错误 `ErrNoMorePackets`：

```go
if _, err = io.ReadFull(pb.r, pb.packetReadBuffer); err != nil {
    if err == io.EOF || err == io.ErrUnexpectedEOF {
        err = ErrNoMorePackets
    } else {
        err = fmt.Errorf("astits: reading %d bytes failed: %w", pb.packetSize, err)
    }
    return
}
```

因此，上层 `Demuxer.NextData()` 看到的只有“没有更多完整传输包”，看不到最后一次读取到底是零字节 EOF 还是半截包。

自动包长探测还会放大这个边界问题：未显式设置包长时，初始化需要先 peek/read 前 193 字节。短输入、截断输入或探测阶段的非 EOF 错误都可能在 `NextPacket()` 首次创建 `packetBuffer` 时返回，此时 `packetPool` 可能还没有接收任何包，也就没有已缓存 PES 可排空。显式使用 `DemuxerOptPacketSize(188)` 可以把问题限定在逐包读取阶段，避免把包长探测失败和 PES EOF 语义混在一起。

### 2. PES 组装层：`packetPool` 与每个 PID 的 accumulator

每个 PID 都有独立队列。`packetPool.addUnlocked()` 会先丢弃带 transport error indicator 的包和没有 payload 的包，然后把剩余包交给对应 PID 的 `packetAccumulator`。

accumulator 的刷新条件有三类：

1. 同一 PID 出现新的 `PayloadUnitStartIndicator`，先刷新上一段 PES，再缓存新 PES。
2. PSI 根据 section 长度判断完整后刷新。
3. PES 满足 `isPESComplete()` 后刷新。

`isPESComplete()` 对 PES 的处理是：

- PES 固定头不足 6 字节：不完整。
- `PES_packet_length == 0`：永远返回 false，因为流内没有自描述长度。
- `PES_packet_length > 0`：只有累计 payload 长度至少达到 `6 + PES_packet_length`，且 PES 头可解析，才算完整。

这意味着零长度视频 PES 不能靠自身长度在常规路径交付，只能依赖两个外部边界：

- 同一 PID 的下一个 PUSI；
- `NextData()` 收到 `ErrNoMorePackets` 后执行 `packetPool.dumpUnlocked()`。

### 3. EOF 排空路径

`NextData()` 收到 `ErrNoMorePackets` 后，会按 PID 数值顺序反复调用 `dumpUnlocked()`：

- dump 出来的包组交给 `parseData()`。
- 能解析成 PES/PSI 时，返回一个 `DemuxerData`；剩余包仍保留在 pool 中，下一次 `NextData()` 从 `dataBuffer` 或继续 dump 中取得。
- 解析失败时只写 logger 并继续排空下一个 PID。默认 logger 是空实现，所以调用方通常只能看到最后返回的 `ErrNoMorePackets`，看不到某个欠长 PES 已被丢弃的具体原因。
- pool 清空后，本次 `NextData()` 以零数据和 `ErrNoMorePackets` 返回。

所以读取层决定“何时触发排空”，组装层决定“排空出来的缓存能否变成 PES 数据”。把截断读错误归并成 `ErrNoMorePackets` 后，截断会获得与干净 EOF 相同的排空时机。

## 最小传输流序列与结果

下面的序列使用显式 188 字节包长，避免自动探测干扰。TS 头只列出关键字段：PID 256、payload present、continuity counter 从 0 开始；PES 使用视频 stream id `0xe0` 和最小 optional header `80 00 00`。

| 序列 | PES 形态 | 当前行为 | 调用方结果 |
| --- | --- | --- | --- |
| `T1(PUSI, PES length=0)` + 干净 EOF | 合法视频零长度 PES | `isPESComplete()` 始终 false；EOF dump 后 `parsePESHeader()` 把 `dataEnd` 设为当前 payload 末尾，可正常解析 | 第一次 `NextData()` 交付 PES；第二次返回 `ErrNoMorePackets` |
| `T1(PUSI, PES length=13)`，单包内含 6+13 字节 | 有界且完整的 PES | 累计长度满足声明值，常规路径立即刷新，不需要等待 EOF | `NextData()` 直接交付 |
| `T1(PUSI, PES length=200)` + 干净 EOF，实际只有 184 字节 payload | 有界但欠长 | 常规路径不刷新；EOF dump 后解析发现声明末尾超过实际数据，`parsePESData()` 报错，错误被记录并跳过 | 数据为 nil，最终 `ErrNoMorePackets`；默认 logger 下看不到解析错误 |
| `T1(PUSI, 零长度 PES)` + `T2(无 PUSI, 欠长 PES 数据)` + 50 字节半截 TS 包 | 前一个视频 PES 已完整缓存，最后传输包截断 | `io.ErrUnexpectedEOF` 被转换成 `ErrNoMorePackets`；EOF dump 先交付前一个零长度 PES，欠长队列和半截 TS 包无法再交付 | 第一次得到前一个 PES；第二次得到 `ErrNoMorePackets`，无法知道存在截断 |
| 只有 50 字节半截 TS 包 | 没有完整传输包 | `ReadFull` 返回 `io.ErrUnexpectedEOF`，但它被转换成 `ErrNoMorePackets`；没有完整包进入 pool | 数据为 nil，`ErrNoMorePackets`，截断被表现成正常 EOF |
| `T1(PUSI, 零长度 PES)` + 上游返回自定义非 EOF 错误 | 已有缓存 PES，但读取层发生 I/O 故障 | 错误不是 `io.EOF`/`io.ErrUnexpectedEOF`，不进入 EOF dump，pool 中已有 PES 暂不交付 | 返回包装后的原始错误，可用 `errors.Is` 判断上游错误 |

同一 PID 中，新的 PUSI 也会作为零长度 PES 的边界。例如 `T1(PUSI, length=0)` 后接 `T2(PUSI, 有界完整 PES)`：处理 `T2` 时先刷新 `T1`，`T2` 自己则在满足声明长度后立即返回，或在 EOF dump 时返回。

多 PID 场景要注意排空顺序：常规路径中已经满足完整性条件的有界 PES 可能先返回；EOF dump 则按 PID 数值升序返回仍在 pool 中的数据。因此，输入中的 PID 顺序不等于 EOF 阶段的交付顺序。

## 对后续节目和音视频数据的影响

### 视频零长度 PES

视频流允许 `PES_packet_length=0`，这类 PES 没有内置结束位置。当前实现依赖下一个 PUSI 或 EOF 判定边界：

- 文件或完整字节流结尾正常时，最后一个视频访问单元可以通过 EOF dump 交付。
- 管道/socket 在传输中途断开并产生半截 TS 包时，断开会被当成 EOF，最后一个零长度 PES 可能被当作“到流尾自然结束”的完整数据交付。调用方无法确认它后面是否本来还应有 continuation 包。
- 如果后续输入其实还会到达，但当前 reader 暂时返回了非 EOF 错误，已有 PES 不会被提前交付；reader 恢复后状态仍保留。

### 声明长度的音频/视频 PES

有界 PES 的完整性由声明长度保护：

- 达到声明长度时立即交付，不受 EOF 影响。
- 未达到声明长度时，即使 EOF dump 拿到了缓存包，也会因为声明范围超出实际 payload 而解析失败并丢弃。
- 默认 logger 会吞掉这条解析错误，上层只看到正常结束，可能把“尾部音频帧/视频帧丢失”误判为流本来就没有更多数据。

### PSI、节目映射和后续 PID

EOF dump 按 PID 排序，而 PAT/PMT 等 PSI 也在各自 PID 的 accumulator 中缓存。若尾部 PSI section 欠长，解析失败后会继续处理其他 PID；这可以避免一个坏 section 阻塞所有剩余 PES，但调用方同样只能得到最终 EOF，无法从返回值知道节目映射更新是否丢失。

如果截断发生在节目切换或 PMT 更新附近，可能出现：

- 新 PMT 未完整交付，后续 elementary PID 无法建立 stream map。
- 已经解析出的旧节目映射继续被使用，新节目的部分 PES 被当作未知数据或按旧映射解释。
- 音视频某一 PID 的尾部 PES 被丢弃，另一 PID 的缓存仍能在 EOF dump 中交付，表现为音频和视频尾部长度不一致。

### continuity counter、错误包和重复包

在进入 EOF 逻辑前，组装层还可能提前清空队列：

- transport error indicator 为 1 的包在 `packetPool.addUnlocked()` 入口直接丢弃。
- 未设置 adaptation discontinuity indicator 且 continuity counter 不连续时，当前 PID 队列会被重置，正在组装的 PES 被丢弃。
- 相同 continuity counter 的重复 payload 包会被忽略。

这些情况不会统一转换成 EOF；它们影响的是 pool 中已有数据的连续性和最终可 dump 内容。

## 上游读取错误是否被误当成正常结束

分两类判断：

1. **普通非 EOF 读取错误：没有被误当成 EOF。**

   例如网络连接 reset、自定义 reader 返回 `errors.New("upstream failed")`。`packetBuffer.next()` 会保留错误并包装成 `astits: reading 188 bytes failed: ...`，`NextData()` 不执行 pool dump，直接返回；可以通过 `errors.Is(err, upstreamErr)` 找到原因。

2. **半截传输包：当前会被误当成正常 EOF。**

   `io.ReadFull` 在读到部分字节后遇到 EOF 时返回 `io.ErrUnexpectedEOF`。当前代码把它与零字节 `io.EOF` 一起转换成 `ErrNoMorePackets`。调用方无法通过 `errors.Is(err, io.ErrUnexpectedEOF)` 检查，因为转换后原始错误没有被 wrap，只留下哨兵值。

## 合理修复边界

### 应修复的范围

最小修复应只区分读取层的三种结果，不改变 PES 对长度字段的既有语义：

- 零字节 `io.EOF` 继续映射为 `ErrNoMorePackets`，保留当前 EOF pool dump 行为。
- `io.ErrUnexpectedEOF` 保留或包装为可识别的截断错误，例如新增 `ErrTruncatedPacket`，并保证 `errors.Is(err, io.ErrUnexpectedEOF)` 成立。
- 其他 I/O 错误继续包装返回，不映射为 EOF。

截断错误是否要在返回前先排空 pool 需要明确 API 语义。保守方案是：

- 检测到半截传输包时，先排空已经完整进入 pool 的数据，让零长度视频 PES 和其他已缓存但可解析的数据有机会交付。
- pool 排空完成后，下一次 `NextData()` 返回截断错误，而不是 `ErrNoMorePackets`。
- 欠长有界 PES 仍然不伪装成完整 PES；解析错误应通过可观察 logger 或聚合错误暴露，避免默认静默丢弃。

这个方案把“已经完整读取到的 TS 包组成的数据”和“最后一个不完整 TS 包”分开处理，既不扩大交付范围，也不吞掉截断原因。

### 不应越过的边界

- 不应把欠长的有界 PES 强行裁剪交付。`PES_packet_length` 已明确声明所需字节数，裁剪会制造看似成功的数据损坏。
- 不应仅凭 EOF 保证零长度 PES 一定完整。零长度 PES 只有在业务上接受“reader 结束即视频 PES 结束”时才能交付；截断错误必须另行上报。
- 不应在普通非 EOF I/O 错误时自动 dump 并清空 pool。瞬时错误可能可恢复，提前清空会破坏后续 continuation 包组装；直接返回错误、保留 pool 更安全。
- 不应改变 continuity counter、transport error indicator 和重复包的既有丢弃策略来掩盖截断问题；这些是组装层的独立完整性规则。
- 不应把自动包长探测失败解释成 PES EOF。探测阶段需要单独保留读取错误，或要求已知包长的调用方显式设置 `DemuxerOptPacketSize(188)`。

## 验证记录

使用临时黑盒测试构造 188 字节最小 TS 序列，覆盖以下场景并通过：

- 零长度视频 PES 在干净 EOF dump 后交付。
- 有界完整 PES 在达到声明长度后立即交付。
- 有界欠长 PES 在 EOF 后不交付，最终返回 `ErrNoMorePackets`。
- 完整 TS 包后追加 50 字节半截包时，已有缓存 PES 可交付，但截断本身仍表现为 `ErrNoMorePackets`。
- 只有半截 TS 包时，调用方得到 `ErrNoMorePackets` 而不是 `io.ErrUnexpectedEOF`。
- 自定义非 EOF reader 错误被包装返回，且 `errors.Is` 可匹配原始错误。

验证命令：

```sh
go test . -run TestScratchPESEOFBoundary -count=1 -v -timeout=10s
```

该临时测试只用于本次分析，不保留在仓库中。
