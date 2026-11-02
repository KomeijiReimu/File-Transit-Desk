#!/usr/bin/env python3
"""Small launcher acceptance check with native tool stubs, never the real server/config.

Run: python scripts/check-dev-launchers.py [--powershell]
Artifacts stay under .opencode; the same scenarios run on Windows CI.
"""

import argparse
import ctypes
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time


STUB = r'''
package main
import ("encoding/json"; "fmt"; "net/http"; "os"; "os/exec"; "os/signal"; "path/filepath"; "strconv"; "strings"; "syscall"; "time")
func main() {
  args := os.Args[1:]
  if len(args)>0 && args[0]=="child" { for { time.Sleep(time.Second) } }
  name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
  role := "backend"
  if name=="go" { role="build" } else if name=="bun" { role="frontend"; if len(args)>0 && args[0]=="install" { role="install" } }
  cwd,_ := os.Getwd()
  event := map[string]interface{}{"role":role,"args":args,"cwd":cwd,"pid":os.Getpid(),"transfer":os.Getenv("VITE_TRANSFER_ORIGIN"),"proxy":os.Getenv("VITE_BACKEND_ORIGIN")}
  if os.Getenv("STUB_CHILD")==role {
    executable,err:=os.Executable(); if err!=nil {panic(err)}
    child := exec.Command(executable, "child")
    child.Stdout=os.Stdout; child.Stderr=os.Stderr
    if err:=child.Start(); err!=nil { panic(err) }
    event["child"]=child.Process.Pid
  }
  f,err:=os.OpenFile(os.Getenv("STUB_TRACE"),os.O_CREATE|os.O_APPEND|os.O_WRONLY,0600); if err!=nil {panic(err)}
  json.NewEncoder(f).Encode(event); f.Close()
  if os.Getenv("STUB_HANG")==role { for { time.Sleep(time.Second) } }
  if os.Getenv("STUB_FAIL")==role { code,_:=strconv.Atoi(os.Getenv("STUB_CODE")); fmt.Fprintln(os.Stderr,"intentional native failure"); os.Exit(code) }
  if role=="build" { executable,err:=os.Executable(); if err!=nil {panic(err)}; self,err:=os.ReadFile(executable); if err!=nil {panic(err)}; if err:=os.WriteFile(args[2],self,0755); err!=nil {panic(err)}; return }
  if role=="install" { return }
  if role=="backend" && os.Getenv("STUB_DRAIN_STARTED")!="" {
    signals:=make(chan os.Signal,2); signal.Notify(signals,os.Interrupt,syscall.SIGTERM)
    go func(){
      <-signals
      os.WriteFile(os.Getenv("STUB_DRAIN_STARTED"),[]byte("draining"),0600)
      for { if _,err:=os.Stat(os.Getenv("STUB_DRAIN_RELEASE"));err==nil {break}; time.Sleep(50*time.Millisecond) }
      os.WriteFile(os.Getenv("STUB_DRAIN_COMPLETE"),[]byte("completed"),0600)
      os.Exit(0)
    }()
  }
  port:=os.Getenv("STUB_BACKEND_PORT"); if role=="frontend" { port=os.Getenv("STUB_FRONTEND_PORT") }
  http.HandleFunc("/",func(w http.ResponseWriter,r *http.Request){ fmt.Fprint(w,"stub ready") })
  if err:=http.ListenAndServe("127.0.0.1:"+port,nil); err!=nil {panic(err)}
}
'''

# Acceptance-only helper: attach to a NEW_CONSOLE test fixture, not the CI/user
# console. Production relies on the user's shared-console Ctrl+C, with no interop.
CONSOLE_INTERRUPT = r'''
import ctypes, sys, time
kernel = ctypes.windll.kernel32
kernel.FreeConsole()
if not kernel.AttachConsole(int(sys.argv[1])):
    raise ctypes.WinError()
if not kernel.SetConsoleCtrlHandler(None, True):
    raise ctypes.WinError()
if not kernel.GenerateConsoleCtrlEvent(0, 0):
    raise ctypes.WinError()
time.sleep(0.2)
'''


def unused_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def alive(pid):
    if os.name == "nt":
        kernel = ctypes.windll.kernel32
        kernel.OpenProcess.restype = ctypes.c_void_p
        kernel.GetExitCodeProcess.argtypes = [ctypes.c_void_p, ctypes.POINTER(ctypes.c_ulong)]
        kernel.CloseHandle.argtypes = [ctypes.c_void_p]
        handle = kernel.OpenProcess(0x1000, False, pid)
        if not handle:
            return False
        code = ctypes.c_ulong()
        kernel.GetExitCodeProcess(handle, ctypes.byref(code))
        kernel.CloseHandle(handle)
        return code.value == 259
    try:
        os.kill(pid, 0)
        status = Path(f"/proc/{pid}/stat")
        return not status.exists() or status.read_text().split(")", 1)[1].strip()[0] != "Z"
    except ProcessLookupError:
        return False


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--powershell", action="store_true")
    args = parser.parse_args()
    source = Path(__file__).resolve().parent
    artifacts = source.parent / ".opencode" / "validation"
    artifacts.mkdir(parents=True, exist_ok=True)
    work = Path(tempfile.mkdtemp(prefix="launcher 空格-", dir=artifacts))
    root = work / "repo 中文 path"
    (root / "scripts").mkdir(parents=True)
    (root / "backend").mkdir()
    (root / "frontend").mkdir()
    for name in ("dev.sh", "dev.ps1", "dev-job.cs"):
        shutil.copy2(source / name, root / "scripts" / name)
    (root / "backend/config.example.yaml").write_text("admin:\n  username: admin\n  password_hash: replace-me\n", encoding="utf-8")
    (root / "backend/settings.example.yaml").write_text("server: {}\n", encoding="utf-8")
    tools = work / "tools"
    tools.mkdir()
    tmp = work / "tmp"
    tmp.mkdir()
    env = dict(os.environ, TMPDIR=str(tmp), TMP=str(tmp), TEMP=str(tmp))
    stub_source = tools / "stub.go"
    stub_source.write_text(STUB, encoding="utf-8")
    extension = ".exe" if os.name == "nt" else ""
    stub = tools / ("stub" + extension)
    subprocess.run(["go", "build", "-o", str(stub), str(stub_source)], env=env, check=True)
    for name in ("go", "bun"):
        shutil.copy2(stub, tools / (name + extension))
    env["PATH"] = str(tools) + os.pathsep + env["PATH"]
    env["BACKEND_CONFIG"] = "custom 中文/config space.yaml"
    env["INSTALL_TIMEOUT_SECONDS"] = "2"
    env["BUILD_TIMEOUT_SECONDS"] = "10"
    env["BACKEND_READY_TIMEOUT_SECONDS"] = "3"
    env["FRONTEND_READY_TIMEOUT_SECONDS"] = "3"
    env["FRONTEND_TRANSFER_ORIGIN"] = "https://transfer.example.test"
    env["BACKEND_PORT"] = str(unused_port())
    env["FRONTEND_PORT"] = str(unused_port())
    env["BACKEND_ORIGIN"] = "http://127.0.0.1:" + env["BACKEND_PORT"]
    env["STUB_BACKEND_PORT"] = env["BACKEND_PORT"]
    env["STUB_FRONTEND_PORT"] = env["FRONTEND_PORT"]
    env["STUB_TRACE"] = str(work / "trace.jsonl")
    command = ["pwsh", "-NoLogo", "-NoProfile", "-File", str(root / "scripts/dev.ps1")] if args.powershell else ["bash", str(root / "scripts/dev.sh")]

    def events():
        trace = Path(env["STUB_TRACE"])
        return [json.loads(line) for line in trace.read_text(encoding="utf-8").splitlines()] if trace.exists() else []

    def run(expected, label="", **overrides):
        Path(env["STUB_TRACE"]).unlink(missing_ok=True)
        result = subprocess.run(command, cwd=work, env=dict(env, **overrides), capture_output=True, text=True, encoding="utf-8", timeout=40)
        assert result.returncode == expected, (result.returncode, expected, result.stdout, result.stderr)
        trace = events()
        # Give job/process-group termination a short, bounded grace for kernel teardown.
        deadline = time.monotonic() + 3
        owned = [event[key] for event in trace for key in ("pid", "child") if key in event]
        while any(alive(pid) for pid in owned) and time.monotonic() < deadline:
            time.sleep(0.05)
        assert not any(alive(pid) for pid in owned), ("leaked owned processes", trace)
        print(f"PASS exit={expected} {label or overrides or 'first-run paired templates'}")
        return trace

    run(1)
    config = root / env["BACKEND_CONFIG"]
    settings = config.parent / "settings.yaml"
    assert config.is_file() and settings.is_file()
    settings.write_text("preserve existing settings\n", encoding="utf-8")
    config.unlink()
    run(1)
    assert settings.read_text(encoding="utf-8") == "preserve existing settings\n"
    # A pre-existing listener must be refused, not adopted or killed by cleanup.
    unrelated = subprocess.Popen([str(stub)], cwd=work, env=dict(env, STUB_TRACE=str(work / "unrelated.jsonl")), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        deadline = time.monotonic() + 5
        while True:
            try:
                with socket.create_connection(("127.0.0.1", int(env["BACKEND_PORT"])), timeout=0.2):
                    break
            except OSError:
                if time.monotonic() >= deadline:
                    raise AssertionError("unrelated stub listener never started")
                time.sleep(0.05)
        trace = run(1, label="occupied listener remains alive")
        assert unrelated.poll() is None
        assert not any(event["role"] == "backend" for event in trace)
    finally:
        unrelated.terminate()
        unrelated.wait(timeout=5)
    trace = run(37, STUB_FAIL="install", STUB_CODE="37", STUB_CHILD="install")
    assert [event["role"] for event in trace] == ["install"]
    run(124, STUB_HANG="install", STUB_CHILD="install")
    run(124, STUB_HANG="build", STUB_CHILD="build", BUILD_TIMEOUT_SECONDS="2")
    trace = run(31, STUB_FAIL="backend", STUB_CODE="31", STUB_CHILD="backend")
    backend = next(event for event in trace if event["role"] == "backend")
    assert Path(backend["args"][1]) == config
    assert backend["args"][2:] == ["-dev", "-dev-frontend-port", env["FRONTEND_PORT"]]
    run(1, STUB_FAIL="backend", STUB_CODE="0")
    run(124, STUB_HANG="backend", STUB_CHILD="backend")
    trace = run(29, STUB_FAIL="frontend", STUB_CODE="29", STUB_CHILD="frontend")
    frontend = next(event for event in trace if event["role"] == "frontend")
    assert frontend["transfer"] == env["FRONTEND_TRANSFER_ORIGIN"]
    assert frontend["proxy"] == env["BACKEND_ORIGIN"]
    # One admitted-transfer stand-in stays pending beyond the old forced-kill grace.
    # POSIX drives SIGINT; Windows sends actual shared-console Ctrl+C in a private
    # test console, so the event cannot reach unrelated CI/user processes.
    Path(env["STUB_TRACE"]).unlink(missing_ok=True)
    started, release, complete = (work / name for name in ("draining", "release", "completed"))
    graceful_env = dict(env, STUB_CHILD="backend", STUB_DRAIN_STARTED=str(started), STUB_DRAIN_RELEASE=str(release), STUB_DRAIN_COMPLETE=str(complete))
    with (work / "interrupt.log").open("w+", encoding="utf-8") as output:
        # CREATE_NEW_CONSOLE=0x10; only the Windows acceptance fixture uses it.
        process = subprocess.Popen(command, cwd=work, env=graceful_env, stdout=output, stderr=output, start_new_session=not args.powershell, creationflags=0x10 if args.powershell else 0)
        try:
            deadline = time.monotonic() + 25
            while time.monotonic() < deadline:
                if "[运行] 前后端已就绪" in (work / "interrupt.log").read_text(encoding="utf-8"):
                    break
                if process.poll() is not None:
                    raise AssertionError(("exited before runtime", process.returncode))
                time.sleep(0.1)
            else:
                raise AssertionError("graceful fixture never became ready")
            if args.powershell:
                subprocess.run([sys.executable, "-c", CONSOLE_INTERRUPT, str(process.pid)], env=graceful_env, check=True, timeout=5)
            else:
                os.kill(process.pid, signal.SIGINT)
            deadline = time.monotonic() + 15
            while not started.exists() and time.monotonic() < deadline:
                time.sleep(0.05)
            assert started.exists(), "backend never received a graceful stop signal"
            backend = next(event for event in events() if event["role"] == "backend")
            time.sleep(2.2)
            assert process.poll() is None and alive(backend["pid"]), "backend was killed before admitted transfer completed"
            assert not complete.exists()
            release.touch()
            code = process.wait(timeout=10)
            if not args.powershell:
                assert code == 130
            print(f"PASS first console interruption completed launcher exit={code}")
            assert complete.exists(), "backend drain never completed"
        finally:
            release.touch()
            if process.poll() is None:
                process.terminate()
                process.wait(timeout=15)
    assert not any(alive(event[key]) for event in events() for key in ("pid", "child") if key in event)
    print("PASS runtime shutdown waits for admitted transfer release beyond old kill grace, then cleans owned trees")
    print(f"Launcher acceptance passed; artifacts: {work}")


if __name__ == "__main__":
    main()
