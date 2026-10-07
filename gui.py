"""metasearch_cli 极简操作界面（tkinter）。

设计原则（用户明确要求）：
  * 极其轻量：界面与核心操作挤在这一个文件里，核心操作就是调同目录的 metasearch_cli.exe；
  * GUI 不做任何结果展示：输入关键词 → 原生 JSON 直接打到**命令行窗口**（子进程继承本控制台
    的 stdout，Go 程序打什么这里就显示什么，一行不加一行不减）；
  * 密钥管理只保留最不容易出错的一种形态：每个 provider 只允许存一个 key，
    写入新的就覆盖/删除原有的（底层 apikey set 本身就是原位替换）。

用法：在终端里 `python gui.py` 启动（必须有控制台，JSON 才有地方打）。
"""

import os
import subprocess
import sys
import threading
import tkinter as tk
from pathlib import Path
from tkinter import ttk, messagebox

EXE = Path(__file__).with_name("metasearch_cli.exe")
PROVIDERS = ["qianfan", "exa", "tavily", "serpapi", "tinyfish", "anysearch", "metaso"]


def exe_ready() -> bool:
    return EXE.is_file()


def run_search(keyword: str) -> None:
    """跑一次搜索：原生 JSON 直接输出到当前控制台，不做任何包装。"""
    subprocess.run(
        [str(EXE), keyword, "-limit", "15"],
        cwd=str(EXE.parent),
    )


def save_key(provider: str, key: str) -> int:
    """写入并保存 key：写入新的就删掉原来有的（apikey set = 原位替换，每个 provider 只存一个）。"""
    return subprocess.run(
        [str(EXE), "apikey", "set", provider, key],
        cwd=str(EXE.parent),
    ).returncode


def delete_key(provider: str) -> int:
    return subprocess.run(
        [str(EXE), "apikey", "unset", provider],
        cwd=str(EXE.parent),
    ).returncode


class App:
    def __init__(self, root):
        self.root = root
        root.title("metasearch_cli")
        root.resizable(False, False)

        pad = {"padx": 8, "pady": 4}
        frm = ttk.Frame(root)
        frm.grid(sticky="nsew", **pad)

        # ---- 第 0 行：关键词 + 搜索 ----
        ttk.Label(frm, text="关键词:").grid(row=0, column=0, sticky="e", **pad)
        self.query = ttk.Entry(frm, width=42)
        self.query.grid(row=0, column=1, sticky="we", **pad)
        self.query.bind("<Return>", lambda _e: self.on_search())
        self.btn_search = ttk.Button(frm, text="搜索", command=self.on_search)
        self.btn_search.grid(row=0, column=2, **pad)

        # ---- 第 1 行：引擎选择 ----
        ttk.Label(frm, text="引擎:").grid(row=1, column=0, sticky="e", **pad)
        self.provider = ttk.Combobox(
            frm, values=PROVIDERS, state="readonly", width=12
        )
        self.provider.set(PROVIDERS[0])
        self.provider.grid(row=1, column=1, sticky="w", **pad)

        # ---- 第 2 行：API Key + 保存/删除 ----
        ttk.Label(frm, text="API Key:").grid(row=2, column=0, sticky="e", **pad)
        self.key = ttk.Entry(frm, width=42)
        self.key.grid(row=2, column=1, sticky="we", **pad)
        self.btn_save = ttk.Button(frm, text="保存密钥", command=self.on_save)
        self.btn_save.grid(row=2, column=2, sticky="we", **pad)
        self.btn_del = ttk.Button(frm, text="删除密钥", command=self.on_delete)
        self.btn_del.grid(row=3, column=2, sticky="we", padx=8)

        # ---- 状态行 ----
        self.status = tk.StringVar(value="JSON 结果直接打印在本命令行窗口")
        ttk.Label(
            frm, textvariable=self.status, foreground="#666"
        ).grid(row=4, column=0, columnspan=3, sticky="w", **pad)

        frm.columnconfigure(1, weight=1)

    # ---------- 事件 ----------
    def on_search(self):
        kw = self.query.get().strip()
        if not kw:
            self.status.set("请输入关键词")
            return
        self._run("搜索中…（JSON 输出到控制台）", lambda: run_search(kw))

    def on_save(self):
        p = self.provider.get()
        k = self.key.get().strip()
        if not k:
            self.status.set("请输入 API Key")
            return
        self._run(
            "保存密钥中…",
            lambda: save_key(p, k),
            done=lambda rc: (
                self.status.set(
                    f"{p} 密钥已保存（旧密钥已被覆盖）"
                    if rc == 0
                    else f"保存失败（rc=%d，详见控制台）" % rc
                ),
                self.key.delete(0, tk.END),
            ),
        )

    def on_delete(self):
        p = self.provider.get()
        self._run(
            "删除密钥中…",
            lambda: delete_key(p),
            done=lambda rc: self.status.set(
                f"{p} 密钥已删除"
                if rc == 0
                else f"删除失败（rc=%d，详见控制台）" % rc
            ),
        )

    # ---------- 后台执行 ----------
    def _run(self, msg, fn, done=None):
        self._set_buttons(False)
        self.status.set(msg)

        def worker():
            rc = 0
            try:
                fn()
            except Exception as exc:
                print("执行失败: %s" % exc, file=sys.stderr)
                rc = 1

            def finish():
                self._set_buttons(True)
                if done:
                    done(rc)

            self.root.after(0, finish)

        threading.Thread(target=worker, daemon=True).start()

    def _set_buttons(self, enabled):
        state = "normal" if enabled else "disabled"
        self.btn_search["state"] = state
        self.btn_save["state"] = state
        self.btn_del["state"] = state


def main():
    if not exe_ready():
        root = tk.Tk()
        root.withdraw()
        messagebox.showerror(
            "metasearch_cli", "未找到 metasearch_cli.exe（需与本脚本同目录）"
        )
        sys.exit(1)

    print("== metasearch_cli 控制台就绪：搜索的 JSON 会直接打印在这里 ==\n")
    root = tk.Tk()
    App(root)
    root.mainloop()


if __name__ == "__main__":
    main()
