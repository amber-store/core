// Package tarextract extracts a PAX tar (as produced by tarexport) into a
// directory, restoring permissions, ownership (when running as root), extended
// attributes (best-effort), nanosecond mtimes, symlinks, fifos, and device
// nodes. Directory permissions and mtimes are applied after all members are
// written, so a read-only or past-dated directory does not block writing its
// children — and creating children does not disturb a restored directory's
// mtime.
package tarextract

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const xattrPrefix = "SCHILY.xattr."

// Extract reads a tar from r and materializes it under destDir, which is created
// if necessary. destDir itself keeps default attributes (the export root's own
// metadata is not part of the archive).
func Extract(r io.Reader, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	var dirs []*tar.Header // directories, for deferred metadata
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		// Archive-wide metadata, not a member; `git archive` writes one.
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		target, err := safeJoin(destDir, h.Name)
		if err != nil {
			return err
		}
		// safeJoin is lexical. An earlier symlink member could still lead
		// MkdirAll/OpenFile out of destDir.
		if err := rejectSymlinkComponents(destDir, target); err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, h)
		case tar.TypeReg:
			if err := writeRegular(target, tr); err != nil {
				return err
			}
			if err := applyMeta(target, h, false); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
			if err := applyMeta(target, h, true); err != nil {
				return err
			}
		case tar.TypeFifo:
			if err := unix.Mkfifo(target, uint32(h.Mode&0o7777)); err != nil {
				return fmt.Errorf("%s: mkfifo: %w", target, err)
			}
			if err := applyMeta(target, h, false); err != nil {
				return err
			}
		case tar.TypeChar, tar.TypeBlock:
			mode := uint32(h.Mode & 0o7777)
			if h.Typeflag == tar.TypeChar {
				mode |= unix.S_IFCHR
			} else {
				mode |= unix.S_IFBLK
			}
			dev := int(unix.Mkdev(uint32(h.Devmajor), uint32(h.Devminor)))
			if err := unix.Mknod(target, mode, dev); err != nil {
				if isPrivilegeError(err) {
					fmt.Fprintf(os.Stderr, "amber-store: skipping device node %s: %v\n", target, err)
					continue
				}
				return fmt.Errorf("%s: mknod: %w", target, err)
			}
			if err := applyMeta(target, h, false); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: unsupported tar type %q", target, h.Typeflag)
		}
	}
	// Apply directory metadata last so child writes do not disturb it and a
	// read-only mode does not block them.
	for _, h := range dirs {
		target, err := safeJoin(destDir, h.Name)
		if err != nil {
			return err
		}
		if err := applyMeta(target, h, false); err != nil {
			return err
		}
	}
	return nil
}

// rejectSymlinkComponents fails if any existing component of path below
// dest is not a real directory.
func rejectSymlinkComponents(dest, path string) error {
	for p := path; strings.HasPrefix(p, dest+string(os.PathSeparator)); p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("refusing to extract through non-directory %s", p)
		}
	}
	return nil
}

// safeJoin joins name under dest, rejecting any name that escapes dest.
// It rejects names that contain ".." components (in any position) as well as
// absolute paths, so that a tar archive cannot write outside of destDir.
func safeJoin(dest, name string) (string, error) {
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if part == ".." {
			return "", fmt.Errorf("refusing unsafe entry name %q", name)
		}
	}
	clean := filepath.Clean("/" + name)
	target := filepath.Join(dest, clean)
	if target != dest && !strings.HasPrefix(target, dest+string(os.PathSeparator)) {
		return "", fmt.Errorf("refusing unsafe entry name %q", name)
	}
	return target, nil
}

func writeRegular(target string, r io.Reader) error {
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// applyMeta restores permissions, ownership, xattrs, and mtime for target.
// Permissions and mtime are faithful; ownership only when running as root;
// xattrs best-effort. chown runs before chmod because it clears
// setuid/setgid. mtime is set last. Symlink targets are not chmod'd and their xattrs are skipped.
func applyMeta(target string, h *tar.Header, isSymlink bool) error {
	if os.Geteuid() == 0 {
		if err := os.Lchown(target, h.Uid, h.Gid); err != nil {
			return fmt.Errorf("%s: chown: %w", target, err)
		}
	}
	if !isSymlink {
		if err := unix.Chmod(target, uint32(h.Mode&0o7777)); err != nil {
			return fmt.Errorf("%s: chmod: %w", target, err)
		}
		for k, v := range h.PAXRecords {
			if !strings.HasPrefix(k, xattrPrefix) {
				continue
			}
			name := strings.TrimPrefix(k, xattrPrefix)
			if err := unix.Lsetxattr(target, name, []byte(v), 0); err != nil {
				if isPrivilegeError(err) || err == unix.ENOTSUP {
					fmt.Fprintf(os.Stderr, "amber-store: skipping xattr %q on %s: %v\n", name, target, err)
					continue
				}
				return fmt.Errorf("setting xattr %q on %s: %w", name, target, err)
			}
		}
	}
	return setMtime(target, h.ModTime.UnixNano(), isSymlink)
}

func setMtime(target string, ns int64, isSymlink bool) error {
	ts := unix.NsecToTimespec(ns)
	flags := 0
	if isSymlink {
		flags = unix.AT_SYMLINK_NOFOLLOW
	}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, target, []unix.Timespec{ts, ts}, flags); err != nil {
		return fmt.Errorf("%s: set mtime: %w", target, err)
	}
	return nil
}

func isPrivilegeError(err error) bool {
	return err == unix.EPERM || err == unix.EACCES || err == unix.EOPNOTSUPP
}
