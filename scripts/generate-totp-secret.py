#!/usr/bin/env python3
"""生成可写入 auth.totp_secret 的 Base32 TOTP Secret。"""

import argparse
import base64
import os


def main() -> None:
    parser = argparse.ArgumentParser(description="生成 Base32 TOTP Secret")
    parser.add_argument(
        "--bytes",
        type=int,
        default=20,
        help="随机字节数，默认 20 字节，低于 10 字节会被拒绝",
    )
    args = parser.parse_args()

    if args.bytes < 10:
        raise SystemExit("错误：TOTP Secret 至少需要 10 字节随机数。")

    # TOTP 标准工具通常接受无填充 Base32；去掉 = 可减少复制到配置时的歧义。
    secret = base64.b32encode(os.urandom(args.bytes)).decode("ascii").rstrip("=")
    print(secret)


if __name__ == "__main__":
    main()
