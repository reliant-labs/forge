package mlcollect

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

var normalizedAttachedCases = []struct {
	name   string
	script string
	want   string
	secret string
}{
	{"single quoted whole flag", `agent '--token=first second'`, `agent '--token=REDACTED'`, "second"},
	{"double quoted whole flag", `agent "--token=first second"`, `agent '--token=REDACTED'`, "second"},
	{"escaped flag name", `agent --to\ken=opaque-value`, `agent '--token=REDACTED'`, "opaque-value"},
	{"quoted mysql password", `mysql '-popaque-mysql-value'`, `mysql '-pREDACTED'`, "opaque-mysql-value"},
}

func TestRedactNormalizedAttachedCredentials(t *testing.T) {
	t.Parallel()
	for _, tc := range normalizedAttachedCases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactArgs([]string{"sh", "-c", tc.script})
			if got[2] != tc.want {
				t.Errorf("script = %q, want %q", got[2], tc.want)
			}
		})
	}
}

func TestCollectSerializesNormalizedAttachedCredentials(t *testing.T) {
	t.Parallel()
	for _, tc := range normalizedAttachedCases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.kube.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, &corev1.PodList{Items: []corev1.Pod{{
					ObjectMeta: meta("serving", "agent"),
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: "agent", Image: "agent:1", Command: []string{"sh", "-c"}, Args: []string{tc.script},
					}}},
				}}}, nil
			})
			s, err := Collect(context.Background(), f.clients(), Options{Now: fixedNow, ConfigRefs: noRefs})
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Pods) != 1 || len(s.Pods[0].Spec.Containers) != 1 {
				t.Fatal("missing collected container")
			}
			args := s.Pods[0].Spec.Containers[0].Args
			if len(args) != 1 || args[0] != tc.want {
				t.Errorf("collected args = %q, want %q", args, tc.want)
			}
			data, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), tc.secret) || strings.Contains(string(data), "first") {
				t.Error("serialized snapshot retained part of an attached credential")
			}
			if !strings.Contains(string(data), "REDACTED") {
				t.Error("serialized snapshot lacks redaction marker")
			}
		})
	}
}
