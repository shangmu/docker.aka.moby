package winlayers

import (
	"archive/tar"
	"context"
	"io"
	"runtime"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/archive"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	cerrdefs "github.com/containerd/errdefs"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

func NewFileSystemApplierWithWindows(cs content.Provider, a diff.Applier) diff.Applier {
	if runtime.GOOS == "windows" {
		return a
	}

	return &winApplier{
		cs: cs,
		a:  a,
	}
}

type winApplier struct {
	cs content.Provider
	a  diff.Applier
}

func (s *winApplier) Apply(ctx context.Context, desc ocispecs.Descriptor, mounts []mount.Mount, opts ...diff.ApplyOpt) (d ocispecs.Descriptor, err error) {
	// HACK:, containerd doesn't know about vnd.docker.image.rootfs.diff.tar.zstd, but that
	// media type is compatible w/ the oci type, so just lie and say it's the oci type
	if desc.MediaType == images.MediaTypeDockerSchema2Layer+".zstd" {
		desc.MediaType = ocispecs.MediaTypeImageLayerZstd
	}

	if !hasWindowsLayerMode(ctx) {
		return s.apply(ctx, desc, mounts, opts...)
	}

	compressed, err := images.DiffCompression(ctx, desc.MediaType)
	if err != nil {
		return ocispecs.Descriptor{}, errors.Wrapf(cerrdefs.ErrNotImplemented, "unsupported diff media type: %v", desc.MediaType)
	}

	var ocidesc ocispecs.Descriptor
	if err := mount.WithTempMount(ctx, mounts, func(root string) error {
		ra, err := s.cs.ReaderAt(ctx, desc)
		if err != nil {
			return errors.Wrap(err, "failed to get reader from content store")
		}
		defer ra.Close()

		r := content.NewReader(ra)
		if compressed != "" {
			ds, err := compression.DecompressStream(r)
			if err != nil {
				return err
			}
			defer ds.Close()
			r = ds
		}

		digester := digest.Canonical.Digester()
		rc := &readCounter{
			r: io.TeeReader(r, digester.Hash()),
		}

		rc2, discard := filter(rc, func(hdr *tar.Header) bool {
			if after, ok := strings.CutPrefix(hdr.Name, "Files/"); ok {
				hdr.Name = after
				hdr.Linkname = strings.TrimPrefix(hdr.Linkname, "Files/")
				// TODO: could convert the windows PAX headers to xattr here to reuse
				// the original ones in diff for parent directories and file modifications
				return true
			}
			return false
		})

		if _, err := archive.Apply(ctx, root, rc2); err != nil {
			discard(err)
			return err
		}

		// Read any trailing data
		if _, err := io.Copy(io.Discard, rc); err != nil {
			discard(err)
			return err
		}

		ocidesc = ocispecs.Descriptor{
			MediaType: ocispecs.MediaTypeImageLayer,
			Size:      rc.c,
			Digest:    digester.Digest(),
		}
		return nil
	}); err != nil {
		return ocispecs.Descriptor{}, err
	}
	return ocidesc, nil
}

type readCounter struct {
	r io.Reader
	c int64
}

func (rc *readCounter) Read(p []byte) (n int, err error) {
	n, err = rc.r.Read(p)
	rc.c += int64(n)
	return
}

func filter(in io.Reader, f func(*tar.Header) bool) (io.Reader, func(error)) {
	pr, pw := io.Pipe()

	rc := &readCanceler{Reader: in}

	go func() {
		tarReader := tar.NewReader(rc)
		tarWriter := tar.NewWriter(pw)

		pw.CloseWithError(func() error {
			for {
				h, err := tarReader.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
				if f(h) {
					if err := tarWriter.WriteHeader(h); err != nil {
						return err
					}
					if h.Size > 0 {
						//nolint:gosec // never read into memory
						if _, err := io.Copy(tarWriter, tarReader); err != nil {
							return err
						}
					}
				} else if h.Size > 0 {
					//nolint:gosec // never read into memory
					if _, err := io.Copy(io.Discard, tarReader); err != nil {
						return err
					}
				}
			}
			return tarWriter.Close()
		}())
	}()

	discard := func(err error) {
		rc.cancel(err)
		pw.CloseWithError(err)
	}

	return pr, discard
}

type readCanceler struct {
	mu sync.Mutex
	io.Reader
	err error
}

func (r *readCanceler) Read(b []byte) (int, error) {
	r.mu.Lock()
	if r.err != nil {
		r.mu.Unlock()
		return 0, r.err
	}
	n, err := r.Reader.Read(b)
	r.mu.Unlock()
	return n, err
}

func (r *readCanceler) cancel(err error) {
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
}
