package mlcollect

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCredentialWhitespaceAndNumericPasswords(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		argv    []string
		want    []string
		secrets []string
	}{
		{
			name:    "attached user spaces",
			argv:    []string{"curl", "-uadmin:first second", "https://example.com"},
			want:    []string{"curl", "-u" + Redacted, "https://example.com"},
			secrets: []string{"first", "second"},
		},
		{
			name:    "attached proxy user newline",
			argv:    []string{"curl", "-Uadmin:first\nsecond", "https://example.com"},
			want:    []string{"curl", "-U" + Redacted, "https://example.com"},
			secrets: []string{"first", "second"},
		},
		{
			name:    "quoted shell user newline",
			argv:    []string{"sh", "-c", "curl '-uadmin:first \nsecond' https://example.com"},
			want:    []string{"sh", "-c", "curl '-u" + Redacted + "' https://example.com"},
			secrets: []string{"first", "second"},
		},
		{
			name:    "quoted shell proxy user tab",
			argv:    []string{"sh", "-c", "curl \"-Uadmin%first\tsecond\" https://example.com"},
			want:    []string{"sh", "-c", "curl '-U" + Redacted + "' https://example.com"},
			secrets: []string{"first", "second"},
		},
		{
			name:    "sshpass numeric password",
			argv:    []string{"sshpass", "-p", "123456", "ssh", "example.com"},
			want:    []string{"sshpass", "-p", Redacted, "ssh", "example.com"},
			secrets: []string{"123456"},
		},
		{
			name:    "shell numeric passwords",
			argv:    []string{"sh", "-c", "mysql -p 123456; sshpass -p 654321 ssh example.com"},
			want:    []string{"sh", "-c", "mysql -p " + Redacted + "; sshpass -p " + Redacted + " ssh example.com"},
			secrets: []string{"123456", "654321"},
		},
		{
			name: "docker ports",
			argv: []string{"docker", "run", "-p", "8080:80", "-p", "443", "nginx"},
			want: []string{"docker", "run", "-p", "8080:80", "-p", "443", "nginx"},
		},
		{
			name: "mysql port",
			argv: []string{"mysql", "-P", "3306"},
			want: []string{"mysql", "-P", "3306"},
		},
	}
	for program := range mysqlLike {
		cases = append(cases, struct {
			name    string
			argv    []string
			want    []string
			secrets []string
		}{program + " numeric password", []string{program, "-p", "123456"}, []string{program, "-p", Redacted}, []string{"123456"}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactArgs(tc.argv); !slices.Equal(got, tc.want) {
				t.Errorf("RedactArgs = %q, want %q", got, tc.want)
			}
			f := newFixture(t)
			_, err := f.kube.CoreV1().Pods("default").Create(context.Background(), &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: "default"},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "c", Image: "test", Command: tc.argv[:1], Args: tc.argv[1:],
				}}},
			}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			s, err := Collect(context.Background(), f.clients(), Options{Now: fixedNow, ConfigRefs: noRefs})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, pod := range s.Pods {
				if pod.Name == "credentials" {
					found = true
					c := pod.Spec.Containers[0]
					if got := append(c.Command, c.Args...); !slices.Equal(got, tc.want) {
						t.Errorf("collected argv = %q, want %q", got, tc.want)
					}
				}
			}
			if !found {
				t.Fatal("credential pod missing from snapshot")
			}
			data, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range tc.secrets {
				if strings.Contains(string(data), secret) {
					t.Errorf("serialized snapshot kept credential fragment %q", secret)
				}
			}
		})
	}
}
