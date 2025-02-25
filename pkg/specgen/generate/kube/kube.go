package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/containers/common/libimage"
	"github.com/containers/common/pkg/parse"
	"github.com/containers/common/pkg/secrets"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/podman/v3/libpod/network/types"
	ann "github.com/containers/podman/v3/pkg/annotations"
	"github.com/containers/podman/v3/pkg/domain/entities"
	"github.com/containers/podman/v3/pkg/specgen"
	"github.com/containers/podman/v3/pkg/specgen/generate"
	"github.com/containers/podman/v3/pkg/util"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func ToPodOpt(ctx context.Context, podName string, p entities.PodCreateOptions, podYAML *v1.PodTemplateSpec) (entities.PodCreateOptions, error) {
	//	p := specgen.NewPodSpecGenerator()
	p.Net = &entities.NetOptions{}
	p.Name = podName
	p.Labels = podYAML.ObjectMeta.Labels
	// Kube pods must share {ipc, net, uts} by default
	p.Share = append(p.Share, "ipc")
	p.Share = append(p.Share, "net")
	p.Share = append(p.Share, "uts")
	// TODO we only configure Process namespace. We also need to account for Host{IPC,Network,PID}
	// which is not currently possible with pod create
	if podYAML.Spec.ShareProcessNamespace != nil && *podYAML.Spec.ShareProcessNamespace {
		p.Share = append(p.Share, "pid")
	}
	p.Hostname = podYAML.Spec.Hostname
	if p.Hostname == "" {
		p.Hostname = podName
	}
	if podYAML.Spec.HostNetwork {
		p.Net.Network = specgen.Namespace{NSMode: "host"}
	}
	if podYAML.Spec.HostAliases != nil {
		hosts := make([]string, 0, len(podYAML.Spec.HostAliases))
		for _, hostAlias := range podYAML.Spec.HostAliases {
			for _, host := range hostAlias.Hostnames {
				hosts = append(hosts, host+":"+hostAlias.IP)
			}
		}
		p.Net.AddHosts = hosts
	}
	podPorts := getPodPorts(podYAML.Spec.Containers)
	p.Net.PublishPorts = podPorts

	if dnsConfig := podYAML.Spec.DNSConfig; dnsConfig != nil {
		// name servers
		if dnsServers := dnsConfig.Nameservers; len(dnsServers) > 0 {
			servers := make([]net.IP, 0)
			for _, server := range dnsServers {
				servers = append(servers, net.ParseIP(server))
			}
			p.Net.DNSServers = servers
		}
		// search domains
		if domains := dnsConfig.Searches; len(domains) > 0 {
			p.Net.DNSSearch = domains
		}
		// dns options
		if options := dnsConfig.Options; len(options) > 0 {
			dnsOptions := make([]string, 0)
			for _, opts := range options {
				d := opts.Name
				if opts.Value != nil {
					d += ":" + *opts.Value
				}
				dnsOptions = append(dnsOptions, d)
			}
		}
	}
	return p, nil
}

type CtrSpecGenOptions struct {
	// Container as read from the pod yaml
	Container v1.Container
	// Image available to use (pulled or found local)
	Image *libimage.Image
	// Volumes for all containers
	Volumes map[string]*KubeVolume
	// PodID of the parent pod
	PodID string
	// PodName of the parent pod
	PodName string
	// PodInfraID as the infrastructure container id
	PodInfraID string
	// ConfigMaps the configuration maps for environment variables
	ConfigMaps []v1.ConfigMap
	// SeccompPaths for finding the seccomp profile path
	SeccompPaths *KubeSeccompPaths
	// RestartPolicy defines the restart policy of the container
	RestartPolicy string
	// NetNSIsHost tells the container to use the host netns
	NetNSIsHost bool
	// SecretManager to access the secrets
	SecretsManager *secrets.SecretsManager
	// LogDriver which should be used for the container
	LogDriver string
	// Labels define key-value pairs of metadata
	Labels map[string]string
	//
	IsInfra bool
	// InitContainerType sets what type the init container is
	// Note: When playing a kube yaml, the inti container type will be set to "always" only
	InitContainerType string
}

func ToSpecGen(ctx context.Context, opts *CtrSpecGenOptions) (*specgen.SpecGenerator, error) {
	s := specgen.NewSpecGenerator(opts.Container.Image, false)

	// pod name should be non-empty for Deployment objects to be able to create
	// multiple pods having containers with unique names
	if len(opts.PodName) < 1 {
		return nil, errors.Errorf("got empty pod name on container creation when playing kube")
	}

	s.Name = fmt.Sprintf("%s-%s", opts.PodName, opts.Container.Name)

	s.Terminal = opts.Container.TTY

	s.Pod = opts.PodID

	s.LogConfiguration = &specgen.LogConfig{
		Driver: opts.LogDriver,
	}

	s.InitContainerType = opts.InitContainerType

	setupSecurityContext(s, opts.Container)
	err := setupLivenessProbe(s, opts.Container, opts.RestartPolicy)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to configure livenessProbe")
	}

	// Since we prefix the container name with pod name to work-around the uniqueness requirement,
	// the seccomp profile should reference the actual container name from the YAML
	// but apply to the containers with the prefixed name
	s.SeccompProfilePath = opts.SeccompPaths.FindForContainer(opts.Container.Name)

	s.ResourceLimits = &spec.LinuxResources{}
	milliCPU, err := quantityToInt64(opts.Container.Resources.Limits.Cpu())
	if err != nil {
		return nil, errors.Wrap(err, "Failed to set CPU quota")
	}
	if milliCPU > 0 {
		period, quota := util.CoresToPeriodAndQuota(float64(milliCPU) / 1000)
		s.ResourceLimits.CPU = &spec.LinuxCPU{
			Quota:  &quota,
			Period: &period,
		}
	}

	limit, err := quantityToInt64(opts.Container.Resources.Limits.Memory())
	if err != nil {
		return nil, errors.Wrap(err, "Failed to set memory limit")
	}

	memoryRes, err := quantityToInt64(opts.Container.Resources.Requests.Memory())
	if err != nil {
		return nil, errors.Wrap(err, "Failed to set memory reservation")
	}

	if limit > 0 || memoryRes > 0 {
		s.ResourceLimits.Memory = &spec.LinuxMemory{}
	}

	if limit > 0 {
		s.ResourceLimits.Memory.Limit = &limit
	}

	if memoryRes > 0 {
		s.ResourceLimits.Memory.Reservation = &memoryRes
	}

	// TODO: We don't understand why specgen does not take of this, but
	// integration tests clearly pointed out that it was required.
	imageData, err := opts.Image.Inspect(ctx, false)
	if err != nil {
		return nil, err
	}
	s.WorkDir = "/"
	// Entrypoint/Command handling is based off of
	// https://kubernetes.io/docs/tasks/inject-data-application/define-command-argument-container/#notes
	if imageData != nil && imageData.Config != nil {
		if imageData.Config.WorkingDir != "" {
			s.WorkDir = imageData.Config.WorkingDir
		}
		if s.User == "" {
			s.User = imageData.Config.User
		}

		exposed, err := generate.GenExposedPorts(imageData.Config.ExposedPorts)
		if err != nil {
			return nil, err
		}

		for k, v := range s.Expose {
			exposed[k] = v
		}
		s.Expose = exposed
		// Pull entrypoint and cmd from image
		s.Entrypoint = imageData.Config.Entrypoint
		s.Command = imageData.Config.Cmd
		s.Labels = imageData.Config.Labels
		if len(imageData.Config.StopSignal) > 0 {
			stopSignal, err := util.ParseSignal(imageData.Config.StopSignal)
			if err != nil {
				return nil, err
			}
			s.StopSignal = &stopSignal
		}
	}
	// If only the yaml.Command is specified, set it as the entrypoint and drop the image Cmd
	if !opts.IsInfra && len(opts.Container.Command) != 0 {
		s.Entrypoint = opts.Container.Command
		s.Command = []string{}
	}
	// Only override the cmd field if yaml.Args is specified
	// Keep the image entrypoint, or the yaml.command if specified
	if !opts.IsInfra && len(opts.Container.Args) != 0 {
		s.Command = opts.Container.Args
	}

	// FIXME,
	// we are currently ignoring imageData.Config.ExposedPorts
	if !opts.IsInfra && opts.Container.WorkingDir != "" {
		s.WorkDir = opts.Container.WorkingDir
	}

	annotations := make(map[string]string)
	if opts.PodInfraID != "" {
		annotations[ann.SandboxID] = opts.PodInfraID
		annotations[ann.ContainerType] = ann.ContainerTypeContainer
	}
	s.Annotations = annotations

	// Environment Variables
	envs := map[string]string{}
	for _, env := range imageData.Config.Env {
		keyval := strings.Split(env, "=")
		envs[keyval[0]] = keyval[1]
	}

	for _, env := range opts.Container.Env {
		value, err := envVarValue(env, opts)
		if err != nil {
			return nil, err
		}

		envs[env.Name] = value
	}
	for _, envFrom := range opts.Container.EnvFrom {
		cmEnvs, err := envVarsFrom(envFrom, opts)
		if err != nil {
			return nil, err
		}

		for k, v := range cmEnvs {
			envs[k] = v
		}
	}
	s.Env = envs

	for _, volume := range opts.Container.VolumeMounts {
		volumeSource, exists := opts.Volumes[volume.Name]
		if !exists {
			return nil, errors.Errorf("Volume mount %s specified for container but not configured in volumes", volume.Name)
		}

		dest, options, err := parseMountPath(volume.MountPath, volume.ReadOnly)
		if err != nil {
			return nil, err
		}

		volume.MountPath = dest
		switch volumeSource.Type {
		case KubeVolumeTypeBindMount:
			mount := spec.Mount{
				Destination: volume.MountPath,
				Source:      volumeSource.Source,
				Type:        "bind",
				Options:     options,
			}
			s.Mounts = append(s.Mounts, mount)
		case KubeVolumeTypeNamed:
			namedVolume := specgen.NamedVolume{
				Dest:    volume.MountPath,
				Name:    volumeSource.Source,
				Options: options,
			}
			s.Volumes = append(s.Volumes, &namedVolume)
		default:
			return nil, errors.Errorf("Unsupported volume source type")
		}
	}

	s.RestartPolicy = opts.RestartPolicy

	if opts.NetNSIsHost {
		s.NetNS.NSMode = specgen.Host
	}
	// Always set the userns to host since k8s doesn't have support for userns yet
	s.UserNS.NSMode = specgen.Host

	// Add labels that come from kube
	if len(s.Labels) == 0 {
		// If there are no labels, let's use the map that comes
		// from kube
		s.Labels = opts.Labels
	} else {
		// If there are already labels in the map, append the ones
		// obtained from kube
		for k, v := range opts.Labels {
			s.Labels[k] = v
		}
	}

	return s, nil
}

func parseMountPath(mountPath string, readOnly bool) (string, []string, error) {
	options := []string{}
	splitVol := strings.Split(mountPath, ":")
	if len(splitVol) > 2 {
		return "", options, errors.Errorf("%q incorrect volume format, should be ctr-dir[:option]", mountPath)
	}
	dest := splitVol[0]
	if len(splitVol) > 1 {
		options = strings.Split(splitVol[1], ",")
	}
	if err := parse.ValidateVolumeCtrDir(dest); err != nil {
		return "", options, errors.Wrapf(err, "parsing MountPath")
	}
	if readOnly {
		options = append(options, "ro")
	}
	opts, err := parse.ValidateVolumeOpts(options)
	if err != nil {
		return "", opts, errors.Wrapf(err, "parsing MountOptions")
	}
	return dest, opts, nil
}

func setupLivenessProbe(s *specgen.SpecGenerator, containerYAML v1.Container, restartPolicy string) error {
	var err error
	if containerYAML.LivenessProbe == nil {
		return nil
	}
	emptyHandler := v1.Handler{}
	if containerYAML.LivenessProbe.Handler != emptyHandler {
		var commandString string
		failureCmd := "exit 1"
		probe := containerYAML.LivenessProbe
		probeHandler := probe.Handler

		// append `exit 1` to `cmd` so healthcheck can be marked as `unhealthy`.
		// append `kill 1` to `cmd` if appropriate restart policy is configured.
		if restartPolicy == "always" || restartPolicy == "onfailure" {
			// container will be restarted so we can kill init.
			failureCmd = "kill 1"
		}

		// configure healthcheck on the basis of Handler Actions.
		if probeHandler.Exec != nil {
			execString := strings.Join(probeHandler.Exec.Command, " ")
			commandString = fmt.Sprintf("%s || %s", execString, failureCmd)
		} else if probeHandler.HTTPGet != nil {
			commandString = fmt.Sprintf("curl %s://%s:%d/%s  || %s", probeHandler.HTTPGet.Scheme, probeHandler.HTTPGet.Host, probeHandler.HTTPGet.Port.IntValue(), probeHandler.HTTPGet.Path, failureCmd)
		} else if probeHandler.TCPSocket != nil {
			commandString = fmt.Sprintf("nc -z -v %s %d || %s", probeHandler.TCPSocket.Host, probeHandler.TCPSocket.Port.IntValue(), failureCmd)
		}
		s.HealthConfig, err = makeHealthCheck(commandString, probe.PeriodSeconds, probe.FailureThreshold, probe.TimeoutSeconds, probe.InitialDelaySeconds)
		if err != nil {
			return err
		}
		return nil
	}
	return nil
}

func makeHealthCheck(inCmd string, interval int32, retries int32, timeout int32, startPeriod int32) (*manifest.Schema2HealthConfig, error) {
	// Every healthcheck requires a command
	if len(inCmd) == 0 {
		return nil, errors.New("Must define a healthcheck command for all healthchecks")
	}

	// first try to parse option value as JSON array of strings...
	cmd := []string{}

	if inCmd == "none" {
		cmd = []string{"NONE"}
	} else {
		err := json.Unmarshal([]byte(inCmd), &cmd)
		if err != nil {
			// ...otherwise pass it to "/bin/sh -c" inside the container
			cmd = []string{"CMD-SHELL"}
			cmd = append(cmd, strings.Split(inCmd, " ")...)
		}
	}
	hc := manifest.Schema2HealthConfig{
		Test: cmd,
	}

	if interval < 1 {
		//kubernetes interval defaults to 10 sec and cannot be less than 1
		interval = 10
	}
	hc.Interval = (time.Duration(interval) * time.Second)
	if retries < 1 {
		//kubernetes retries defaults to 3
		retries = 3
	}
	hc.Retries = int(retries)
	if timeout < 1 {
		//kubernetes timeout defaults to 1
		timeout = 1
	}
	timeoutDuration := (time.Duration(timeout) * time.Second)
	if timeoutDuration < time.Duration(1) {
		return nil, errors.New("healthcheck-timeout must be at least 1 second")
	}
	hc.Timeout = timeoutDuration

	startPeriodDuration := (time.Duration(startPeriod) * time.Second)
	if startPeriodDuration < time.Duration(0) {
		return nil, errors.New("healthcheck-start-period must be 0 seconds or greater")
	}
	hc.StartPeriod = startPeriodDuration

	return &hc, nil
}

func setupSecurityContext(s *specgen.SpecGenerator, containerYAML v1.Container) {
	if containerYAML.SecurityContext == nil {
		return
	}
	if containerYAML.SecurityContext.ReadOnlyRootFilesystem != nil {
		s.ReadOnlyFilesystem = *containerYAML.SecurityContext.ReadOnlyRootFilesystem
	}
	if containerYAML.SecurityContext.Privileged != nil {
		s.Privileged = *containerYAML.SecurityContext.Privileged
	}

	if containerYAML.SecurityContext.AllowPrivilegeEscalation != nil {
		s.NoNewPrivileges = !*containerYAML.SecurityContext.AllowPrivilegeEscalation
	}

	if seopt := containerYAML.SecurityContext.SELinuxOptions; seopt != nil {
		if seopt.User != "" {
			s.SelinuxOpts = append(s.SelinuxOpts, fmt.Sprintf("user:%s", seopt.User))
		}
		if seopt.Role != "" {
			s.SelinuxOpts = append(s.SelinuxOpts, fmt.Sprintf("role:%s", seopt.Role))
		}
		if seopt.Type != "" {
			s.SelinuxOpts = append(s.SelinuxOpts, fmt.Sprintf("type:%s", seopt.Type))
		}
		if seopt.Level != "" {
			s.SelinuxOpts = append(s.SelinuxOpts, fmt.Sprintf("level:%s", seopt.Level))
		}
	}
	if caps := containerYAML.SecurityContext.Capabilities; caps != nil {
		for _, capability := range caps.Add {
			s.CapAdd = append(s.CapAdd, string(capability))
		}
		for _, capability := range caps.Drop {
			s.CapDrop = append(s.CapDrop, string(capability))
		}
	}
	if containerYAML.SecurityContext.RunAsUser != nil {
		s.User = fmt.Sprintf("%d", *containerYAML.SecurityContext.RunAsUser)
	}
	if containerYAML.SecurityContext.RunAsGroup != nil {
		if s.User == "" {
			s.User = "0"
		}
		s.User = fmt.Sprintf("%s:%d", s.User, *containerYAML.SecurityContext.RunAsGroup)
	}
}

func quantityToInt64(quantity *resource.Quantity) (int64, error) {
	if i, ok := quantity.AsInt64(); ok {
		return i, nil
	}

	if i, ok := quantity.AsDec().Unscaled(); ok {
		return i, nil
	}

	return 0, errors.Errorf("Quantity cannot be represented as int64: %v", quantity)
}

// read a k8s secret in JSON format from the secret manager
func k8sSecretFromSecretManager(name string, secretsManager *secrets.SecretsManager) (map[string][]byte, error) {
	_, jsonSecret, err := secretsManager.LookupSecretData(name)
	if err != nil {
		return nil, err
	}

	var secrets map[string][]byte
	if err := json.Unmarshal(jsonSecret, &secrets); err != nil {
		return nil, errors.Errorf("Secret %v is not valid JSON: %v", name, err)
	}
	return secrets, nil
}

// envVarsFrom returns all key-value pairs as env vars from a configMap or secret that matches the envFrom setting of a container
func envVarsFrom(envFrom v1.EnvFromSource, opts *CtrSpecGenOptions) (map[string]string, error) {
	envs := map[string]string{}

	if envFrom.ConfigMapRef != nil {
		cmRef := envFrom.ConfigMapRef
		err := errors.Errorf("Configmap %v not found", cmRef.Name)

		for _, c := range opts.ConfigMaps {
			if cmRef.Name == c.Name {
				envs = c.Data
				err = nil
				break
			}
		}

		if err != nil && (cmRef.Optional == nil || !*cmRef.Optional) {
			return nil, err
		}
	}

	if envFrom.SecretRef != nil {
		secRef := envFrom.SecretRef
		secret, err := k8sSecretFromSecretManager(secRef.Name, opts.SecretsManager)
		if err == nil {
			for k, v := range secret {
				envs[k] = string(v)
			}
		} else if secRef.Optional == nil || !*secRef.Optional {
			return nil, err
		}
	}

	return envs, nil
}

// envVarValue returns the environment variable value configured within the container's env setting.
// It gets the value from a configMap or secret if specified, otherwise returns env.Value
func envVarValue(env v1.EnvVar, opts *CtrSpecGenOptions) (string, error) {
	if env.ValueFrom != nil {
		if env.ValueFrom.ConfigMapKeyRef != nil {
			cmKeyRef := env.ValueFrom.ConfigMapKeyRef
			err := errors.Errorf("Cannot set env %v: configmap %v not found", env.Name, cmKeyRef.Name)

			for _, c := range opts.ConfigMaps {
				if cmKeyRef.Name == c.Name {
					if value, ok := c.Data[cmKeyRef.Key]; ok {
						return value, nil
					}
					err = errors.Errorf("Cannot set env %v: key %s not found in configmap %v", env.Name, cmKeyRef.Key, cmKeyRef.Name)
					break
				}
			}
			if cmKeyRef.Optional == nil || !*cmKeyRef.Optional {
				return "", err
			}
			return "", nil
		}

		if env.ValueFrom.SecretKeyRef != nil {
			secKeyRef := env.ValueFrom.SecretKeyRef
			secret, err := k8sSecretFromSecretManager(secKeyRef.Name, opts.SecretsManager)
			if err == nil {
				if val, ok := secret[secKeyRef.Key]; ok {
					return string(val), nil
				}
				err = errors.Errorf("Secret %v has not %v key", secKeyRef.Name, secKeyRef.Key)
			}
			if secKeyRef.Optional == nil || !*secKeyRef.Optional {
				return "", errors.Errorf("Cannot set env %v: %v", env.Name, err)
			}
			return "", nil
		}
	}

	return env.Value, nil
}

// getPodPorts converts a slice of kube container descriptions to an
// array of portmapping
func getPodPorts(containers []v1.Container) []types.PortMapping {
	var infraPorts []types.PortMapping
	for _, container := range containers {
		for _, p := range container.Ports {
			if p.HostPort != 0 && p.ContainerPort == 0 {
				p.ContainerPort = p.HostPort
			}
			if p.Protocol == "" {
				p.Protocol = "tcp"
			}
			portBinding := types.PortMapping{
				HostPort:      uint16(p.HostPort),
				ContainerPort: uint16(p.ContainerPort),
				Protocol:      strings.ToLower(string(p.Protocol)),
				HostIP:        p.HostIP,
			}
			// only hostPort is utilized in podman context, all container ports
			// are accessible inside the shared network namespace
			if p.HostPort != 0 {
				infraPorts = append(infraPorts, portBinding)
			}
		}
	}
	return infraPorts
}
