package image

import (
	"context"
	"fmt"
	"github.com/containers/image/v5/docker"
	"github.com/containers/image/v5/image"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/types"
	"golang.org/x/sys/unix"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/klog/v2"
	"multiarch-operator/controllers/core"
	"multiarch-operator/pkg/system_config"
	"os"
	"sync"
	"time"
)

type registryInspector struct {
	globalPullSecret []byte
	// mutex is used to protect the globalPullSecret field of the singletonImageFacade from concurrent write access
	mutex sync.Mutex
}

func (i *registryInspector) GetCompatibleArchitecturesSet(ctx context.Context, imageReference string, secrets [][]byte) (supportedArchitectures sets.Set[string], err error) {
	// Create the auth file
	authFile, err := i.createAuthFile(append([][]byte{i.globalPullSecret}, secrets...)...)
	if err != nil {
		klog.Warningf("Couldn't write auth file for: %v", err)
		return nil, err
	} else {
		defer func(f *os.File) {
			if err := f.Close(); err != nil {
				klog.Warningf("Failed to close auth file %s %v", f.Name(), err)
			}
		}(authFile)
	}
	// Check if the image is a manifest list
	ref, err := docker.ParseReference(imageReference)
	if err != nil {
		klog.Warningf("Error parsing the image reference for the image %s: %v", imageReference, err)
		return nil, err
	}
	sys := &types.SystemContext{
		AuthFilePath:                authFile.Name(),
		SystemRegistriesConfPath:    system_config.RegistriesConfPath,
		SystemRegistriesConfDirPath: system_config.RegistryCertsDir,
		SignaturePolicyPath:         system_config.PolicyConfPath,
		DockerPerHostCertDirPath:    system_config.DockerCertsDir,
	}
	src, err := ref.NewImageSource(ctx, sys)
	if err != nil {
		klog.Warningf("Error creating the image source: %v", err)
		return nil, err
	}
	defer func(src types.ImageSource) {
		err := src.Close()
		if err != nil {
			klog.Warningf("Error closing the image source for the image %s: %v", imageReference, err)
		}
	}(src)
	rawManifest, _, err := src.GetManifest(ctx, nil)
	if err != nil {
		klog.Infof("Error getting the image manifest: %v", err)
		return nil, err
	}
	supportedArchitectures = sets.New[string]()
	if manifest.MIMETypeIsMultiImage(manifest.GuessMIMEType(rawManifest)) {
		klog.V(5).Infof("image %s is a manifest list... getting the list of supported architectures",
			imageReference)
		// The image is a manifest list
		index, err := manifest.OCI1IndexFromManifest(rawManifest)
		if err != nil {
			klog.Warningf("Error parsing the OCI index from the raw manifest of the image %s: %v",
				imageReference, err)
		}
		for _, m := range index.Manifests {
			supportedArchitectures = sets.Insert(supportedArchitectures, m.Platform.Architecture)
		}
		return supportedArchitectures, nil
	} else {
		klog.V(5).Infof("image %s is not a manifest list... getting the supported architecture", imageReference)
		parsedImage, err := image.FromUnparsedImage(ctx, sys, image.UnparsedInstance(src, nil))
		if err != nil {
			klog.Warningf("Error parsing the manifest of the image %s: %v", imageReference, err)
			return nil, err
		}
		config, err := parsedImage.OCIConfig(ctx)
		if err != nil {
			// Ignore errors due to invalid images at this stage
			klog.Warningf("Error parsing the OCI config of the image %s: %v", imageReference, err)
			return nil, err
		}
		supportedArchitectures = sets.Insert(supportedArchitectures, config.Architecture)
	}
	return supportedArchitectures, nil
}

func (i *registryInspector) createAuthFile(secrets ...[]byte) (*os.File, error) {
	// Create the auth file
	authCfgContent := &authCfg{
		Auths: make(map[string]authData),
	}
	for _, secret := range secrets {
		if err := authCfgContent.unmarshallAuthsDataAndStore(secret); err != nil {
			klog.Warningf("Error unmarshalling pull secrets")
			continue
		}
	}
	authJson, err := authCfgContent.marshallAuths()
	if err != nil {
		klog.Warningf("Error marshalling pull secrets")
		return nil, err
	}
	// TODO: constant-name-for-now is a placeholder. Do we need this parameter at all?
	fd, err := writeMemFile("constant-name-for-now", authJson)
	if err != nil {
		return nil, err
	}
	// filepath to our newly created in-memory file descriptor
	fp := fmt.Sprintf("/proc/self/fd/%d", fd)
	return os.NewFile(uintptr(fd), fp), nil
}

// writeMemFile creates an in memory file based on memfd_create
// returns a file descriptor. Once all references to the file are
// dropped it is automatically released. It is up to the caller
// to close the returned descriptor.
func writeMemFile(name string, b []byte) (int, error) {
	fd, err := unix.MemfdCreate(name, 0)
	if err != nil {
		return 0, fmt.Errorf("MemfdCreate: %v", err)
	}
	err = unix.Ftruncate(fd, int64(len(b)))
	if err != nil {
		return 0, fmt.Errorf("Ftruncate: %v", err)
	}
	data, err := unix.Mmap(fd, 0, len(b), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return 0, fmt.Errorf("Mmap: %v", err)
	}
	copy(data, b)
	err = unix.Munmap(data)
	if err != nil {
		return 0, fmt.Errorf("Munmap: %v", err)
	}
	return fd, nil
}

func (i *registryInspector) storeGlobalPullSecret(pullSecret []byte) {
	i.mutex.Lock()
	defer i.mutex.Unlock()
	i.globalPullSecret = pullSecret
}

func newRegistryInspector() iRegistryInspector {
	ri := &registryInspector{}
	err := core.NewSingleObjectEventHandler[*v1.Secret, *v1.SecretList](context.Background(),
		"pull-secret", "openshift-config", time.Hour, func(et watch.EventType, s *v1.Secret) {
			if et == watch.Deleted || et == watch.Bookmark {
				klog.Warningf("Ignoring event type: %+v", et)
				return
			}
			klog.Warningln("global pull secret update")
			if pullSecret, err := ExtractAuthFromSecret(s); err == nil {
				ri.storeGlobalPullSecret(pullSecret)
			} else {
				klog.Warningf("Error extracting the auth from the secret: %v", err)
			}
		}, nil)
	if err != nil {
		// This is a fatal error because we cannot continue without the global pull secret controller running.
		// We expect the kubernetes self-healing mechanism to restart the controller's pod and try recovering
		// in case of temporary errors or initiate a CrashLoopBackOff.
		klog.Fatalf("Error creating the event handler for the global pull secret: %v", err)
	}
	return ri
}
