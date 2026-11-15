# 交付说明：DecodeHook 执行层级契约回归测试体系

## 背景与问题

一次性装载服务用**有状态钩子**做数据脱敏，联调时发现钩子在单次装载中被
反复触发，连 map 的键都被钩了一遍，状态机被打穿。根因是钩子的「触发层级
契约」——每个节点钩几次、什么顺序、键和值是否分别钩——在文档与测试里都是
空白，任何后续重构都可能悄悄改掉行为而不被发现。

本交付补齐该契约的**回归测试 + 文档 + 变异验证**，不改任何解码逻辑。

## 钩子调用点分布（动手前先核查的结论）

全仓库钩子只有**一个真实调用点**：`mapstructure.go` 中 `Decoder.decode` 的
前置分派块。所有 kind 专属解码器（`decodeStructFromMap`、
`decodeMapFromMap`、`decodeSlice`、`decodeArray`、`decodePtr` 等）都通过
递归调用 `decode` 间接触发钩子。因此：

- 每个被解码节点在进入 kind 专属解码前，恰好经过钩子一次；
- 递归产生**深度优先、先序**遍历；
- map 的每个 entry 有**键、值两条独立通道**，先键后值；
- `nil` 节点默认在钩子之前早退（零触发），`DecodeNil` 打开后改为合成输入
  并在该层级触发一次。

## 交付物

| 文件 | 作用 |
|------|------|
| `decode_hook_hierarchy_test.go` | 层级契约回归测试（内部包测试，9 个顶层测试，含子测试） |
| `docs/hook-execution-hierarchy-contract.md` | 契约的正式文档化表述（英文，与仓库语言一致） |
| `tools/mutationtest/mutate_hook_hierarchy.py` | 变异验证器（独立 Python 脚本，非 Go 包，不进构建） |
| `tools/mutationtest/preflight.py` | 变异锚点唯一性预检 |
| `tools/mutationtest/mutation-report.txt` | 变异对照读数（机器生成，已落库） |
| `mapstructure.go` | **仅注释**：分派点、`DecodeHook`、`DecodeNil` 增加契约说明，零逻辑改动 |

## 测试覆盖的层级与用例

`TestDecodeHookHierarchy_*` 系列覆盖：

- **根容器 / 嵌套容器 / map 键 / 叶子值 / 切片容器 / 切片元素**全层级的触发
  计数与顺序（`FullWalk` 断言完整 13 步先序序列与各层级计数）；
- map 多 entry 的键先于值不变式与聚合计数（跨 entry 顺序受 Go 随机迭代影响，
  按设计只断言不变式而非全序列）；
- struct-kind 输入、指针层级（指针节点 + 指向元素各一次）；
- **零值通道组合**：`DecodeNil` 关/开两种闸门 × 字段级 nil、集合内部 nil 元素/
  值、nil 根输入，并区分真实值与合成（`synth`）事件；
- 有状态脱敏钩子端到端：每节点一次状态迁移、键不被脱敏、值被脱敏；
- 同一 decoder 多次 `Decode` 计数一致、无跨次泄漏。

## 变异验证（有效性证明）

命令（Python 隔离运行，副本上变异，不污染源码）：

```sh
python -I tools/mutationtest/mutate_hook_hierarchy.py --repo <模块路径>
python -I tools/mutationtest/preflight.py <模块路径>   # 锚点唯一性预检
```

对照读数（与 `mutation-report.txt` 一致）：

| 变异 | 改动 | 期望 | 实测 |
|------|------|------|------|
| M01 | 跳过 map 键的 `decode()` 通道 | 杀死 | KILLED |
| M02 | 跳过 map 值的 `decode()` 通道 | 杀死 | KILLED |
| M03 | map-kind 输入关闭钩子闸门 | 杀死 | KILLED |
| M04 | slice-kind 输入关闭钩子闸门 | 杀死 | KILLED |
| M05 | struct-kind 输入关闭钩子闸门 | 杀死 | KILLED |
| M06 | 切片元素不经 `decode()` 直接赋值 | 杀死 | KILLED |
| M07 | struct 字段不经 `decode()` 直接赋值 | 杀死 | KILLED |
| M08 | 指针目标关闭钩子闸门 | 杀死 | KILLED |
| M09 | 强制 `DecodeNil` 闸门常闭 | 杀死 | KILLED |
| M10 | 根节点钩子重复触发一次 | 杀死 | KILLED |
| C01 | 反转弱类型 int→bool 转换（不增删/重排任何钩子调用） | 存活 | SURVIVED |

**结果：10/10 个层级契约变异全部被杀死，1/1 个阴性对照存活。**

阴性对照 C01 的意义：它是真实行为变更（全量未过滤套件会经
`TestDecode_TypeConversion` 捕获），但不触碰任何钩子调用，层级套件正确地
**不越界**捕获它——证明本测试体系是专门守护钩子分派契约，而非对任意改动
都会失败的「伪有效」测试。

## 既有测试

全量既有测试保持通过。本地以多个 `-shuffle` 种子运行均绿：

```sh
go test ./...                 # ok
go test -shuffle=<seed> ./... # 多种子全绿，共 1109 个测试用例
go vet ./...                  # 通过
gofmt / gofumpt               # 新增与修改的 .go 文件均干净（LF 行尾）
```

说明：CI 还带 `-race`，本机 Windows 无 gcc/cgo 无法执行 `-race`，该门禁由
Linux CI 保证；本改动未引入并发代码。

## 复现实验的临时产物

排查阶段在仓库外的 scratch Go 模块（通过 `go.mod replace` 引用本仓库）仅用于
观测真实钩子序列，不属于交付物，可删除。
