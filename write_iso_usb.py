#!/usr/bin/env python3
"""Write an ISO image to a USB drive, with a progress bar."""

import glob
import json
import os
import pwd
import readline
import stat
import subprocess
import sys
import time
from pathlib import Path

# Bytes per write call.
CHUNK_SIZE = 4 * 1024 * 1024
# Flush to the device after this many bytes, so the progress bar tracks the
# drive rather than the page cache.
SYNC_EVERY = 64 * 1024 * 1024
# Width of the progress bar in characters.
BAR_WIDTH = 40
# Minimum seconds between progress redraws.
REDRAW_INTERVAL = 0.2
MIB = 1024 * 1024


def list_usb_disks():
    result = subprocess.run(
        ["lsblk", "--json", "--bytes", "--output", "PATH,SIZE,MODEL,TRAN,RM,TYPE,MOUNTPOINTS"],
        capture_output=True, text=True, check=True,
    )
    devices = json.loads(result.stdout)["blockdevices"]
    # Older lsblk emits RM as the string "0"/"1", and "0" is truthy.
    return [d for d in devices
            if d["type"] == "disk" and (d["tran"] == "usb" or d["rm"] in (True, "1"))]


def mounted_partitions(disk):
    mounts = [m for m in disk.get("mountpoints") or [] if m]
    for child in disk.get("children") or []:
        mounts += [m for m in child.get("mountpoints") or [] if m]
    return mounts


def choose_disk(disks):
    print("USB drives:")
    for i, disk in enumerate(disks, 1):
        model = (disk["model"] or "unknown").strip()
        print(f"  {i}) {disk['path']}  {disk['size'] / 1e9:.1f} GB  {model}")
    while True:
        answer = input(f"Select drive [1-{len(disks)}]: ").strip()
        if answer.isdigit() and 1 <= int(answer) <= len(disks):
            return disks[int(answer) - 1]
        print("Invalid choice.")


def expand_user(path):
    # Under sudo, HOME is root's, but "~" should mean the invoking user's home.
    sudo_user = os.environ.get("SUDO_USER")
    if sudo_user and (path == "~" or path.startswith("~/")):
        return pwd.getpwnam(sudo_user).pw_dir + path[1:]
    return os.path.expanduser(path)


def complete_path(text, state):
    matches = sorted(glob.glob(expand_user(text) + "*"))
    matches = [m + "/" if os.path.isdir(m) else m for m in matches]
    return matches[state] if state < len(matches) else None


def choose_iso():
    readline.set_completer(complete_path)
    # The whole line is one path, so spaces and slashes must not split it.
    readline.set_completer_delims("")
    readline.parse_and_bind("tab: complete")
    while True:
        answer = input("Path to ISO file: ").strip().strip("'\"")
        iso = Path(expand_user(answer))
        if iso.is_file():
            return iso
        print(f"Not a file: {iso}")


def format_progress(done, total, started):
    elapsed = max(time.monotonic() - started, 1e-6)
    fraction = done / total if total else 1.0
    filled = int(BAR_WIDTH * fraction)
    bar = "#" * filled + "-" * (BAR_WIDTH - filled)
    speed = done / elapsed
    eta = (total - done) / speed if speed else 0
    return (f"\r[{bar}] {fraction:6.1%}  {done / MIB:,.0f}/{total / MIB:,.0f} MiB"
            f"  {speed / MIB:5.1f} MiB/s  ETA {int(eta) // 60:02d}:{int(eta) % 60:02d}")


def write_image(iso, target):
    total = iso.stat().st_size
    done = 0
    unsynced = 0
    started = time.monotonic()
    last_draw = 0.0
    # No O_CREAT: if the device node vanished (drive unplugged), open() must fail
    # instead of creating a regular file in /dev. O_EXCL on a block device fails
    # with EBUSY while the kernel has it, or any of its partitions, mounted.
    fd = os.open(target, os.O_WRONLY | os.O_EXCL)
    with open(iso, "rb") as src, open(fd, "wb") as dst:
        if not stat.S_ISBLK(os.fstat(fd).st_mode):
            sys.exit(f"{target} is not a block device.")
        while chunk := src.read(CHUNK_SIZE):
            dst.write(chunk)
            done += len(chunk)
            unsynced += len(chunk)
            if unsynced >= SYNC_EVERY:
                dst.flush()
                os.fsync(fd)
                unsynced = 0
            now = time.monotonic()
            if now - last_draw >= REDRAW_INTERVAL:
                print(format_progress(done, total, started), end="", flush=True)
                last_draw = now
        print("\nFlushing remaining data...", flush=True)
        dst.flush()
        os.fsync(fd)
    print(format_progress(done, total, started))


def main():
    if os.geteuid() != 0:
        sys.exit("Run as root (sudo) to write to a block device.")

    disks = list_usb_disks()
    if not disks:
        sys.exit("No USB drives found.")
    disk = choose_disk(disks)

    mounts = mounted_partitions(disk)
    if mounts:
        sys.exit(f"{disk['path']} is mounted at {', '.join(mounts)}. Unmount it first.")

    iso = choose_iso()
    iso_size = iso.stat().st_size
    if iso_size > disk["size"]:
        sys.exit(f"ISO ({iso_size / 1e9:.1f} GB) is larger than the drive ({disk['size'] / 1e9:.1f} GB).")

    model = (disk["model"] or "unknown").strip()
    print(f"\nALL DATA ON {disk['path']} ({disk['size'] / 1e9:.1f} GB, {model}) WILL BE DESTROYED.")
    if input(f"Type the device path ({disk['path']}) to confirm: ").strip() != disk["path"]:
        sys.exit("Aborted.")

    write_image(iso, disk["path"])
    print("Done. You can remove the drive.")


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        sys.exit("\nInterrupted. The drive is likely in an unusable state; rewrite it.")
