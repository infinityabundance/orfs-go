// Command orfs-s3 serves a minimal WORM S3 gateway over one or more OrFS
// volumes.
//
// It does not mount FUSE: it opens the volume(s) read-write and appends through
// the library, so an object PUT is an ordinary authoritative OrFS append. See
// internal/s3gw for the protocol surface and docs/S3-GATEWAY.md for exactly
// what is and is not implemented.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/infinityabundance/orfs-go/internal/manifest"
	"github.com/infinityabundance/orfs-go/internal/record"
	"github.com/infinityabundance/orfs-go/internal/s3gw"
)

func main() { os.Exit(run(os.Args[1:])) }

// bucketMap collects repeated -bucket NAME=PATH flags.
type bucketMap map[string]string

func (m bucketMap) String() string {
	var parts []string
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ",")
}

func (m bucketMap) Set(v string) error {
	name, path, ok := strings.Cut(v, "=")
	if !ok || name == "" || path == "" {
		return fmt.Errorf("want NAME=PATH, got %q", v)
	}
	m[name] = path
	return nil
}

func run(args []string) int {
	fs := flag.NewFlagSet("orfs-s3", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addr := fs.String("addr", ":9000", "listen address")
	volume := fs.String("volume", "", "OrFS backing directory serving buckets with no explicit mapping")
	initVolume := fs.Bool("init", false, "create the -volume backing if it is not a volume yet")
	segmentSize := fs.Uint64("segment-size", record.DefaultSegmentSize, "segment size for -init")
	buckets := bucketMap{}
	fs.Var(buckets, "bucket", "map an S3 bucket to an OrFS backing, NAME=PATH (repeatable)")
	accessKey := fs.String("access-key", os.Getenv("ORFS_S3_ACCESS_KEY"), "SigV4 access key (env ORFS_S3_ACCESS_KEY)")
	secretKey := fs.String("secret-key", os.Getenv("ORFS_S3_SECRET_KEY"), "SigV4 secret key (env ORFS_S3_SECRET_KEY)")
	region := fs.String("region", "us-east-1", "SigV4 region and reported bucket location")
	retention := fs.String("default-retention", "forever",
		"bucket default WORM retention: forever | a duration (24h) | 0 for none")
	defaultHold := fs.Bool("default-legal-hold", false, "apply a legal hold to objects that send no legal-hold header")
	manifestFlag := fs.Bool("manifest", true, "publish a manifested body per PUT (cache-admitted reads)")
	manifestCache := fs.Int("manifest-cache", manifest.DefaultAdmissionCapacity, "resident manifested-body cache capacity")
	manifestPolicy := fs.String("manifest-policy", "s3fifo", "manifested-body admission policy: s3fifo | lru")
	checkpointEvery := fs.Uint64("checkpoint-every", 0, "write an index checkpoint every n transactions (0 = off)")
	groupCommit := fs.Bool("group-commit", true, "enable the log's group committer (ORFS_GROUP_COMMIT=1)")
	metricsAuth := fs.Bool("metrics-require-auth", false, "require SigV4 on /metrics")
	maxObject := fs.Int64("max-object-bytes", 0, "single-PUT body limit (0 = 64 MiB)")
	uid := fs.Uint("uid", uint(os.Getuid()), "owner UID for created objects")
	gid := fs.Uint("gid", uint(os.Getgid()), "owner GID for created objects")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "orfs-s3: unexpected arguments:", strings.Join(fs.Args(), " "))
		return 2
	}
	if *volume == "" && len(buckets) == 0 {
		fmt.Fprintln(os.Stderr, "orfs-s3: at least one of -volume or -bucket is required")
		return 2
	}
	if *accessKey == "" || *secretKey == "" {
		fmt.Fprintln(os.Stderr, "orfs-s3: -access-key and -secret-key (or ORFS_S3_ACCESS_KEY/ORFS_S3_SECRET_KEY) are required; this gateway refuses unsigned requests")
		return 2
	}
	def, err := s3gw.ParseRetention(*retention)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orfs-s3: -default-retention: %v\n", err)
		return 2
	}
	policy, err := parseManifestPolicy(*manifestPolicy)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orfs-s3: %v\n", err)
		return 2
	}

	if *groupCommit {
		// The log's group committer is opt-in via the environment (there is no
		// volume-level option); set it before any volume is opened.
		os.Setenv("ORFS_GROUP_COMMIT", "1")
	}

	vopts := s3gw.VolumeOptions{
		ManifestCacheCapacity:       *manifestCache,
		ManifestPolicy:              policy,
		DisableManifest:             !*manifestFlag,
		CheckpointEveryTransactions: *checkpointEvery,
	}

	var sets []*s3gw.VolumeSet
	closeAll := func() {
		for _, vs := range sets {
			vs.Close()
		}
	}

	cfg := s3gw.Config{
		Buckets:            map[string]*s3gw.VolumeSet{},
		Credential:         s3gw.Credential{AccessKey: *accessKey, SecretKey: *secretKey},
		Region:             *region,
		UID:                uint32(*uid),
		GID:                uint32(*gid),
		DefaultRetention:   def,
		DefaultLegalHold:   *defaultHold,
		PublishManifests:   *manifestFlag,
		MetricsRequireAuth: *metricsAuth,
		MaxObjectBytes:     *maxObject,
	}

	if *volume != "" {
		vs, err := openVolume(*volume, *initVolume, *segmentSize, vopts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "orfs-s3: open %s: %v\n", *volume, err)
			return 1
		}
		sets = append(sets, vs)
		cfg.Default = vs
	}
	for name, path := range buckets {
		vs, err := s3gw.OpenVolumeSet(path, vopts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "orfs-s3: open bucket %s at %s: %v\n", name, path, err)
			closeAll()
			return 1
		}
		sets = append(sets, vs)
		cfg.Buckets[name] = vs
	}
	defer closeAll()

	gw := s3gw.New(cfg)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           gw,
		ReadHeaderTimeout: 15 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	fmt.Fprintf(os.Stderr, "orfs-s3: listening on %s (region %s)\n", *addr, *region)
	select {
	case sig := <-sigCh:
		fmt.Fprintf(os.Stderr, "orfs-s3: %s, shutting down\n", sig)
		_ = srv.Close()
		return 0
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "orfs-s3: serve: %v\n", err)
			return 1
		}
		return 0
	}
}

func openVolume(path string, initVol bool, segmentSize uint64, vopts s3gw.VolumeOptions) (*s3gw.VolumeSet, error) {
	vs, err := s3gw.OpenVolumeSet(path, vopts)
	if err == nil {
		return vs, nil
	}
	if !initVol {
		return nil, err
	}
	return s3gw.InitVolumeSet(path, segmentSize, vopts)
}

func parseManifestPolicy(name string) (manifest.Policy, error) {
	switch name {
	case "s3fifo", "":
		return manifest.PolicyS3FIFO, nil
	case "lru":
		return manifest.PolicyLRU, nil
	}
	return manifest.PolicyS3FIFO, fmt.Errorf("unknown manifest policy %q (want s3fifo or lru)", name)
}
