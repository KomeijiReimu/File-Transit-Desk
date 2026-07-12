#!/usr/bin/env python3
"""通过后端 Argon2id CLI 生成管理员密码配置值。"""

import argparse
import getpass
from pathlib import Path
import subprocess
import sys
from typing import Optional


def read_password(argument_password: Optional[str]) -> str:
    if argument_password is not None:
        print(
            "警告：通过进程参数传入密码可能被进程列表或 shell 历史记录读取。",
            file=sys.stderr,
        )
        return argument_password
    if not sys.stdin.isatty():
        return sys.stdin.read().rstrip("\r\n")
    password = getpass.getpass("请输入管理员密码：")
    confirm = getpass.getpass("请再次输入管理员密码：")
    if password != confirm:
        raise SystemExit("错误：两次输入的密码不一致。")
    return password


def main() -> None:
    parser = argparse.ArgumentParser(description="生成管理员 Argon2id 密码哈希")
    parser.add_argument(
        "password",
        nargs="?",
        help="兼容选项：直接传入密码（存在进程参数泄露风险，建议使用隐藏输入）。",
    )
    parser.add_argument(
        "--format",
        choices=("yaml", "phc", "legacy-sha256"),
        default="yaml",
        help="输出格式，默认 yaml。legacy-sha256 仅用于短期回滚。",
    )
    args = parser.parse_args()
    password = read_password(args.password)
    if password == "":
        raise SystemExit("错误：管理员密码不能为空。")
    if len(password.encode("utf-8")) > 1024:
        raise SystemExit("错误：管理员密码不能超过 1024 字节。")

    repo_root = Path(__file__).resolve().parents[1]
    backend = repo_root / "backend"
    completed = subprocess.run(
        ["go", "run", "./cmd/hash-admin-password", "--format", args.format],
        cwd=backend,
        input=password + "\n",
        text=True,
        capture_output=True,
        shell=False,
        check=False,
    )
    if completed.returncode != 0:
        message = completed.stderr.strip() or "管理员密码哈希生成失败。"
        raise SystemExit(message)
    sys.stdout.write(completed.stdout)


if __name__ == "__main__":
    main()
