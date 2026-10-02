package compose

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/cluster-manager/internal/registry"
)

// A workload cluster is a Flux HelmRelease of the provider's release chart in
// the organization's namespace (giantswarm/giantswarm#37614): the releases
// repository pulls `cluster-<provider>` at the version a Release CR pins,
// renames it `release-<provider>`, versions it as the Release and pushes it
// beside the other charts. One number, the OCIRepository's tag, pins the
// cluster chart and every default app; the person's values are a ConfigMap
// the release reads through valuesFrom, after the installation's.
const (
	// ReleaseChartRepository is where the release charts are published.
	ReleaseChartRepository = "oci://gsoci.azurecr.io/charts/giantswarm"
	// ReleaseChartPrefix and ClusterChartPrefix name a provider's release
	// chart and the cluster chart its Release CR pins.
	ReleaseChartPrefix = "release-"
	ClusterChartPrefix = "cluster-"
	// InstallationValuesConfigMap is the installation's defaults for every
	// cluster, synced into every org namespace: first in a cluster release's
	// valuesFrom, so the person's values win over it.
	InstallationValuesConfigMap = "cluster-app-installation-values"
	// ClusterValuesSuffix names the ConfigMap of a cluster's own values.
	ClusterValuesSuffix = "-values"
	// LabelOrganization marks an object with the organization it belongs
	// to.
	LabelOrganization = "giantswarm.io/organization"
	// MaxClusterNameLength is the cluster charts' cap on
	// `global.metadata.name`.
	MaxClusterNameLength = 20

	valuesSchemaFile = "values.schema.json"
	valuesFile       = "values.yaml"

	// valuesGlobal is the cluster charts' top-level values key.
	valuesGlobal = "global"
)

// ClusterNamePattern is a cluster name's shape: a DNS label starting with a
// letter, at most MaxClusterNameLength characters (the cluster charts' cap;
// the name prefixes every cloud resource of the cluster).
var ClusterNamePattern = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,18}[a-z0-9])?$`)

// identityPaths is where each provider line's cluster chart takes the cloud
// identity the cluster runs under, by name: the identity object exists on
// the installation, no credential passes through cluster-manager. The lines
// listed are the ones create_cluster offers.
var identityPaths = map[string][]string{
	"aws":   {valuesGlobal, "providerSpecific", "awsClusterRoleIdentityName"},
	"eks":   {valuesGlobal, "providerSpecific", "awsClusterRoleIdentityName"},
	"azure": {valuesGlobal, "providerSpecific", "azureClusterIdentity", "name"},
}

// Providers are the provider lines create_cluster offers, sorted.
func Providers() []string {
	out := make([]string, 0, len(identityPaths))
	for p := range identityPaths {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ReleaseChartURL is the release chart of a provider line.
func ReleaseChartURL(provider string) string {
	return ReleaseChartRepository + "/" + ReleaseChartPrefix + provider
}

// ClusterValuesName names the ConfigMap of a cluster's own values.
func ClusterValuesName(cluster string) string { return cluster + ClusterValuesSuffix }

// ClusterSpec is a new workload cluster: the caller's choices and the
// release they resolved to.
type ClusterSpec struct {
	Organization string
	Name         string
	Provider     string
	// Release is the release version (`36.0.0`), the release chart's tag.
	Release string
	// Identity names the provider's cloud identity object; empty keeps the
	// chart's default.
	Identity    string
	Description string
	// Values are the caller's values, under the composed ones.
	Values map[string]any
	// TenantServiceAccount is the org namespace's tenant ServiceAccount
	// the release runs under (deliverAsTenant).
	TenantServiceAccount string
}

// Namespace is the organization's namespace, where the cluster lives.
func (s ClusterSpec) Namespace() string { return "org-" + s.Organization }

// Validate checks the caller's choices before anything is read for them.
func (s ClusterSpec) Validate() error {
	if !ClusterNamePattern.MatchString(s.Name) {
		return fmt.Errorf("cluster name %q: must be a DNS label of at most %d characters starting with a letter (lowercase letters, digits and dashes, no dash at the end)", s.Name, MaxClusterNameLength)
	}
	if s.Organization == "" {
		return fmt.Errorf("organization: required, the cluster lives in its org- namespace")
	}
	if _, ok := identityPaths[s.Provider]; !ok {
		return fmt.Errorf("provider %q: create_cluster offers %s", s.Provider, strings.Join(Providers(), ", "))
	}
	return nil
}

// ClusterValues are the cluster's own values: the caller's, with the name,
// organization, description, release version and identity composed on top.
// A caller value that contradicts a composed one is refused, naming the
// argument that sets it, rather than silently overridden.
func ClusterValues(s ClusterSpec) (map[string]any, error) {
	values, _ := deepCopy(s.Values).(map[string]any)
	if values == nil {
		values = map[string]any{}
	}
	type composed struct {
		path  []string
		value string
		arg   string
	}
	set := []composed{
		{[]string{valuesGlobal, fieldMetadata, "name"}, s.Name, "name"},
		{[]string{valuesGlobal, fieldMetadata, "organization"}, s.Organization, "organization"},
		{[]string{valuesGlobal, "release", "version"}, s.Release, "release"},
	}
	if s.Description != "" {
		set = append(set, composed{[]string{valuesGlobal, fieldMetadata, "description"}, s.Description, "description"})
	}
	if s.Identity != "" {
		set = append(set, composed{identityPaths[s.Provider], s.Identity, "identity"})
	}
	for _, f := range set {
		if have, found, _ := unstructured.NestedFieldNoCopy(values, f.path...); found && have != f.value {
			return nil, fmt.Errorf("values: %s is %v, but the %s argument sets it to %q — leave it out of values", strings.Join(f.path, "."), have, f.arg, f.value)
		}
		if err := unstructured.SetNestedField(values, f.value, f.path...); err != nil {
			return nil, fmt.Errorf("values: %s: %w", strings.Join(f.path, "."), err)
		}
	}
	return values, nil
}

// ClusterRelease renders the cluster: the OCIRepository of the release chart
// at the release's tag, the ConfigMap of the cluster's values and the
// HelmRelease reading the installation's values and then the cluster's. The
// release runs under the org's tenant ServiceAccount: the cluster's objects
// live in the org namespace on the installation.
func ClusterRelease(s ClusterSpec, values map[string]any) ([]*unstructured.Unstructured, error) {
	raw, err := yaml.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("encode the cluster's values: %w", err)
	}
	meta := objectMeta(Cluster{Name: s.Name, Namespace: s.Namespace()}, map[string]any{
		LabelChartName:    ReleaseChartPrefix + s.Provider,
		LabelManagedBy:    ManagedBy,
		LabelCluster:      s.Name,
		LabelOrganization: s.Organization,
	})
	source := object(OCIRepositoryGVR, "OCIRepository", meta(s.Name), map[string]any{"spec": ociRepositorySpec(ReleaseChartURL(s.Provider), "tag", s.Release)})
	config := object(ConfigMapGVR, "ConfigMap", meta(ClusterValuesName(s.Name)), map[string]any{"data": map[string]any{ValuesSecretKey: string(raw)}})
	spec := helmReleaseSpec(s.Name, false, nil)
	delete(spec, "values")
	spec["valuesFrom"] = []any{
		map[string]any{"kind": "ConfigMap", "name": InstallationValuesConfigMap, "valuesKey": ValuesSecretKey},
		map[string]any{"kind": "ConfigMap", "name": config.GetName(), "valuesKey": ValuesSecretKey},
	}
	deliverAsTenant(spec, s.TenantServiceAccount)
	return []*unstructured.Unstructured{source, config, object(HelmReleaseGVR, "HelmRelease", meta(s.Name), map[string]any{"spec": spec})}, nil
}

// ValidateClusterValues checks the values the release would install against
// the release chart's own values.schema.json, the way Helm does at install:
// the chart's defaults, under the installation's values, under the
// cluster's. Every violation is named with its path. A chart without a
// schema is refused: there is nothing to check a typo against.
func ValidateClusterValues(chart *registry.Chart, installation, cluster map[string]any) error {
	rawSchema := chart.File(valuesSchemaFile)
	if rawSchema == nil {
		return fmt.Errorf("%s %s ships no %s: the values cannot be checked", chart.Ref, chart.Version, valuesSchemaFile)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(rawSchema))
	if err != nil {
		return fmt.Errorf("%s %s: read %s: %w", chart.Ref, chart.Version, valuesSchemaFile, err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(valuesSchemaFile, doc); err != nil {
		return fmt.Errorf("%s %s: %s: %w", chart.Ref, chart.Version, valuesSchemaFile, err)
	}
	schema, err := c.Compile(valuesSchemaFile)
	if err != nil {
		return fmt.Errorf("%s %s: compile %s: %w", chart.Ref, chart.Version, valuesSchemaFile, err)
	}
	merged := map[string]any{}
	if raw := chart.File(valuesFile); raw != nil {
		if err := yaml.Unmarshal(raw, &merged); err != nil {
			return fmt.Errorf("%s %s: read %s: %w", chart.Ref, chart.Version, valuesFile, err)
		}
	}
	coalesce(merged, installation)
	coalesce(merged, cluster)
	// The schema validator takes JSON's types: a round trip turns the YAML
	// decoder's numbers into float64s and json.Numbers alike.
	j, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("encode the merged values: %w", err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(j))
	if err != nil {
		return fmt.Errorf("decode the merged values: %w", err)
	}
	if err := schema.Validate(instance); err != nil {
		var verr *jsonschema.ValidationError
		if errors.As(err, &verr) {
			return fmt.Errorf("the values do not match %s %s's schema: %s", chart.Ref, chart.Version, strings.Join(violations(verr), "; "))
		}
		return fmt.Errorf("validate the values against %s %s's schema: %w", chart.Ref, chart.Version, err)
	}
	return nil
}

var english = message.NewPrinter(language.English)

// violations flattens a validation error into one line per failing leaf:
// the instance path and what the schema wants there.
func violations(err *jsonschema.ValidationError) []string {
	if len(err.Causes) == 0 {
		path := "/" + strings.Join(err.InstanceLocation, "/")
		return []string{fmt.Sprintf("%s: %s", path, err.ErrorKind.LocalizedString(english))}
	}
	var out []string
	for _, cause := range err.Causes {
		out = append(out, violations(cause)...)
	}
	sort.Strings(out)
	return out
}

// coalesce merges src into dst the way Helm layers values: maps merge key
// by key, anything else replaces, and a null deletes the key.
func coalesce(dst, src map[string]any) {
	for k, v := range src {
		if v == nil {
			delete(dst, k)
			continue
		}
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				coalesce(dm, sm)
				continue
			}
		}
		dst[k] = deepCopy(v)
	}
}

// deepCopy copies the maps and lists of a decoded document, so a layer
// merged into it is never changed by a later one.
func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopy(e)
		}
		return out
	default:
		return v
	}
}
