package detect

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func childRelease(name string, conditions ...map[string]any) *unstructured.Unstructured {
	hr := &unstructured.Unstructured{Object: map[string]any{}}
	hr.SetName(name)
	hr.SetNamespace("org-acme")
	if len(conditions) > 0 {
		list := make([]any, len(conditions))
		for i, c := range conditions {
			list[i] = c
		}
		_ = unstructured.SetNestedSlice(hr.Object, list, "status", "conditions")
	}
	return hr
}

func readyCondition(status, reason, message string) map[string]any {
	return map[string]any{"type": "Ready", "status": status, "reason": reason, "message": message}
}

func TestChildEvidenceRendersTheConditionStatus(t *testing.T) {
	children := []ReleaseState{
		NewReleaseState(childRelease("a-progressing", readyCondition("Unknown", "Progressing", "Fulfilling prerequisites"))),
		NewReleaseState(childRelease("b-failed", readyCondition("False", "InstallFailed", "Helm install failed"))),
		NewReleaseState(childRelease("c-new")),
		NewReleaseState(childRelease("d-ready", readyCondition("True", "InstallSucceeded", ""))),
	}

	assert.Equal(t, []string{
		"HelmRelease org-acme/a-progressing not Ready (Ready=Unknown [Progressing] Fulfilling prerequisites)",
		"HelmRelease org-acme/b-failed not Ready (Ready=False [InstallFailed] Helm install failed)",
		"HelmRelease org-acme/c-new not Ready (no Ready condition yet)",
	}, childEvidence(children))
}
