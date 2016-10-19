package docker

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	dockermessage "github.com/docker/docker/pkg/jsonmessage"
	dockerstdcopy "github.com/docker/docker/pkg/stdcopy"
	dockerapi "github.com/docker/engine-api/client"
	dockertypes "github.com/docker/engine-api/types"
	dockercontainer "github.com/docker/engine-api/types/container"
	dockernetwork "github.com/docker/engine-api/types/network"
	"github.com/docker/go-connections/tlsconfig"
	"golang.org/x/net/context"

	"github.com/openshift/source-to-image/pkg/api"
	"github.com/openshift/source-to-image/pkg/errors"
	"github.com/openshift/source-to-image/pkg/tar"
	"github.com/openshift/source-to-image/pkg/util"
	"github.com/openshift/source-to-image/pkg/util/interrupt"
)

const (
	// ScriptsURLEnvironment is a deprecated environment variable name that
	// specifies where to look for S2I scripts. Use ScriptsURLLabel instead.
	ScriptsURLEnvironment = "STI_SCRIPTS_URL"
	// LocationEnvironment is a deprecated environment variable name that
	// specifies where to place artifacts in a builder image. Use
	// DestinationLabel instead.
	LocationEnvironment = "STI_LOCATION"

	// ScriptsURLLabel is the name of the Docker image LABEL that tells S2I where
	// to look for the S2I scripts. This label is also copied into the output
	// image.
	// The previous name of this label was 'io.s2i.scripts-url'. This is now
	// deprecated.
	ScriptsURLLabel = api.DefaultNamespace + "scripts-url"
	// DestinationLabel is the name of the Docker image LABEL that tells S2I where
	// to place the artifacts (scripts, sources) in the builder image.
	// The previous name of this label was 'io.s2i.destination'. This is now
	// deprecated
	DestinationLabel = api.DefaultNamespace + "destination"
	// AssembleInputFilesLabel is the name of the Docker image LABEL that tells S2I which
	// files wil be copied from builder to a runtime image.
	AssembleInputFilesLabel = api.DefaultNamespace + "assemble-input-files"

	// DefaultDestination is the destination where the artifacts will be placed
	// if DestinationLabel was not specified.
	DefaultDestination = "/tmp"
	// DefaultTag is the image tag, being applied if none is specified.
	DefaultTag = "latest"

	// DefaultDockerTimeout specifies a timeout for Docker API calls. When this
	// timeout is reached, certain Docker API calls might error out.
	DefaultDockerTimeout = 2 * time.Minute

	// DefaultShmSize is the default shared memory size to use (in bytes) if not specified.
	DefaultShmSize = int64(1024 * 1024 * 64)
)

// containerNamePrefix prefixes the name of containers launched by S2I. We
// cannot reuse the prefix "k8s" because we don't want the containers to be
// managed by a kubelet.
const containerNamePrefix = "s2i"

// containerName creates names for Docker containers launched by S2I. It is
// meant to resemble Kubernetes' pkg/kubelet/dockertools.BuildDockerName.
func containerName(image string) string {
	uid := fmt.Sprintf("%08x", rand.Uint32())
	// Replace invalid characters for container name with underscores.
	image = strings.Map(func(r rune) rune {
		if ('0' <= r && r <= '9') || ('A' <= r && r <= 'Z') || ('a' <= r && r <= 'z') {
			return r
		}
		return '_'
	}, image)
	return fmt.Sprintf("%s_%s_%s", containerNamePrefix, image, uid)
}

// Docker is the interface between STI and the docker engine-api.
// It contains higher level operations called from the STI
// build or usage commands
type Docker interface {
	IsImageInLocalRegistry(name string) (bool, error)
	IsImageOnBuild(string) bool
	GetOnBuild(string) ([]string, error)
	RemoveContainer(id string) error
	GetScriptsURL(name string) (string, error)
	GetAssembleInputFiles(string) (string, error)
	RunContainer(opts RunContainerOptions) error
	GetImageID(name string) (string, error)
	GetImageWorkdir(name string) (string, error)
	CommitContainer(opts CommitContainerOptions) (string, error)
	RemoveImage(name string) error
	CheckImage(name string) (*api.Image, error)
	PullImage(name string) (*api.Image, error)
	CheckAndPullImage(name string) (*api.Image, error)
	BuildImage(opts BuildImageOptions) error
	GetImageUser(name string) (string, error)
	GetImageEntrypoint(name string) ([]string, error)
	GetLabels(name string) (map[string]string, error)
	UploadToContainer(srcPath, destPath, container string) error
	UploadToContainerWithCallback(srcPath, destPath, container string, walkFn filepath.WalkFunc, modifyInplace bool) error
	DownloadFromContainer(containerPath string, w io.Writer, container string) error
	Version() (dockertypes.Version, error)
}

// Client contains all methods used when interacting directly with docker engine-api
type Client interface {
	ContainerAttach(ctx context.Context, container string, options dockertypes.ContainerAttachOptions) (dockertypes.HijackedResponse, error)
	ContainerCommit(ctx context.Context, container string, options dockertypes.ContainerCommitOptions) (dockertypes.ContainerCommitResponse, error)
	ContainerCreate(ctx context.Context, config *dockercontainer.Config, hostConfig *dockercontainer.HostConfig, networkingConfig *dockernetwork.NetworkingConfig, containerName string) (dockertypes.ContainerCreateResponse, error)
	ContainerInspect(ctx context.Context, containerID string) (dockertypes.ContainerJSON, error)
	ContainerRemove(ctx context.Context, containerID string, options dockertypes.ContainerRemoveOptions) error
	ContainerStart(ctx context.Context, containerID string, options dockertypes.ContainerStartOptions) error
	ContainerWait(ctx context.Context, containerID string) (int, error)
	CopyToContainer(ctx context.Context, container, path string, content io.Reader, opts dockertypes.CopyToContainerOptions) error
	CopyFromContainer(ctx context.Context, container, srcPath string) (io.ReadCloser, dockertypes.ContainerPathStat, error)
	ImageBuild(ctx context.Context, buildContext io.Reader, options dockertypes.ImageBuildOptions) (dockertypes.ImageBuildResponse, error)
	ImageInspectWithRaw(ctx context.Context, imageID string) (dockertypes.ImageInspect, []byte, error)
	ImagePull(ctx context.Context, ref string, options dockertypes.ImagePullOptions) (io.ReadCloser, error)
	ImageRemove(ctx context.Context, imageID string, options dockertypes.ImageRemoveOptions) ([]dockertypes.ImageDelete, error)
	ServerVersion(ctx context.Context) (dockertypes.Version, error)
}

type stiDocker struct {
	client   Client
	pullAuth dockertypes.AuthConfig
}

func (d stiDocker) InspectImage(name string) (*dockertypes.ImageInspect, error) {
	ctx, cancel := getDefaultContext()
	defer cancel()
	resp, _, err := d.client.ImageInspectWithRaw(ctx, name)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// PostExecutor is an interface which provides a PostExecute function
type PostExecutor interface {
	PostExecute(containerID, destination string) error
}

// PullResult is the result returned by the PullImage function
type PullResult struct {
	OnBuild bool
	Image   *api.Image
}

// RunContainerOptions are options passed in to the RunContainer method
type RunContainerOptions struct {
	Image           string
	PullImage       bool
	PullAuth        api.AuthConfig
	ExternalScripts bool
	ScriptsURL      string
	Destination     string
	Env             []string
	// Entrypoint will be used to override the default entrypoint
	// for the image if it has one.  If the image has no entrypoint,
	// this value is ignored.
	Entrypoint       []string
	Stdin            io.Reader
	Stdout           io.Writer
	Stderr           io.Writer
	OnStart          func(containerID string) error
	PostExec         PostExecutor
	TargetImage      bool
	NetworkMode      string
	User             string
	CGroupLimits     *api.CGroupLimits
	CapDrop          []string
	Binds            []string
	Command          string
	CommandOverrides func(originalCmd string) string
	// CommandExplicit provides a full control on the CMD directive.
	// It won't modified in any way and will be passed to the docker as-is.
	// Use this option when you want to use arbitrary command as CMD directive.
	// In this case you can't use Command because 1) it's just a string
	// 2) it will be modified by prepending base dir and cleaned by the path.Join().
	// You also can't use CommandOverrides because 1) it's a string
	// 2) it only gets applied when Command equals to "assemble" or "usage" script
	// AND script is inside of the tar archive.
	CommandExplicit []string
}

// asDockerConfig converts a RunContainerOptions into a Config understood by the
// docker client
func (rco RunContainerOptions) asDockerConfig() dockercontainer.Config {
	return dockercontainer.Config{
		Image:        getImageName(rco.Image),
		User:         rco.User,
		Env:          rco.Env,
		Entrypoint:   rco.Entrypoint,
		OpenStdin:    rco.Stdin != nil,
		StdinOnce:    rco.Stdin != nil,
		AttachStdout: rco.Stdout != nil,
	}
}

// asDockerHostConfig converts a RunContainerOptions into a HostConfig
// understood by the docker client
func (rco RunContainerOptions) asDockerHostConfig() dockercontainer.HostConfig {
	hostConfig := dockercontainer.HostConfig{
		CapDrop:         rco.CapDrop,
		PublishAllPorts: rco.TargetImage,
		NetworkMode:     dockercontainer.NetworkMode(rco.NetworkMode),
		Binds:           rco.Binds,
	}
	if rco.CGroupLimits != nil {
		hostConfig.Resources.Memory = rco.CGroupLimits.MemoryLimitBytes
		hostConfig.Resources.MemorySwap = rco.CGroupLimits.MemorySwap
		hostConfig.Resources.CPUShares = rco.CGroupLimits.CPUShares
		hostConfig.Resources.CPUQuota = rco.CGroupLimits.CPUQuota
		hostConfig.Resources.CPUPeriod = rco.CGroupLimits.CPUPeriod
	}
	return hostConfig
}

// asDockerCreateContainerOptions converts a RunContainerOptions into a
// ContainerCreateConfig understood by the docker client
func (rco RunContainerOptions) asDockerCreateContainerOptions() dockertypes.ContainerCreateConfig {
	config := rco.asDockerConfig()
	hostConfig := rco.asDockerHostConfig()
	return dockertypes.ContainerCreateConfig{
		Name:       containerName(rco.Image),
		Config:     &config,
		HostConfig: &hostConfig,
	}
}

// asDockerAttachToContainerOptions converts a RunContainerOptions into a
// ContainerAttachOptions understood by the docker client
func (rco RunContainerOptions) asDockerAttachToContainerOptions() dockertypes.ContainerAttachOptions {
	return dockertypes.ContainerAttachOptions{
		Stdin:  rco.Stdin != nil,
		Stdout: rco.Stdout != nil,
		Stderr: rco.Stderr != nil,
		Stream: rco.Stdout != nil,
	}
}

// CommitContainerOptions are options passed in to the CommitContainer method
type CommitContainerOptions struct {
	ContainerID string
	Repository  string
	User        string
	Command     []string
	Env         []string
	Entrypoint  []string
	Labels      map[string]string
}

// BuildImageOptions are options passed in to the BuildImage method
type BuildImageOptions struct {
	Name         string
	Stdin        io.Reader
	Stdout       io.Writer
	CGroupLimits *api.CGroupLimits
}

// NewEngineAPIClient creates a new Docker engine API client
func NewEngineAPIClient(config *api.DockerConfig) (*dockerapi.Client, error) {
	var httpClient *http.Client

	if config.UseTLS || config.TLSVerify {
		tlscOptions := tlsconfig.Options{
			InsecureSkipVerify: !config.TLSVerify,
		}

		if _, err := os.Stat(config.CAFile); !os.IsNotExist(err) {
			tlscOptions.CAFile = config.CAFile
		}
		if _, err := os.Stat(config.CertFile); !os.IsNotExist(err) {
			tlscOptions.CertFile = config.CertFile
		}
		if _, err := os.Stat(config.KeyFile); !os.IsNotExist(err) {
			tlscOptions.KeyFile = config.KeyFile
		}

		tlsc, err := tlsconfig.Client(tlscOptions)
		if err != nil {
			return nil, err
		}

		httpClient = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: tlsc,
			},
		}
	}
	return dockerapi.NewClient(config.Endpoint, os.Getenv("DOCKER_API_VERSION"), httpClient, nil)
}

// New creates a new implementation of the STI Docker interface
func New(config *api.DockerConfig, auth api.AuthConfig) (Docker, error) {
	client, err := NewEngineAPIClient(config)
	if err != nil {
		return nil, err
	}
	return &stiDocker{
		client: client,
		pullAuth: dockertypes.AuthConfig{
			Username:      auth.Username,
			Password:      auth.Password,
			Email:         auth.Email,
			ServerAddress: auth.ServerAddress,
		},
	}, nil
}

func getDefaultContext() (context.Context, context.CancelFunc) {
	// the intention is: all docker API calls with the exception of known long-
	// running calls (ContainerWait, ImagePull, ImageBuild) must complete within a
	// certain timeout otherwise we bail.
	return context.WithTimeout(context.Background(), DefaultDockerTimeout)
}

// GetImageWorkdir returns the WORKDIR property for the given image name.
// When the WORKDIR is not set or empty, return "/" instead.
func (d *stiDocker) GetImageWorkdir(name string) (string, error) {
	resp, err := d.InspectImage(name)
	if err != nil {
		return "", err
	}
	workdir := resp.Config.WorkingDir
	if len(workdir) == 0 {
		// This is a default destination used by UploadToContainer when the WORKDIR
		// is not set or it is empty. To show user where the injections will end up,
		// we set this to "/".
		workdir = "/"
	}
	return workdir, nil
}

// GetImageEntrypoint returns the ENTRYPOINT property for the given image name.
func (d *stiDocker) GetImageEntrypoint(name string) ([]string, error) {
	image, err := d.InspectImage(name)
	if err != nil {
		return nil, err
	}
	return image.Config.Entrypoint, nil
}

// UploadToContainer uploads artifacts to the container.
func (d *stiDocker) UploadToContainer(src, dest, container string) error {
	makeFileWorldWritable := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Skip chmod if on windows OS and for symlinks
		if runtime.GOOS == "windows" || info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		mode := os.FileMode(0666)
		if info.IsDir() {
			mode = 0777
		}
		return os.Chmod(path, mode)
	}
	return d.UploadToContainerWithCallback(src, dest, container, makeFileWorldWritable, false)
}

// UploadToContainerWithCallback uploads artifacts to the container.
// If the source is a directory, then all files and sub-folders are copied into
// the destination (which has to be directory as well).
// If the source is a single file, then the file copied into destination (which
// has to be full path to a file inside the container).
// If the destination path is empty or set to ".", then we will try to figure
// out the WORKDIR of the image that the container was created from and use that
// as a destination. If the WORKDIR is not set, then we copy files into "/"
// folder (docker upload default).
func (d *stiDocker) UploadToContainerWithCallback(src, dest, container string, walkFn filepath.WalkFunc, modifyInplace bool) error {
	path := filepath.Dir(dest)
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	info, _ := f.Stat()
	defer f.Close()
	t := tar.New()
	r, w := io.Pipe()
	if info.IsDir() {
		path = dest
		go func() {
			defer w.Close()
			if err := t.StreamDirAsTarWithCallback(src, w, walkFn, modifyInplace); err != nil {
				glog.V(0).Infof("error: Uploading directory to container failed: %v", err)
			}
		}()
	} else {
		go func() {
			defer w.Close()
			if err := t.StreamFileAsTarWithCallback(src, filepath.Base(dest), w, walkFn, modifyInplace); err != nil {
				glog.V(0).Infof("error: Uploading files to container failed: %v", err)
			}
		}()
	}
	glog.V(3).Infof("Uploading %q to %q ...", src, path)
	ctx, cancel := getDefaultContext()
	defer cancel()
	return d.client.CopyToContainer(ctx, container, path, r, dockertypes.CopyToContainerOptions{})
}

// DownloadFromContainer downloads file (or directory) from the container.
func (d *stiDocker) DownloadFromContainer(containerPath string, w io.Writer, container string) error {
	ctx, cancel := getDefaultContext()
	defer cancel()
	readCloser, _, err := d.client.CopyFromContainer(ctx, container, containerPath)
	if err != nil {
		return err
	}
	defer readCloser.Close()
	_, err = io.Copy(w, readCloser)
	return err
}

// IsImageInLocalRegistry determines whether the supplied image is in the local registry.
func (d *stiDocker) IsImageInLocalRegistry(name string) (bool, error) {
	name = getImageName(name)
	resp, err := d.InspectImage(name)
	if resp != nil {
		return true, nil
	}
	if err != nil && !dockerapi.IsErrImageNotFound(err) {
		return false, errors.NewInspectImageError(name, err)
	}
	return false, nil
}

// GetImageUser finds and retrieves the user associated with
// an image if one has been specified
func (d *stiDocker) GetImageUser(name string) (string, error) {
	name = getImageName(name)
	resp, err := d.InspectImage(name)
	if err != nil {
		glog.V(4).Infof("error inspecting image %s: %v", name, err)
		return "", errors.NewInspectImageError(name, err)
	}
	user := resp.ContainerConfig.User
	if len(user) == 0 {
		user = resp.Config.User
	}
	return user, nil
}

// Version returns information of the docker client and server host
func (d *stiDocker) Version() (dockertypes.Version, error) {
	ctx, cancel := getDefaultContext()
	defer cancel()
	return d.client.ServerVersion(ctx)
}

// IsImageOnBuild provides information about whether the Docker image has
// OnBuild instruction recorded in the Image Config.
func (d *stiDocker) IsImageOnBuild(name string) bool {
	onbuild, err := d.GetOnBuild(name)
	return err == nil && len(onbuild) > 0
}

// GetOnBuild returns the set of ONBUILD Dockerfile commands to execute
// for the given image
func (d *stiDocker) GetOnBuild(name string) ([]string, error) {
	name = getImageName(name)
	resp, err := d.InspectImage(name)
	if err != nil {
		glog.V(4).Infof("error inspecting image %s: %v", name, err)
		return nil, errors.NewInspectImageError(name, err)
	}
	return resp.Config.OnBuild, nil
}

// CheckAndPullImage pulls an image into the local registry if not present
// and returns the image metadata
func (d *stiDocker) CheckAndPullImage(name string) (*api.Image, error) {
	name = getImageName(name)
	displayName := name

	if !glog.Is(3) {
		// For less verbose log levels (less than 3), shorten long iamge names like:
		//     "centos/php-56-centos7@sha256:51c3e2b08bd9fadefccd6ec42288680d6d7f861bdbfbd2d8d24960621e4e27f5"
		// to include just enough characters to differentiate the build from others in the docker repository:
		//     "centos/php-56-centos7@sha256:51c3e2b08bd..."
		// 18 characters is somewhat arbitrary, but should be enough to avoid a name collision.
		split := strings.Split(name, "@")
		if len(split) > 1 && len(split[1]) > 18 {
			displayName = split[0] + "@" + split[1][:18] + "..."
		}
	}

	image, err := d.CheckImage(name)
	if err != nil && !strings.Contains(err.(errors.Error).Details.Error(), "No such image") {
		return nil, err
	}
	if image == nil {
		glog.V(1).Infof("Image %q not available locally, pulling ...", displayName)
		return d.PullImage(name)
	}

	glog.V(3).Infof("Using locally available image %q", displayName)
	return image, nil
}

// CheckImage checks image from the local registry.
func (d *stiDocker) CheckImage(name string) (*api.Image, error) {
	name = getImageName(name)
	inspect, err := d.InspectImage(name)
	if err != nil {
		glog.V(4).Infof("error inspecting image %s: %v", name, err)
		return nil, errors.NewInspectImageError(name, err)
	}
	if inspect != nil {
		image := &api.Image{}
		updateImageWithInspect(image, inspect)
		return image, nil
	}

	return nil, nil
}

func base64EncodeAuth(auth dockertypes.AuthConfig) (string, error) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(auth); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(buf.Bytes()), nil
}

// PullImage pulls an image into the local registry
func (d *stiDocker) PullImage(name string) (*api.Image, error) {
	name = getImageName(name)

	// RegistryAuth is the base64 encoded credentials for the registry
	base64Auth, err := base64EncodeAuth(d.pullAuth)
	if err != nil {
		return nil, errors.NewPullImageError(name, err)
	}

	err = util.TimeoutAfter(DefaultDockerTimeout, fmt.Sprintf("pulling image %q", name), func(timer *time.Timer) error {
		resp, pullErr := d.client.ImagePull(context.Background(), name, dockertypes.ImagePullOptions{RegistryAuth: base64Auth})
		if pullErr != nil {
			return pullErr
		}
		defer resp.Close()

		decoder := json.NewDecoder(resp)
		for {
			if !timer.Stop() {
				return &util.TimeoutError{}
			}
			timer.Reset(DefaultDockerTimeout)

			var msg dockermessage.JSONMessage
			pullErr = decoder.Decode(&msg)
			if pullErr == io.EOF {
				return nil
			}
			if pullErr != nil {
				return pullErr
			}

			if msg.Error != nil {
				return msg.Error
			}
			if msg.ProgressMessage != "" {
				glog.V(4).Infof("pulling image %s: %s", name, msg.ProgressMessage)
			}
		}
	})
	if err != nil {
		return nil, errors.NewPullImageError(name, err)
	}

	inspectResp, err := d.InspectImage(name)
	if err != nil {
		return nil, errors.NewPullImageError(name, err)
	}
	if inspectResp != nil {
		image := &api.Image{}
		updateImageWithInspect(image, inspectResp)
		return image, nil
	}
	return nil, nil
}

func updateImageWithInspect(image *api.Image, inspect *dockertypes.ImageInspect) {
	image.ID = inspect.ID
	if inspect.Config != nil {
		image.Config = &api.ContainerConfig{
			Labels: inspect.Config.Labels,
			Env:    inspect.Config.Env,
		}
	}
	if inspect.ContainerConfig != nil {
		image.ContainerConfig = &api.ContainerConfig{
			Labels: inspect.ContainerConfig.Labels,
			Env:    inspect.ContainerConfig.Env,
		}
	}
}

// RemoveContainer removes a container and its associated volumes.
func (d *stiDocker) RemoveContainer(id string) error {
	ctx, cancel := getDefaultContext()
	defer cancel()
	opts := dockertypes.ContainerRemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	}
	return d.client.ContainerRemove(ctx, id, opts)
}

// GetLabels retrieves the labels of the given image.
func (d *stiDocker) GetLabels(name string) (map[string]string, error) {
	name = getImageName(name)
	resp, err := d.InspectImage(name)
	if err != nil {
		glog.V(4).Infof("error inspecting image %s: %v", name, err)
		return nil, errors.NewInspectImageError(name, err)
	}
	return resp.Config.Labels, nil
}

// getImageName checks the image name and adds DefaultTag if none is specified
func getImageName(name string) string {
	_, tag, id := parseRepositoryTag(name)
	if len(tag) == 0 && len(id) == 0 {
		//_, tag, _ := parseRepositoryTag(name)
		//if len(tag) == 0 {
		return strings.Join([]string{name, DefaultTag}, ":")
	}

	return name
}

// getLabel gets label's value from the image metadata
func getLabel(image *api.Image, name string) string {
	if value, ok := image.Config.Labels[name]; ok {
		return value
	}
	if value, ok := image.ContainerConfig.Labels[name]; ok {
		return value
	}
	return ""
}

// getVariable gets environment variable's value from the image metadata
func getVariable(image *api.Image, name string) string {
	envName := name + "="
	env := append(image.ContainerConfig.Env, image.Config.Env...)
	for _, v := range env {
		if strings.HasPrefix(v, envName) {
			return strings.TrimSpace((v[len(envName):]))
		}
	}

	return ""
}

// GetScriptsURL finds a scripts-url label on the given image.
func (d *stiDocker) GetScriptsURL(image string) (string, error) {
	imageMetadata, err := d.CheckAndPullImage(image)
	if err != nil {
		return "", err
	}

	return getScriptsURL(imageMetadata), nil
}

// GetAssembleInputFiles finds a io.openshift.s2i.assemble-input-files label on the given image.
func (d *stiDocker) GetAssembleInputFiles(image string) (string, error) {
	imageMetadata, err := d.CheckAndPullImage(image)
	if err != nil {
		return "", err
	}

	label := getLabel(imageMetadata, AssembleInputFilesLabel)
	if len(label) == 0 {
		glog.V(0).Infof("warning: Image %q does not contain a value for the %s label", image, AssembleInputFilesLabel)
	} else {
		glog.V(3).Infof("Image %q contains %s set to %q", image, AssembleInputFilesLabel, label)
	}
	return label, nil
}

// getScriptsURL finds a scripts url label in the image metadata
func getScriptsURL(image *api.Image) string {
	if image == nil {
		return ""
	}
	scriptsURL := getLabel(image, ScriptsURLLabel)

	// For backward compatibility, support the old label schema
	if len(scriptsURL) == 0 {
		scriptsURL = getLabel(image, "io.s2i.scripts-url")
		if len(scriptsURL) > 0 {
			glog.V(0).Infof("warning: Image %s uses deprecated label 'io.s2i.scripts-url', please migrate it to %s instead!",
				image.ID, ScriptsURLLabel)
		}
	}
	if len(scriptsURL) == 0 {
		scriptsURL = getVariable(image, ScriptsURLEnvironment)
		if len(scriptsURL) != 0 {
			glog.V(0).Infof("warning: Image %s uses deprecated environment variable %s, please migrate it to %s label instead!",
				image.ID, ScriptsURLEnvironment, ScriptsURLLabel)
		}
	}
	if len(scriptsURL) == 0 {
		glog.V(0).Infof("warning: Image %s does not contain a value for the %s label", image.ID, ScriptsURLLabel)
	} else {
		glog.V(2).Infof("Image %s contains %s set to %q", image.ID, ScriptsURLLabel, scriptsURL)
	}

	return scriptsURL
}

// getDestination finds a destination label in the image metadata
func getDestination(image *api.Image) string {
	if val := getLabel(image, DestinationLabel); len(val) != 0 {
		return val
	}
	// For backward compatibility, support the old label schema
	if val := getLabel(image, "io.s2i.destination"); len(val) != 0 {
		glog.V(0).Infof("warning: Image %s uses deprecated label 'io.s2i.destination', please migrate it to %s instead!",
			image.ID, DestinationLabel)
		return val
	}
	if val := getVariable(image, LocationEnvironment); len(val) != 0 {
		glog.V(0).Infof("warning: Image %s uses deprecated environment variable %s, please migrate it to %s label instead!",
			image.ID, LocationEnvironment, DestinationLabel)
		return val
	}

	// default directory if none is specified
	return DefaultDestination
}

func constructCommand(opts RunContainerOptions, imageMetadata *api.Image, tarDestination string) []string {
	// base directory for all S2I commands
	commandBaseDir := determineCommandBaseDir(opts, imageMetadata, tarDestination)

	// NOTE: We use path.Join instead of filepath.Join to avoid converting the
	// path to UNC (Windows) format as we always run this inside container.
	binaryToRun := path.Join(commandBaseDir, opts.Command)

	// when calling assemble script with Stdin parameter set (the tar file)
	// we need to first untar the whole archive and only then call the assemble script
	if opts.Stdin != nil && (opts.Command == api.Assemble || opts.Command == api.Usage) {
		untarAndRun := fmt.Sprintf("tar -C %s -xf - && %s", tarDestination, binaryToRun)

		resultedCommand := untarAndRun
		if opts.CommandOverrides != nil {
			resultedCommand = opts.CommandOverrides(untarAndRun)
		}
		return []string{"/bin/sh", "-c", resultedCommand}
	}

	return []string{binaryToRun}
}

func determineTarDestinationDir(opts RunContainerOptions, imageMetadata *api.Image) string {
	if len(opts.Destination) != 0 {
		return opts.Destination
	}
	return getDestination(imageMetadata)
}

func determineCommandBaseDir(opts RunContainerOptions, imageMetadata *api.Image, tarDestination string) string {
	if opts.ExternalScripts {
		// for external scripts we must always append 'scripts' because this is
		// the default subdirectory inside tar for them
		// NOTE: We use path.Join instead of filepath.Join to avoid converting the
		// path to UNC (Windows) format as we always run this inside container.
		glog.V(2).Infof("Both scripts and untarred source will be placed in '%s'", tarDestination)
		return path.Join(tarDestination, "scripts")
	}

	// for internal scripts we can have separate path for scripts and untar operation destination
	scriptsURL := opts.ScriptsURL
	if len(scriptsURL) == 0 {
		scriptsURL = getScriptsURL(imageMetadata)
	}

	commandBaseDir := strings.TrimPrefix(scriptsURL, "image://")
	glog.V(2).Infof("Base directory for S2I scripts is '%s'. Untarring destination is '%s'.",
		commandBaseDir, tarDestination)

	return commandBaseDir
}

// dumpContainerInfo dumps information about a running container (port/IP/etc).
func dumpContainerInfo(container dockertypes.ContainerCreateResponse, d *stiDocker, image string) {
	ctx, cancel := getDefaultContext()
	defer cancel()

	containerJSON, err := d.client.ContainerInspect(ctx, container.ID)
	if err != nil {
		return
	}

	liveports := "\n\nPort Bindings:  "
	for port, bindings := range containerJSON.NetworkSettings.NetworkSettingsBase.Ports {
		liveports = liveports + "\n  Container Port:  " + string(port)
		liveports = liveports + "\n        Public Host / Port Mappings:"
		for _, binding := range bindings {
			liveports = liveports + "\n            IP: " + binding.HostIP + " Port: " + binding.HostPort
		}
	}
	liveports = liveports + "\n"
	glog.V(0).Infof("\n\n\n\n\nThe image %s has been started in container %s as a result of the --run=true option.  The container's stdout/stderr will be redirected to this command's glog output to help you validate its behavior.  You can also inspect the container with docker commands if you like.  If the container is set up to stay running, you will have to Ctrl-C to exit this command, which should also stop the container %s.  This particular invocation attempts to run with the port mappings %+v \n\n\n\n\n", image, container.ID, container.ID, liveports)
}

// begin block copy of two methods from kube_docker_client.go
// ... essentially, if that code could give us a way to inspect the error from engin-api's ContainerAttach call prior
// to blocking on the IO, we could just leverage the kube_docker_client.go code entirely ... essentially a "non-blocking" attach like go-dockerclient provided

// redirectResponseToOutputStream redirect the response stream to stdout and stderr. When tty is true, all stream will
// only be redirected to stdout.
func (d *stiDocker) redirectResponseToOutputStream(tty bool, outputStream, errorStream io.Writer, resp io.Reader) error {
	if outputStream == nil {
		outputStream = ioutil.Discard
	}
	if errorStream == nil {
		errorStream = ioutil.Discard
	}
	var err error
	if tty {
		_, err = io.Copy(outputStream, resp)
	} else {
		_, err = dockerstdcopy.StdCopy(outputStream, errorStream, resp)
	}
	return err
}

// holdHijackedConnection hold the HijackedResponse, redirect the inputStream to the connection, and redirect the response
// stream to stdout and stderr. NOTE: If needed, we could also add context in this function.
func (d *stiDocker) holdHijackedConnection(tty bool, inputStream io.Reader, outputStream, errorStream io.Writer, resp dockertypes.HijackedResponse) error {
	receiveStdout := make(chan error)
	if outputStream != nil || errorStream != nil {
		go func() {
			receiveStdout <- d.redirectResponseToOutputStream(tty, outputStream, errorStream, resp.Reader)
		}()
	}

	stdinDone := make(chan struct{})
	go func() {
		if inputStream != nil {
			io.Copy(resp.Conn, inputStream)
		}
		resp.CloseWrite()
		close(stdinDone)
	}()

	select {
	case err := <-receiveStdout:
		return err
	case <-stdinDone:
		if outputStream != nil || errorStream != nil {
			return <-receiveStdout
		}
	}
	return nil
}

// end block copy of two methods from kube_docker_client.go

// RunContainer creates and starts a container using the image specified in opts
// with the ability to stream input and/or output.
func (d *stiDocker) RunContainer(opts RunContainerOptions) error {
	createOpts := opts.asDockerCreateContainerOptions()

	// get info about the specified image
	image := createOpts.Config.Image
	inspect, err := d.InspectImage(image)
	imageMetadata := &api.Image{}
	if err == nil {
		updateImageWithInspect(imageMetadata, inspect)
		if opts.PullImage {
			_, err = d.CheckAndPullImage(image)
		}
	}

	if err != nil {
		glog.V(0).Infof("error: Unable to get image metadata for %s: %v", image, err)
		return err
	}

	entrypoint, err := d.GetImageEntrypoint(image)
	if err != nil {
		return fmt.Errorf("Couldn't get entrypoint of %q image: %v", image, err)
	}

	// If the image has an entrypoint already defined,
	// it will be overridden either by DefaultEntrypoint,
	// or by the value in opts.Entrypoint.
	// If the image does not have an entrypoint, but
	// opts.Entrypoint is supplied, opts.Entrypoint will
	// be respected.
	if len(entrypoint) != 0 && len(opts.Entrypoint) == 0 {
		opts.Entrypoint = DefaultEntrypoint
	}

	// tarDestination will be passed as location to PostExecute function
	// and will be used as the prefix for the CMD (scripts/run)
	var tarDestination string

	var cmd []string
	if !opts.TargetImage {
		if len(opts.CommandExplicit) != 0 {
			cmd = opts.CommandExplicit
		} else {
			tarDestination = determineTarDestinationDir(opts, imageMetadata)
			cmd = constructCommand(opts, imageMetadata, tarDestination)
		}
		glog.V(5).Infof("Setting %q command for container ...", strings.Join(cmd, " "))
	}
	createOpts.Config.Cmd = cmd

	// Create a new container.
	glog.V(2).Infof("Creating container with options {Name:%q Config:%+v HostConfig:%+v} ...", createOpts.Name, createOpts.Config, createOpts.HostConfig)
	ctx, cancel := getDefaultContext()
	defer cancel()
	if createOpts.HostConfig != nil && createOpts.HostConfig.ShmSize <= 0 {
		createOpts.HostConfig.ShmSize = DefaultShmSize
	}
	container, err := d.client.ContainerCreate(ctx, createOpts.Config, createOpts.HostConfig, createOpts.NetworkingConfig, createOpts.Name)
	if err != nil {
		return err
	}

	// Container was created, so we defer its removal, and also remove it if we get a SIGINT/SIGTERM/SIGQUIT/SIGHUP.
	removeContainer := func() {
		glog.V(4).Infof("Removing container %q ...", container.ID)
		if removeErr := d.RemoveContainer(container.ID); removeErr != nil {
			glog.V(0).Infof("warning: Failed to remove container %q: %v", container.ID, removeErr)
		} else {
			glog.V(4).Infof("Removed container %q", container.ID)
		}
	}
	dumpStack := func(signal os.Signal) {
		if signal == syscall.SIGQUIT {
			buf := make([]byte, 1<<16)
			runtime.Stack(buf, true)
			fmt.Printf("%s", buf)
		}
		os.Exit(2)
	}
	return interrupt.New(dumpStack, removeContainer).Run(func() error {
		// Attach to the container on go thread (different than with go-dockerclient, since it provided a non-blocking attach which we don't seem to have with k8s/engine-api)

		// Attach to  the container on go thread to mimic blocking behavior we had with go-dockerclient (k8s wrapper blocks); then use borrowed code
		// from k8s to dump logs via return
		// still preserve the flow of attaching before starting to handle various timing issues encountered in the past, as well as allow for --run option
		glog.V(2).Infof("Attaching to container %q ...", container.ID)
		errorChannel := make(chan error)
		timeoutTimer := time.NewTimer(DefaultDockerTimeout)
		var attachLoggingError error
		// unit tests found a DATA RACE on attachLoggingError; at first a simple mutex seemed sufficient, but a race condition in holdHijackedConnection manifested
		// where <-receiveStdout would block even after the container had exitted, blocking the return with attachLoggingError; rather than trying to discern if the
		// container exited in holdHijackedConnection, we'll using channel based signaling coupled with a time to avoid blocking forever
		attachExit := make(chan bool, 1)
		go func() {
			ctx, cancel := getDefaultContext()
			defer cancel()
			resp, attachErr := d.client.ContainerAttach(ctx, container.ID, opts.asDockerAttachToContainerOptions())
			errorChannel <- attachErr
			if attachErr != nil {
				glog.V(0).Infof("error: Unable to attach to container %q: %v", container.ID, attachErr)
				return
			}
			defer resp.Close()
			attachLoggingError = d.holdHijackedConnection(false, opts.Stdin, opts.Stdout, opts.Stderr, resp)
			attachExit <- true
		}()

		// this error check should handle the result from the d.client.ContainerAttach call ... we progress to start when that occurs
		select {
		case err = <-errorChannel:
			// in non-error scenarios, temporary tracing confirmed that
			// unless the container starts, then exits, the attach blocks and
			// never returns either a nil for success or whatever err it might
			// return if the attach failed
			if err != nil {
				return err
			}
			break
		case <-timeoutTimer.C:
			return fmt.Errorf("timed out waiting to attach to container %s ", container.ID)
		}

		// Start the container
		glog.V(2).Infof("Starting container %q ...", container.ID)
		ctx, cancel := getDefaultContext()
		defer cancel()
		err = d.client.ContainerStart(ctx, container.ID, dockertypes.ContainerStartOptions{})
		if err != nil {
			return err
		}

		// Run OnStart hook if defined. OnStart might block, so we run it in a
		// new goroutine, and wait for it to be done later on.
		onStartDone := make(chan error, 1)
		if opts.OnStart != nil {
			go func() {
				onStartDone <- opts.OnStart(container.ID)
			}()
		}

		if opts.TargetImage {
			// When TargetImage is true, we're dealing with an invocation of `s2i build ... --run`
			// so this will, e.g., run a web server and block until the user interrupts it (or
			// the container exits normally).  dump port/etc information for the user.
			dumpContainerInfo(container, d, image)
		}

		// Return an error if the exit code of the container is
		// non-zero.
		glog.V(4).Infof("Waiting for container %q to stop ...", container.ID)
		exitCode, err := d.client.ContainerWait(context.Background(), container.ID)
		if err != nil {
			return fmt.Errorf("waiting for container %q to stop: %v", container.ID, err)
		}
		if exitCode != 0 {
			return errors.NewContainerError(container.ID, exitCode, "")
		}

		// FIXME: If Stdout or Stderr can be closed, close it to notify that
		// there won't be any more writes. This is a hack to close the write
		// half of a pipe so that the read half sees io.EOF.
		// In particular, this is needed to eventually terminate code that runs
		// on OnStart and blocks reading from the pipe.
		if c, ok := opts.Stdout.(io.Closer); ok {
			c.Close()
		}
		if c, ok := opts.Stderr.(io.Closer); ok {
			c.Close()
		}

		// OnStart must be done before we move on.
		if opts.OnStart != nil {
			if err = <-onStartDone; err != nil {
				return err
			}
		}
		// Run PostExec hook if defined.
		if opts.PostExec != nil {
			glog.V(2).Infof("Invoking PostExecute function")
			if err = opts.PostExec.PostExecute(container.ID, tarDestination); err != nil {
				return err
			}
		}

		select {
		case <-attachExit:
			return attachLoggingError
		case <-time.After(DefaultDockerTimeout):
			return nil
		}
	})

}

// GetImageID retrieves the ID of the image identified by name
func (d *stiDocker) GetImageID(name string) (string, error) {
	name = getImageName(name)
	image, err := d.InspectImage(name)
	if err != nil {
		return "", err
	}
	return image.ID, nil
}

// CommitContainer commits a container to an image with a specific tag.
// The new image ID is returned
func (d *stiDocker) CommitContainer(opts CommitContainerOptions) (string, error) {
	ctx, cancel := getDefaultContext()
	defer cancel()
	dockerOpts := dockertypes.ContainerCommitOptions{
		Reference: opts.Repository,
	}
	if opts.Command != nil || opts.Entrypoint != nil {
		config := dockercontainer.Config{
			Cmd:        opts.Command,
			Entrypoint: opts.Entrypoint,
			Env:        opts.Env,
			Labels:     opts.Labels,
			User:       opts.User,
		}
		dockerOpts.Config = &config
		glog.V(2).Infof("Committing container with dockerOpts: %+v, config: %+v", dockerOpts, config)
	}

	resp, err := d.client.ContainerCommit(ctx, opts.ContainerID, dockerOpts)
	if err == nil {
		return resp.ID, nil
	}
	return "", err
}

// RemoveImage removes the image with specified ID
func (d *stiDocker) RemoveImage(imageID string) error {
	ctx, cancel := getDefaultContext()
	defer cancel()
	_, err := d.client.ImageRemove(ctx, imageID, dockertypes.ImageRemoveOptions{})
	return err
}

// BuildImage builds the image according to specified options
func (d *stiDocker) BuildImage(opts BuildImageOptions) error {
	dockerOpts := dockertypes.ImageBuildOptions{
		Tags:           []string{opts.Name},
		NoCache:        true,
		SuppressOutput: false,
		Remove:         true,
		ForceRemove:    true,
	}
	if opts.CGroupLimits != nil {
		dockerOpts.Memory = opts.CGroupLimits.MemoryLimitBytes
		dockerOpts.MemorySwap = opts.CGroupLimits.MemorySwap
		dockerOpts.CPUShares = opts.CGroupLimits.CPUShares
		dockerOpts.CPUPeriod = opts.CGroupLimits.CPUPeriod
		dockerOpts.CPUQuota = opts.CGroupLimits.CPUQuota
	}
	glog.V(2).Infof("Building container using config: %+v", dockerOpts)
	resp, err := d.client.ImageBuild(context.Background(), opts.Stdin, dockerOpts)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// since can't pass in output stream to engine-api, need to copy contents of
	// the output stream they create into our output stream
	_, err = io.Copy(opts.Stdout, resp.Body)
	return err
}
