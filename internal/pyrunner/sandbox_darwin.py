import ctypes
import json
import mmap
from pathlib import Path
import os
import resource
import sys


def configure_worker():
    # Python is already loaded. The policy can therefore deny all subsequent execs.
    policy = ['(version 1)', '(deny default)', '(allow sysctl-read)',
              '(allow file-read* (subpath "/System/Library") (subpath "/usr/lib"))',
              '(allow file-map-executable (subpath "/usr/lib")'
              ' (subpath "/System/Library/Frameworks") (subpath "/System/Library/PrivateFrameworks"))']
    for index, path in enumerate(sys.argv[1:]):
        label = json.dumps(path, ensure_ascii=False)
        scope = "subpath" if index < 2 else "literal"
        policy.append(f'(allow file-read* ({scope} {label}))')
        # Directory traversal and Python import stat calls need ancestor metadata.
        for parent in Path(path).parents:
            policy.append(f'(allow file-read-metadata (literal {json.dumps(str(parent), ensure_ascii=False)}))')
        if index < 2:
            # dyld needs executable mapping permission when import loads a .so,
            # independently of the file read rule used for Python source.
            extensions = json.dumps(os.path.join(path, "lib-dynload"), ensure_ascii=False)
            policy.append(f'(allow file-map-executable (subpath {extensions}))')
            packages = json.dumps(os.path.join(path, "site-packages"), ensure_ascii=False)
            policy.append(f'(deny file-read* (subpath {packages}))')

    sb = ctypes.CDLL("/usr/lib/libsandbox.1.dylib")
    error = ctypes.c_char_p()
    sb.sandbox_init.argtypes = [ctypes.c_char_p, ctypes.c_uint64, ctypes.POINTER(ctypes.c_char_p)]
    if sb.sandbox_init("\n".join(policy).encode(), 0, ctypes.byref(error)):
        raise RuntimeError("cannot install macOS worker sandbox: " +
                           (error.value or b"unknown error").decode(errors="replace"))

    # Darwin counts the already-mapped dyld shared cache in the VM baseline.
    # Find the smallest permissible soft limit, then cap future growth at 256 MiB.
    budget = 256 << 20
    soft, hard = resource.getrlimit(resource.RLIMIT_AS)
    unit = 1 << 20
    upper = (1 << 48) if soft == resource.RLIM_INFINITY else soft
    low, high = 0, upper // unit
    while low < high:
        mid = (low + high) // 2
        try:
            resource.setrlimit(resource.RLIMIT_AS, (mid * unit, hard))
        except (ValueError, OSError):
            low = mid + 1
        else:
            high = mid
    cap = min(upper, high * unit + budget)
    resource.setrlimit(resource.RLIMIT_AS, (cap, cap))
    resource.setrlimit(resource.RLIMIT_FSIZE, (1 << 20, 1 << 20))
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    # A kernel that accepts but does not enforce RLIMIT_AS must fail preflight.
    try:
        allocation = mmap.mmap(-1, budget + 2 * unit)
    except (MemoryError, OSError):
        pass
    else:
        allocation.close()
        raise RuntimeError("macOS worker address-space limit is not enforced")


try:
    configure_worker()
except Exception as exc:
    print(json.dumps({"op": "error", "error": str(exc)[:500]}), flush=True)
    raise SystemExit(1)
