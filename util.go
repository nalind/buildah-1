package buildah

import (
	"archive/tar"
	"bytes"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/containers/image/docker/reference"
	"github.com/containers/image/pkg/sysregistries"
	"github.com/containers/image/pkg/sysregistriesv2"
	"github.com/containers/image/types"
	"github.com/containers/storage"
	"github.com/containers/storage/pkg/archive"
	"github.com/containers/storage/pkg/chrootarchive"
	"github.com/containers/storage/pkg/idtools"
	"github.com/containers/storage/pkg/reexec"
	"github.com/opencontainers/image-spec/specs-go/v1"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/selinux/go-selinux"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// InitReexec is a wrapper for reexec.Init().  It should be called at
// the start of main(), and if it returns true, main() should return
// immediately.
func InitReexec() bool {
	return reexec.Init()
}

func copyStringStringMap(m map[string]string) map[string]string {
	n := map[string]string{}
	for k, v := range m {
		n[k] = v
	}
	return n
}

func copyStringSlice(s []string) []string {
	t := make([]string, len(s))
	copy(t, s)
	return t
}

func copyHistory(history []v1.History) []v1.History {
	if len(history) == 0 {
		return nil
	}
	h := make([]v1.History, 0, len(history))
	for _, entry := range history {
		created := entry.Created
		if created != nil {
			timestamp := *created
			created = &timestamp
		}
		h = append(h, v1.History{
			Created:    created,
			CreatedBy:  entry.CreatedBy,
			Author:     entry.Author,
			Comment:    entry.Comment,
			EmptyLayer: entry.EmptyLayer,
		})
	}
	return h
}

func convertStorageIDMaps(UIDMap, GIDMap []idtools.IDMap) ([]rspec.LinuxIDMapping, []rspec.LinuxIDMapping) {
	uidmap := make([]rspec.LinuxIDMapping, 0, len(UIDMap))
	gidmap := make([]rspec.LinuxIDMapping, 0, len(GIDMap))
	for _, m := range UIDMap {
		uidmap = append(uidmap, rspec.LinuxIDMapping{
			HostID:      uint32(m.HostID),
			ContainerID: uint32(m.ContainerID),
			Size:        uint32(m.Size),
		})
	}
	for _, m := range GIDMap {
		gidmap = append(gidmap, rspec.LinuxIDMapping{
			HostID:      uint32(m.HostID),
			ContainerID: uint32(m.ContainerID),
			Size:        uint32(m.Size),
		})
	}
	return uidmap, gidmap
}

func convertRuntimeIDMaps(UIDMap, GIDMap []rspec.LinuxIDMapping) ([]idtools.IDMap, []idtools.IDMap) {
	uidmap := make([]idtools.IDMap, 0, len(UIDMap))
	gidmap := make([]idtools.IDMap, 0, len(GIDMap))
	for _, m := range UIDMap {
		uidmap = append(uidmap, idtools.IDMap{
			HostID:      int(m.HostID),
			ContainerID: int(m.ContainerID),
			Size:        int(m.Size),
		})
	}
	for _, m := range GIDMap {
		gidmap = append(gidmap, idtools.IDMap{
			HostID:      int(m.HostID),
			ContainerID: int(m.ContainerID),
			Size:        int(m.Size),
		})
	}
	return uidmap, gidmap
}

// copyFileWithTar returns a function which copies a single file from outside
// of any container into our working container, mapping permissions using the
// container's ID maps, possibly overridden using the passed-in chownOpts
func (b *Builder) copyFileWithTar(chownOpts *idtools.IDPair, hasher io.Writer, dryRun bool) func(src, dest string) error {
	return func(src, dest string) error {
		f, err := os.Open(src)
		if err != nil {
			return errors.Wrapf(err, "error opening %q to digest its contents")
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return errors.Wrapf(err, "error reading attributes of %q")
		}

		hdr := tar.Header{
			Name:    filepath.Base(src),
			Mode:    int64(fi.Mode()),
			Size:    fi.Size(),
			ModTime: fi.ModTime(),
		}
		var buffer bytes.Buffer
		if err := tar.NewWriter(&buffer).WriteHeader(&hdr); err != nil {
			return errors.Wrapf(err, "error building header information for %q", dest)
		}
		reader := io.MultiReader(&buffer, f)

		untar := b.untar(chownOpts, hasher, dryRun)
		return untar(reader, filepath.Dir(dest))
	}
}

// copyWithTar returns a function which copies a directory tree from outside of
// any container into our working container, mapping permissions using the
// container's ID maps, possibly overridden using the passed-in chownOpts
func (b *Builder) copyWithTar(chownOpts *idtools.IDPair, excludePatterns []string, hasher io.Writer, dryRun bool) func(src, dest string) error {
	return func(src, dest string) error {
		tar := b.tarPath()
		untar := b.untar(chownOpts, hasher, dryRun)
		rc, err := tar(src, excludePatterns)
		if err != nil {
			return errors.Wrapf(err, "error archiving %q for copy", src)
		}
		defer rc.Close()
		return untar(rc, dest)
	}
}

// untarPath returns a function which extracts an archive in a specified
// location into our working container, mapping permissions using the
// container's ID maps, possibly overridden using the passed-in chownOpts
func (b *Builder) untarPath(chownOpts *idtools.IDPair, hasher io.Writer, dryRun bool) func(src, dest string) error {
	return func(src, dest string) error {
		f, err := os.Open(src)
		if err != nil {
			return errors.Wrapf(err, "error opening %q to copy its contents")
		}
		untar := b.untar(chownOpts, hasher, dryRun)
		return untar(f, dest)
	}
}

// untar returns a function which extracts an archive stream to a specified
// location in the container's filesystem, mapping permissions using the
// container's ID maps, possibly overridden using the passed-in chownOpts
func (b *Builder) untar(chownOpts *idtools.IDPair, hasher io.Writer, dryRun bool) func(tarArchive io.Reader, dest string) error {
	convertedUIDMap, convertedGIDMap := convertRuntimeIDMaps(b.IDMappingOptions.UIDMap, b.IDMappingOptions.GIDMap)
	untarMappings := idtools.NewIDMappingsFromMaps(convertedUIDMap, convertedGIDMap)
	options := &archive.TarOptions{
		UIDMaps: untarMappings.UIDs(),
		GIDMaps: untarMappings.GIDs(),
	}
	archiver := chrootarchive.NewArchiverWithChown(nil, chownOpts, untarMappings)
	if dryRun {
		archiver.Untar = func(tarArchive io.Reader, dest string, options *archive.TarOptions) error {
			_, err := io.Copy(ioutil.Discard, tarArchive)
			return err
		}
	}
	if hasher != nil {
		originalUntar := archiver.Untar
		archiver.Untar = func(tarArchive io.Reader, dest string, options *archive.TarOptions) error {
			return originalUntar(io.TeeReader(tarArchive, hasher), dest, options)
		}
	}
	return func(tarArchive io.Reader, dest string) error { return archiver.Untar(tarArchive, dest, options) }
}

// tarPath returns a function which creates an archive of a specified
// location in the container's filesystem, mapping permissions using the
// container's ID maps
func (b *Builder) tarPath() func(path string, excludePatterns []string) (io.ReadCloser, error) {
	convertedUIDMap, convertedGIDMap := convertRuntimeIDMaps(b.IDMappingOptions.UIDMap, b.IDMappingOptions.GIDMap)
	tarMappings := idtools.NewIDMappingsFromMaps(convertedUIDMap, convertedGIDMap)
	return func(path string, excludePatterns []string) (io.ReadCloser, error) {
		options := &archive.TarOptions{
			ExcludePatterns: excludePatterns,
			Compression:     archive.Uncompressed,
			UIDMaps:         tarMappings.UIDs(),
			GIDMaps:         tarMappings.GIDs(),
		}
		return archive.TarWithOptions(path, options)
	}
}

// isRegistryBlocked checks if the named registry is marked as blocked
func isRegistryBlocked(registry string, sc *types.SystemContext) (bool, error) {
	reginfo, err := sysregistriesv2.FindRegistry(sc, registry)
	if err != nil {
		return false, errors.Wrapf(err, "unable to parse the registries configuration (%s)", sysregistries.RegistriesConfPath(sc))
	}
	if reginfo != nil {
		if reginfo.Blocked {
			logrus.Debugf("registry %q is marked as blocked in registries configuration %q", registry, sysregistries.RegistriesConfPath(sc))
		} else {
			logrus.Debugf("registry %q is not marked as blocked in registries configuration %q", registry, sysregistries.RegistriesConfPath(sc))
		}
		return reginfo.Blocked, nil
	}
	logrus.Debugf("registry %q is not listed in registries configuration %q, assuming it's not blocked", registry, sysregistries.RegistriesConfPath(sc))
	return false, nil
}

// isReferenceSomething checks if the registry part of a reference is insecure or blocked
func isReferenceSomething(ref types.ImageReference, sc *types.SystemContext, what func(string, *types.SystemContext) (bool, error)) (bool, error) {
	if ref != nil && ref.DockerReference() != nil {
		if named, ok := ref.DockerReference().(reference.Named); ok {
			if domain := reference.Domain(named); domain != "" {
				return what(domain, sc)
			}
		}
	}
	return false, nil
}

// isReferenceBlocked checks if the registry part of a reference is blocked
func isReferenceBlocked(ref types.ImageReference, sc *types.SystemContext) (bool, error) {
	if ref != nil && ref.Transport() != nil {
		switch ref.Transport().Name() {
		case "docker":
			return isReferenceSomething(ref, sc, isRegistryBlocked)
		}
	}
	return false, nil
}

// ReserveSELinuxLabels reads containers storage and reserves SELinux containers
// fall all existing buildah containers
func ReserveSELinuxLabels(store storage.Store, id string) error {
	if selinux.GetEnabled() {
		containers, err := store.Containers()
		if err != nil {
			return errors.Wrapf(err, "error getting list of containers")
		}

		for _, c := range containers {
			if id == c.ID {
				continue
			} else {
				b, err := OpenBuilder(store, c.ID)
				if err != nil {
					if os.IsNotExist(errors.Cause(err)) {
						// Ignore not exist errors since containers probably created by other tool
						// TODO, we need to read other containers json data to reserve their SELinux labels
						continue
					}
					return err
				}
				// Prevent different containers from using same MCS label
				if err := label.ReserveLabel(b.ProcessLabel); err != nil {
					return errors.Wrapf(err, "error reserving SELinux label %q", b.ProcessLabel)
				}
			}
		}
	}
	return nil
}
