package selfupgrade

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/matty-v/kyber/pkg/updates"
	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
)

const (
	credentialProtocolAnnotation   = "kyber.io/operator-credential-authority-protocol"
	credentialImageAnnotation      = "kyber.io/operator-credential-release-image"
	supportedCredentialProtocol    = "1"
	officialControlPlaneRepository = "ghcr.io/matty-v/kyber-control-plane"
	maxCompatibilityManifestBytes  = 16 * 1024 * 1024
)

var errCredentialCompatibility = errors.New("operator credential compatibility could not be verified; use a supported Kyber release without a custom control-plane image")

// credentialCompatibleDeployment checks the effective workload, including the
// release-image assertion carried independently of operator image values. The
// assertion is a contract of the trusted Kyber chart publisher, not a signature
// or protection against a privileged operator rewriting release history.
//
// Never return parser errors: manifests can contain installation credentials.
func credentialCompatibleDeployment(manifest, namespace, name string) (*appsv1.Deployment, error) {
	if namespace == "" || name == "" || len(manifest) > maxCompatibilityManifestBytes {
		return nil, errCredentialCompatibility
	}
	decoder := yaml.NewDecoder(strings.NewReader(manifest))
	var found *appsv1.Deployment
	for {
		var object map[string]any
		if err := decoder.Decode(&object); err == io.EOF {
			break
		} else if err != nil {
			return nil, errCredentialCompatibility
		}
		if len(object) == 0 {
			continue
		}
		for key := range object {
			if strings.EqualFold(key, "kind") && key != "kind" {
				return nil, errCredentialCompatibility
			}
		}
		if object["kind"] == "List" {
			return nil, errCredentialCompatibility
		}
		if object["kind"] != "Deployment" {
			continue
		}
		raw, err := json.Marshal(object)
		if err != nil {
			return nil, errCredentialCompatibility
		}
		var deployment appsv1.Deployment
		if json.Unmarshal(raw, &deployment) != nil {
			return nil, errCredentialCompatibility
		}
		if deployment.Name != name && deployment.Labels["app.kubernetes.io/component"] != "control-plane" {
			continue
		}
		if found != nil || deployment.APIVersion != "apps/v1" || deployment.Name != name || deployment.Namespace != namespace || deployment.Labels["app.kubernetes.io/component"] != "control-plane" {
			return nil, errCredentialCompatibility
		}
		annotations := deployment.Spec.Template.Annotations
		if annotations[credentialProtocolAnnotation] != supportedCredentialProtocol || len(deployment.Spec.Template.Spec.Containers) != 1 || len(deployment.Spec.Template.Spec.InitContainers) != 0 {
			return nil, errCredentialCompatibility
		}
		container := deployment.Spec.Template.Spec.Containers[0]
		releaseImage := annotations[credentialImageAnnotation]
		if container.Name != "control-plane" || container.Image != releaseImage || len(container.Command) != 0 || !strings.HasPrefix(releaseImage, officialControlPlaneRepository+":") || imageTag(releaseImage) == "" || strings.Contains(releaseImage, "@") {
			return nil, errCredentialCompatibility
		}
		tag := imageTag(releaseImage)
		version, err := updates.ParseVersion(tag)
		if err != nil || version.String() != strings.TrimPrefix(tag, "v") {
			return nil, errCredentialCompatibility
		}
		// Supported charts use the official image entrypoint and only these two
		// process flags. Do not infer compatibility for a replaced entrypoint.
		for _, arg := range container.Args {
			if strings.HasPrefix(arg, "--metrics-bind-address=") {
				continue
			}
			if arg == "--leader-elect=true" || arg == "--leader-elect=false" {
				continue
			}
			return nil, errCredentialCompatibility
		}
		for _, mount := range container.VolumeMounts {
			// Never allow mounted content to shadow the binary, libraries or the
			// installation namespace used by the fixed authority resolver.
			if mount.MountPath != "/etc/kyber" && mount.MountPath != "/etc/kyber-anthropic-key" {
				return nil, errCredentialCompatibility
			}
		}
		found = &deployment
	}
	if found == nil {
		return nil, errCredentialCompatibility
	}
	return found, nil
}
