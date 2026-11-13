// Copyright 2016 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fuse

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func getMaxWrite() int {
	return 1 << 20
}

func unixgramSocketpair() (l, r *os.File, err error) {
	fd, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, os.NewSyscallError("socketpair",
			err.(syscall.Errno))
	}
	l = os.NewFile(uintptr(fd[0]), "socketpair-half1")
	r = os.NewFile(uintptr(fd[1]), "socketpair-half2")
	return
}

// fuseTServerPath is the default location of the FUSE-T NFS server
// binary. Like libfuse-t, it can be overridden with the
// FUSE_NFSSRV_PATH environment variable.
const fuseTServerPath = "/usr/local/bin/go-nfsv4"

// fuseTMonitor is the local half of the FUSE-T monitor socket. It must
// stay open for the lifetime of the mount: FUSE-T treats its closure as
// a request to unmount.
var fuseTMonitor *os.File

// Create a FUSE FS on the specified mount point.  The returned
// mount point is always absolute. FUSE-T is preferred when installed,
// otherwise macFUSE/osxfuse is used.
func mount(mountPoint string, opts *MountOptions, ready chan<- error) (fd int, err error) {
	if bin, err := fuseTBinary(); err == nil {
		fuseTStream = true
		return mountFuseT(bin, mountPoint, opts, ready)
	}
	fuseTStream = false
	return mountOSXFuse(mountPoint, opts, ready)
}

// mountFuseT mounts through FUSE-T. Unlike macFUSE, the FUSE protocol
// is carried over a SOCK_STREAM socket shared with the go-nfsv4
// server, so requests are length-prefixed rather than message-framed
// (see readFrame).
func mountFuseT(bin string, mountPoint string, opts *MountOptions, ready chan<- error) (fd int, err error) {
	local, remote, err := unixgramSocketpair()
	if err != nil {
		return -1, err
	}
	defer local.Close()
	defer remote.Close()

	mon, remoteMon, err := unixgramSocketpair()
	if err != nil {
		return -1, err
	}
	defer remoteMon.Close()

	args := []string{}
	if opts.Debug {
		args = append(args, "-d")
	}
	// FUSE-T uses the volume name as the NFS export path, so it must
	// not contain slashes. Prefer an explicit volname= option (the
	// macFUSE convention), else fall back to the base of FsName.
	volname := ""
	namedAttr := false
	if opts.FsName != "" {
		volname = filepath.Base(opts.FsName)
	}
	for _, o := range opts.optionsStrings() {
		switch {
		case o == "ro":
			args = append(args, "-r")
		case strings.HasPrefix(o, "volname="):
			volname = strings.TrimPrefix(o, "volname=")
		case o == "namedattr":
			namedAttr = true
		}
	}
	volname = strings.ReplaceAll(volname, "/", "_")
	if volname != "" && volname != "." && volname != "_" {
		args = append(args, "--volname", volname)
	}
	// Without named attributes, the NFS client stores xattrs as
	// AppleDouble "._" files instead of issuing FUSE xattr calls. They
	// are only enabled on request (like in libfuse-t), because every
	// xattr operation then goes through short-lived attribute vnodes,
	// which makes the macOS NFS client much more likely to panic
	// ("nfs_vinvalbuf2: ubc_msync failed!, error 22", see fuse-t issue
	// #109).
	if namedAttr && !opts.DisableXAttrs {
		args = append(args, "--namedattr=true")
	}
	args = append(args, fmt.Sprintf("--rwsize=%d", opts.MaxWrite), mountPoint)

	cmd := exec.Command(bin, args...)
	cmd.ExtraFiles = []*os.File{remote, remoteMon} // fd would be (index + 3)
	cmd.Env = append(os.Environ(),
		"_FUSE_COMMFD=3",
		"_FUSE_MONFD=4",
		"_FUSE_COMMVERS=2")

	if err = cmd.Start(); err != nil {
		mon.Close()
		return -1, err
	}
	cmd.Process.Release()

	// Hand an independent copy of the comm socket to the caller,
	// which wraps it in its own *os.File (see newFuseFD).
	fd, err = syscall.Dup(int(local.Fd()))
	if err != nil {
		mon.Close()
		return -1, os.NewSyscallError("dup", err)
	}
	syscall.CloseOnExec(fd)
	syscall.CloseOnExec(int(mon.Fd()))
	fuseTMonitor = mon

	go func() {
		var err error
		if _, werr := mon.Write([]byte("mount")); werr != nil {
			err = fmt.Errorf("FUSE-T mount failed: %v", werr)
		} else {
			reply := make([]byte, 4)
			if _, rerr := mon.Read(reply); rerr != nil {
				err = fmt.Errorf("FUSE-T mount failed: %v", rerr)
			}
		}
		ready <- err
		close(ready)
	}()

	return fd, nil
}

// mountOSXFuse mounts through macFUSE/osxfuse.
func mountOSXFuse(mountPoint string, opts *MountOptions, ready chan<- error) (fd int, err error) {
	local, remote, err := unixgramSocketpair()
	if err != nil {
		return
	}

	defer local.Close()
	defer remote.Close()

	bin, err := fusermountBinary()
	if err != nil {
		return 0, err
	}

	cmd := exec.Command(bin,
		"-o", strings.Join(opts.optionsStrings(), ","),
		"-o", fmt.Sprintf("iosize=%d", opts.MaxWrite),
		mountPoint)
	cmd.ExtraFiles = []*os.File{remote} // fd would be (index + 3)
	cmd.Env = helperEnv(
		"_FUSE_CALL_BY_LIB=",
		"_FUSE_DAEMON_PATH="+os.Args[0],
		"_FUSE_COMMFD=3",
		"_FUSE_COMMVERS=2",
		"MOUNT_OSXFUSE_CALL_BY_LIB=",
		"MOUNT_OSXFUSE_DAEMON_PATH="+os.Args[0])

	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	if err = cmd.Start(); err != nil {
		return
	}

	fd, err = getConnection(local)
	if err != nil {
		return -1, err
	}

	go func() {
		// On macos, mount_osxfuse is not just a suid
		// wrapper. It interacts with the FS setup, and will
		// not exit until the filesystem has successfully
		// responded to INIT and STATFS. This means we can
		// only wait on the mount_osxfuse process after the
		// server has fully started
		if err := cmd.Wait(); err != nil {
			err = fmt.Errorf("mount_osxfusefs failed: %v. Stderr: %s, Stdout: %s",
				err, errOut.String(), out.String())
		}

		ready <- err
		close(ready)
	}()

	// golang sets CLOEXEC on file descriptors when they are
	// acquired through normal operations (e.g. open).
	// Buf for fd, we have to set CLOEXEC manually
	syscall.CloseOnExec(fd)

	return fd, err
}

func unmount(mountPoint string) error {
	return syscall.Unmount(mountPoint, 0)
}

func fusermountBinary() (string, error) {
	binPaths := []string{
		"/Library/Filesystems/macfuse.fs/Contents/Resources/mount_macfuse",
		"/Library/Filesystems/osxfuse.fs/Contents/Resources/mount_osxfuse",
	}

	for _, path := range binPaths {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("no FUSE mount utility found")
}

// fuseTBinary returns the path of the FUSE-T NFS server, or an error
// if FUSE-T is not installed.
func fuseTBinary() (string, error) {
	path := os.Getenv("FUSE_NFSSRV_PATH")
	if path == "" {
		path = fuseTServerPath
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("FUSE-T not found: %v", err)
	}
	return path, nil
}
