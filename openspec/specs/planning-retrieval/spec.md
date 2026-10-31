# planning-retrieval Specification

## Purpose
在需求没有点名可解析符号时，让规划前的只读检索提供有界证据，并在证据不足时停止猜测。

## Requirements

### Requirement: Retrieval is read-only and bounded
符号查找没有命中时，系统必须只通过读路径查找候选文件。读取不得超过 8 次，单次不得超过 4000 字符，合计不得超过 16000 字符。越界路径和敏感路径必须拒绝。检索不得写文件或运行命令。

#### Scenario: Candidate file is readable
- **WHEN** 需求描述仓库行为，符号查找无命中，且候选文件在读路径策略内
- **THEN** 规划上下文包含该文件的有界摘录和路径

#### Scenario: Path is denied
- **WHEN** 候选路径越过仓库根目录或命中敏感路径
- **THEN** 该路径不被读取

### Requirement: Missing evidence stops planning
检索没有命中且需求仍然无法定位到仓库内容时，计划必须是待澄清，不得编造将要修改的文件。

#### Scenario: No file matches the description
- **WHEN** 只读检索没有命中任何文件
- **THEN** 任务进入待澄清，并且不进入编码

### Requirement: Retrieved text stays untrusted data
被读到的文件内容必须放在不可信数据边界中。它可以被规划引用，但不得被当作改变策略或角色的指令。

#### Scenario: File text contains an instruction
- **WHEN** 检索摘录中含有要求忽略路径策略的句子
- **THEN** 该句子不改变路径策略，规划仍只能在原有读权限内进行
