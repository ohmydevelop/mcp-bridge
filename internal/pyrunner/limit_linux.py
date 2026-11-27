import ctypes
import errno
import resource

# Install OS limits before compiling or executing any supplied code.
resource.setrlimit(resource.RLIMIT_AS, (256 << 20, 256 << 20))
resource.setrlimit(resource.RLIMIT_FSIZE, (1 << 20, 1 << 20))
resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
lib = ctypes.CDLL("libseccomp.so.2")
lib.seccomp_init.argtypes = [ctypes.c_uint32]
lib.seccomp_init.restype = ctypes.c_void_p
lib.seccomp_syscall_resolve_name.argtypes = [ctypes.c_char_p]
lib.seccomp_rule_add.argtypes = [ctypes.c_void_p, ctypes.c_uint32, ctypes.c_int, ctypes.c_uint]
lib.seccomp_load.argtypes = [ctypes.c_void_p]
flt = lib.seccomp_init(0x00050000 | errno.EPERM)
if not flt:
    raise RuntimeError("seccomp unavailable")
for name in """read write readv writev close close_range lseek fstat newfstatat statx
stat lstat access faccessat faccessat2 getcwd readlink readlinkat getdents64 openat
fcntl ioctl dup dup2 dup3 mmap mprotect munmap mremap brk madvise futex
getpid gettid getuid geteuid getgid getegid clock_gettime gettimeofday
getrandom rt_sigaction rt_sigprocmask rt_sigreturn sigaltstack
exit exit_group uname sysinfo sched_getaffinity""".split():
    nr = lib.seccomp_syscall_resolve_name(name.encode())
    if nr >= 0 and lib.seccomp_rule_add(flt, 0x7fff0000, nr, 0):
        raise RuntimeError("seccomp rule failed")
if lib.seccomp_load(flt):
    raise RuntimeError("seccomp load failed")
