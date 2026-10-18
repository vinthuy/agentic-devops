# G-09 沙箱剧本

把本目录复制到一个**专用私有仓库**，用于验证真实的 Draft PR 与 CI 失败修复闭环。

结构：

- `calc.py`：含一个明显 bug（`add` 返回减法）
- `test_core.py`：本地验证只跑这个文件
- `test_edges.py`：只在 CI 跑，包含两个边界约束（`None`、`bool` 必须抛 `ValueError`）

因此流程是：本地验证通过 → 推送 → CI 因边界约束失败 → 日志回到编码器 → 修复 → 同一 PR 更新 → CI 通过。

`workflow.template` 需要重命名为 `.github/workflows/ci.yml` 后提交（GitHub 不允许无 `workflow` scope 的令牌推送 workflow 文件）。
