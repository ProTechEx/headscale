package hsic

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"

	"github.com/juanfont/headscale"
	v1 "github.com/juanfont/headscale/gen/go/headscale/v1"
	"github.com/juanfont/headscale/integration/dockertestutil"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
)

const (
	hsicHashLength    = 6
	dockerContextPath = "../."
	aclPolicyPath     = "/etc/headscale/acl.hujson"
)

var errHeadscaleStatusCodeNotOk = errors.New("headscale status code not ok")

type HeadscaleInContainer struct {
	hostname string
	port     int

	pool      *dockertest.Pool
	container *dockertest.Resource
	network   *dockertest.Network

	// optional config
	aclPolicy *headscale.ACLPolicy
	env       []string
}

type Option = func(c *HeadscaleInContainer)

func WithACLPolicy(acl *headscale.ACLPolicy) Option {
	return func(hsic *HeadscaleInContainer) {
		hsic.aclPolicy = acl
	}
}

func WithConfigEnv(configEnv map[string]string) Option {
	return func(hsic *HeadscaleInContainer) {
		env := []string{}

		for key, value := range configEnv {
			env = append(env, fmt.Sprintf("%s=%s", key, value))
		}

		hsic.env = env
	}
}

func New(
	pool *dockertest.Pool,
	port int,
	network *dockertest.Network,
	opts ...Option,
) (*HeadscaleInContainer, error) {
	hash, err := headscale.GenerateRandomStringDNSSafe(hsicHashLength)
	if err != nil {
		return nil, err
	}

	hostname := fmt.Sprintf("hs-%s", hash)
	portProto := fmt.Sprintf("%d/tcp", port)

	hsic := &HeadscaleInContainer{
		hostname: hostname,
		port:     port,

		pool:    pool,
		network: network,
	}

	for _, opt := range opts {
		opt(hsic)
	}

	if hsic.aclPolicy != nil {
		hsic.env = append(hsic.env, fmt.Sprintf("HEADSCALE_ACL_POLICY_PATH=%s", aclPolicyPath))
	}

	headscaleBuildOptions := &dockertest.BuildOptions{
		Dockerfile: "Dockerfile.debug",
		ContextDir: dockerContextPath,
	}

	runOptions := &dockertest.RunOptions{
		Name:         hostname,
		ExposedPorts: []string{portProto},
		Networks:     []*dockertest.Network{network},
		// Cmd:          []string{"headscale", "serve"},
		// TODO(kradalby): Get rid of this hack, we currently need to give us some
		// to inject the headscale configuration further down.
		Entrypoint: []string{"/bin/bash", "-c", "/bin/sleep 3 ; headscale serve"},
		Env:        hsic.env,
	}

	// dockertest isnt very good at handling containers that has already
	// been created, this is an attempt to make sure this container isnt
	// present.
	err = pool.RemoveContainerByName(hostname)
	if err != nil {
		return nil, err
	}

	container, err := pool.BuildAndRunWithBuildOptions(
		headscaleBuildOptions,
		runOptions,
		dockertestutil.DockerRestartPolicy,
		dockertestutil.DockerAllowLocalIPv6,
		dockertestutil.DockerAllowNetworkAdministration,
	)
	if err != nil {
		return nil, fmt.Errorf("could not start headscale container: %w", err)
	}
	log.Printf("Created %s container\n", hostname)

	hsic.container = container

	err = hsic.WriteFile("/etc/headscale/config.yaml", []byte(DefaultConfigYAML()))
	if err != nil {
		return nil, fmt.Errorf("failed to write headscale config to container: %w", err)
	}

	if hsic.aclPolicy != nil {
		data, err := json.Marshal(hsic.aclPolicy)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal ACL Policy to JSON: %w", err)
		}

		err = hsic.WriteFile(aclPolicyPath, data)
		if err != nil {
			return nil, fmt.Errorf("failed to write ACL policy to container: %w", err)
		}
	}

	return hsic, nil
}

func (t *HeadscaleInContainer) Shutdown() error {
	return t.pool.Purge(t.container)
}

func (t *HeadscaleInContainer) Execute(
	command []string,
) (string, error) {
	log.Println("command", command)
	log.Printf("running command for %s\n", t.hostname)
	stdout, stderr, err := dockertestutil.ExecuteCommand(
		t.container,
		command,
		[]string{},
	)
	if err != nil {
		log.Printf("command stderr: %s\n", stderr)

		return "", err
	}

	if stdout != "" {
		log.Printf("command stdout: %s\n", stdout)
	}

	return stdout, nil
}

func (t *HeadscaleInContainer) GetIP() string {
	return t.container.GetIPInNetwork(t.network)
}

func (t *HeadscaleInContainer) GetPort() string {
	portProto := fmt.Sprintf("%d/tcp", t.port)

	return t.container.GetPort(portProto)
}

func (t *HeadscaleInContainer) GetHealthEndpoint() string {
	hostEndpoint := fmt.Sprintf("%s:%d",
		t.GetIP(),
		t.port)

	return fmt.Sprintf("http://%s/health", hostEndpoint)
}

func (t *HeadscaleInContainer) GetEndpoint() string {
	hostEndpoint := fmt.Sprintf("%s:%d",
		t.GetIP(),
		t.port)

	return fmt.Sprintf("http://%s", hostEndpoint)
}

func (t *HeadscaleInContainer) WaitForReady() error {
	url := t.GetHealthEndpoint()

	log.Printf("waiting for headscale to be ready at %s", url)

	return t.pool.Retry(func() error {
		resp, err := http.Get(url) //nolint
		if err != nil {
			return fmt.Errorf("headscale is not ready: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			return errHeadscaleStatusCodeNotOk
		}

		return nil
	})
}

func (t *HeadscaleInContainer) CreateNamespace(
	namespace string,
) error {
	command := []string{"headscale", "namespaces", "create", namespace}

	_, _, err := dockertestutil.ExecuteCommand(
		t.container,
		command,
		[]string{},
	)
	if err != nil {
		return err
	}

	return nil
}

func (t *HeadscaleInContainer) CreateAuthKey(
	namespace string,
) (*v1.PreAuthKey, error) {
	command := []string{
		"headscale",
		"--namespace",
		namespace,
		"preauthkeys",
		"create",
		"--reusable",
		"--expiration",
		"24h",
		"--output",
		"json",
	}

	result, _, err := dockertestutil.ExecuteCommand(
		t.container,
		command,
		[]string{},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to execute create auth key command: %w", err)
	}

	var preAuthKey v1.PreAuthKey
	err = json.Unmarshal([]byte(result), &preAuthKey)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal auth key: %w", err)
	}

	return &preAuthKey, nil
}

func (t *HeadscaleInContainer) ListMachinesInNamespace(
	namespace string,
) ([]*v1.Machine, error) {
	command := []string{"headscale", "--namespace", namespace, "nodes", "list", "--output", "json"}

	result, _, err := dockertestutil.ExecuteCommand(
		t.container,
		command,
		[]string{},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to execute list node command: %w", err)
	}

	var nodes []*v1.Machine
	err = json.Unmarshal([]byte(result), &nodes)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal nodes: %w", err)
	}

	return nodes, nil
}

func (t *HeadscaleInContainer) WriteFile(path string, data []byte) error {
	dirPath, fileName := filepath.Split(path)

	file := bytes.NewReader(data)

	buf := bytes.NewBuffer([]byte{})

	tarWriter := tar.NewWriter(buf)

	header := &tar.Header{
		Name: fileName,
		Size: file.Size(),
		// Mode:    int64(stat.Mode()),
		// ModTime: stat.ModTime(),
	}

	err := tarWriter.WriteHeader(header)
	if err != nil {
		return fmt.Errorf("failed write file header to tar: %w", err)
	}

	_, err = io.Copy(tarWriter, file)
	if err != nil {
		return fmt.Errorf("failed to copy file to tar: %w", err)
	}

	err = tarWriter.Close()
	if err != nil {
		return fmt.Errorf("failed to close tar: %w", err)
	}

	log.Printf("tar: %s", buf.String())

	// Ensure the directory is present inside the container
	_, err = t.Execute([]string{"mkdir", "-p", dirPath})
	if err != nil {
		return fmt.Errorf("failed to ensure directory: %w", err)
	}

	err = t.pool.Client.UploadToContainer(
		t.container.Container.ID,
		docker.UploadToContainerOptions{
			NoOverwriteDirNonDir: false,
			Path:                 dirPath,
			InputStream:          bytes.NewReader(buf.Bytes()),
		},
	)
	if err != nil {
		return err
	}

	return nil
}
