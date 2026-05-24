#!/usr/bin/env python3
"""生成可写入 auth.admin.password_sha256 的管理员密码 SHA-256 摘要。"""

import argparse
import getpass
import hashlib
import sys
from typing import Optional


def read_password(argument_password: Optional[str]) -> str:
    if argument_password is not None:
        return argument_password

    if not sys.stdin.isatty():
        # 管道输入时只移除末尾换行，保持与 printf '%s' 的无额外换行语义一致。
        return sys.stdin.read().rstrip("\n")

    password = getpass.getpass("请输入管理员密码：")
    confirm = getpass.getpass("请再次输入管理员密码：")
    if password != confirm:
        raise SystemExit("错误：两次输入的密码不一致。")
    return password


def main() -> None:
    parser = argparse.ArgumentParser(description="生成管理员密码 SHA-256 摘要")
    parser.add_argument(
        "password",
        nargs="?",
        help="可选：直接传入密码。生产环境更建议不传参数，按提示隐藏输入。",
    )
    args = parser.parse_args()

    password = read_password(args.password)
    if password == "":
        raise SystemExit("错误：管理员密码不能为空。")

    # 后端配置只保存十六进制摘要，不保存明文密码。
    print(hashlib.sha256(password.encode("utf-8")).hexdigest())


if __name__ == "__main__":
    main()
