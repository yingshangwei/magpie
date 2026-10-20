import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest


RUNNER = Path(__file__).resolve().parents[1] / "scripts/agents.py"
TOKEN = "fixture-gateway-key-not-a-real-credential"
ENDPOINT = "http://127.0.0.1:19273"


class AgentRuntimeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="lane-test-")
        self.root = Path(self.temp.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.work = self.root / "work"
        self.work.mkdir()
        self.settings = self.root / "settings.json"
        self.rows = [
            {"model": "claude/sonnet-demo", "label": "Claude Sonnet Demo v1"},
            {"model": "group/sonnet-demo", "label": "Claude Sonnet Demo v2"},
            {"model": "codex/gpt-demo", "label": "GPT Demo"},
        ]
        self.write_settings()
        catalog = self.root / "codex-models.json"
        catalog.write_text(json.dumps({"models": [
            {"slug": "codex/gpt-demo", "display_name": "GPT Demo", "shell_type": "unified_exec",
             "base_instructions": "Complete the task.", "description": "GPT via Magpie"}
        ]}))
        self.prompt = self.root / "task.md"
        self.prompt.write_text("Review the supplied files.")
        self.env = dict(os.environ, HOME=str(self.root / "home"), USERPROFILE=str(self.root / "home"), PATH=str(self.bin) + os.pathsep + os.environ["PATH"],
                        MULTI_LANE_SETTINGS=str(self.settings),
                        MULTI_LANE_CONFIG_DIR=str(self.root / "config"),
                        MULTI_LANE_STATE_DIR=str(self.root / "state"),
                        MULTI_LANE_CODEX_CATALOG=str(catalog))
        self.env.pop("MULTI_LANE_RUNTIME_CONFIG", None)
        self.runner = RUNNER
        fake = """#!{python}
import json,os,pathlib,subprocess,sys,time
root=pathlib.Path({root})
args=sys.argv[1:]
kind=pathlib.Path(sys.argv[0]).name
prompt=sys.stdin.read()
route=args[args.index("--model")+1]
token=os.environ.get("ANTHROPIC_API_KEY") or os.environ.get("MULTI_LANE_API_KEY")
record={{"kind":kind,"route":route,"args":args,"cwd":os.getcwd(),"prompt":prompt,
         "pid":os.getpid(),"api_key":token,
         "config_dir":os.environ.get("CLAUDE_CONFIG_DIR") or os.environ.get("CODEX_HOME"),
         "no_proxy":os.environ.get("NO_PROXY")}}
if kind=="codex":
 record["config"]=pathlib.Path(record["config_dir"],"config.toml").read_text()
if "SLEEP_CHILD" in prompt:
 child=subprocess.Popen([sys.executable,"-c","import time; time.sleep(30)"])
 record["child_pid"]=child.pid
(root/(kind+"-capture.json")).write_text(json.dumps(record))
if "FAIL" in prompt:
 print("Magpie transport failed "+token+" "+{endpoint}+" "+route,file=sys.stderr)
 sys.exit(7)
if "SLEEP_CHILD" in prompt:
 time.sleep(30)
result="Reviewed with "+route+" via Magpie at "+{endpoint}+" token="+token
if kind=="claude":
 if "--restricted" not in args or "--safe-mode" not in args or "--tools" not in args:sys.exit(9)
 print(json.dumps({{"is_error":"ERROR_RESULT" in prompt,"result":result}}))
else:
 pathlib.Path(args[args.index("--output-last-message")+1]).write_text(result)
 print(json.dumps({{"type":"done","debug":result}}))
""".format(python=sys.executable, root=repr(str(self.root)), endpoint=repr(ENDPOINT))
        for name in ("claude", "codex"):
            path = self.bin / name
            path.write_text(fake)
            path.chmod(0o700)

    def tearDown(self):
        self.temp.cleanup()

    def write_settings(self):
        self.settings.write_text(json.dumps({
            "env": {"ANTHROPIC_BASE_URL": ENDPOINT, "ANTHROPIC_AUTH_TOKEN": TOKEN},
            "modelPicker": {"options": self.rows},
        }))

    def cli(self, *args, expected=0):
        result = subprocess.run([sys.executable, "-B", str(self.runner), *args], env=self.env,
                                capture_output=True, text=True, timeout=15)
        self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
        self.assertNotIn(TOKEN, result.stdout + result.stderr)
        self.assertNotIn(ENDPOINT, result.stdout + result.stderr)
        self.assertNotIn("magpie", (result.stdout + result.stderr).lower())
        return [json.loads(line) for line in result.stdout.splitlines()]

    def use_private_config(self):
        source = self.root / "private/runtime.json"
        source.parent.mkdir(exist_ok=True)
        source.write_text(json.dumps({
            "schema": 1, "endpoint": ENDPOINT, "token_env": "LANE_TEST_KEY",
            "models": [{"route": row["model"], "name": row["label"]} for row in self.rows],
        }))
        self.env["MULTI_LANE_RUNTIME_CONFIG"] = str(source)
        self.env["LANE_TEST_KEY"] = TOKEN
        return source

    def create(self, name="reviewer", engine="claude-code", model="sonnet-demo-v1", *extra):
        return self.cli("create", name, "--engine", engine, "--model", model,
                        "--instructions", "Check facts against the supplied files.", *extra)[0]

    def run_args(self, *names, timeout="5"):
        args = ["run", "--cwd", str(self.work), "--prompt-file", str(self.prompt),
                "--output", str(self.root / "output"), "--timeout", timeout]
        for name in names:
            args += ["--agent", name]
        return args

    def assert_clean_output(self, output):
        for path in Path(output).rglob("*"):
            if path.is_file():
                text = path.read_text()
                for secret in (TOKEN, ENDPOINT, "claude/sonnet-demo",
                               "group/sonnet-demo", "codex/gpt-demo", "Magpie"):
                    self.assertNotIn(secret, text)

    def test_catalog_ids_stable_and_sources_distinct(self):
        first = self.cli("models")[0]["models"]
        self.assertEqual(len({row["id"] for row in first}), 3)
        for row in first:
            self.assertEqual(set(row), {"id", "name", "alias"})
        self.rows = list(reversed(self.rows)) + [{"model": "vendor/new", "label": "New Model"}]
        self.write_settings()
        second = {row["name"]: row["id"] for row in self.cli("models")[0]["models"]}
        for row in first:
            self.assertEqual(row["id"], second[row["name"]])

    def test_private_config_runs_both_engines_without_claude_settings(self):
        self.use_private_config()
        self.settings.unlink()
        self.create("reviewer")
        self.create("coder", "codex", "gpt-demo")
        result = self.cli(*self.run_args("reviewer", "coder"))[-1]
        self.assertEqual(result["status"], "completed")
        self.assert_clean_output(result["output_dir"])
        for engine in ("claude", "codex"):
            capture = json.loads((self.root / (engine + "-capture.json")).read_text())
            self.assertEqual(capture["api_key"], TOKEN)

    def test_copy_is_self_contained_and_creates_no_local_state_inside_skill(self):
        source = self.use_private_config()
        self.settings.unlink()
        installed = self.root / "installed/multi-lane"
        shutil.copytree(RUNNER.parent.parent, installed, ignore=shutil.ignore_patterns("__pycache__"))
        before = {str(p.relative_to(installed)): p.read_bytes()
                  for p in installed.rglob("*") if p.is_file()}
        self.runner = installed / "scripts/agents.py"
        self.create()
        result = self.cli(*self.run_args("reviewer"))[-1]
        self.assertEqual(result["status"], "completed")
        after = {str(p.relative_to(installed)): p.read_bytes()
                 for p in installed.rglob("*") if p.is_file()}
        self.assertEqual(after, before)
        self.assertNotIn(TOKEN, source.read_text())
        self.assertFalse((self.root / "home/.claude").exists())
        self.assertFalse((self.root / "home/.codex").exists())

    def test_default_private_config_takes_precedence_and_preserves_model_binding(self):
        original = self.create()["agent"]["model"]["id"]
        source = self.use_private_config()
        config = Path(self.env["MULTI_LANE_CONFIG_DIR"])
        source.replace(config / "runtime.json")
        self.env.pop("MULTI_LANE_RUNTIME_CONFIG")
        self.settings.write_text("unreadable legacy JSON")
        self.assertEqual(self.cli("list")[0]["agents"][0]["model"]["id"], original)
        self.assertEqual(self.cli(*self.run_args("reviewer"))[-1]["status"], "completed")

    def test_explicit_config_errors_never_fall_back_to_working_legacy_settings(self):
        source = self.use_private_config()
        valid = json.loads(source.read_text())
        cases = [
            ("invalid JSON", "CONFIG_UNAVAILABLE"),
            (json.dumps({**valid, "schema": 2}), "CONFIG_INVALID"),
            (json.dumps({**valid, "token": TOKEN}), "CONFIG_INVALID"),
            (json.dumps({**valid, "models": []}), "CATALOG_EMPTY"),
        ]
        for data, code in cases:
            with self.subTest(code=code):
                source.write_text(data)
                self.assertEqual(self.cli("models", expected=1)[0]["error"]["code"], code)
        source.unlink()
        self.assertEqual(self.cli("models", expected=1)[0]["error"]["code"], "CONFIG_UNAVAILABLE")

    def test_missing_or_empty_referenced_key_does_not_become_unauthenticated(self):
        self.use_private_config()
        for value in (None, "", " "):
            with self.subTest(value=value):
                if value is None:
                    self.env.pop("LANE_TEST_KEY", None)
                else:
                    self.env["LANE_TEST_KEY"] = value
                error = self.cli("check", expected=1)[0]["error"]["code"]
                self.assertEqual(error, "CONNECTION_UNAVAILABLE")

    def test_portable_config_without_key_supports_unauthenticated_loopback(self):
        source = self.use_private_config()
        data = json.loads(source.read_text())
        del data["token_env"]
        source.write_text(json.dumps(data))
        self.env.pop("LANE_TEST_KEY")
        self.create()
        self.cli(*self.run_args("reviewer"))
        capture = json.loads((self.root / "claude-capture.json").read_text())
        self.assertEqual(capture["api_key"], "local-runtime")

    def test_catalog_rejects_account_or_connection_data_in_display_names(self):
        for label in ("Example user@example.invalid", ENDPOINT, TOKEN, "provider/model", "/private/home"):
            with self.subTest(label=label):
                self.rows[0]["label"] = label
                self.write_settings()
                self.assertEqual(self.cli("models", expected=1)[0]["error"]["code"], "CATALOG_INVALID")

    def test_loopback_connection_with_empty_key(self):
        data = json.loads(self.settings.read_text())
        data["env"]["ANTHROPIC_AUTH_TOKEN"] = ""
        self.settings.write_text(json.dumps(data))
        self.assertTrue(self.cli("check")[0]["connection_configured"])
        self.create()
        result = self.cli(*self.run_args("reviewer"))[-1]
        self.assertEqual(result["status"], "completed")
        capture = json.loads((self.root / "claude-capture.json").read_text())
        self.assertEqual(capture["api_key"], "local-runtime")
        self.assertNotIn("local-runtime", (Path(result["output_dir"]) / "reviewer.md").read_text())

    def test_ambiguous_display_name_requires_exact_id(self):
        self.rows += [{"model": "vendor/gpt-demo", "label": "GPT Demo"}]
        self.write_settings()
        error = self.cli("create", "ambiguous", "--engine", "claude-code",
                         "--model", "gpt-demo", "--instructions", "Review", expected=1)[0]
        self.assertEqual(error["error"]["code"], "MODEL_AMBIGUOUS")
        model = self.cli("models", "--match", "GPT")[0]["models"][1]
        self.create("specific", "claude-code", model["id"])

    def test_profile_pins_route_when_name_changes_and_fails_if_route_disappears(self):
        profile = self.create()["agent"]
        self.rows[0]["label"] = "Claude Renamed Model v1"
        self.write_settings()
        visible = self.cli("list")[0]["agents"][0]
        self.assertEqual(visible["model"]["id"], profile["model"]["id"])
        self.assertEqual(visible["model"]["name"], "Claude Renamed Model v1")
        self.rows = self.rows[1:]
        self.write_settings()
        error = self.cli(*self.run_args("reviewer"), expected=1)[0]
        self.assertEqual(error["error"]["code"], "MODEL_UNAVAILABLE")
        self.assertFalse((self.root / "claude-capture.json").exists())

    def test_missing_engine_capabilities_does_not_start_a_fallback_model(self):
        self.create("coder", "codex", "gpt-demo")
        Path(self.env["MULTI_LANE_CODEX_CATALOG"]).write_text('{"models":[]}')
        result = self.cli(*self.run_args("coder"), expected=1)[-1]
        self.assertEqual(result["agents"][0]["error"]["code"], "MODEL_UNAVAILABLE")
        self.assertFalse((self.root / "codex-capture.json").exists())

    def test_remote_endpoint_does_not_start_an_agent(self):
        self.create()
        data = json.loads(self.settings.read_text())
        data["env"]["ANTHROPIC_BASE_URL"] = "https://example.invalid"
        self.settings.write_text(json.dumps(data))
        error = self.cli(*self.run_args("reviewer"), expected=1)[0]
        self.assertEqual(error["error"]["code"], "CONNECTION_UNAVAILABLE")
        self.assertFalse((self.root / "claude-capture.json").exists())

    def test_both_engines_receive_exact_models_and_return_clean_results(self):
        self.create("reviewer")
        self.create("coder", "codex", "gpt-demo", "--mode", "write", "--effort", "high")
        response = self.cli(*self.run_args("reviewer", "coder"))[-1]
        self.assertEqual(response["status"], "completed")
        self.assertEqual({a["status"] for a in response["agents"]}, {"completed"})
        self.assert_clean_output(response["output_dir"])
        claude = json.loads((self.root / "claude-capture.json").read_text())
        codex = json.loads((self.root / "codex-capture.json").read_text())
        self.assertEqual(claude["route"], "claude/sonnet-demo")
        self.assertEqual(codex["route"], "codex/gpt-demo")
        self.assertEqual(claude["api_key"], TOKEN)
        self.assertEqual(codex["api_key"], TOKEN)
        self.assertEqual(claude["cwd"], str(self.work.resolve()))
        self.assertEqual(claude["args"][claude["args"].index("--tools") + 1], "Read,Grep,Glob")
        self.assertEqual(codex["args"][codex["args"].index("--sandbox") + 1], "workspace-write")
        self.assertIn('base_url = "' + ENDPOINT + '/v1"', codex["config"])
        self.assertIn("127.0.0.1", claude["no_proxy"])
        for capture in (claude, codex):
            self.assertNotIn(TOKEN, "\n".join(capture["args"]))
            self.assertFalse(Path(capture["config_dir"]).exists())
        saved = (self.root / "config/agents/reviewer.json").read_text()
        self.assertNotIn("claude/sonnet-demo", saved)
        self.assertNotIn(TOKEN, saved)

    def test_failed_run_does_not_reuse_previous_result_or_expose_stderr(self):
        self.create()
        first = self.cli(*self.run_args("reviewer"))[-1]
        self.prompt.write_text("FAIL")
        second = self.cli(*self.run_args("reviewer"), expected=1)[-1]
        self.assertNotEqual(first["run_id"], second["run_id"])
        self.assertEqual(second["agents"][0]["error"]["code"], "ENGINE_FAILED")
        self.assertFalse((Path(second["output_dir"]) / "reviewer.md").exists())
        self.assert_clean_output(second["output_dir"])
        for path in (self.root / "state").rglob("*.log"):
            self.assertNotIn(TOKEN, path.read_text())
            self.assertNotIn(ENDPOINT, path.read_text())

    def test_claude_error_result_is_not_reported_as_success(self):
        self.create()
        self.prompt.write_text("ERROR_RESULT")
        response = self.cli(*self.run_args("reviewer"), expected=1)[-1]
        self.assertEqual(response["agents"][0]["status"], "failed")
        self.assertFalse((Path(response["output_dir"]) / "reviewer.md").exists())

    def assert_process_gone(self, pid):
        for _ in range(40):
            try:
                os.kill(pid, 0)
            except ProcessLookupError:
                return
            time.sleep(0.05)
        self.fail("Child process survived cleanup")

    def test_timeout_terminates_agent_process_group(self):
        self.create()
        self.prompt.write_text("SLEEP_CHILD")
        result = self.cli(*self.run_args("reviewer", timeout="0.4"), expected=1)[-1]
        self.assertEqual(result["agents"][0]["error"]["code"], "TIMEOUT")
        capture = json.loads((self.root / "claude-capture.json").read_text())
        self.assert_process_gone(capture["pid"])
        self.assert_process_gone(capture["child_pid"])

    def assert_cancelled_process_group(self, signum):
        self.create()
        self.prompt.write_text("SLEEP_CHILD")
        process = subprocess.Popen([sys.executable, str(RUNNER), *self.run_args("reviewer")],
                                   env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        capture_path = self.root / "claude-capture.json"
        for _ in range(100):
            if capture_path.exists():
                break
            time.sleep(0.02)
        self.assertTrue(capture_path.exists())
        process.send_signal(signum)
        stdout, stderr = process.communicate(timeout=10)
        self.assertEqual(process.returncode, 130, stdout + stderr)
        capture = json.loads(capture_path.read_text())
        self.assert_process_gone(capture["pid"])
        self.assert_process_gone(capture["child_pid"])

    def test_interrupt_terminates_active_agent_process_group(self):
        self.assert_cancelled_process_group(signal.SIGINT)

    def test_termination_terminates_active_agent_process_group(self):
        self.assert_cancelled_process_group(signal.SIGTERM)


if __name__ == "__main__":
    unittest.main(verbosity=2)
