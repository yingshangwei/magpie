#!/usr/bin/env python3
"""Create named agents and run independent CLI sessions."""

import argparse
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
import urllib.request
import uuid


ENGINES = {"claude-code": "claude", "codex": "codex"}
PROXY_KEYS = ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
              "http_proxy", "https_proxy", "all_proxy")
NAME = re.compile(r"[a-z][a-z0-9_-]{0,63}\Z")
ENV_NAME = re.compile(r"[A-Za-z_][A-Za-z0-9_]*\Z")


class Failure(Exception):
    def __init__(self, code, message):
        self.code, self.message = code, message
        super().__init__(message)


def emit(value):
    print(json.dumps(value, ensure_ascii=False), flush=True)


def read_json(path):
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError, UnicodeError):
        raise Failure("CONFIG_UNAVAILABLE", "Local runtime configuration is unavailable.") from None


def private_dir(path):
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.chmod(0o700)
    return path


def save_json(path, value, replace=True):
    fd, temporary = tempfile.mkstemp(prefix=".save-", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            json.dump(value, stream, ensure_ascii=False, indent=2)
            stream.write("\n")
        if replace:
            os.replace(temporary, path)
        else:
            try:
                os.link(temporary, path)
            except FileExistsError:
                raise Failure("AGENT_EXISTS", "Agent already exists; use --replace to update it.") from None
    finally:
        Path(temporary).unlink(missing_ok=True)


def read_text(path):
    try:
        value = Path(path).expanduser().resolve(strict=True).read_text(encoding="utf-8")
        if not value.strip():
            raise ValueError()
        return value
    except (OSError, ValueError, UnicodeError):
        raise Failure("INPUT_UNAVAILABLE", "A required input file is missing, empty or not UTF-8.") from None


def slug(name):
    name = re.sub(r"^claude\s+", "", name, flags=re.I)
    return re.sub(r"[^a-z0-9.]+", "-", name.casefold()).strip("-")


def load_configuration(config):
    """Read machine-owned settings; never write connection data into the skill."""
    explicit = os.environ.get("MULTI_LANE_RUNTIME_CONFIG")
    source = Path(explicit).expanduser() if explicit else config / "runtime.json"
    if explicit or source.exists():
        data = read_json(source)
        if (not isinstance(data, dict) or data.get("schema") != 1
                or set(data) - {"schema", "endpoint", "token_env", "models"}
                or not isinstance(data.get("endpoint"), str)
                or not isinstance(data.get("models"), list)):
            raise Failure("CONFIG_INVALID", "Local runtime configuration is invalid.")
        connection = {"ANTHROPIC_BASE_URL": data["endpoint"]}
        key = data.get("token_env")
        if key is not None:
            if not isinstance(key, str) or not ENV_NAME.fullmatch(key):
                raise Failure("CONFIG_INVALID", "The configured credential reference is invalid.")
            token = os.environ.get(key)
            if not token or not token.strip():
                raise Failure("CONNECTION_UNAVAILABLE", "The configured local credential is unavailable.")
            connection["ANTHROPIC_AUTH_TOKEN"] = token
        rows = []
        for item in data["models"]:
            if (not isinstance(item, dict) or set(item) != {"route", "name"}
                    or not isinstance(item["name"], str) or not item["name"].strip()):
                raise Failure("CATALOG_INVALID", "The local model catalog contains an invalid entry.")
            rows.append({"model": item["route"], "label": item["name"]})
        return connection, rows

    # Compatibility with an existing agent setup. A broken explicit file never
    # falls back to this source or silently selects another model.
    source = Path(os.environ.get("MULTI_LANE_SETTINGS", Path.home() / ".claude/settings.json")).expanduser()
    settings = read_json(source)
    if not isinstance(settings, dict) or not isinstance(settings.get("env"), dict):
        raise Failure("CONFIG_INVALID", "Local runtime configuration is invalid.")
    picker = settings.get("modelPicker", {})
    if not isinstance(picker, dict) or not isinstance(picker.get("options"), list):
        raise Failure("CATALOG_INVALID", "The local model catalog is unavailable.")
    return settings["env"], picker["options"]


class Runtime:
    def __init__(self):
        user = Path.home()
        self.config = Path(os.environ.get("MULTI_LANE_CONFIG_DIR", user / ".config/multi-lane")).expanduser()
        self.state = Path(os.environ.get("MULTI_LANE_STATE_DIR", user / ".local/state/multi-lane")).expanduser()
        self.connection, rows = load_configuration(self.config)
        self.models = {}
        for row in rows:
            if not isinstance(row, dict):
                raise Failure("CATALOG_INVALID", "The local model catalog contains an invalid entry.")
            route = row.get("model", "")
            if not isinstance(route, str) or not route or any(ord(c) < 32 for c in route):
                raise Failure("CATALOG_INVALID", "The local model catalog contains an invalid entry.")
            label = row.get("label") or route.rsplit("/", 1)[-1]
            if not isinstance(label, str):
                raise Failure("CATALOG_INVALID", "The local model catalog contains an invalid entry.")
            label = re.sub(r"\s+·.*$", "", label).strip()
            label = re.sub(r"\bmagpie\b", "", label, flags=re.I).strip()
            if (not label or any(ord(c) < 32 for c in label)
                    or "@" in label or "://" in label or "/" in label or "\\" in label):
                raise Failure("CATALOG_INVALID", "Use model display names without connection or account details.")
            for key in ("ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL"):
                value = self.connection.get(key)
                if isinstance(value, str) and value and value in label:
                    raise Failure("CATALOG_INVALID", "Use model display names without connection or account details.")
            key = "m_" + hashlib.sha256(route.encode()).hexdigest()[:16]
            if key in self.models and self.models[key]["route"] != route:
                raise Failure("CATALOG_INVALID", "The local model catalog contains conflicting identifiers.")
            self.models[key] = {"id": key, "name": label, "alias": slug(label), "route": route}
        if not self.models:
            raise Failure("CATALOG_EMPTY", "No configured models are available.")

    def public_model(self, model):
        return {key: model[key] for key in ("id", "name", "alias")}

    def resolve(self, value):
        if value in self.models:
            return self.models[value]
        matches = [m for m in self.models.values()
                   if value.casefold() in (m["name"].casefold(), m["alias"])]
        if len(matches) > 1:
            raise Failure("MODEL_AMBIGUOUS", "Model name is ambiguous; select an exact id from models.")
        if not matches:
            raise Failure("MODEL_UNAVAILABLE", "Model is unavailable; select an id from models.")
        return matches[0]

    def profile_path(self, name):
        if not NAME.fullmatch(name):
            raise Failure("INVALID_NAME", "Use a lowercase agent name with letters, digits, - or _.")
        return self.config / "agents" / (name + ".json")

    def profile(self, name):
        path = self.profile_path(name)
        if not path.is_file():
            raise Failure("AGENT_UNAVAILABLE", "Named agent does not exist; create it first.")
        profile = read_json(path)
        if (profile.get("name") != name or profile.get("engine") not in ENGINES
                or profile.get("mode") not in ("read", "write")
                or not isinstance(profile.get("instructions"), str)):
            raise Failure("AGENT_INVALID", "Named agent configuration is invalid.")
        self.resolve(profile.get("model_id", ""))
        return profile

    def public_profile(self, profile):
        model = self.resolve(profile["model_id"])
        return {"name": profile["name"], "engine": profile["engine"],
                "model": self.public_model(model), "mode": profile["mode"],
                "effort": profile.get("effort")}

    def redact(self, text):
        for key in ("ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL"):
            value = self.connection.get(key)
            if isinstance(value, str) and value:
                text = text.replace(value, "[connection]")
        for model in sorted(self.models.values(), key=lambda m: -len(m["route"])):
            text = text.replace(model["route"], model["name"])
            text = text.replace(model["route"].removesuffix("[1m]"), model["name"])
        text = text.replace("local-runtime", "[connection]")
        return re.sub(r"\bmagpie\b", "[runtime]", text, flags=re.I)

    def endpoint(self):
        base = self.connection.get("ANTHROPIC_BASE_URL", "").rstrip("/")
        token = self.connection.get("ANTHROPIC_AUTH_TOKEN") or self.connection.get("ANTHROPIC_API_KEY")
        try:
            parsed = urllib.parse.urlsplit(base)
            valid = (parsed.scheme in ("http", "https")
                     and parsed.hostname in ("127.0.0.1", "localhost", "::1")
                     and not parsed.username and not parsed.password
                     and not parsed.query and not parsed.fragment)
        except ValueError:
            valid = False
        if not valid or token is not None and not isinstance(token, str):
            raise Failure("CONNECTION_UNAVAILABLE", "The configured local connection is unavailable.")
        # A loopback server can accept requests without a key while a CLI
        # still requires a nonempty API-key field to start in API mode.
        return base.removesuffix("/v1"), token or "local-runtime"

    def codex_model(self, model):
        cache = os.environ.get("MULTI_LANE_CODEX_CATALOG")
        if cache:
            catalog = read_json(Path(cache))
        else:
            base, token = self.endpoint()
            request = urllib.request.Request(
                base + "/v1/codex/models", headers={"Authorization": "Bearer " + token}
            )
            try:
                opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
                with opener.open(request, timeout=5) as response:
                    catalog = json.loads(response.read(2 << 20))
            except Exception:
                raise Failure("CATALOG_UNAVAILABLE", "The selected engine's model capabilities are unavailable.") from None
        route = model["route"]
        entries = {item.get("slug"): item for item in catalog.get("models", [])}
        entry = entries.get(route) or entries.get(route.removesuffix("[1m]"))
        if not entry:
            raise Failure("MODEL_UNAVAILABLE", "The selected model is unavailable for this engine.")
        entry = dict(entry)
        entry.update(slug=route, display_name=model["name"], description=model["name"])

        def clean(value):
            if isinstance(value, str):
                return self.redact(value)
            if isinstance(value, dict):
                return {key: clean(item) for key, item in value.items()}
            if isinstance(value, list):
                return [clean(item) for item in value]
            return value

        for key in ("base_instructions", "model_messages"):
            if key in entry:
                entry[key] = clean(entry[key])
        return entry

    def command(self, profile, folder, workspace):
        base, token = self.endpoint()
        model = self.resolve(profile["model_id"])
        executable = shutil.which(ENGINES[profile["engine"]])
        if not executable:
            raise Failure("ENGINE_UNAVAILABLE", "The selected agent engine is not installed.")
        keep = ("PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "TMPDIR",
                "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "APPDATA", "LOCALAPPDATA",
                "SSL_CERT_FILE", "SSL_CERT_DIR")
        env = {key: os.environ[key] for key in keep if key in os.environ}
        for key in PROXY_KEYS:
            value = self.connection.get(key, os.environ.get(key))
            if isinstance(value, str):
                env[key] = value
        for key in ("NO_PROXY", "no_proxy"):
            values = [v for v in env.get(key, "").split(",") if v]
            env[key] = ",".join(dict.fromkeys(values + ["localhost", "127.0.0.1", "::1"]))
        if profile["engine"] == "claude-code":
            tools = "Read,Grep,Glob" + (",Edit,Write" if profile["mode"] == "write" else "")
            env.update({
                "CLAUDE_CONFIG_DIR": str(folder / "config"),
                "ANTHROPIC_BASE_URL": base, "ANTHROPIC_API_KEY": token,
                "ANTHROPIC_MODEL": model["route"],
                "ANTHROPIC_SMALL_FAST_MODEL": model["route"],
                "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
                "CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT": "1",
                "DISABLE_AUTOUPDATER": "1", "DISABLE_TELEMETRY": "1",
                "DISABLE_ERROR_REPORTING": "1",
            })
            private_dir(folder / "config")
            settings = {"model": model["route"], "permissions": {"defaultMode": "dontAsk"}}
            save_json(folder / "settings.json", settings)
            command = [executable, "--restricted", "--safe-mode", "--print", "--output-format", "json",
                       "--no-session-persistence", "--setting-sources", "",
                       "--settings", str(folder / "settings.json"),
                       "--strict-mcp-config", "--mcp-config", '{"mcpServers":{}}',
                       "--model", model["route"], "--tools", tools,
                       "--allowedTools=" + tools, "--permission-mode", "dontAsk",
                       "--append-system-prompt", profile["instructions"]]
            if profile.get("effort"):
                command += ["--effort", profile["effort"]]
        else:
            env.update({"CODEX_HOME": str(folder / "config"), "MULTI_LANE_API_KEY": token})
            config = private_dir(folder / "config")
            save_json(folder / "models.json", {"models": [self.codex_model(model)]})
            contents = ('model_provider = "runtime"\n'
                        'model = ' + json.dumps(model["route"]) + '\n'
                        'model_catalog_json = ' + json.dumps(str(folder / "models.json")) + '\n'
                        'approval_policy = "never"\n'
                        '[model_providers.runtime]\nname = "Runtime"\n'
                        'base_url = ' + json.dumps(base + "/v1") + '\n'
                        'env_key = "MULTI_LANE_API_KEY"\nwire_api = "responses"\n'
                        'requires_openai_auth = false\nsupports_websockets = false\n')
            (config / "config.toml").write_text(contents, encoding="utf-8")
            (config / "config.toml").chmod(0o600)
            command = [executable, "exec", "--ephemeral", "--skip-git-repo-check",
                       "--sandbox", "read-only" if profile["mode"] == "read" else "workspace-write",
                       "--model", model["route"], "--color", "never", "--json",
                       "--output-last-message", str(folder / "result.md")]
            if profile.get("effort"):
                command += ["-c", "model_reasoning_effort=" + json.dumps(profile["effort"])]
            command.append("-")
        return command, env


def stop_process(process):
    if os.name == "posix":
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            return
        try:
            process.wait(timeout=2)
        except subprocess.TimeoutExpired:
            pass
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    elif process.poll() is None:
        subprocess.run(["taskkill", "/PID", str(process.pid), "/T", "/F"],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    process.wait()


def run_agents(runtime, args):
    names = list(dict.fromkeys(args.agent))
    profiles = [runtime.profile(name) for name in names]
    workspace = Path(args.cwd).expanduser().resolve()
    if not workspace.is_dir() or not 0 < args.timeout <= 86400:
        raise Failure("INVALID_RUN", "Use an existing working directory and a positive timeout up to 86400 seconds.")
    prompt = read_text(args.prompt_file)
    runtime.endpoint()
    for profile in profiles:
        if not shutil.which(ENGINES[profile["engine"]]):
            raise Failure("ENGINE_UNAVAILABLE", "A selected agent engine is not installed.")
    run_id = time.strftime("%Y%m%d-%H%M%S", time.gmtime()) + "-" + uuid.uuid4().hex[:10]
    public = Path(args.output).expanduser().resolve() / run_id
    public.mkdir(parents=True, exist_ok=False)
    private = private_dir(private_dir(runtime.state) / run_id)
    statuses = [{"agent": p["name"], "engine": p["engine"], "status": "pending",
                 "model": runtime.public_model(runtime.resolve(p["model_id"]))} for p in profiles]
    status_lock, process_lock = threading.Lock(), threading.Lock()
    processes, cancelled = {}, threading.Event()

    def update(index, **values):
        with status_lock:
            statuses[index].update(values)
            save_json(public / "status.json", {"run_id": run_id, "agents": statuses})

    def work(index, profile):
        start = time.monotonic()
        folder = private_dir(private / profile["name"])
        process = None
        try:
            command, env = runtime.command(profile, folder, workspace)
            payload = prompt
            if profile["engine"] == "codex":
                payload = "Agent instructions:\n" + profile["instructions"] + "\n\nTask:\n" + prompt
            with (folder / "stdout.log").open("wb") as stdout, (folder / "stderr.log").open("wb") as stderr:
                (folder / "stdout.log").chmod(0o600)
                (folder / "stderr.log").chmod(0o600)
                with process_lock:
                    if cancelled.is_set():
                        raise Failure("CANCELLED", "Run was cancelled.")
                    process = subprocess.Popen(
                        command, cwd=workspace, env=env, stdin=subprocess.PIPE,
                        stdout=stdout, stderr=stderr, start_new_session=(os.name == "posix"),
                    )
                    processes[index] = process
                update(index, status="running")
                try:
                    process.communicate(input=payload.encode(), timeout=args.timeout)
                except subprocess.TimeoutExpired:
                    raise Failure("TIMEOUT", "Agent exceeded the configured timeout.") from None
                if cancelled.is_set():
                    raise Failure("CANCELLED", "Run was cancelled.")
                if process.returncode:
                    raise Failure("ENGINE_FAILED", "Agent process failed; diagnostic details are retained locally.")
            if profile["engine"] == "claude-code":
                data = read_json(folder / "stdout.log")
                if not isinstance(data, dict) or data.get("is_error"):
                    raise Failure("ENGINE_FAILED", "Agent did not complete successfully.")
                result = data.get("result", "")
            else:
                result = read_text(folder / "result.md")
            if not isinstance(result, str) or not result.strip():
                raise Failure("EMPTY_RESULT", "Agent returned no final result.")
            (public / (profile["name"] + ".md")).write_text(runtime.redact(result), encoding="utf-8")
            update(index, status="completed", result_file=str(public / (profile["name"] + ".md")),
                   seconds=round(time.monotonic() - start, 2))
            return True
        except Failure as error:
            update(index, status="failed", error={"code": error.code, "message": error.message},
                   seconds=round(time.monotonic() - start, 2))
            return False
        except Exception:
            update(index, status="failed", error={"code": "RUNTIME_FAILED", "message": "Agent execution failed."},
                   seconds=round(time.monotonic() - start, 2))
            return False
        finally:
            if process:
                stop_process(process)
            with process_lock:
                processes.pop(index, None)
            # Keep diagnostics separate from task outputs, without keeping
            # generated connection settings or authentication material.
            shutil.rmtree(folder / "config", ignore_errors=True)
            (folder / "settings.json").unlink(missing_ok=True)
            (folder / "models.json").unlink(missing_ok=True)
            for diagnostic in ("stdout.log", "stderr.log", "result.md"):
                path = folder / diagnostic
                if path.is_file():
                    text = path.read_text(encoding="utf-8", errors="replace")
                    path.write_text(runtime.redact(text), encoding="utf-8")
                    path.chmod(0o600)

    save_json(public / "status.json", {"run_id": run_id, "agents": statuses})
    emit({"run_id": run_id, "status": "started", "output_dir": str(public), "agents": names})
    executor = concurrent.futures.ThreadPoolExecutor(max_workers=min(4, len(profiles)))
    try:
        futures = [executor.submit(work, index, p) for index, p in enumerate(profiles)]
        outcomes = [future.result() for future in concurrent.futures.as_completed(futures)]
    except BaseException:
        cancelled.set()
        with process_lock:
            for process in processes.values():
                stop_process(process)
        raise
    finally:
        executor.shutdown(wait=True, cancel_futures=True)
        if cancelled.is_set():
            for index, status in enumerate(statuses):
                if status["status"] in ("pending", "running"):
                    update(index, status="failed",
                           error={"code": "CANCELLED", "message": "Run was cancelled."})
    emit({"run_id": run_id, "status": "completed" if all(outcomes) else "failed",
          "output_dir": str(public), "agents": statuses})
    return 0 if all(outcomes) else 1


def main():
    parser = argparse.ArgumentParser(description="Create named agents and run independent CLI sessions.")
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("check", help="Check installed engines and the model catalog.")
    models = sub.add_parser("models", help="List model names and stable selection ids.")
    models.add_argument("--match", default="")
    create = sub.add_parser("create", help="Create or update a named agent.")
    create.add_argument("name")
    create.add_argument("--engine", choices=ENGINES, required=True)
    create.add_argument("--model", required=True)
    instructions = create.add_mutually_exclusive_group(required=True)
    instructions.add_argument("--instructions")
    instructions.add_argument("--instructions-file")
    create.add_argument("--mode", choices=("read", "write"), default="read")
    create.add_argument("--effort", choices=("low", "medium", "high", "xhigh", "max"))
    create.add_argument("--replace", action="store_true")
    sub.add_parser("list", help="List named agents.")
    run = sub.add_parser("run", help="Run one or more named agents.")
    run.add_argument("--agent", action="append", required=True)
    run.add_argument("--cwd", required=True)
    run.add_argument("--prompt-file", required=True)
    run.add_argument("--output", required=True)
    run.add_argument("--timeout", type=float, default=3000, help="Timeout per agent in seconds (default: 3000).")
    args = parser.parse_args()

    def cancel(signum, frame):
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, cancel)
    try:
        runtime = Runtime()
        if args.command == "check":
            runtime.endpoint()
            emit({"engines": {name: bool(shutil.which(exe)) for name, exe in ENGINES.items()},
                  "model_count": len(runtime.models), "connection_configured": True})
        elif args.command == "models":
            emit({"models": [runtime.public_model(m) for m in runtime.models.values()
                             if args.match.casefold() in m["name"].casefold()]})
        elif args.command == "create":
            path = runtime.profile_path(args.name)
            model = runtime.resolve(args.model)
            text = read_text(args.instructions_file) if args.instructions_file else args.instructions
            if not text or not text.strip():
                raise Failure("EMPTY_INSTRUCTIONS", "Agent instructions must not be empty.")
            profile = {"schema": 1, "name": args.name, "engine": args.engine,
                       "model_id": model["id"], "instructions": text,
                       "mode": args.mode, "effort": args.effort}
            private_dir(path.parent)
            save_json(path, profile, replace=args.replace)
            emit({"agent": runtime.public_profile(profile)})
        elif args.command == "list":
            folder = runtime.config / "agents"
            emit({"agents": [runtime.public_profile(runtime.profile(p.stem))
                             for p in sorted(folder.glob("*.json"))]})
        else:
            return run_agents(runtime, args)
        return 0
    except Failure as error:
        emit({"error": {"code": error.code, "message": error.message}})
        return 1
    except KeyboardInterrupt:
        emit({"error": {"code": "CANCELLED", "message": "Run was cancelled."}})
        return 130
    except Exception:
        emit({"error": {"code": "RUNTIME_FAILED", "message": "Local agent operation failed."}})
        return 1


if __name__ == "__main__":
    sys.exit(main())
