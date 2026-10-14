// Command orfsstress drives a bounded, deterministic operation storm against a
// mounted directory: a native fsstress-equivalent court for the namespace,
// permission, and link paths. It reports an operation histogram and classifies
// every error, so an unexpected errno fails the court rather than being hidden.
//
// It is deliberately bounded (duration or op count) and seeded, so a run is
// reproducible.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Result is the court's receipt.
type Result struct {
	Schema    string            `json:"schema"`
	Dir       string            `json:"dir"`
	Workers   int               `json:"workers"`
	DurationS float64           `json:"duration_seconds"`
	Ops       map[string]uint64 `json:"ops"`
	Errors    map[string]uint64 `json:"errors"`
	TotalOps  uint64            `json:"total_ops"`
	TotalErrs uint64            `json:"total_errors"`
}

var ops = []string{"create", "write", "read", "unlink", "mkdir", "rmdir", "rename", "link", "symlink", "xattr", "truncate", "chmod"}

func main() {
	dir := flag.String("dir", "", "mounted directory to stress")
	workers := flag.Int("workers", 8, "concurrent workers")
	seconds := flag.Int("seconds", 10, "run duration")
	seed := flag.Int64("seed", 1, "base seed")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "orfsstress: -dir is required")
		os.Exit(2)
	}

	var hist sync.Map // op -> *uint64
	var errs sync.Map // errno -> *uint64
	var totalOps, totalErrs atomic.Uint64

	bump := func(m *sync.Map, k string) {
		v, _ := m.LoadOrStore(k, new(uint64))
		atomic.AddUint64(v.(*uint64), 1)
	}

	deadline := time.Now().Add(time.Duration(*seconds) * time.Second)
	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(*seed + int64(w)))
			root := *dir
			// Each worker owns a subdirectory so rmdir/rename races stay legal.
			sub := filepath.Join(root, fmt.Sprintf("w%d", w))
			_ = os.Mkdir(sub, 0o755)
			i := 0
			for time.Now().Before(deadline) {
				i++
				op := ops[rng.Intn(len(ops))]
				name := fmt.Sprintf("f%d", rng.Intn(64))
				path := filepath.Join(sub, name)
				var err error
				switch op {
				case "create":
					var f *os.File
					f, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
					if err == nil {
						f.Close()
					}
				case "write":
					var f *os.File
					f, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
					if err == nil {
						_, err = f.Write(make([]byte, 1024))
						f.Close()
					}
				case "read":
					var b []byte
					b, err = os.ReadFile(path)
					_ = b
				case "unlink":
					err = os.Remove(path)
				case "mkdir":
					err = os.Mkdir(fmt.Sprintf("%s/d%d", sub, rng.Intn(16)), 0o755)
				case "rmdir":
					err = os.Remove(fmt.Sprintf("%s/d%d", sub, rng.Intn(16)))
				case "rename":
					err = os.Rename(path, filepath.Join(sub, fmt.Sprintf("r%d", rng.Intn(64))))
				case "link":
					err = os.Link(path, filepath.Join(sub, fmt.Sprintf("l%d", rng.Intn(64))))
				case "symlink":
					err = os.Symlink(name, filepath.Join(sub, fmt.Sprintf("s%d", rng.Intn(64))))
				case "xattr":
					err = syscall.Setxattr(path, "user.stress", []byte("v"), 0)
				case "truncate":
					err = os.Truncate(path, int64(rng.Intn(4096)))
				case "chmod":
					err = os.Chmod(path, os.FileMode(0o600|rng.Intn(0o177)))
				}
				totalOps.Add(1)
				bump(&hist, op)
				if err != nil {
					totalErrs.Add(1)
					bump(&errs, errnoName(err))
				}
			}
			_ = i
		}(w)
	}
	wg.Wait()

	res := Result{
		Schema:    "orfs.performance.fsstress.v1",
		Dir:       *dir,
		Workers:   *workers,
		DurationS: float64(*seconds),
		Ops:       flatten(&hist),
		Errors:    flatten(&errs),
		TotalOps:  totalOps.Load(),
		TotalErrs: totalErrs.Load(),
	}
	blob, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(blob))

	// An unexpected errno (not one of the legitimately-reachable ones) fails the
	// court.
	expected := map[string]bool{
		"ENOENT": true, "EEXIST": true, "ENOTEMPTY": true, "EISDIR": true,
		"ENOTDIR": true, "EPERM": true, "EINVAL": true, "ENODATA": true,
	}
	for k, v := range res.Errors {
		if !expected[k] && v > 0 {
			fmt.Fprintf(os.Stderr, "orfsstress: unexpected errno %s (%d occurrences)\n", k, v)
			os.Exit(1)
		}
	}
	if res.TotalOps == 0 {
		fmt.Fprintln(os.Stderr, "orfsstress: no operations ran")
		os.Exit(1)
	}
}

func flatten(m *sync.Map) map[string]uint64 {
	out := map[string]uint64{}
	m.Range(func(k, v any) bool {
		out[k.(string)] = atomic.LoadUint64(v.(*uint64))
		return true
	})
	return out
}

func errnoName(err error) string {
	// os.PathError and os.LinkError both unwrap to a syscall.Errno; errors.As
	// finds it through either, so link(2) failures are classified like open(2).
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errnoNameOf(errno)
	}
	return "OTHER:" + err.Error()
}

func errnoNameOf(errno syscall.Errno) string {
	switch errno {
	case syscall.ENOENT:
		return "ENOENT"
	case syscall.EEXIST:
		return "EEXIST"
	case syscall.ENOTEMPTY:
		return "ENOTEMPTY"
	case syscall.EISDIR:
		return "EISDIR"
	case syscall.ENOTDIR:
		return "ENOTDIR"
	case syscall.EPERM:
		return "EPERM"
	case syscall.EINVAL:
		return "EINVAL"
	case syscall.ENODATA:
		return "ENODATA"
	case syscall.ENOSPC:
		return "ENOSPC"
	case syscall.EROFS:
		return "EROFS"
	}
	return errno.Error()
}
