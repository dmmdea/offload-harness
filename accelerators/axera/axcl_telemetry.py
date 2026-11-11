#!/usr/bin/env python3
"""Full telemetry of every Axera AX650-class card on this host, read through the AXCL runtime library.

    axcl_telemetry.py [--pretty]               one JSON document on stdout, exit 0 (an unreadable card is a field, not a crash)
    axcl_telemetry.py --apply-perf [--pretty]  card CPU to its top frequency on every card, then report what it set

Why the library and not axcl-smi: axcl-smi is single-instance (a SysV shared-memory guard private to the
CLI) and other readers on the same host poll it, so a second poller gets "already running, please try
later". The runtime's own calls are separate, sequence-numbered requests over the card's control channel
and take no such lock, so this reader runs beside axcl-smi, beside other pollers and beside inference.

What it reads, per card:
  * axclrtGetDeviceProperties: firmware build, unique id, PCI address, junction temperature, card system
    memory and the CMM pool (the contiguous memory every model is loaded into), average CPU and NPU load.
  * one shell request on the card (axclrtControlExecuteShellCmd, the call axcl-smi itself uses), batched
    into a single round trip: per-core NPU utilisation (/proc/ax_proc/npu/top), the NPU and NN-bus clocks
    and their maxima (devfreq), the card CPU clock, its maximum and governor, the thermal trip points,
    the per-process CMM blocks (/proc/ax_proc/mem_cmm_info), the firmware version string.
  * axclrtGetDevicePidInfo: the host processes holding the card.
  * host sysfs: the PCIe link speed and width against the slot's capability, AER error counters, the bound
    driver.

Power and fan are not reported: the card exposes no power sensor and no fan tachometer (axcl-smi prints
"--" for both by construction). The cooling on this card is a passive heatsink with a separate host fan,
so the host's own hwmon is the place to read that fan.

The sidecar (server.py) runs this file as a child process rather than importing it, so a telemetry read
never initialises, resets or finalises the runtime inside a process that holds live inference sessions.
Standard library only.
"""
from __future__ import annotations

import ctypes
import glob
import json
import os
import re
import sys
import time

LIB_CANDIDATES = ("/usr/lib/axcl/libaxcl_rt.so",)
MAX_DEVICES = 256

# The card-side reads. The card's shell request refuses a long script (measured: a ~1.2 KB script
# failed with 0x80300405 while each short command answers in 1-3 ms), so the values go out as three
# short requests: one loop over the sysfs/proc files below (each line framed key=value), the per-core
# NPU utilisation and the CMM block table.
_CARD_FILES = {
    "cpu_cur_khz": "/sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq",
    "cpu_max_khz": "/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq",
    "cpu_governor": "/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor",
    "cpu_freqs_khz": "/sys/devices/system/cpu/cpu0/cpufreq/scaling_available_frequencies",
    "npu_cur_hz": "/sys/class/devfreq/16400000.ax_npu/cur_freq",
    "npu_max_hz": "/sys/class/devfreq/16400000.ax_npu/max_freq",
    "nnbus_cur_hz": "/sys/class/devfreq/4210000.aclk_top_nn/cur_freq",
    "nnbus_max_hz": "/sys/class/devfreq/4210000.aclk_top_nn/max_freq",
    "tz_type": "/sys/class/thermal/thermal_zone0/type",
    "tz_policy": "/sys/class/thermal/thermal_zone0/policy",
    "trip0": "/sys/class/thermal/thermal_zone0/trip_point_0_temp",
    "trip1": "/sys/class/thermal/thermal_zone0/trip_point_1_temp",
    "trip2": "/sys/class/thermal/thermal_zone0/trip_point_2_temp",
    "fw": "/proc/ax_proc/version",
    "chip": "/proc/ax_proc/chip_type",
}
_FILES_CMD = "for f in " + " ".join(_CARD_FILES.values()) + '; do echo "$f=$(cat $f 2>/dev/null)"; done'


class _Props(ctypes.Structure):
    _fields_ = [("swVersion", ctypes.c_char * 64), ("uid", ctypes.c_uint64), ("pciDomain", ctypes.c_uint32),
                ("pciBusID", ctypes.c_uint32), ("pciDeviceID", ctypes.c_uint32), ("temperature", ctypes.c_int32),
                ("totalMemSize", ctypes.c_uint32), ("freeMemSize", ctypes.c_uint32), ("totalCmmSize", ctypes.c_uint32),
                ("freeCmmSize", ctypes.c_uint32), ("cpuLoading", ctypes.c_uint32), ("npuLoading", ctypes.c_uint32),
                ("reserved", ctypes.c_uint32 * 32)]


class _DevList(ctypes.Structure):
    _fields_ = [("num", ctypes.c_uint32), ("devices", ctypes.c_int32 * MAX_DEVICES)]


class _PidInfo(ctypes.Structure):
    _fields_ = [("device", ctypes.c_uint), ("num", ctypes.c_uint), ("pid", ctypes.POINTER(ctypes.c_uint))]


_lib = None


def _load():
    global _lib
    if _lib is not None:
        return _lib
    paths = [os.environ["AXCL_RT_LIB"]] if os.environ.get("AXCL_RT_LIB") else []
    paths += list(LIB_CANDIDATES) + sorted(glob.glob("/usr/lib/axcl/libaxcl_rt.so*"))
    last = None
    for p in paths:
        try:
            lib = ctypes.CDLL(p)
            break
        except OSError as e:
            last = e
    else:
        raise OSError(f"libaxcl_rt not found ({last})")
    lib.axclInit.argtypes = [ctypes.c_char_p]
    lib.axclrtGetDeviceList.argtypes = [ctypes.POINTER(_DevList)]
    lib.axclrtSetDevice.argtypes = [ctypes.c_int32]
    lib.axclrtResetDevice.argtypes = [ctypes.c_int32]
    lib.axclrtGetDeviceProperties.argtypes = [ctypes.c_int32, ctypes.POINTER(_Props)]
    lib.axclrtGetDevicePidInfo.argtypes = [ctypes.c_int32, ctypes.POINTER(_PidInfo)]
    lib.axclrtControlExecuteShellCmd.argtypes = [ctypes.c_char_p, ctypes.c_void_p, ctypes.c_size_t,
                                                 ctypes.POINTER(ctypes.c_char_p), ctypes.c_int32]
    for fn in ("axclInit", "axclFinalize", "axclrtGetDeviceList", "axclrtSetDevice", "axclrtResetDevice",
               "axclrtGetDeviceProperties", "axclrtGetDevicePidInfo", "axclrtControlExecuteShellCmd"):
        getattr(lib, fn).restype = ctypes.c_int32
    _lib = lib
    return lib


def _err(code: int) -> str:
    return f"0x{code & 0xFFFFFFFF:08x}"


def _kv(text: str) -> dict:
    out = {}
    for line in text.splitlines():
        if "=" in line and not line.startswith("--"):
            k, _, v = line.partition("=")
            out[k.strip()] = v.strip()
    return out


def _int(v, scale=1):
    try:
        return int(str(v).split()[0]) // scale
    except (ValueError, IndexError):
        return None


def _npu_cores(text: str) -> list[dict]:
    """/proc/ax_proc/npu/top: blocks of core:/time:/period:/utilization: lines, one per (v)NPU core."""
    cores, cur = [], {}
    for line in text.splitlines():
        m = re.match(r"\s*(core|utilization|time|period)\s*:\s*(\S+)", line)
        if not m:
            continue
        k, v = m.groups()
        if k == "core" and cur:
            cores.append(cur)
            cur = {}
        if k == "utilization":
            cur["util_pct"] = _int(v.rstrip("%"))
        elif k == "core":
            cur["core"] = v
        else:
            cur[k] = _int(v)
    if cur:
        cores.append(cur)
    return [c for c in cores if "core" in c]


def _cmm_blocks(text: str) -> dict:
    """/proc/ax_proc/mem_cmm_info block lines:  |-Block: phys(..), cache =.., length=80KB(0MB), name="TDP_DEV"
    Summed per owner name in MiB (the firmware's own codec/ISP buffers carry fixed names; NPU blocks carry
    the owning process id in their name), and per pid in KiB."""
    by_owner: dict[str, int] = {}
    by_pid: dict[int, int] = {}
    for line in text.splitlines():
        m = re.search(r'length=(\d+)KB.*?name="([^"]*)"', line)
        if not m:
            continue
        kib, name = int(m.group(1)), m.group(2)
        pm = re.search(r'pid_?(\d+)', name)
        owner = re.sub(r'\d+$', '', name) if pm else name
        by_owner[owner] = by_owner.get(owner, 0) + kib
        if pm:
            by_pid[int(pm.group(1))] = by_pid.get(int(pm.group(1)), 0) + kib
    return {"by_owner": {k: round(v / 1024, 1) for k, v in sorted(by_owner.items(), key=lambda kv: -kv[1])},
            "by_pid": by_pid}


def _sh(lib, cmd: str, timeout_ms: int) -> tuple[int, str]:
    out = ctypes.c_char_p()
    rc = lib.axclrtControlExecuteShellCmd(cmd.encode(), None, 0, ctypes.byref(out), timeout_ms)
    return rc, (out.value or b"").decode(errors="replace") if rc == 0 else ""


def _host_pci(dom: int, bus: int, dev: int) -> dict:
    bdf = f"{dom:04x}:{bus:02x}:{dev:02x}.0"
    base = f"/sys/bus/pci/devices/{bdf}"

    def rd(name):
        try:
            with open(os.path.join(base, name)) as f:
                return f.read().strip()
        except OSError:
            return None

    out = {"bdf": bdf, "link_speed": rd("current_link_speed"), "link_width": _int(rd("current_link_width")),
           "max_link_speed": rd("max_link_speed"), "max_link_width": _int(rd("max_link_width"))}
    try:
        out["driver"] = os.path.basename(os.readlink(os.path.join(base, "driver")))
    except OSError:
        out["driver"] = None
    aer = {}
    for kind in ("correctable", "nonfatal", "fatal"):
        txt = rd(f"aer_dev_{kind}")
        if txt:
            m = re.search(r"TOTAL_ERR_\w+\s+(\d+)", txt)
            aer[kind] = int(m.group(1)) if m else None
    if aer:
        out["aer"] = aer
    if out["link_speed"] and out["max_link_speed"]:
        out["link_degraded"] = (out["link_speed"] != out["max_link_speed"]) or (
            out["link_width"] is not None and out["max_link_width"] is not None and out["link_width"] < out["max_link_width"])
    return out


def _exe(pid: int) -> str | None:
    try:
        return os.path.basename(os.readlink(f"/proc/{pid}/exe"))
    except OSError:
        try:
            with open(f"/proc/{pid}/comm") as f:
                return f.read().strip()
        except OSError:
            return None


def _card(lib, dev: int, timeout_ms: int) -> dict:
    card = {"device": dev}
    rc = lib.axclrtSetDevice(dev)
    if rc != 0:
        return {"device": dev, "error": f"axclrtSetDevice {_err(rc)}"}
    try:
        p = _Props()
        rc = lib.axclrtGetDeviceProperties(dev, ctypes.byref(p))
        if rc != 0:
            card["error"] = f"axclrtGetDeviceProperties {_err(rc)}"
        else:
            t = p.temperature
            card.update({
                "firmware": p.swVersion.decode(errors="replace").replace("Ax_Version", "").strip("\x00 \n"),
                "uid": f"{p.uid:016x}",
                "temp_c": round(t / 1000.0, 1) if abs(t) > 1000 else float(t),
                "mem_total_mib": p.totalMemSize // 1024, "mem_used_mib": (p.totalMemSize - p.freeMemSize) // 1024,
                "cmm_total_mib": p.totalCmmSize // 1024, "cmm_used_mib": (p.totalCmmSize - p.freeCmmSize) // 1024,
                "cmm_free_mib": p.freeCmmSize // 1024,
                "cpu_load_pct": round(p.cpuLoading / 100.0, 2), "npu_load_pct": round(p.npuLoading / 100.0, 2),
            })
            card["pcie"] = _host_pci(p.pciDomain, p.pciBusID, p.pciDeviceID)
        rc, files = _sh(lib, _FILES_CMD, timeout_ms)
        if rc == 0 and files:
            by_path = _kv(files)
            kv = {k: by_path.get(p, "") for k, p in _CARD_FILES.items()}
            _, top = _sh(lib, "cat /proc/ax_proc/npu/top", timeout_ms)
            _, cmm = _sh(lib, "cat /proc/ax_proc/mem_cmm_info", timeout_ms)
            card["clocks"] = {
                "cpu_mhz": _int(kv.get("cpu_cur_khz"), 1000), "cpu_max_mhz": _int(kv.get("cpu_max_khz"), 1000),
                "cpu_governor": kv.get("cpu_governor") or None,
                "cpu_steps_mhz": [int(x) // 1000 for x in (kv.get("cpu_freqs_khz") or "").split() if x.isdigit()],
                "npu_mhz": _int(kv.get("npu_cur_hz"), 1000000), "npu_max_mhz": _int(kv.get("npu_max_hz"), 1000000),
                "nnbus_mhz": _int(kv.get("nnbus_cur_hz"), 1000000), "nnbus_max_mhz": _int(kv.get("nnbus_max_hz"), 1000000),
            }
            c = card["clocks"]
            c["at_max"] = bool(c["cpu_mhz"] and c["cpu_mhz"] == c["cpu_max_mhz"] and c["npu_mhz"] and c["npu_mhz"] == c["npu_max_mhz"])
            trips = [_int(kv.get(f"trip{i}")) for i in range(3)]
            card["thermal"] = {"zone": kv.get("tz_type") or None, "policy": kv.get("tz_policy") or None,
                               "trips_c": [t // 1000 for t in trips if t], "hw_reset_c": 120}
            card["fw_build"] = (kv.get("fw") or "").replace("Ax_Version", "").strip() or None
            card["chip"] = kv.get("chip") or None
            cores = _npu_cores(top or "")
            card["npu_cores"] = cores
            card["npu_busy"] = any((x.get("util_pct") or 0) > 0 for x in cores)
            blocks = _cmm_blocks(cmm or "")
            card["cmm_blocks"] = blocks["by_owner"]
            card["_cmm_by_pid"] = blocks["by_pid"]
        else:
            card["card_shell_error"] = _err(rc) if rc else "empty reply"
        pi = _PidInfo()
        rc = lib.axclrtGetDevicePidInfo(dev, ctypes.byref(pi))
        procs = []
        if rc == 0:
            by = card.pop("_cmm_by_pid", {})
            for i in range(pi.num):
                pid = int(pi.pid[i])
                if pid == os.getpid():
                    continue  # this reader itself
                procs.append({"pid": pid, "exe": _exe(pid), "cmm_mib": (by.get(pid) // 1024) if pid in by else None})
            if pi.num and pi.pid:
                ctypes.CDLL(None).free(pi.pid)
        card.pop("_cmm_by_pid", None)
        card["processes"] = procs
    finally:
        lib.axclrtResetDevice(dev)
    return card


def _host_cooling() -> dict:
    """The host's own fan tachometers and chassis/ambient temperatures (hwmon), the card having none: a card
    cooled by a heatsink and a separate host fan is only as cool as that fan and the air around it."""
    fans, temps = [], []
    for hw in sorted(glob.glob("/sys/class/hwmon/hwmon*")):
        try:
            with open(os.path.join(hw, "name")) as f:
                chip = f.read().strip()
        except OSError:
            continue
        for inp in sorted(glob.glob(os.path.join(hw, "fan*_input"))):
            label = _read_text(inp.replace("_input", "_label")) or f"{chip} {os.path.basename(inp)[:-6]}"
            rpm = _int(_read_text(inp))
            if rpm is not None:
                fans.append({"label": label, "rpm": rpm})
        for inp in sorted(glob.glob(os.path.join(hw, "temp*_input"))):
            label = _read_text(inp.replace("_input", "_label")) or ""
            if re.search(r"chassis|ambient|system|board", label, re.I):
                v = _int(_read_text(inp))
                if v is not None:
                    temps.append({"label": label, "c": round(v / 1000.0, 1)})
    return {"fans": fans, "temps": temps}


def _read_text(path: str) -> str | None:
    try:
        with open(path) as f:
            return f.read().strip()
    except OSError:
        return None


def _session(fn) -> dict:
    """Run fn(lib, devices) inside one axclInit/axclFinalize pair; failures become fields, never raises."""
    t0 = time.monotonic()
    doc = {"source": "axcl-runtime", "sampled_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
    try:
        lib = _load()
    except OSError as e:
        doc["error"] = str(e)
        return doc
    rc = lib.axclInit(None)
    if rc != 0:
        doc["error"] = f"axclInit {_err(rc)}"
        return doc
    try:
        dl = _DevList()
        rc = lib.axclrtGetDeviceList(ctypes.byref(dl))
        if rc != 0:
            doc["error"] = f"axclrtGetDeviceList {_err(rc)}"
            return doc
        doc["cards"] = fn(lib, [dl.devices[i] for i in range(dl.num)])
    finally:
        lib.axclFinalize()
        doc["read_ms"] = int((time.monotonic() - t0) * 1000)
    return doc


def read(timeout_ms: int = 2000) -> dict:
    """One telemetry sample of every card plus the host's cooling. Never raises: failures are fields."""
    doc = _session(lambda lib, devs: [_card(lib, d, timeout_ms) for d in devs])
    doc["host"] = _host_cooling()
    return doc


# The card CPU runs the runtime's control plane, pre/post stages the card does itself and every PCIe
# transfer's bookkeeping; its governor ships as userspace at 1.2 GHz of a 1.5 GHz table (measured). This
# pins every policy to its own maximum: userspace -> scaling_setspeed, any other governor -> performance.
# Short on purpose: the card's shell request refuses a long script.
_PERF_CMD = ('for d in /sys/devices/system/cpu/cpufreq/policy*; do m=$(cat $d/cpuinfo_max_freq); '
             'if [ "$(cat $d/scaling_governor)" = userspace ]; then echo $m > $d/scaling_setspeed; '
             'else echo performance > $d/scaling_governor; fi; '
             'echo "${d##*/}=$(cat $d/scaling_governor),$(cat $d/scaling_cur_freq),$m"; done')


def apply_perf(timeout_ms: int = 2000) -> dict:
    """Card CPU to its top frequency on every card (volatile: the card resets with the host). The NPU and
    NN-bus devfreq clocks ship at their maxima and are reported by read(), never written here."""

    def per_card(lib, devs):
        out = []
        for dev in devs:
            rc = lib.axclrtSetDevice(dev)
            if rc != 0:
                out.append({"device": dev, "error": f"axclrtSetDevice {_err(rc)}"})
                continue
            try:
                rc, text = _sh(lib, _PERF_CMD, timeout_ms)
                if rc != 0:
                    out.append({"device": dev, "error": f"card shell {_err(rc)}"})
                    continue
                pols = {}
                for k, v in _kv(text).items():
                    gov, cur, mx = (v.split(",") + ["", "", ""])[:3]
                    pols[k] = {"governor": gov, "cpu_mhz": _int(cur, 1000), "cpu_max_mhz": _int(mx, 1000)}
                out.append({"device": dev, "policies": pols,
                            "at_max": bool(pols) and all(p["cpu_mhz"] == p["cpu_max_mhz"] for p in pols.values())})
            finally:
                lib.axclrtResetDevice(dev)
        return out

    return _session(per_card)


def main() -> int:
    doc = apply_perf() if "--apply-perf" in sys.argv else read()
    json.dump(doc, sys.stdout, indent=1 if "--pretty" in sys.argv else None)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
