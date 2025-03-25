package tarfile

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/internal/private"
	"github.com/containers/image/v5/internal/set"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/types"
	"github.com/opencontainers/go-digest"
	"github.com/sirupsen/logrus"
)

// Writer allows creating a (docker save)-formatted tar archive containing one or more images.
type Writer struct {
	mutex sync.Mutex
	// ALL of the following members can only be accessed with the mutex held.
	// Use Writer.lock() to obtain the mutex.
	writer io.Writer
	tar    *tar.Writer // nil if the Writer has already been closed.
	// Other state.
	blobs            map[digest.Digest]types.BlobInfo // list of already-sent blobs
	repositories     map[string]map[string]string
	legacyLayers     *set.Set[string] // A set of IDs of legacy layers that have been already sent.
	manifest         []ManifestItem
	manifestByConfig map[digest.Digest]int // A map from config digest to an entry index in manifest above.
}

// NewWriter returns a Writer for the specified io.Writer.
// The caller must eventually call .Close() on the returned object to create a valid archive.
func NewWriter(dest io.Writer) *Writer {
	return &Writer{
		writer:           dest,
		tar:              tar.NewWriter(dest),
		blobs:            make(map[digest.Digest]types.BlobInfo),
		repositories:     map[string]map[string]string{},
		legacyLayers:     set.New[string](),
		manifestByConfig: map[digest.Digest]int{},
	}
}

// lock does some sanity checks and locks the Writer.
// If this function succeeds, the caller must call w.unlock.
// Do not use Writer.mutex directly.
func (w *Writer) lock() error {
	w.mutex.Lock()
	if w.tar == nil {
		w.mutex.Unlock()
		return errors.New("Internal error: trying to use an already closed tarfile.Writer")
	}
	return nil
}

// unlock releases the lock obtained by Writer.lock
// Do not use Writer.mutex directly.
func (w *Writer) unlock() {
	w.mutex.Unlock()
}

// tryReusingBlobLocked checks whether the transport already contains, a blob, and if so, returns its metadata.
// info.Digest must not be empty.
// If the blob has been successfully reused, returns (true, info, nil).
// If the transport can not reuse the requested blob, tryReusingBlob returns (false, {}, nil); it returns a non-nil error only on an unexpected failure.
// The caller must have locked the Writer.
func (w *Writer) tryReusingBlobLocked(info types.BlobInfo) (bool, private.ReusedBlob, error) {
	if info.Digest == "" {
		return false, private.ReusedBlob{}, errors.New("Can not check for a blob with unknown digest")
	}
	if blob, ok := w.blobs[info.Digest]; ok {
		return true, private.ReusedBlob{Digest: info.Digest, Size: blob.Size}, nil
	}
	return false, private.ReusedBlob{}, nil
}

// recordBlob records metadata of a recorded blob, which must contain at least a digest and size.
// The caller must have locked the Writer.
func (w *Writer) recordBlobLocked(info types.BlobInfo) {
	w.blobs[info.Digest] = info
}

// ensureSingleLegacyLayerLocked writes legacy VERSION and configuration files for a single layer
// The caller must have locked the Writer.
func (w *Writer) ensureSingleLegacyLayerLocked(layerID string, layerDigest digest.Digest, configBytes []byte) error {
	if !w.legacyLayers.Contains(layerID) {
		// Create a symlink for the legacy format, where there is one subdirectory per layer ("image").
		// See also the comment in physicalLayerPath.
		physicalLayerPath, err := w.physicalLayerPath(layerDigest)
		if err != nil {
			return err
		}
		if err := w.sendSymlinkLocked(filepath.Join(layerID, legacyLayerFileName), filepath.Join("..", physicalLayerPath)); err != nil {
			return fmt.Errorf("creating layer symbolic link: %w", err)
		}

		b := []byte("1.0")
		if err := w.sendBytesLocked(filepath.Join(layerID, legacyVersionFileName), b); err != nil {
			return fmt.Errorf("writing VERSION file: %w", err)
		}

		if err := w.sendBytesLocked(filepath.Join(layerID, legacyConfigFileName), configBytes); err != nil {
			return fmt.Errorf("writing config json file: %w", err)
		}

		w.legacyLayers.Add(layerID)
	}
	return nil
}

// writeLegacyMetadataLocked writes legacy layer metadata and records tags for a single image.
func (w *Writer) writeLegacyMetadataLocked(layerDescriptors []manifest.Schema2Descriptor, configBytes []byte, repoTags []reference.NamedTagged) error {
	var chainID digest.Digest
	lastLayerID := ""
	for i, l := range layerDescriptors {
		// The legacy format requires a config file per layer
		layerConfig := make(map[string]any)

		// The root layer doesn't have any parent
		if lastLayerID != "" {
			layerConfig["parent"] = lastLayerID
		}
		// The top layer configuration file is generated by using subpart of the image configuration
		if i == len(layerDescriptors)-1 {
			var config map[string]*json.RawMessage
			err := json.Unmarshal(configBytes, &config)
			if err != nil {
				return fmt.Errorf("unmarshaling config: %w", err)
			}
			for _, attr := range [7]string{"architecture", "config", "container", "container_config", "created", "docker_version", "os"} {
				layerConfig[attr] = config[attr]
			}
		}

		// This chainID value matches the computation in docker/docker/layer.CreateChainID …
		if err := l.Digest.Validate(); err != nil { // This should never fail on this code path, still: make sure the chainID computation is unambiguous.
			return err
		}
		if chainID == "" {
			chainID = l.Digest
		} else {
			chainID = digest.Canonical.FromString(chainID.String() + " " + l.Digest.String())
		}
		// … but note that the image ID does not _exactly_ match docker/docker/image/v1.CreateID, primarily because
		// we create the image configs differently in details. At least recent versions allocate new IDs on load,
		// so this is fine as long as the IDs we use are unique / cannot loop.
		//
		// For intermediate images, we could just use the chainID as an image ID, but using a digest of ~the created
		// config makes sure that everything uses the same “namespace”; a bit less efficient but clearer.
		//
		// Temporarily add the chainID to the config, only for the purpose of generating the image ID.
		layerConfig["layer_id"] = chainID
		b, err := json.Marshal(layerConfig) // Note that layerConfig["id"] is not set yet at this point.
		if err != nil {
			return fmt.Errorf("marshaling layer config: %w", err)
		}
		delete(layerConfig, "layer_id")
		layerID := digest.Canonical.FromBytes(b).Encoded()
		layerConfig["id"] = layerID

		configBytes, err := json.Marshal(layerConfig)
		if err != nil {
			return fmt.Errorf("marshaling layer config: %w", err)
		}

		if err := w.ensureSingleLegacyLayerLocked(layerID, l.Digest, configBytes); err != nil {
			return err
		}

		lastLayerID = layerID
	}

	if lastLayerID != "" {
		for _, repoTag := range repoTags {
			if val, ok := w.repositories[repoTag.Name()]; ok {
				val[repoTag.Tag()] = lastLayerID
			} else {
				w.repositories[repoTag.Name()] = map[string]string{repoTag.Tag(): lastLayerID}
			}
		}
	}
	return nil
}

// checkManifestItemsMatch checks that a and b describe the same image,
// and returns an error if that’s not the case (which should never happen).
func checkManifestItemsMatch(a, b *ManifestItem) error {
	if a.Config != b.Config {
		return fmt.Errorf("Internal error: Trying to reuse ManifestItem values with configs %#v vs. %#v", a.Config, b.Config)
	}
	if !slices.Equal(a.Layers, b.Layers) {
		return fmt.Errorf("Internal error: Trying to reuse ManifestItem values with layers %#v vs. %#v", a.Layers, b.Layers)
	}
	// Ignore RepoTags, that will be built later.
	// Ignore Parent and LayerSources, which we don’t set to anything meaningful.
	return nil
}

// ensureManifestItemLocked ensures that there is a manifest item pointing to (layerDescriptors, configDigest) with repoTags
// The caller must have locked the Writer.
func (w *Writer) ensureManifestItemLocked(layerDescriptors []manifest.Schema2Descriptor, configDigest digest.Digest, repoTags []reference.NamedTagged) error {
	layerPaths := []string{}
	for _, l := range layerDescriptors {
		p, err := w.physicalLayerPath(l.Digest)
		if err != nil {
			return err
		}
		layerPaths = append(layerPaths, p)
	}

	var item *ManifestItem
	configPath, err := w.configPath(configDigest)
	if err != nil {
		return err
	}
	newItem := ManifestItem{
		Config:       configPath,
		RepoTags:     []string{},
		Layers:       layerPaths,
		Parent:       "", // We don’t have this information
		LayerSources: nil,
	}
	if i, ok := w.manifestByConfig[configDigest]; ok {
		item = &w.manifest[i]
		if err := checkManifestItemsMatch(item, &newItem); err != nil {
			return err
		}
	} else {
		i := len(w.manifest)
		w.manifestByConfig[configDigest] = i
		w.manifest = append(w.manifest, newItem)
		item = &w.manifest[i]
	}

	knownRepoTags := set.New[string]()
	knownRepoTags.AddSeq(slices.Values(item.RepoTags))
	for _, tag := range repoTags {
		// For github.com/docker/docker consumers, this works just as well as
		//   refString := ref.String()
		// because when reading the RepoTags strings, github.com/docker/docker/reference
		// normalizes both of them to the same value.
		//
		// Doing it this way to include the normalized-out `docker.io[/library]` does make
		// a difference for github.com/projectatomic/docker consumers, with the
		// “Add --add-registry and --block-registry options to docker daemon” patch.
		// These consumers treat reference strings which include a hostname and reference
		// strings without a hostname differently.
		//
		// Using the host name here is more explicit about the intent, and it has the same
		// effect as (docker pull) in projectatomic/docker, which tags the result using
		// a hostname-qualified reference.
		// See https://github.com/containers/image/issues/72 for a more detailed
		// analysis and explanation.
		refString := fmt.Sprintf("%s:%s", tag.Name(), tag.Tag())

		if !knownRepoTags.Contains(refString) {
			item.RepoTags = append(item.RepoTags, refString)
			knownRepoTags.Add(refString)
		}
	}

	return nil
}

// Close writes all outstanding data about images to the archive, and finishes writing data
// to the underlying io.Writer.
// No more images can be added after this is called.
func (w *Writer) Close() error {
	if err := w.lock(); err != nil {
		return err
	}
	defer w.unlock()

	b, err := json.Marshal(&w.manifest)
	if err != nil {
		return err
	}
	if err := w.sendBytesLocked(manifestFileName, b); err != nil {
		return err
	}

	b, err = json.Marshal(w.repositories)
	if err != nil {
		return fmt.Errorf("marshaling repositories: %w", err)
	}
	if err := w.sendBytesLocked(legacyRepositoriesFileName, b); err != nil {
		return fmt.Errorf("writing config json file: %w", err)
	}

	if err := w.tar.Close(); err != nil {
		return err
	}
	w.tar = nil // Mark the Writer as closed.
	return nil
}

// configPath returns a path we choose for storing a config with the specified digest.
// NOTE: This is an internal implementation detail, not a format property, and can change
// any time.
func (w *Writer) configPath(configDigest digest.Digest) (string, error) {
	if err := configDigest.Validate(); err != nil { // digest.Digest.Encoded() panics on failure, and could possibly result in unexpected paths, so validate explicitly.
		return "", err
	}
	return configDigest.Encoded() + ".json", nil
}

// physicalLayerPath returns a path we choose for storing a layer with the specified digest
// (the actual path, i.e. a regular file, not a symlink that may be used in the legacy format).
// NOTE: This is an internal implementation detail, not a format property, and can change
// any time.
func (w *Writer) physicalLayerPath(layerDigest digest.Digest) (string, error) {
	if err := layerDigest.Validate(); err != nil { // digest.Digest.Encoded() panics on failure, and could possibly result in unexpected paths, so validate explicitly.
		return "", err
	}
	// Note that this can't be e.g. filepath.Join(l.Digest.Encoded(), legacyLayerFileName); due to the way
	// writeLegacyMetadata constructs layer IDs differently from inputinfo.Digest values (as described
	// inside it), most of the layers would end up in subdirectories alone without any metadata; (docker load)
	// tries to load every subdirectory as an image and fails if the config is missing.  So, keep the layers
	// in the root of the tarball.
	return layerDigest.Encoded() + ".tar", nil
}

type tarFI struct {
	path      string
	size      int64
	isSymlink bool
}

func (t *tarFI) Name() string {
	return t.path
}
func (t *tarFI) Size() int64 {
	return t.size
}
func (t *tarFI) Mode() os.FileMode {
	if t.isSymlink {
		return os.ModeSymlink
	}
	return 0444
}
func (t *tarFI) ModTime() time.Time {
	return time.Unix(0, 0)
}
func (t *tarFI) IsDir() bool {
	return false
}
func (t *tarFI) Sys() any {
	return nil
}

// sendSymlinkLocked sends a symlink into the tar stream.
// The caller must have locked the Writer.
func (w *Writer) sendSymlinkLocked(path string, target string) error {
	hdr, err := tar.FileInfoHeader(&tarFI{path: path, size: 0, isSymlink: true}, target)
	if err != nil {
		return err
	}
	logrus.Debugf("Sending as tar link %s -> %s", path, target)
	return w.tar.WriteHeader(hdr)
}

// sendBytesLocked sends a path into the tar stream.
// The caller must have locked the Writer.
func (w *Writer) sendBytesLocked(path string, b []byte) error {
	return w.sendFileLocked(path, int64(len(b)), bytes.NewReader(b))
}

// sendFileLocked sends a file into the tar stream.
// The caller must have locked the Writer.
func (w *Writer) sendFileLocked(path string, expectedSize int64, stream io.Reader) error {
	hdr, err := tar.FileInfoHeader(&tarFI{path: path, size: expectedSize}, "")
	if err != nil {
		return err
	}
	logrus.Debugf("Sending as tar file %s", path)
	if err := w.tar.WriteHeader(hdr); err != nil {
		return err
	}
	// TODO: This can take quite some time, and should ideally be cancellable using a context.Context.
	size, err := io.Copy(w.tar, stream)
	if err != nil {
		return err
	}
	if size != expectedSize {
		return fmt.Errorf("Size mismatch when copying %s, expected %d, got %d", path, expectedSize, size)
	}
	return nil
}
