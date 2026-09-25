package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/giantswarm/gitops-commit/provenance"
	"github.com/giantswarm/gitops-commit/sopsenc"
	yaml "go.yaml.in/yaml/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/giantswarm/cluster-manager/internal/identity"
)

// The Flux objects commit mode follows from a cluster to the repository that
// owns it.
var (
	KustomizationGVR = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
	GitRepositoryGVR = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"}
)

const (
	// labelKustomizeNamespace is the namespace of the Kustomization named by
	// labelKustomizeName.
	labelKustomizeNamespace = "kustomize.toolkit.fluxcd.io/namespace"

	// CommitDirectory is the directory cluster-manager writes a cluster's
	// releases to, under the directory its Kustomization builds from. It
	// carries its own kustomization.yaml listing every file in it, so Flux
	// builds it whether the parent directory has a kustomization.yaml (the
	// entry is added there) or not (Flux generates one that includes every
	// directory with a kustomization.yaml).
	CommitDirectory = "cluster-manager"

	kustomizationFile = "kustomization.yaml"
	sopsConfigFile    = ".sops.yaml"
)

// File actions of a commit.
const (
	fileAdd       = "add"
	fileUpdate    = "update"
	fileRemove    = "remove"
	fileUnchanged = "unchanged"
)

// GitHubRemote is what commit mode needs of GitHub: the pull request seams
// and the reads of the base (gitops-commit's Remote and Reader), acting as
// the person.
type GitHubRemote interface {
	commit.Remote
	commit.Reader
}

// RemoteFor builds the GitHub remote from the person's App user token.
type RemoteFor func(token string) (GitHubRemote, error)

// WithGitHub offers commit mode: the pull request is opened as the person
// with the GitHub token the App-pinned registration carries.
func WithGitHub(remote RemoteFor) Option {
	return func(s *Service) { s.remote = remote }
}

// CommitAvailable reports whether this server offers commit mode.
func (s *Service) CommitAvailable() bool { return s.remote != nil }

// CommitResult is the pull request a write in commit mode opened — or, dry
// run, would open — in the repository that owns the cluster.
type CommitResult struct {
	// Repository is owner/name, Base the branch the Kustomization follows.
	Repository string `json:"repository"`
	Base       string `json:"base"`
	// Directory is where the files go: CommitDirectory under the
	// Kustomization's path.
	Directory string `json:"directory"`
	// Kustomization is the Flux Kustomization (namespace/name) that lands
	// the files after the merge, and Prune its spec.prune: whether files
	// removed from git are removed from the installation too.
	Kustomization string `json:"kustomization"`
	Prune         bool   `json:"prune"`
	// Branch is the pull request's head branch.
	Branch string       `json:"branch"`
	Files  []CommitFile `json:"files"`
	// PullRequest is the URL of the pull request opened as the caller, empty
	// on a dry run and when nothing changes.
	PullRequest string `json:"pullRequest,omitempty"`
	Number      int    `json:"number,omitempty"`
	// Author is the GitHub login the pull request is opened as.
	Author string `json:"author,omitempty"`
	// LiveSteps are what the merge alone does not do on the installation.
	LiveSteps []string `json:"liveSteps,omitempty"`
}

// CommitFile is one file of the commit. Content is shown on a dry run,
// never for a secret file.
type CommitFile struct {
	Path    string `json:"path"`
	Action  string `json:"action"`
	Content string `json:"content,omitempty"`
}

// commitLocation is where a cluster's releases go in git and how Flux lands
// them.
type commitLocation struct {
	provenance.Location
	kustomization string
	prune         bool
}

// dir is the repository-relative directory cluster-manager writes to.
func (l commitLocation) dir() string {
	return path.Join(l.Directory, CommitDirectory)
}

// target is the answer's view of the location.
func (l commitLocation) target() *CommitTarget {
	return &CommitTarget{Repository: l.Repository.String(), Branch: l.Branch, Path: l.dir(), Kustomization: l.kustomization, Prune: l.prune}
}

// errNotFromGit is a cluster no Flux Kustomization owns.
var errNotFromGit = errors.New("not reconciled from git")

// commitLocationOf follows the cluster to the repository that owns it: the
// Flux Kustomization labels on the cluster's HelmRelease or App (named like
// the cluster), else on the Cluster itself, then the Kustomization's
// GitRepository and path. Read as the caller.
func commitLocationOf(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured) (commitLocation, error) {
	ns, name := c.GetNamespace(), c.GetName()
	owner := c
	for _, gvr := range []schema.GroupVersionResource{HelmReleaseGVR, AppGVR} {
		if obj, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
			owner = obj
			break
		}
	}
	ksName := owner.GetLabels()[labelKustomizeName]
	if ksName == "" {
		return commitLocation{}, fmt.Errorf("%w: no Flux Kustomization owns cluster %s/%s (no %s label on its %s)", errNotFromGit, ns, name, labelKustomizeName, owner.GetKind())
	}
	ksNamespace := owner.GetLabels()[labelKustomizeNamespace]
	ks, err := dyn.Resource(KustomizationGVR).Namespace(ksNamespace).Get(ctx, ksName, metav1.GetOptions{})
	if err != nil {
		return commitLocation{}, fmt.Errorf("get Kustomization %s/%s owning cluster %s: %w", ksNamespace, ksName, name, err)
	}
	src := provenance.SourceRef{
		Kind:      nestedString(ks, "spec", "sourceRef", "kind"),
		Name:      nestedString(ks, "spec", "sourceRef", "name"),
		Namespace: nestedString(ks, "spec", "sourceRef", "namespace"),
	}
	flux := provenance.Flux{Kustomizations: []provenance.Kustomization{{Name: ksName, Namespace: ksNamespace, SourceRef: src, Path: nestedString(ks, "spec", "path")}}}
	if src.Kind == provenance.KindGitRepository {
		srcNamespace := src.Namespace
		if srcNamespace == "" {
			srcNamespace = ksNamespace
		}
		repo, err := dyn.Resource(GitRepositoryGVR).Namespace(srcNamespace).Get(ctx, src.Name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return commitLocation{}, fmt.Errorf("get GitRepository %s/%s: %w", srcNamespace, src.Name, err)
		}
		if err == nil {
			flux.GitRepositories = []provenance.GitRepository{{Name: src.Name, Namespace: srcNamespace, URL: nestedString(repo, "spec", "url"), Branch: nestedString(repo, "spec", "ref", "branch")}}
		}
	}
	loc, err := flux.Resolve(ksNamespace, ksName)
	if err != nil {
		return commitLocation{}, fmt.Errorf("cluster %s: %w", name, err)
	}
	prune, _, _ := unstructured.NestedBool(ks.Object, "spec", "prune")
	return commitLocation{Location: loc, kustomization: ksNamespace + "/" + ksName, prune: prune}, nil
}

// commitLocationFor is commitLocationOf for a write in commit mode: a
// cluster that is not reconciled from git is a refusal with the way out.
func commitLocationFor(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured) (commitLocation, error) {
	loc, err := commitLocationOf(ctx, dyn, c)
	if errors.Is(err, errNotFromGit) {
		return loc, &ErrRefused{Reason: err.Error() + ": commit mode writes into the repository that owns the cluster, and this one has none — use mode apply"}
	}
	return loc, err
}

// gitHubFor is the caller's GitHub remote, or the refusal naming the
// consent that gives one.
func (s *Service) gitHubFor(ctx context.Context) (GitHubRemote, *identity.GitHub, error) {
	gh, ok := identity.GitHubFromContext(ctx)
	if !ok {
		return nil, nil, &ErrRefused{Reason: "commit mode opens the pull request with your GitHub authorization, and this call carries none: connect cluster-manager in muster (core_auth_login server=cluster-manager), then call again"}
	}
	remote, err := s.remote(gh.Token)
	if err != nil {
		return nil, nil, commitError(err)
	}
	return remote, gh, nil
}

// commitError answers a token GitHub refused with the consent to renew.
func commitError(err error) error {
	var auth *commit.AuthError
	if errors.As(err, &auth) {
		return &ErrRefused{Reason: fmt.Sprintf("GitHub refused your token on %s (status %d): reconnect cluster-manager in muster (core_auth_login server=cluster-manager) — the App must be installed on the repository and your account must be allowed to push to it", auth.Op, auth.Status)}
	}
	return err
}

// releaseFiles renders the composed objects as the files of the cluster's
// directory: one file per release (its source and the HelmRelease, named
// after them), a Secret in a file of its own named as a secret file.
func releaseFiles(dir string, objs []*unstructured.Unstructured) (map[string][]byte, error) {
	docs := map[string][][]byte{}
	var order []string
	for _, obj := range objs {
		name := obj.GetName() + ".yaml"
		if obj.GetKind() == "Secret" {
			name = obj.GetName() + "-secret.enc.yaml"
		}
		p := path.Join(dir, name)
		raw, err := sigsyaml.Marshal(obj.Object)
		if err != nil {
			return nil, fmt.Errorf("render %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
		if _, seen := docs[p]; !seen {
			order = append(order, p)
		}
		docs[p] = append(docs[p], raw)
	}
	files := make(map[string][]byte, len(order))
	for _, p := range order {
		files[p] = bytes.Join(docs[p], []byte("---\n"))
	}
	return files, nil
}

// commitPlan is the change set of one write: the files by path (nil
// content: removed), their actions, and the rendered secret paths.
type commitPlan struct {
	loc     commitLocation
	remote  GitHubRemote
	files   map[string][]byte
	actions []CommitFile
}

// readBase reads a file at the base branch; nil without error when absent.
func readBase(ctx context.Context, remote GitHubRemote, loc commitLocation, p string) ([]byte, error) {
	raw, err := remote.ReadFile(ctx, loc.Repository, loc.Branch, p)
	if errors.Is(err, commit.ErrFileNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, commitError(err)
	}
	return raw, nil
}

// planCommit decides every file of the change against the base: written
// files (add, update or unchanged — a secret file that exists is left as it
// is, never re-encrypted), removed files that exist, and the kustomization
// entries: the directory's kustomization.yaml lists every file in it, and
// the parent's kustomization.yaml, where there is one, lists the directory.
// Secret files are encrypted for the recipients of the repository's
// .sops.yaml.
func planCommit(ctx context.Context, remote GitHubRemote, loc commitLocation, write map[string][]byte, remove []string) (*commitPlan, error) {
	dir := loc.dir()
	plan := &commitPlan{loc: loc, remote: remote, files: map[string][]byte{}}
	base := map[string][]byte{}
	read := func(p string) ([]byte, error) {
		if raw, ok := base[p]; ok {
			return raw, nil
		}
		raw, err := readBase(ctx, remote, loc, p)
		base[p] = raw
		return raw, err
	}
	var secrets []sopsenc.File
	for _, p := range slices.Sorted(maps.Keys(write)) {
		old, err := read(p)
		if err != nil {
			return nil, err
		}
		if sopsenc.IsSecretFile(p) {
			if old != nil {
				plan.actions = append(plan.actions, CommitFile{Path: p, Action: fileUnchanged})
				continue
			}
			secrets = append(secrets, sopsenc.File{Path: p, Content: write[p]})
			continue
		}
		plan.put(p, old, write[p])
	}
	if len(secrets) > 0 {
		if err := plan.encrypt(read, secrets); err != nil {
			return nil, err
		}
	}
	for _, p := range remove {
		old, err := read(p)
		if err != nil {
			return nil, err
		}
		if old != nil {
			plan.files[p] = nil
			plan.actions = append(plan.actions, CommitFile{Path: p, Action: fileRemove})
		}
	}
	// The directory's own kustomization.yaml: every file it keeps.
	kpath := path.Join(dir, kustomizationFile)
	old, err := read(kpath)
	if err != nil {
		return nil, err
	}
	resources, err := kustomizationResources(old)
	if err != nil {
		return nil, fmt.Errorf("%s in %s: %w", kpath, loc.Repository, err)
	}
	for p, content := range plan.files {
		name := strings.TrimPrefix(p, dir+"/")
		if content == nil {
			resources = slices.DeleteFunc(resources, func(r string) bool { return r == name })
		} else if !slices.Contains(resources, name) {
			resources = append(resources, name)
		}
	}
	for _, f := range plan.actions {
		if name := strings.TrimPrefix(f.Path, dir+"/"); f.Action == fileUnchanged && !slices.Contains(resources, name) {
			resources = append(resources, name)
		}
	}
	slices.Sort(resources)
	emptied := len(resources) == 0
	if emptied {
		if old != nil {
			plan.files[kpath] = nil
			plan.actions = append(plan.actions, CommitFile{Path: kpath, Action: fileRemove})
		}
	} else {
		updated, err := withResources(old, resources)
		if err != nil {
			return nil, fmt.Errorf("%s in %s: %w", kpath, loc.Repository, err)
		}
		plan.put(kpath, old, updated)
	}
	// The parent's kustomization.yaml, when there is one, lists the
	// directory while it has files.
	ppath := path.Join(loc.Directory, kustomizationFile)
	parent, err := read(ppath)
	if err != nil {
		return nil, err
	}
	if parent != nil {
		entries, err := kustomizationResources(parent)
		if err != nil {
			return nil, fmt.Errorf("%s in %s: %w", ppath, loc.Repository, err)
		}
		has := slices.ContainsFunc(entries, isCommitDirectory)
		switch {
		case !emptied && !has:
			updated, err := withResources(parent, append(entries, CommitDirectory))
			if err != nil {
				return nil, fmt.Errorf("%s in %s: %w", ppath, loc.Repository, err)
			}
			plan.put(ppath, parent, updated)
		case emptied && has:
			updated, err := withResources(parent, slices.DeleteFunc(entries, isCommitDirectory))
			if err != nil {
				return nil, fmt.Errorf("%s in %s: %w", ppath, loc.Repository, err)
			}
			plan.put(ppath, parent, updated)
		}
	}
	slices.SortFunc(plan.actions, func(a, b CommitFile) int { return strings.Compare(a.Path, b.Path) })
	return plan, nil
}

func isCommitDirectory(r string) bool {
	return strings.TrimSuffix(strings.TrimPrefix(r, "./"), "/") == CommitDirectory
}

// put records a written file against its base content.
func (p *commitPlan) put(path string, old, content []byte) {
	switch {
	case old == nil:
		p.files[path] = content
		p.actions = append(p.actions, CommitFile{Path: path, Action: fileAdd, Content: string(content)})
	case bytes.Equal(old, content):
		p.actions = append(p.actions, CommitFile{Path: path, Action: fileUnchanged})
	default:
		p.files[path] = content
		p.actions = append(p.actions, CommitFile{Path: path, Action: fileUpdate, Content: string(content)})
	}
}

// encrypt adds the new secret files, encrypted for the recipients the
// repository's .sops.yaml names for their paths.
func (p *commitPlan) encrypt(read func(string) ([]byte, error), secrets []sopsenc.File) error {
	cfg, err := read(sopsConfigFile)
	if err != nil {
		return err
	}
	paths := make([]string, len(secrets))
	for i, f := range secrets {
		paths[i] = f.Path
	}
	refuse := func(why string) error {
		return &ErrRefused{Reason: fmt.Sprintf("%s holds the pool's registry credentials, and %s: cluster-manager commits a secret only encrypted for the repository's age recipients — use mode apply, or give the path an age creation rule", strings.Join(paths, ", "), why)}
	}
	if cfg == nil {
		return refuse(p.loc.Repository.String() + " has no " + sopsConfigFile)
	}
	enc, err := sopsenc.New(cfg)
	if err != nil {
		return refuse(fmt.Sprintf("its %s cannot be used (%v)", sopsConfigFile, err))
	}
	out, err := enc.Encrypt(secrets, func(string) bool { return false })
	if err != nil {
		return refuse(fmt.Sprintf("its %s does not cover them (%v)", sopsConfigFile, err))
	}
	for _, f := range secrets {
		p.files[f.Path] = out[f.Path]
		p.actions = append(p.actions, CommitFile{Path: f.Path, Action: fileAdd})
	}
	return nil
}

// changed reports whether the plan writes or removes anything.
func (p *commitPlan) changed() bool { return len(p.files) > 0 }

// open lands the plan as one pull request as the caller, or reports it on a
// dry run. Nothing to change opens none.
func (p *commitPlan) open(ctx context.Context, gh *identity.GitHub, branch, title, body string, dryRun bool) (*CommitResult, error) {
	out := &CommitResult{
		Repository: p.loc.Repository.String(), Base: p.loc.Branch, Directory: p.loc.dir(),
		Kustomization: p.loc.kustomization, Prune: p.loc.prune, Branch: branch, Files: p.actions, Author: gh.Login,
	}
	if !dryRun {
		for i := range out.Files {
			out.Files[i].Content = ""
		}
	}
	if dryRun || !p.changed() {
		return out, nil
	}
	prs, err := commit.Open(ctx, p.remote, commit.Request{Branch: branch, Title: title, Body: body}, []commit.Change{{Location: p.loc.Location, Files: p.files}})
	if err != nil {
		return nil, commitError(err)
	}
	out.PullRequest, out.Number = prs[0].URL, prs[0].Number
	return out, nil
}

// kustomizationResources is the resources list of a kustomization.yaml
// (nil content: none).
func kustomizationResources(raw []byte) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	var k struct {
		Resources []string `json:"resources"`
	}
	if err := sigsyaml.Unmarshal(raw, &k); err != nil {
		return nil, err
	}
	return k.Resources, nil
}

// withResources is the kustomization.yaml with its resources list replaced,
// every other key and comment kept; a new file for nil content.
func withResources(raw []byte, resources []string) ([]byte, error) {
	var doc yaml.Node
	if raw == nil {
		raw = []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n")
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("not a YAML mapping")
	}
	root := doc.Content[0]
	// An entry that stays keeps its node, and so its comments.
	kept := map[string]*yaml.Node{}
	var old *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "resources" {
			old = root.Content[i+1]
			for _, n := range old.Content {
				kept[n.Value] = n
			}
		}
	}
	list := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, r := range resources {
		n, ok := kept[r]
		if !ok {
			n = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: r}
		}
		list.Content = append(list.Content, n)
	}
	replaced := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "resources" {
			list.Style, list.HeadComment, list.LineComment, list.FootComment = old.Style, old.HeadComment, old.LineComment, old.FootComment
			root.Content[i+1] = list
			replaced = true
		}
	}
	if !replaced {
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "resources"}, list)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
