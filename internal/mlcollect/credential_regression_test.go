package mlcollect

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

func TestProgramAttachedCredentials(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		program, flag, value string
	}{
		{"sshpass", "-p", "123456"},
		{"redis-cli", "-a", "opaque-password"},
		{"openssl", "-k", "opaque pass phrase"},
		{"smbclient", "-U", "opaque-user"},
		{"mysql", "-p", "3306"},
	} {
		t.Run(tc.program, func(t *testing.T) {
			argv := []string{tc.program, tc.flag + tc.value, "host"}
			want := []string{tc.program, tc.flag + Redacted, "host"}
			if got := RedactArgs(argv); !reflect.DeepEqual(got, want) {
				t.Errorf("argv = %q, want %q", got, want)
			}
			script := "env " + tc.program + " " + tc.flag + "'" + tc.value + "' host"
			got := RedactArgs([]string{"sh", "-c", script})[2]
			if strings.Contains(got, tc.value) || !strings.Contains(got, tc.flag+Redacted) {
				t.Errorf("script = %q", got)
			}
		})
	}
	// A program's credential flag must not erase another program's port.
	if got := RedactArgs([]string{"ssh", "-p2222", "host"}); got[1] != "-p2222" {
		t.Errorf("ssh port = %q", got)
	}
}

func TestCollectSerializedCoreCredentials(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	gmsa := `{"credential":"inline-gmsa-secret"}`
	windows := func() *corev1.SecurityContext {
		return &corev1.SecurityContext{WindowsOptions: &corev1.WindowsSecurityContextOptions{GMSACredentialSpec: &gmsa}}
	}
	container := func(name string) corev1.Container {
		return corev1.Container{
			Name: name, Image: "example.com/model:1", SecurityContext: windows(),
			Command: []string{"sshpass"}, Args: []string{"-p123456", "ssh", "host"},
			Env: []corev1.EnvVar{{Name: "CUSTOM", Value: "literal-env-secret"}},
			LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{"redis-cli", "-aprobe-password", "ping"}},
			}},
			Lifecycle: &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/?token=hook-query-secret", HTTPHeaders: []corev1.HTTPHeader{{Name: "Authorization", Value: "hook-header-secret"}}},
			}},
		}
	}
	pod := corev1.Pod{
		ObjectMeta: meta("default", "credential-regression"),
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{container("main")}, InitContainers: []corev1.Container{container("init")},
			EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{
				Name: "debug", Command: []string{"sh", "-c"}, Args: []string{"sshpass -pscript-password ssh host; redis-cli -aredis-password ping"},
				SecurityContext: windows(),
			}}},
			SecurityContext: &corev1.PodSecurityContext{WindowsOptions: &corev1.WindowsSecurityContextOptions{GMSACredentialSpec: &gmsa}},
			Volumes: []corev1.Volume{
				{Name: "repo", VolumeSource: corev1.VolumeSource{GitRepo: &corev1.GitRepoVolumeSource{Repository: "https://user:repository-password@host/repo"}}},
				{Name: "disk", VolumeSource: corev1.VolumeSource{AzureDisk: &corev1.AzureDiskVolumeSource{DataDiskURI: "https://host/disk?signature=disk-signature-secret"}}},
				{Name: "csi", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "driver", VolumeAttributes: map[string]string{"password": "csi-password", "endpoint": "https://user:csi-url-password@host"}}}},
				{Name: "flex", VolumeSource: corev1.VolumeSource{FlexVolume: &corev1.FlexVolumeSource{Driver: "driver", Options: map[string]string{"token": "flex-token"}}}},
				{Name: "ephemeral", VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"config": "claim-annotation-secret"}}}}}},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, Message: "pod-status-secret",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, Message: "pod-condition-secret"}},
		},
	}
	pod.Labels["api-token"] = "label-secret"
	statuses := []corev1.ContainerStatus{{Name: "main", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Message: "waiting-secret"}}, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: "termination-secret"}}}}
	pod.Status.ContainerStatuses = statuses
	pod.Status.InitContainerStatuses = statuses
	pod.Status.EphemeralContainerStatuses = statuses
	f.kube.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.PodList{Items: []corev1.Pod{*pod.DeepCopy()}}, nil
	})
	f.kube.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.NodeList{Items: []corev1.Node{{ObjectMeta: meta("", "gpu-0"),
			Spec: corev1.NodeSpec{ProviderID: "https://user:provider-password@host"},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Message: "node-condition-secret"}}, Images: []corev1.ContainerImage{{Names: []string{"https://user:image-password@host/image"}}}},
		}}}, nil
	})
	s, err := Collect(context.Background(), f.clients(), Options{Now: fixedNow, ConfigRefs: noRefs})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"123456", "probe-password", "script-password", "redis-password", "repository-password",
		"disk-signature-secret", "csi-password", "csi-url-password", "flex-token", "claim-annotation-secret",
		"inline-gmsa-secret", "literal-env-secret", "hook-query-secret", "hook-header-secret", "label-secret",
		"pod-status-secret", "pod-condition-secret", "waiting-secret", "termination-secret",
		"provider-password", "node-condition-secret", "image-password",
	} {
		if strings.Contains(string(b), secret) {
			t.Errorf("serialized snapshot retained %q", secret)
		}
	}
	if len(s.ReadErrors) != 0 || len(s.Pods) != 1 || len(s.Nodes) != 1 {
		t.Fatalf("incomplete collection: %d pods, %d nodes, errors %v", len(s.Pods), len(s.Nodes), s.ReadErrors)
	}
	if s.Pods[0].Spec.Containers[0].Image != "example.com/model:1" || s.Pods[0].Status.Phase != corev1.PodRunning || s.Nodes[0].Status.Conditions[0].Status != corev1.ConditionTrue {
		t.Error("redaction lost inventory or scheduling state")
	}
	if got := s.Pods[0].Spec.Volumes[0].GitRepo.Repository; got != "https://user:"+Redacted+"@host/repo" {
		t.Errorf("repository = %q", got)
	}
}
