---
name: multi-lane
description: 创建可选择模型的具名外部 agent，用 Claude Code 或 Codex 在指定工作目录执行任务、独立评审或并行协作。适用于自定义 agent、指定 agent 和模型组合、多路沙盒评审与交叉验证。
---

# 具名 agent 与多路协作

使用执行器管理 agent 和模型。主 agent 负责准备任务、核对原始证据和交付结果；多路意见不按票数裁决。

执行脚本是本 skill 下的 `scripts/agents.py`，只依赖 Python 3.9+。根据当前加载的 skill 路径定位脚本，无需读取其实现或本机运行配置。下面的 `R` 替换为脚本的绝对路径：

```bash
R=/absolute/path/to/multi-lane/scripts/agents.py
python3 "$R" check
python3 "$R" models
python3 "$R" models --match Sonnet
```

模型列表返回 `id`、`name` 和 `alias`。选择当前列表中的模型；同名条目用 `id` 区分，保留名称中的 `v1/v2` 标记。连接由执行器自动读取本机配置。若 `check` 报配置不可用，向用户说明错误代码，由本机运行环境的维护者完成配置。

## 创建具名 agent

把职责和输出要求写进角色文件，再创建 agent：

```bash
python3 "$R" create reviewer \
  --engine claude-code --model sonnet-4.6-v1 \
  --instructions-file /absolute/review-role.md
python3 "$R" list
```

`--engine` 支持 `claude-code`、`codex`，与模型分别选择；示例模型必须以本机列表为准。重复名称不会被覆盖，修改已有 agent 时加 `--replace`。模型绑定到稳定 ID，列表改名不会改变已绑定的模型；模型不可用时会报错。

默认 `--mode read`。用户任务需要修改文件时，在创建 agent 时指定 `--mode write`。可选 `--effort low|medium|high|xhigh|max`，支持范围取决于所选模型。

## 运行任务

提示文件写明目标、输入材料、允许修改范围和产出要求，涉及文件时使用绝对路径：

```bash
python3 "$R" run --agent reviewer \
  --cwd /absolute/project --prompt-file /absolute/task.md \
  --output /absolute/review-output
```

重复 `--agent` 可同时运行多个已创建的 agent。每个 agent 使用独立会话和配置目录，最多同时运行四个；`--timeout` 设置每个 agent 的秒数，默认 3000。

在 Claude Code 中用 Bash 的 `run_in_background: true` 运行长任务。完成通知后读取执行器返回的结果；需要检查进度时读取返回目录中的 `status.json`。

每轮创建新的输出目录，包含各 agent 的 Markdown 结果和状态。只有 `completed` 才表示该 agent 成功；失败或超时不复用旧结果。向用户说明失败的路数，基于成功结果和原始证据继续判断。

Claude Code 使用受限模式将文件工具限定在工作目录，只读时提供 Read/Grep/Glob，写入时增加 Edit/Write。Codex 使用其 read-only/workspace-write 模式。独立配置和工具限制不等同于完整系统隔离。

外部系统查询与写入遵循当前任务的授权范围；需要主会话已有访问能力的数据时，由主 agent 获取后作为材料提供。
