package k8s

import "testing"

func TestGitOpsManager(t *testing.T) {
	cases := []struct {
		name        string
		labels      map[string]string
		annotations map[string]string
		want        string
	}{
		{"none", nil, nil, ""},
		{"argocd label", map[string]string{"argocd.argoproj.io/instance": "web"}, nil, "argocd"},
		{"argocd annotation", nil, map[string]string{"argocd.argoproj.io/tracking-id": "web:apps/Deployment:api/web"}, "argocd"},
		{"flux kustomize", map[string]string{"kustomize.toolkit.fluxcd.io/name": "apps"}, nil, "flux"},
		{"flux helm", map[string]string{"helm.toolkit.fluxcd.io/name": "release"}, nil, "flux"},
		{"helm annotation", nil, map[string]string{"meta.helm.sh/release-name": "web"}, "helm"},
		{"managed-by helm", map[string]string{"app.kubernetes.io/managed-by": "Helm"}, nil, "helm"},
		{"plain app labels not gitops", map[string]string{"app.kubernetes.io/name": "web"}, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := GitOpsManager(c.labels, c.annotations); got != c.want {
				t.Fatalf("GitOpsManager = %q, want %q", got, c.want)
			}
		})
	}
}
