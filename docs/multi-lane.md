# Named agents with multi-lane

Magpie ships the complete [multi-lane skill](../skills/multi-lane/SKILL.md)
and its Python executor in `skills/multi-lane`. It creates named Claude
Code or Codex agents, binds each to a selected model and runs independent
sessions, up to four at once. Python 3.9+ and the selected CLI must already
be installed.

## Install

In Magpie's Library page, add the `skills/multi-lane` folder from this
checkout, or the GitHub URL of the repository and revision containing it.
Select `multi-lane`, then the agents that should receive it. The existing
Library installer supplies the whole folder, including `scripts/agents.py`.
There is no separate package to install and no new resident process.

For a manual installation, copy this one folder into the desired agent's
skills directory. The skill resolves its executor relative to its own
location, so it works when linked, copied or installed from a fork.

## Workflow and private configuration

The skill's model-facing interface is `check`, `models`, `create`, `list`
and `run`. It accepts agent names, model display names or stable IDs, role
instructions and task files. It returns model labels, status and results.
The executor resolves connection information at process startup.

| Data | Location and owner |
| --- | --- |
| Workflow, executor and synthetic tests | The versioned skill folder |
| Endpoint, model routes and the name of a credential environment variable | A machine-owned `runtime.json` outside the skill and project |
| Gateway key | The launch environment; never a command argument or a skill file |
| Named agents | `~/.config/multi-lane/agents/`; contains stable model IDs and role instructions |
| Temporary CLI config and diagnostics | `~/.local/state/multi-lane/`; temporary configs are removed after each run |
| Task results | A fresh subdirectory of the requested output folder |

By default, the executor first reads
`~/.config/multi-lane/runtime.json`. `MULTI_LANE_RUNTIME_CONFIG` can name
another private file. Its schema is:

```json
{
  "schema": 1,
  "endpoint": "http://127.0.0.1:3425",
  "token_env": "MAGPIE_GATEWAY_KEY",
  "models": [
    {"route": "provider/example-model", "name": "Example Model v1"}
  ]
}
```

This is a synthetic example, not a model catalog to install unchanged.
Copy the exact route IDs from the local Magpie catalog and choose display
names without accounts, URLs or other private details. Keep source markers
such as `v1` and `v2` consistent with the existing picker. Equal display
names remain distinct IDs; an ambiguous name is refused.

`token_env` names an existing environment variable. Provision it locally
through the launcher or a secret manager; do not paste its value into the
skill, a prompt, a command argument or Git. An explicitly named variable
that is missing or empty is an error. Omit `token_env` only for a local
gateway that requires no key. Only loopback HTTP(S) endpoints are accepted.
Store the private file with permissions appropriate to its owner
(`0600` on Unix).

When neither an explicit file nor the default private file exists, the
executor can reuse an existing Claude Code setup: the `env` and
`modelPicker.options` fields in `~/.claude/settings.json`, or the file
named by `MULTI_LANE_SETTINGS`. It only reads that file. An explicit,
malformed or incomplete runtime config fails; it never falls back to a
different connection. This compatibility path does not import OAuth
accounts or alter the global agent settings.

`MULTI_LANE_CONFIG_DIR` changes the profile directory and the default
runtime-file location; `MULTI_LANE_STATE_DIR` changes the private run
directory. Keep these outside the skill and project. For isolated tests,
`MULTI_LANE_CODEX_CATALOG` supplies a local Codex capability fixture.
Normally, Codex capabilities are read from the gateway's
`/v1/codex/models`; a missing model or capability record fails without
substituting another model.

Model IDs are derived from the exact route, so renaming a display label
does not change an agent's binding. Changing the route requires recreating
that binding. Public results and retained diagnostic text redact known
connection values and route prefixes. This is not a general-purpose
redactor for arbitrary sensitive task content. A process with unrestricted
host access can still inspect local files or process environments.

## Execution and verification

Each task gets a separate CLI configuration and session. Claude Code uses
restricted and safe modes with only file tools: Read/Grep/Glob, plus
Edit/Write for an agent created with `--mode write`. Codex uses its
`read-only` or `workspace-write` sandbox. These controls are not a complete
OS isolation boundary. Timeout or cancellation terminates the process
group on Unix; Windows uses `taskkill /T`. Only `completed` means success,
and a failure never reuses an earlier result.

The regression suite uses temporary homes, synthetic credentials and fake
CLI executables. It makes no inference calls:

```sh
python3 -B -m unittest discover -s skills/multi-lane/tests -v
```

macOS and Linux are the CI targets for the process-group checks.
Windows execution is not yet validated. The test workflow also checks
Python 3.9 compatibility. Tests cover configuration precedence, stable
bindings, ambiguous names, missing capabilities, both engine adapters,
output redaction, failures, cancellation and child-process cleanup.
