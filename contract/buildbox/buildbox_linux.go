//go:build linux

package buildbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

const initArg = "--init"

func (b Box) command(ctx context.Context, dir string, argv ...string) *exec.Cmd {
	mem := b.MemoryBytes
	if mem == 0 {
		mem = DefaultMemory
	}
	args := append([]string{initArg, b.Root, dir, strconv.FormatUint(mem, 10), "--"}, argv...)
	cmd := exec.CommandContext(ctx, b.Helper, args...)
	cmd.Env = []string{}
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWNET |
			syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		// Without this the kernel refuses the gid mapping to a process
		// that has no privilege outside.
		GidMappingsEnableSetgroups: false,
		// The box dies with whoever made it.
		Pdeathsig: syscall.SIGKILL,
		Setsid:    true,
	}
	// Killing the helper kills everything: it is process 1 of the box's
	// process namespace.
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	return cmd
}

// Available makes a box and runs nothing in it but a check that it is one.
func (b Box) Available() error {
	if _, err := os.Stat(b.Helper); err != nil {
		return fmt.Errorf("the build box's helper is not installed: %w", err)
	}
	if _, err := os.Stat(filepath.Join(b.Root, "bin", "sh")); err != nil {
		return fmt.Errorf("the build box's toolchain is not installed at %s: %w", b.Root, err)
	}
	dir, err := os.MkdirTemp("", "tap-box-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	out, err := b.Run(context.Background(), dir, "true")
	if err != nil {
		return fmt.Errorf("a build box cannot be made here: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// Run runs command with sh -c, in the box, in dir, which is the only place
// it can write. It returns what the command printed.
func (b Box) Run(ctx context.Context, dir, command string) ([]byte, error) {
	timeout := b.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	cmd := b.command(ctx, abs, "/bin/sh", "-c", command)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return out.Bytes(), fmt.Errorf("the build took longer than %s and was stopped", timeout)
	}
	return out.Bytes(), err
}

// Init is the helper's second stage: it runs as process 1 inside the new
// namespaces, replaces the root, and becomes the command.
//
// args are: root, work directory, memory limit, "--", the command.
func Init(args []string) error {
	if len(args) < 5 || args[3] != "--" {
		return errors.New("usage: --init <root> <work> <memory> -- <command...>")
	}
	root, work, argv := args[0], args[1], args[4:]
	mem, err := strconv.ParseUint(args[2], 10, 64)
	if err != nil {
		return err
	}
	step := func(what string, err error) error {
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	}

	// Nothing mounted here reaches the container.
	if err := step("making mounts private", syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, "")); err != nil {
		return err
	}
	newroot, err := os.MkdirTemp("", "tap-box-root-")
	if err != nil {
		return err
	}
	if err := step("a place for the root", syscall.Mount("tmpfs", newroot, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "size=64k,mode=755")); err != nil {
		return err
	}
	bind := func(from, to string, readOnly bool) error {
		at := filepath.Join(newroot, to)
		if err := os.MkdirAll(at, 0o755); err != nil {
			return err
		}
		if err := syscall.Mount(from, at, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("binding %s: %w", to, err)
		}
		if readOnly {
			if err := syscall.Mount("", at, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV, ""); err != nil {
				return fmt.Errorf("making %s read-only: %w", to, err)
			}
		}
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		switch e.Name() {
		case "proc", "sys", "dev", "tmp", "work", "gocache":
			continue
		}
		if !e.IsDir() {
			continue
		}
		if err := bind(filepath.Join(root, e.Name()), e.Name(), true); err != nil {
			return err
		}
	}
	if err := bind(work, WorkDir, false); err != nil {
		return err
	}
	// The build cache: a copy, in the package directory, of what the image
	// compiled ahead of time. A copy and not a layer over it, because the
	// layer's writable half cannot sit on a container's own filesystem, and
	// not the original read-only, because the go command does not build
	// from a cache it cannot write. It is on disk, not in memory.
	cache := filepath.Join(work, ".box", "gocache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return err
	}
	if lower := filepath.Join(root, "gocache"); exists(lower) {
		if err := copyTree(lower, cache); err != nil {
			return fmt.Errorf("copying the build cache: %w", err)
		}
	}
	if err := bind(cache, "gocache", false); err != nil {
		return err
	}
	for _, d := range []string{"tmp", "proc", "dev"} {
		os.MkdirAll(filepath.Join(newroot, d), 0o755)
	}
	if err := step("tmp", syscall.Mount("tmpfs", filepath.Join(newroot, "tmp"), "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "size=64m,mode=1777")); err != nil {
		return err
	}
	// A proc of the box's own process namespace, where the kernel allows
	// one. A container runtime that masks parts of /proc makes the kernel
	// refuse, and then the box has no /proc at all. The container's own is
	// never used instead: the service's processes are in it, and their
	// environment can be read by the user the box runs as.
	_ = syscall.Mount("proc", filepath.Join(newroot, "proc"), "proc", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "")
	for _, d := range []string{"null", "zero", "urandom"} {
		at := filepath.Join(newroot, "dev", d)
		if f, err := os.Create(at); err == nil {
			f.Close()
		}
		if err := syscall.Mount("/dev/"+d, at, "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("binding /dev/%s: %w", d, err)
		}
	}

	old := filepath.Join(newroot, ".old")
	os.MkdirAll(old, 0o700)
	if err := step("replacing the root", syscall.PivotRoot(newroot, old)); err != nil {
		return err
	}
	if err := step("entering the root", os.Chdir("/")); err != nil {
		return err
	}
	// The container's tree goes, and with it every key and token in it.
	if err := step("detaching the container's files", syscall.Unmount("/.old", syscall.MNT_DETACH)); err != nil {
		return err
	}
	os.Remove("/.old")
	// The root itself is made read-only last.
	if err := step("making the root read-only", syscall.Mount("", "/", "", syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV, "size=64k,mode=755")); err != nil {
		return err
	}
	syscall.Sethostname([]byte("tap-box"))

	for _, l := range []struct {
		what int
		max  uint64
	}{
		{syscall.RLIMIT_AS, mem},
		{syscall.RLIMIT_FSIZE, maxFileBytes},
		{syscall.RLIMIT_CORE, 0},
		{6 /* RLIMIT_NPROC */, maxProcesses},
	} {
		if err := syscall.Setrlimit(l.what, &syscall.Rlimit{Cur: l.max, Max: l.max}); err != nil {
			return fmt.Errorf("setting a limit: %w", err)
		}
	}
	// The go command starts a telemetry process by finding its own
	// executable through /proc, which a box may not have. It is switched
	// off the only way it can be: by its mode file.
	os.MkdirAll(WorkDir+"/.home/.config/go/telemetry", 0o755)
	os.WriteFile(WorkDir+"/.home/.config/go/telemetry/mode", []byte("off 2026-01-01\n"), 0o644)
	if err := step("entering the package", os.Chdir(WorkDir)); err != nil {
		return err
	}
	return step("starting the command", syscall.Exec(argv[0], argv, Env()))
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}
