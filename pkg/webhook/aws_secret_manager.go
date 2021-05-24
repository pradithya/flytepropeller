package webhook

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/flyteorg/flyteidl/gen/pb-go/flyteidl/core"
	"github.com/flyteorg/flytestdlib/logger"
	corev1 "k8s.io/api/core/v1"
)

const (
	AWSSecretArnEnvVar       = "secrets.k8s.aws/secret-arn"
	AWSSecretMountPathEnvVar = "secrets.k8s.aws/mount-path"
	AWSSecretFileNameEnvVar  = "secrets.k8s.aws/secret-filename"
	AWSSecretMountPathPrefix = "/etc/flyte/secrets/"
)

// AWSSecretManagerInjector allows injecting of secrets into pods by specifying annotations on the Pod that either EnvVarSource or SecretVolumeSource in
// the Pod Spec. It'll, by default, mount secrets as files into pods.
// The current version does not allow mounting an entire secret object (with all keys inside it). It only supports mounting
// a single key from the referenced secret object.
// The secret.Group will be used to reference the k8s secret object, the Secret.Key will be used to reference a key inside
// and the secret.Version will be ignored.
// Environment variables will be named _FSEC_<SecretGroup>_<SecretKey>. Files will be mounted on
// /etc/flyte/secrets/<SecretGroup>/<SecretKey>
type AWSSecretManagerInjector struct {
}

func formatAWSSecretArn(secret *core.Secret) string {
	return strings.TrimRight(secret.Group, ":") + ":" + strings.TrimLeft(secret.Key, ":")
}

func formatAWSSecretMount(secret *core.Secret) string {
	return AWSSecretMountPathPrefix + secret.Group
}

func (i AWSSecretManagerInjector) ID() string {
	return "K8s"
}

func (i AWSSecretManagerInjector) Inject(ctx context.Context, secret *core.Secret, p *corev1.Pod) (newP *corev1.Pod, injected bool, err error) {
	if len(secret.Group) == 0 || len(secret.Key) == 0 {
		return nil, false, fmt.Errorf("k8s Secrets Webhook require both key and group to be set. "+
			"Secret: [%v]", secret)
	}

	switch secret.MountRequirement {
	case core.Secret_ANY:
		fallthrough
	case core.Secret_FILE:
		// Inject a Volume that to the pod and all of its containers and init containers that mounts the secret into a
		// file.

		envVars := []corev1.EnvVar{
			{
				Name:  AWSSecretArnEnvVar,
				Value: formatAWSSecretArn(secret),
			},
			{
				Name:  AWSSecretMountPathEnvVar,
				Value: formatAWSSecretMount(secret),
			},
			{
				Name:  AWSSecretFileNameEnvVar,
				Value: secret.Key,
			},
		}

		volume := CreateVolumeForSecret(secret)
		p.Spec.Volumes = append(p.Spec.Volumes, volume)

		// Mount the secret to all containers in the given pod.
		mount := CreateVolumeMountForSecret(volume.Name, secret)
		p.Spec.InitContainers = UpdateVolumeMounts(p.Spec.InitContainers, mount)
		p.Spec.Containers = UpdateVolumeMounts(p.Spec.Containers, mount)

		// Set environment variable to let the container know where to find the mounted files.
		defaultDirEnvVar := corev1.EnvVar{
			Name:  K8sPathDefaultDirEnvVar,
			Value: filepath.Join(K8sSecretPathPrefix...),
		}

		p.Spec.InitContainers = UpdateEnvVars(p.Spec.InitContainers, defaultDirEnvVar)
		p.Spec.Containers = UpdateEnvVars(p.Spec.Containers, defaultDirEnvVar)

		// Sets an empty prefix to let the containers know the file names will match the secret keys as-is.
		prefixEnvVar := corev1.EnvVar{
			Name:  K8sPathFilePrefixEnvVar,
			Value: "",
		}

		p.Spec.InitContainers = UpdateEnvVars(p.Spec.InitContainers, prefixEnvVar)
		p.Spec.Containers = UpdateEnvVars(p.Spec.Containers, prefixEnvVar)
	case core.Secret_ENV_VAR:
		fallthrough
	default:
		err := fmt.Errorf("unrecognized mount requirement [%v] for secret [%v]", secret.MountRequirement.String(), secret.Key)
		logger.Error(ctx, err)
		return p, false, err
	}

	return p, true, nil
}

func NewAWSSecretManagerInjector() AWSSecretManagerInjector {
	return AWSSecretManagerInjector{}
}
