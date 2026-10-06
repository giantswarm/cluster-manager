package detect

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Resources the webhook readiness reads on the target.
var (
	// MutatingWebhookGVR and ValidatingWebhookGVR are the admission webhook
	// configurations: the llmisvc controller's admit every
	// LLMInferenceService model-manager composes.
	MutatingWebhookGVR   = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "mutatingwebhookconfigurations"}
	ValidatingWebhookGVR = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingwebhookconfigurations"}
	// EndpointSliceGVR is the EndpointSlices: a Service's addresses, each
	// with its readiness.
	EndpointSliceGVR = schema.GroupVersionResource{Group: "discovery.k8s.io", Version: "v1", Resource: "endpointslices"}
)

const (
	// LabelServiceName is the label on an EndpointSlice naming its Service.
	LabelServiceName = "kubernetes.io/service-name"
	// llmisvcGroup and llmisvcResource are the admission rule of the webhooks
	// the readiness looks for: the ones admitting LLMInferenceServices.
	llmisvcGroup    = "serving.kserve.io"
	llmisvcResource = "llminferenceservices"
)

// The reasons of a WebhookState that does not serve.
const (
	// WebhookNoConfiguration: no webhook configuration admits
	// LLMInferenceServices yet — the controller's release has not installed
	// them.
	WebhookNoConfiguration = "NoConfiguration"
	// WebhookNoEndpoints: the webhook Service has no ready endpoint.
	WebhookNoEndpoints = "NoEndpoints"
	// WebhookNoCABundle: a webhook's caBundle is not injected yet.
	WebhookNoCABundle = "NoCABundle"
)

// WebhookState is the llmisvc admission webhook's readiness: the webhook
// configurations admitting LLMInferenceServices and the Service they call.
// Ready only when the Service has a ready endpoint and every configuration
// carries its CA bundle. The controller's HelmRelease reads Ready once Helm's
// install finished and its Deployment counts an available replica before the
// webhook Service has an endpoint and cert-manager has injected the bundle;
// a load_model in that window fails admission with "failed calling webhook"
// (giantswarm/cluster-manager#166).
type WebhookState struct {
	// Configurations names the webhook configurations admitting
	// LLMInferenceServices (`<resource>/<name>`), sorted; empty while the
	// controller's release has installed none.
	Configurations []string `json:"configurations"`
	// Namespace and Service are the webhook Service the configurations call,
	// empty without a configuration.
	Namespace string `json:"namespace,omitempty"`
	Service   string `json:"service,omitempty"`
	// Endpoints counts the Service's ready addresses.
	Endpoints int `json:"endpoints"`
	// Ready is true when the Service has a ready endpoint and every
	// configuration carries its CA bundle; null when the endpoints cannot be
	// read, with Message saying why.
	Ready *bool `json:"ready"`
	// Reason is the first of what holds the webhook back, empty while it
	// serves.
	Reason string `json:"reason,omitempty"`
	// Message says what holds the webhook back, empty while it serves.
	Message string `json:"message,omitempty"`
}

// webhookService is the Service a webhook's clientConfig calls.
type webhookService struct {
	namespace, name string
}

func (s webhookService) String() string { return "Service " + s.namespace + "/" + s.name }

// llmisvcWebhook reads the llmisvc admission webhook's readiness on the
// target: the mutating and validating webhook configurations admitting
// LLMInferenceServices, their Service's ready endpoints and their CA bundles.
// Nil when the configurations cannot be listed.
func llmisvcWebhook(ctx context.Context, reader dynamic.Interface) *WebhookState {
	state := &WebhookState{Configurations: []string{}}
	var services []webhookService
	var unsigned []string
	for _, gvr := range []schema.GroupVersionResource{MutatingWebhookGVR, ValidatingWebhookGVR} {
		configs, err := list(ctx, reader, gvr, metav1.NamespaceAll, "")
		if err != nil {
			return nil
		}
		for i := range configs.Items {
			cfg := &configs.Items[i]
			hooks, _, _ := unstructured.NestedSlice(cfg.Object, "webhooks")
			admits := false
			for _, h := range hooks {
				hook, _ := h.(map[string]any)
				if !admitsLLMISVC(hook) {
					continue
				}
				admits = true
				namespace, _, _ := unstructured.NestedString(hook, "clientConfig", "service", "namespace")
				name, _, _ := unstructured.NestedString(hook, "clientConfig", "service", "name")
				if svc := (webhookService{namespace, name}); !slices.Contains(services, svc) {
					services = append(services, svc)
				}
				if bundle, _, _ := unstructured.NestedString(hook, "clientConfig", "caBundle"); bundle == "" {
					hookName, _ := hook["name"].(string)
					unsigned = append(unsigned, cfg.GetName()+"/"+hookName)
				}
			}
			if admits {
				state.Configurations = append(state.Configurations, gvr.Resource+"/"+cfg.GetName())
			}
		}
	}
	sort.Strings(state.Configurations)
	if len(services) == 0 {
		notReady := false
		state.Ready, state.Reason, state.Message = &notReady, WebhookNoConfiguration, "no webhook configuration admits LLMInferenceServices yet"
		return state
	}
	state.Namespace, state.Service = services[0].namespace, services[0].name
	var reasons, details []string
	for _, svc := range services {
		eps, err := list(ctx, reader, EndpointSliceGVR, svc.namespace, LabelServiceName+"="+svc.name)
		if err != nil {
			state.Message = fmt.Sprintf("EndpointSlices of %s not readable: %v", svc, err)
			return state
		}
		ready, total := readyEndpoints(eps)
		state.Endpoints += ready
		switch {
		case total == 0:
			reasons, details = append(reasons, WebhookNoEndpoints), append(details, svc.String()+" has no endpoint yet")
		case ready == 0:
			reasons, details = append(reasons, WebhookNoEndpoints), append(details, fmt.Sprintf("%s has no ready endpoint (0 of %d ready)", svc, total))
		}
	}
	if len(unsigned) > 0 {
		reasons, details = append(reasons, WebhookNoCABundle), append(details, "caBundle not injected yet on "+strings.Join(unsigned, ", "))
	}
	ready := len(details) == 0
	state.Ready = &ready
	if !ready {
		state.Reason, state.Message = reasons[0], strings.Join(details, "; ")
	}
	return state
}

// evidence names the webhook in the serving component's evidence while it
// holds the layer back or cannot be read; empty while it serves.
func (w *WebhookState) evidence() string {
	if w == nil || w.Message == "" {
		return ""
	}
	if w.Ready == nil {
		return "llmisvc webhook readiness unknown (" + w.Message + ")"
	}
	return "llmisvc webhook not ready (" + w.Message + ")"
}

// admitsLLMISVC reports whether a webhook's rules cover LLMInferenceServices.
func admitsLLMISVC(hook map[string]any) bool {
	rules, _, _ := unstructured.NestedSlice(hook, "rules")
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		groups, _, _ := unstructured.NestedStringSlice(rule, "apiGroups")
		resources, _, _ := unstructured.NestedStringSlice(rule, "resources")
		if covers(groups, llmisvcGroup) && covers(resources, llmisvcResource) {
			return true
		}
	}
	return false
}

// covers reports whether a rule's list names the item or everything.
func covers(list []string, item string) bool {
	return slices.Contains(list, item) || slices.Contains(list, "*")
}

// readyEndpoints counts the addresses of a Service's EndpointSlices and how
// many of them are ready: an endpoint without a ready condition is ready, as
// the EndpointSlice API has consumers read it.
func readyEndpoints(eps *unstructured.UnstructuredList) (ready, total int) {
	for i := range eps.Items {
		endpoints, _, _ := unstructured.NestedSlice(eps.Items[i].Object, "endpoints")
		for _, e := range endpoints {
			endpoint, _ := e.(map[string]any)
			addresses, _, _ := unstructured.NestedStringSlice(endpoint, "addresses")
			if len(addresses) == 0 {
				continue
			}
			total++
			if isReady, found, _ := unstructured.NestedBool(endpoint, "conditions", "ready"); !found || isReady {
				ready++
			}
		}
	}
	return ready, total
}
