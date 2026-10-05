package sandbox

import (
	"context"
	"sync"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/sys"
)

// WASI snapshot preview1 errno values (wasip1.Errno iota order).
const (
	errnoSuccess = 0
	errnoBadf    = 8  // EBADF: bad file descriptor
	errnoFault   = 21 // EFAULT: bad address
	errnoInval   = 28 // EINVAL: invalid argument
	errnoNosys   = 52 // ENOSYS: function not implemented
)

// simulatedClockNanos is the fixed timestamp returned by clock_time_get.
// The sandbox supplies deterministic simulated time instead of the host
// clock so guest behavior is reproducible.
const simulatedClockNanos int64 = 1_700_000_000_000_000_000

// instantiateEnv provides the env.jsFunctionProxy import. The stock qjs
// adapter routes this to a Go function registry; the capability-free harness
// never creates Go-to-JS proxies, so the stub is never invoked. It returns 0
// (a valid JS integer) to stay safe if it ever is.
func (e *Engine) instantiateEnv(ctx context.Context) error {
	_, err := e.rt.NewHostModuleBuilder("env").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, jsCtx uint32, thisVal uint64, argc uint32, argv uint32) uint64 {
			return 0
		}).
		Export("jsFunctionProxy").
		Instantiate(ctx)
	return err
}

// instantiateWASI provides the wasi_snapshot_preview1 imports with
// capability-free stubs. Every filesystem, path, environment, and argument
// import is denied. Only fd_write (bounded capture for stdout/stderr),
// fd_read (EOF on stdin), fd_fdstat_get (character device for fd 0/1/2),
// clock_time_get (fixed simulated time), and poll_oneoff (non-blocking) are
// implemented, because the QuickJS std/os modules need them to run pure
// computation. No socket, process, or random imports exist to provide.
//
// The stubs are engine-wide: per-instance state (captured output) is routed
// through the engine's instance registry keyed by the calling guest module,
// so any number of isolated instances can share one runtime.
func (e *Engine) instantiateWASI(ctx context.Context) error {
	b := e.rt.NewHostModuleBuilder("wasi_snapshot_preview1")

	b.NewFunctionBuilder().WithFunc(e.argsGet).Export("args_get")
	b.NewFunctionBuilder().WithFunc(e.argsSizesGet).Export("args_sizes_get")
	b.NewFunctionBuilder().WithFunc(e.environGet).Export("environ_get")
	b.NewFunctionBuilder().WithFunc(e.environSizesGet).Export("environ_sizes_get")
	b.NewFunctionBuilder().WithFunc(e.clockTimeGet).Export("clock_time_get")
	b.NewFunctionBuilder().WithFunc(e.fdClose).Export("fd_close")
	b.NewFunctionBuilder().WithFunc(e.fdFdstatGet).Export("fd_fdstat_get")
	b.NewFunctionBuilder().WithFunc(e.fdFdstatSetFlags).Export("fd_fdstat_set_flags")
	b.NewFunctionBuilder().WithFunc(e.fdPrestatGet).Export("fd_prestat_get")
	b.NewFunctionBuilder().WithFunc(e.fdPrestatDirName).Export("fd_prestat_dir_name")
	b.NewFunctionBuilder().WithFunc(e.fdRead).Export("fd_read")
	b.NewFunctionBuilder().WithFunc(e.fdReaddir).Export("fd_readdir")
	b.NewFunctionBuilder().WithFunc(e.fdSeek).Export("fd_seek")
	b.NewFunctionBuilder().WithFunc(e.fdWrite).Export("fd_write")
	b.NewFunctionBuilder().WithFunc(e.pathCreateDirectory).Export("path_create_directory")
	b.NewFunctionBuilder().WithFunc(e.pathFilestatGet).Export("path_filestat_get")
	b.NewFunctionBuilder().WithFunc(e.pathFilestatSetTimes).Export("path_filestat_set_times")
	b.NewFunctionBuilder().WithFunc(e.pathOpen).Export("path_open")
	b.NewFunctionBuilder().WithFunc(e.pathRemoveDirectory).Export("path_remove_directory")
	b.NewFunctionBuilder().WithFunc(e.pathRename).Export("path_rename")
	b.NewFunctionBuilder().WithFunc(e.pathUnlinkFile).Export("path_unlink_file")
	b.NewFunctionBuilder().WithFunc(e.pollOneoff).Export("poll_oneoff")
	b.NewFunctionBuilder().WithFunc(e.procExit).Export("proc_exit")

	_, err := b.Instantiate(ctx)
	return err
}

// The stubs below use explicit wasm signatures via WithFunc; wazero maps
// uint32 -> i32 and uint64 -> i64.

func (e *Engine) argsGet(ctx context.Context, mod api.Module, argv, argvBuf uint32) uint32 {
	return errnoSuccess
}

func (e *Engine) argsSizesGet(ctx context.Context, mod api.Module, argc, argvLen uint32) uint32 {
	mem := mod.Memory()
	_ = mem.WriteUint32Le(argc, 0)
	_ = mem.WriteUint32Le(argvLen, 0)
	return errnoSuccess
}

func (e *Engine) environGet(ctx context.Context, mod api.Module, environ, environBuf uint32) uint32 {
	return errnoSuccess
}

func (e *Engine) environSizesGet(ctx context.Context, mod api.Module, environCount, environLen uint32) uint32 {
	mem := mod.Memory()
	_ = mem.WriteUint32Le(environCount, 0)
	_ = mem.WriteUint32Le(environLen, 0)
	return errnoSuccess
}

func (e *Engine) clockTimeGet(ctx context.Context, mod api.Module, id uint32, precision uint64, time uint32) uint32 {
	_ = id
	_ = precision
	_ = mod.Memory().WriteUint64Le(time, uint64(simulatedClockNanos))
	return errnoSuccess
}

func (e *Engine) fdClose(ctx context.Context, mod api.Module, fd uint32) uint32 {
	_ = ctx
	_ = mod
	_ = fd
	return errnoNosys
}

// fdFdstatGet reports fd 0/1/2 as character devices so the QuickJS std
// helpers can probe stdin/stdout/stderr; every other fd is denied.
func (e *Engine) fdFdstatGet(ctx context.Context, mod api.Module, fd, fdstat uint32) uint32 {
	_ = ctx
	if fd > 2 {
		return errnoBadf
	}
	mem := mod.Memory()
	// 24-byte fdstat: filetype(u8) @0, flags(u16) @2, rights_base(u64) @8,
	// rights_inheriting(u64) @16.
	var buf [24]byte
	buf[0] = 2 // FILETYPE_CHARACTER_DEVICE
	rights := uint64(0)
	switch fd {
	case 0:
		rights = 1 << 1 // FD_READ
	case 1, 2:
		rights = 1 << 6 // FD_WRITE
	}
	writeU64Le(buf[8:], rights)
	writeU64Le(buf[16:], rights)
	if !mem.Write(fdstat, buf[:]) {
		return errnoFault
	}
	return errnoSuccess
}

func (e *Engine) fdFdstatSetFlags(ctx context.Context, mod api.Module, fd, flags uint32) uint32 {
	_ = ctx
	_ = mod
	_ = fd
	_ = flags
	return errnoNosys
}

func (e *Engine) fdPrestatGet(ctx context.Context, mod api.Module, fd, prestat uint32) uint32 {
	_ = ctx
	_ = mod
	_ = fd
	_ = prestat
	return errnoNosys
}

func (e *Engine) fdPrestatDirName(ctx context.Context, mod api.Module, fd, path, pathLen uint32) uint32 {
	_ = ctx
	_ = mod
	_ = fd
	_ = path
	_ = pathLen
	return errnoNosys
}

// fdRead returns EOF (0 bytes) for stdin and denies every other fd.
func (e *Engine) fdRead(ctx context.Context, mod api.Module, fd, iovs, iovsLen, nread uint32) uint32 {
	_ = ctx
	_ = iovs
	_ = iovsLen
	if fd != 0 {
		return errnoBadf
	}
	if !mod.Memory().WriteUint32Le(nread, 0) {
		return errnoFault
	}
	return errnoSuccess
}

func (e *Engine) fdReaddir(ctx context.Context, mod api.Module, fd, buf, bufLen uint32, cookie uint64, bufUsed uint32) uint32 {
	_ = ctx
	_ = mod
	_ = fd
	_ = buf
	_ = bufLen
	_ = cookie
	_ = bufUsed
	return errnoNosys
}

func (e *Engine) fdSeek(ctx context.Context, mod api.Module, fd uint32, offset uint64, whence, newoffset uint32) uint32 {
	_ = ctx
	_ = mod
	_ = fd
	_ = offset
	_ = whence
	_ = newoffset
	return errnoNosys
}

// fdWrite captures stdout (fd 1) and stderr (fd 2) into the calling
// instance's bounded output buffer and denies every other fd. The calling
// guest module identifies the instance through the engine registry.
func (e *Engine) fdWrite(ctx context.Context, mod api.Module, fd, iovs, iovsLen, nwritten uint32) uint32 {
	_ = ctx
	if fd != 1 && fd != 2 {
		return errnoBadf
	}
	inst := e.instanceFor(mod)
	if inst == nil {
		return errnoBadf
	}
	mem := mod.Memory()
	var total uint32
	for j := uint32(0); j < iovsLen; j++ {
		offset := iovs + j*8
		bufOffset, ok := mem.ReadUint32Le(offset)
		if !ok {
			return errnoFault
		}
		bufLen, ok := mem.ReadUint32Le(offset + 4)
		if !ok {
			return errnoFault
		}
		data, ok := mem.Read(bufOffset, bufLen)
		if !ok {
			return errnoFault
		}
		inst.output.append(data)
		total += bufLen
	}
	if !mem.WriteUint32Le(nwritten, total) {
		return errnoFault
	}
	return errnoSuccess
}

func (e *Engine) pathCreateDirectory(ctx context.Context, mod api.Module, fd, path, pathLen uint32) uint32 {
	_ = ctx
	_ = mod
	_ = fd
	_ = path
	_ = pathLen
	return errnoNosys
}

func (e *Engine) pathFilestatGet(ctx context.Context, mod api.Module, fd, flags, path, pathLen, filestat uint32) uint32 {
	_ = ctx
	_ = mod
	_ = fd
	_ = flags
	_ = path
	_ = pathLen
	_ = filestat
	return errnoNosys
}

func (e *Engine) pathFilestatSetTimes(ctx context.Context, mod api.Module, fd, flags, path, pathLen uint32, atim, mtim uint64, fstFlags uint32) uint32 {
	_ = ctx
	_ = mod
	_ = fd
	_ = flags
	_ = path
	_ = pathLen
	_ = atim
	_ = mtim
	_ = fstFlags
	return errnoNosys
}

func (e *Engine) pathOpen(ctx context.Context, mod api.Module, fd, dirflags, path, pathLen, oflags uint32, rightsBase, rightsInheriting uint64, fdflags, openedFd uint32) uint32 {
	_ = ctx
	_ = mod
	_ = fd
	_ = dirflags
	_ = path
	_ = pathLen
	_ = oflags
	_ = rightsBase
	_ = rightsInheriting
	_ = fdflags
	_ = openedFd
	return errnoNosys
}

func (e *Engine) pathRemoveDirectory(ctx context.Context, mod api.Module, fd, path, pathLen uint32) uint32 {
	_ = ctx
	_ = mod
	_ = fd
	_ = path
	_ = pathLen
	return errnoNosys
}

func (e *Engine) pathRename(ctx context.Context, mod api.Module, fd, oldPath, oldPathLen, newFd, newPath, newPathLen uint32) uint32 {
	_ = ctx
	_ = mod
	_ = fd
	_ = oldPath
	_ = oldPathLen
	_ = newFd
	_ = newPath
	_ = newPathLen
	return errnoNosys
}

func (e *Engine) pathUnlinkFile(ctx context.Context, mod api.Module, fd, path, pathLen uint32) uint32 {
	_ = ctx
	_ = mod
	_ = fd
	_ = path
	_ = pathLen
	return errnoNosys
}

// pollOneoff is non-blocking: it reports zero events so the guest event loop
// never blocks on the host. Guest timers (setTimeout) therefore do not fire;
// generated applications are event-driven from the host instead.
func (e *Engine) pollOneoff(ctx context.Context, mod api.Module, in, out, nsubscriptions, nevents uint32) uint32 {
	_ = ctx
	_ = in
	_ = out
	_ = nsubscriptions
	if !mod.Memory().WriteUint32Le(nevents, 0) {
		return errnoFault
	}
	return errnoSuccess
}

// procExit mirrors wazero's default: close the module with the exit code and
// panic with ExitError so no guest code runs after the call.
func (e *Engine) procExit(ctx context.Context, mod api.Module, exitCode uint32) {
	_ = mod.CloseWithExitCode(ctx, exitCode)
	panic(sys.NewExitError(exitCode))
}

// boundedOutput is a concurrency-safe, size-capped stdout/stderr capture.
type boundedOutput struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

func (o *boundedOutput) append(p []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.truncated {
		return
	}
	// Copy: the caller's slice views guest linear memory and may be
	// invalidated by later guest execution.
	cp := make([]byte, len(p))
	copy(cp, p)
	if len(o.buf)+len(cp) > o.limit {
		o.buf = append(o.buf, cp[:o.limit-len(o.buf)]...)
		o.truncated = true
		return
	}
	o.buf = append(o.buf, cp...)
}

func (o *boundedOutput) string() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return string(o.buf)
}

func writeU64Le(b []byte, v uint64) {
	for k := 0; k < 8; k++ {
		b[k] = byte(v >> (8 * k))
	}
}
