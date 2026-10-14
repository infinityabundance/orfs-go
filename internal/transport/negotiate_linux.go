//go:build linux

package transport

import (
	"encoding/binary"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// FUSE/io_uring wire constants, taken verbatim from this host's
// /usr/include/linux/fuse.h (FUSE_KERNEL_VERSION 7, FUSE_OVER_IO_URING
// 1ULL<<41) and /usr/include/linux/io_uring.h.
const (
	fuseInitOpcode = 26 // FUSE_INIT
	fuseKernelMaj  = 7

	// FUSE_INIT_EXT (1<<30) must be echoed in init_out.flags for the kernel to
	// honour init_out.flags2 at all.
	fuseInitExt = uint32(1) << 30
	// FUSE_OVER_IO_URING is 1ULL<<41; the flags2 field carries bits 32..63, so
	// inside flags2 the bit is 41-32 = 9.
	fuseOverIOUringFlags2 = uint32(1) << 9

	fuseAsyncRead = uint32(1) << 0
	fuseBigWrites = uint32(1) << 5

	initDataMax = 1 << 20 // plenty for INIT plus one following request
)

// Negotiation is the empirical result of attempting the FUSE INIT handshake,
// with and without FUSE_OVER_IO_URING, against the running kernel.
type Negotiation struct {
	KernelRelease string `json:"kernel_release"`
	Mounted       bool   `json:"mounted"`

	// The kernel's INIT request, as sent to the daemon.
	KernelMajor  uint32 `json:"kernel_major"`
	KernelMinor  uint32 `json:"kernel_minor"`
	KernelFlags  uint32 `json:"kernel_flags"`
	KernelFlags2 uint32 `json:"kernel_flags2"`

	// Did the kernel set FUSE_OVER_IO_URING in the INIT it sent us? That is
	// the kernel advertising the transport.
	KernelAdvertisesIOUring bool `json:"kernel_advertises_io_uring"`

	// Control: INIT replied without FUSE_OVER_IO_URING. We expect the following
	// request on /dev/fuse.
	ControlRequestOnDevFuse bool `json:"control_request_on_dev_fuse"`
	// Test: INIT replied with FUSE_OVER_IO_URING. A request no longer arriving
	// on /dev/fuse within the grace window is the observational signature of a
	// live io_uring transport. This is evidence, not proof.
	UringReplyRequestOnDevFuse *bool `json:"uring_reply_request_on_dev_fuse"`

	Note  string `json:"note"`
	Error string `json:"error,omitempty"`
}

// Negotiate attempts the real INIT handshake against the running kernel: once
// as a control (no io_uring), once requesting FUSE_OVER_IO_URING, recording
// exactly what the kernel sent and whether a follow-up request was delivered on
// the plain /dev/fuse path. mnt must be an existing, empty directory.
//
// It performs no io_uring syscalls: the goal is the negotiation, which is what
// decides whether the transport is reachable at all.
func Negotiate(mnt string, timeout time.Duration) (*Negotiation, error) {
	n := &Negotiation{
		KernelRelease: kernelRelease(),
		Note: "control = INIT replied without FUSE_OVER_IO_URING; test = INIT replied with FUSE_OVER_IO_URING. " +
			"A request ceasing to arrive on /dev/fuse is the observational signature of an io_uring connection; " +
			"it is evidence, not a completed io_uring REGISTER (this build performs no unsafe pointer syscalls).",
	}

	ctrl, err := negotiateOnce(mnt, timeout, false)
	if err != nil {
		n.Error = err.Error()
		return n, err
	}
	n.Mounted = true
	n.KernelMajor = ctrl.kernelMajor
	n.KernelMinor = ctrl.kernelMinor
	n.KernelFlags = ctrl.kernelFlags
	n.KernelFlags2 = ctrl.kernelFlags2
	n.KernelAdvertisesIOUring = ctrl.advertisesUring
	n.ControlRequestOnDevFuse = ctrl.requestAfterReply

	// A second mount is required to change the INIT reply; the flag is chosen at
	// INIT and cannot be revisited on a live connection.
	test, err := negotiateOnce(mnt, timeout, true)
	if err != nil {
		n.Error = "io_uring round: " + err.Error()
		return n, nil
	}
	v := test.requestAfterReply
	n.UringReplyRequestOnDevFuse = &v
	if test.kernelMinor != 0 {
		n.KernelMinor = test.kernelMinor
	}
	return n, nil
}

type roundResult struct {
	kernelMajor       uint32
	kernelMinor       uint32
	kernelFlags       uint32
	kernelFlags2      uint32
	advertisesUring   bool
	requestAfterReply bool
}

func negotiateOnce(mnt string, timeout time.Duration, requestUring bool) (roundResult, error) {
	var res roundResult

	fd, err := unix.Open("/dev/fuse", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return res, fmt.Errorf("open /dev/fuse: %w", err)
	}
	defer unix.Close(fd)

	var st unix.Stat_t
	if err := unix.Stat(mnt, &st); err != nil {
		return res, fmt.Errorf("stat %s: %w", mnt, err)
	}
	data := fmt.Sprintf("fd=%d,rootmode=%o,user_id=%d,group_id=%d,max_read=%d",
		fd, st.Mode&unix.S_IFMT, unix.Geteuid(), unix.Getegid(), 131072)
	if err := unix.Mount("orfs-transport-probe", mnt, "fuse.orfsprobe",
		uintptr(unix.MS_NOSUID|unix.MS_NODEV), data); err != nil {
		return res, fmt.Errorf("mount fuse at %s: %w", mnt, err)
	}
	defer func() { _ = unix.Unmount(mnt, unix.MNT_DETACH) }()

	// --- read and parse FUSE_INIT ------------------------------------------
	buf := make([]byte, initDataMax)
	n, timedOut, err := readTimeout(fd, buf, timeout)
	if err != nil {
		return res, fmt.Errorf("read INIT: %w", err)
	}
	if timedOut {
		return res, fmt.Errorf("no FUSE_INIT within %s", timeout)
	}
	if n < 40+20 {
		return res, fmt.Errorf("short INIT: %d bytes", n)
	}
	if op := binary.LittleEndian.Uint32(buf[4:8]); op != fuseInitOpcode {
		return res, fmt.Errorf("first opcode %d, want FUSE_INIT (%d)", op, fuseInitOpcode)
	}
	unique := binary.LittleEndian.Uint64(buf[8:16])
	res.kernelMajor = binary.LittleEndian.Uint32(buf[40:44])
	res.kernelMinor = binary.LittleEndian.Uint32(buf[44:48])
	res.kernelFlags = binary.LittleEndian.Uint32(buf[48:52])
	res.kernelFlags2 = binary.LittleEndian.Uint32(buf[52:56])
	res.advertisesUring = res.kernelFlags2&fuseOverIOUringFlags2 != 0

	// --- reply --------------------------------------------------------------
	if err := writeInitReply(fd, unique, res, requestUring); err != nil {
		return res, fmt.Errorf("write INIT reply: %w", err)
	}

	// --- observe whether a follow-up request still arrives on /dev/fuse -----
	// Trigger a stat of the mount root from another goroutine; if the transport
	// is io_uring, this request is delivered to the (absent) ring, not here.
	go func() {
		time.Sleep(50 * time.Millisecond)
		var s unix.Stat_t
		_ = unix.Stat(mnt, &s)
	}()
	// Drain whatever the plain path delivers until it goes quiet; if it goes
	// quiet with nothing delivered, the connection is not being served on
	// /dev/fuse.
	got := false
	for i := 0; i < 8; i++ {
		_, to, err := readTimeout(fd, buf, 200*time.Millisecond)
		if err != nil || to {
			break
		}
		got = true
	}
	res.requestAfterReply = got
	return res, nil
}

func writeInitReply(fd int, unique uint64, res roundResult, requestUring bool) error {
	out := make([]byte, 16+64)
	binary.LittleEndian.PutUint32(out[0:4], uint32(len(out)))
	binary.LittleEndian.PutUint32(out[4:8], 0)
	binary.LittleEndian.PutUint64(out[8:16], unique)

	o := out[16:]
	binary.LittleEndian.PutUint32(o[0:4], fuseKernelMaj) // major
	binary.LittleEndian.PutUint32(o[4:8], res.kernelMinor)
	binary.LittleEndian.PutUint32(o[8:12], 131072) // max_readahead
	flags := fuseInitExt | (res.kernelFlags & (fuseAsyncRead | fuseBigWrites))
	binary.LittleEndian.PutUint32(o[12:16], flags)
	binary.LittleEndian.PutUint16(o[16:18], 12) // max_background
	binary.LittleEndian.PutUint16(o[18:20], 9)  // congestion_threshold
	binary.LittleEndian.PutUint32(o[20:24], 131072)
	binary.LittleEndian.PutUint32(o[24:28], 1)  // time_gran
	binary.LittleEndian.PutUint16(o[28:30], 32) // max_pages
	binary.LittleEndian.PutUint16(o[30:32], 0)  // map_alignment
	flags2 := uint32(0)
	if requestUring {
		flags2 = fuseOverIOUringFlags2
	}
	binary.LittleEndian.PutUint32(o[32:36], flags2)
	binary.LittleEndian.PutUint32(o[36:40], 0) // max_stack_depth
	binary.LittleEndian.PutUint16(o[40:42], 0) // request_timeout

	_, err := unix.Write(fd, out)
	return err
}

// readTimeout reads once, returning (n, timedOut, err). A timed-out read leaves
// its goroutine parked on the fd; the caller's unmount aborts the connection
// and releases it.
func readTimeout(fd int, buf []byte, d time.Duration) (int, bool, error) {
	type res struct {
		n   int
		err error
	}
	ch := make(chan res, 1)
	go func() {
		n, err := unix.Read(fd, buf)
		ch <- res{n, err}
	}()
	select {
	case r := <-ch:
		return r.n, false, r.err
	case <-time.After(d):
		return 0, true, nil
	}
}
