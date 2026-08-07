package agent

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// kyber#566: the agent pod must carry its control-plane-signed pod-token so its
// clients can authenticate to the internal API. The volume is Optional so a pod
// still schedules during the migration window before the Secret is minted.
func TestBuildPodSpec_PodTokenVolumeAndMounts(t *testing.T) {
	agent := testAgent()
	adapter := testAdapter()

	pod, err := BuildPodSpec(agent, adapter, "node-01")
	if err != nil {
		t.Fatalf("BuildPodSpec: %v", err)
	}

	// Volume: Optional Secret source pointing at <name>-pod-token.
	var vol *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == PodTokenVolumeName {
			vol = &pod.Volumes[i]
			break
		}
	}
	if vol == nil {
		t.Fatalf("pod-token volume %q not found", PodTokenVolumeName)
	}
	if vol.Secret == nil {
		t.Fatalf("pod-token volume is not a Secret source")
	}
	if vol.Secret.SecretName != PodTokenSecretName("dave") {
		t.Errorf("pod-token secret name: got %q, want %q", vol.Secret.SecretName, PodTokenSecretName("dave"))
	}
	if vol.Secret.Optional == nil || !*vol.Secret.Optional {
		t.Errorf("pod-token volume must be Optional for migration safety")
	}

	// Main container mounts it read-only at PodTokenMountDir.
	if !hasMount(pod.Containers[0].VolumeMounts, PodTokenVolumeName, PodTokenMountDir) {
		t.Errorf("main container missing pod-token mount at %s", PodTokenMountDir)
	}

	// Init container (session-brief) mounts it too AND presents it as a Bearer.
	if len(pod.InitContainers) == 0 {
		t.Fatal("expected a session-brief init container")
	}
	init := pod.InitContainers[0]
	if !hasMount(init.VolumeMounts, PodTokenVolumeName, PodTokenMountDir) {
		t.Errorf("init container missing pod-token mount at %s", PodTokenMountDir)
	}
	joinedArgs := strings.Join(init.Args, " ")
	if !strings.Contains(joinedArgs, "Authorization: Bearer") {
		t.Errorf("init container curl does not present a Bearer token; args: %q", joinedArgs)
	}
	if !strings.Contains(joinedArgs, PodTokenMountDir+"/"+PodTokenSecretKey) {
		t.Errorf("init container does not read the token from %s/%s; args: %q",
			PodTokenMountDir, PodTokenSecretKey, joinedArgs)
	}
}

// The status-sidecar reads the same pod-token path; it must get the mount too.
func TestAppendStatusSidecar_MountsPodToken(t *testing.T) {
	spec := &corev1.PodSpec{}
	AppendStatusSidecar(spec, SidecarConfig{AgentName: "dave", Image: "ghcr.io/x/sidecar:latest"})
	// kyber#575: the sidecar is now a native sidecar in InitContainers; the
	// pod-token mount must carry over to it unchanged.
	if len(spec.InitContainers) != 1 {
		t.Fatalf("expected sidecar appended to InitContainers, got %d", len(spec.InitContainers))
	}
	if !hasMount(spec.InitContainers[0].VolumeMounts, PodTokenVolumeName, PodTokenMountDir) {
		t.Errorf("status-sidecar missing pod-token mount at %s", PodTokenMountDir)
	}
}

func hasMount(mounts []corev1.VolumeMount, name, path string) bool {
	for _, vm := range mounts {
		if vm.Name == name && vm.MountPath == path {
			return true
		}
	}
	return false
}
